package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"start/until"
)

// 升级服务（启动器）—— 容器 PID 1。
//
// v3.2.3：在线升级的前端整包替换改为**纯文件级**落盘（原实现是目录级
// rename，生产环境报 invalid cross-device link —— /app/web 是独立挂载点
// 或来自镜像层，rename 跨设备必然失败）。见 until/webbundle.go。
var VERSION = "v3.2.3"

// 环境预检标记文件：镜像里可能还有旧版 iptv，它会检查这两个文件来
// 跳过重复检查。新版 iptv 走 IPTV_PRECHECKED 环境变量，两条路都留着。
const (
	flagPrivileged = "/tmp/check_privileged"
	flagRam        = "/tmp/check_start_ram"
)

type app struct {
	cfg *until.Config

	mu        sync.Mutex
	installed bool
	api       *until.Child
	engine    *until.Child
	nginx     *until.Child
	lastUpd   string

	startedAt time.Time
}

func main() {
	// -version 必须在**任何检查之前**处理并退出，理由与 api/引擎一致：
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(VERSION)
		return
	}

	log.SetFlags(log.LstdFlags)

	log.Println("升级服务版本号:", VERSION)
	a := &app{cfg: until.LoadConfig(), startedAt: time.Now()}
	log.Printf("运行环境: 架构=%s 运行目录=%s 数据目录=%s 对外端口=%s 后端端口=%s 管理接口=%s",
		until.GoArch(), a.cfg.BinDir, a.cfg.ConfDir, a.cfg.NginxPort, a.cfg.APIPort, a.cfg.AdminAddr)

	if err := a.init(); err != nil {
		log.Println("启动终止:", err)
		os.Exit(1)
	}

	a.startChildren()
	a.serveAdmin()

	a.waitSignal()
	a.shutdown()
}

// 启动前检查

// init 完成"能不能启动"的全部判断与一次性准备工作。
func (a *app) init() error {
	cfg := a.cfg

	// 镜像完整性：缺二进制没有继续的意义
	if !until.Exists(cfg.WebBin()) {
		return fmt.Errorf("镜像不完整，缺少 %s，请重新拉取镜像", cfg.WebBin())
	}
	if !cfg.NoEngine && !until.Exists(cfg.EngineBin()) {
		return fmt.Errorf("镜像不完整，缺少 %s，请重新拉取镜像", cfg.EngineBin())
	}
	// nginx 与前端产物是一体的：缺了任何一个，对外就是白屏。
	// 这里提前失败，好过用户看到"容器 Up、首页 404"。
	if !cfg.NoNginx {
		if !until.Exists(cfg.NginxBin()) {
			return fmt.Errorf("镜像不完整，缺少 %s，请重新拉取镜像", cfg.NginxBin())
		}
		if !until.Exists(cfg.NginxConf()) {
			return fmt.Errorf("镜像不完整，缺少 %s，请重新拉取镜像", cfg.NginxConf())
		}
		if !until.Exists(cfg.WebDir() + "/index.html") {
			return fmt.Errorf("镜像不完整，缺少前端产物 %s/index.html，请重新拉取镜像", cfg.WebDir())
		}
	}

	if !cfg.SkipPrivCheck {
		log.Println("运行环境检查...")
		if !until.IsPrivileged() {
			return errors.New("请使用 privileged(特权模式) 运行本镜像：docker run 加 --privileged，" +
				"compose 加 privileged: true（中转访问需要监听端口/做转发，普通容器无法提供）")
		}
	}
	os.MkdirAll(flagPrivileged, 0o755)

	if until.CheckRam() {
		return errors.New("可用内存不足256MB，无法运行")
	}
	os.MkdirAll(flagRam, 0o755)

	if !until.Exists(cfg.ConfDir) {
		return fmt.Errorf("请映射config文件夹到容器 %s 中", cfg.ConfDir)
	}
	if err := os.Chmod(cfg.ConfDir, 0o777); err != nil {
		return fmt.Errorf("%s 文件夹权限设置失败,请手动设置", cfg.ConfDir)
	}
	if err := until.FixPerm(cfg.ConfDir); err != nil {
		return fmt.Errorf("%s 文件夹权限设置失败,请手动设置", cfg.ConfDir)
	}

	// 数据目录整棵刚被刷成 0777（含私钥），这里把证书目录单独收回来；
	// 同时把 /config/nginx 建好 —— nginx.conf 对 HTTPS 与 80 跳转用的是
	// 通配 include，提前有目录可扫，就不依赖"某个文件先被写出来"的启动顺序。
	if err := until.PrepareCertDir(cfg.ConfDir); err != nil {
		log.Println("证书目录准备失败（HTTPS 可能不可用）:", err)
	}
	if err := until.EnsureNginxDir(cfg.ConfDir); err != nil {
		log.Println("准备 nginx 配置目录失败（HTTPS 可能不可用）:", err)
	}

	// 编译 APK 这条链路的前置条件在这里查一次。
	if !cfg.SkipPreCheck {
		// 两个端口都要查：nginx 占对外口，iptv-api 占后端口。
		if !cfg.NoNginx && !until.CheckPort(cfg.NginxPort) {
			return fmt.Errorf("端口 %s 被占用", cfg.NginxPort)
		}
		if !until.CheckPort(cfg.APIPort) {
			return fmt.Errorf("端口 %s 被占用", cfg.APIPort)
		}
		if !until.CheckJava() {
			return errors.New("请安装大于Java JDK 1.8环境")
		}
		if !until.CheckApktool() {
			return errors.New("请安装apktool环境")
		}
		// 配置语法必须先过一遍：nginx 的语法错误会让它在启动瞬间退出，
		// 而监管循环只会看到"秒退"，日志里全是重启记录，看不出真正原因。
		if !cfg.NoNginx && !until.CheckNginx(cfg.NginxBin(), cfg.NginxConf()) {
			return errors.New("nginx 配置校验失败，请检查 nginx.conf")
		}
		os.Setenv("IPTV_PRECHECKED", "true")
	}

	a.refreshInstalled()
	a.applyPendingUpdate()

	webVer, frontVer, engineVer := until.VersionSummary(cfg.BinDir, cfg.WebDir(), cfg.EngineBin())
	if a.Installed() {
		log.Printf("系统已安装: 管理系统=%s 前端=%s 引擎=%s", webVer, frontVer, engineVer)
		// 有组件靠镜像内置清单兜底时单独提示一句：值本身不带标记（保持与正常读取一致），
		// 但排障要知道"这版本号是产物自己说的，还是清单兜的"。
		if fell := until.VersionFallbackNote(cfg.BinDir, cfg.WebDir(), cfg.EngineBin()); len(fell) > 0 {
			log.Printf("⚠️ 以下组件读不出自身版本号，已回落到镜像内置版本: %s", until.JoinList(fell))
		}
	} else {
		log.Println("检测到系统未安装：跳过引擎启动，安装完成后管理系统会自动拉起引擎")
	}
	return nil
}

// refreshInstalled 重新判定安装态（安装流程结束后需要刷新一次）。
func (a *app) refreshInstalled() bool {
	ok := until.IsInstalled(a.cfg.ConfDir)
	a.mu.Lock()
	a.installed = ok
	a.mu.Unlock()
	return ok
}

// Installed 返回当前安装态。
func (a *app) Installed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.installed
}

// applyPendingUpdate 在启动阶段维护运行目录里的产物，再应用"上次没来得及生效"的升级包。
func (a *app) applyPendingUpdate() {
	// 先把运行目录里的产物核对/恢复到镜像内置版本。
	// /app 是容器可写层，docker restart 抹不掉上一轮升级写进去的东西 ⇒
	// 产物被换坏或被降级后，只有重建容器才回得来。这一步把它变成自动的。
	if healed, err := until.SelfHealBinaries(a.cfg.BinDir); err != nil {
		log.Println("产物自愈失败:", err)
	} else if len(healed) > 0 {
		log.Printf("⚠️ 运行目录产物与镜像内置不一致，已用镜像副本恢复: %s", until.JoinList(healed))
	}
	// 再清掉低于镜像版本的陈旧升级包：留着它们只会每次启动都被判"降级"。
	if removed := until.CleanStaleUpdata(a.cfg.BinDir, a.cfg.UpdataDir()); len(removed) > 0 {
		log.Println("已清理低于镜像版本的陈旧升级包:", until.JoinList(removed))
	}

	apply, why := until.ShouldApplyUpdate(a.cfg.BinDir, a.cfg.WebDir(), a.cfg.UpdataDir())
	log.Println("检查待应用更新:", why)
	if !apply {
		return
	}
	applied, err := until.ApplyUpdate(a.cfg.BinDir, a.cfg.WebDir(), a.cfg.UpdataDir())
	if err != nil {
		log.Println("应用升级包失败:", err)
		return
	}
	if len(applied) == 0 {
		log.Println("升级包内没有可应用的文件，忽略")
		return
	}
	log.Println("已应用升级包:", until.JoinList(applied))
	a.setLastUpdate(applied)
}

func (a *app) setLastUpdate(applied []string) {
	a.mu.Lock()
	a.lastUpd = until.JoinList(applied)
	a.mu.Unlock()
}

// 子进程编排

// 启动顺序：引擎 → 后端 → nginx。
func (a *app) startChildren() {
	// 已安装才拉引擎。未安装时引擎没有可服务的数据，拉起来只会让
	if a.Installed() && !a.cfg.NoEngine {
		if err := a.ensureEngine(true); err != nil {
			// 引擎起不来不拖死整个容器：管理系统起不来才是彻底不可用。
			// 监管循环会持续退避重试，恢复情况在 /status 里可见。
			log.Println("引擎启动失败（监管将自动重试）:", err)
		}
	} else if a.cfg.NoEngine {
		log.Println("IPTV_NO_ENGINE=true，跳过引擎启动")
	}

	log.Println("启动 IPTV...")
	api := a.newAPIClild()
	if err := api.Start(); err != nil {
		log.Println("IPTV 启动失败（监管将自动重试）:", err)
	} else {
		a.setAPI(api)
	}

	if !a.cfg.NoNginx {
		log.Println("启动 nginx...")
		ng := a.newNginxChild()
		if err := ng.Start(); err != nil {
			log.Println("nginx 启动失败（监管将自动重试）:", err)
		} else {
			a.setNginx(ng)
		}
	} else {
		log.Println("IPTV_NO_NGINX=true，跳过 nginx 启动（前端站点将不可达）")
	}

	log.Println("启动完成:", a.summary())
}

func (a *app) newAPIClild() *until.Child {
	// 端口与绑定地址都交给 iptv 自己解析（-port / -host）。
	c := until.NewChild("IPTV", a.cfg.WebBin(),
		[]string{"-port", a.cfg.APIPort, "-host", a.cfg.APIHost})
	c.MinDelay = a.cfg.RestartMin
	c.MaxDelay = a.cfg.RestartMax
	return c
}

func (a *app) newEngineChild() *until.Child {
	c := until.NewChild("引擎", a.cfg.EngineBin(), nil)
	c.LogFile = a.cfg.EngineLog()
	c.MinDelay = a.cfg.RestartMin
	c.MaxDelay = a.cfg.RestartMax
	return c
}

// newNginxChild 构造 nginx 子进程。
func (a *app) newNginxChild() *until.Child {
	c := until.NewChild("nginx", a.cfg.NginxBin(), []string{
		"-c", a.cfg.NginxConf(),
		"-g", "daemon off;",
	})
	c.LogFile = a.cfg.NginxLog()
	c.MinDelay = a.cfg.RestartMin
	c.MaxDelay = a.cfg.RestartMax
	return c
}

// ensureEngine 保证引擎子进程存在且被监管；wait 为真时额外等待其就绪。
func (a *app) ensureEngine(wait bool) error {
	a.mu.Lock()
	c := a.engine
	if c == nil {
		c = a.newEngineChild()
		a.engine = c
	}
	a.mu.Unlock()

	log.Println("启动引擎...")
	if err := c.Start(); err != nil {
		return err
	}
	if wait {
		if err := until.WaitEngineReady(a.cfg.EngineWS, a.cfg.EngineWait, c.Running); err != nil {
			return err
		}
	} else {
		go a.logEngineReady(c)
	}
	return nil
}

// logEngineReady 后台等待引擎就绪并把结果写进日志（供 /engineRestart 使用）。
func (a *app) logEngineReady(c *until.Child) {
	if err := until.WaitEngineReady(a.cfg.EngineWS, a.cfg.EngineWait, c.Running); err != nil {
		log.Println("引擎未就绪:", err)
	}
}

func (a *app) apiChild() *until.Child {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.api
}

func (a *app) engineChild() *until.Child {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.engine
}

func (a *app) nginxChild() *until.Child {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nginx
}

func (a *app) setAPI(c *until.Child) {
	a.mu.Lock()
	a.api = c
	a.mu.Unlock()
}

func (a *app) setNginx(c *until.Child) {
	a.mu.Lock()
	a.nginx = c
	a.mu.Unlock()
}

// 管理接口 (:82)

// 管理接口只服务于容器内的 iptv-api，因此默认绑 127.0.0.1。
func (a *app) serveAdmin() {
	mux := http.NewServeMux()
	mux.HandleFunc("/engineRestart", a.handleEngineRestart)
	// 改造前的路径，保留为别名：镜像里那半份"新旧混编"的状态
	// （例如只替换了 iptv 没替换 start）不至于因为改名而彻底断链。
	mux.HandleFunc("/licRestart", a.handleEngineRestart)
	mux.HandleFunc("/update", a.handleUpdate)
	mux.HandleFunc("/status", a.handleStatus)
	mux.HandleFunc("/healthz", a.handleHealthz)

	srv := &http.Server{
		Addr:              a.cfg.AdminAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("管理接口监听 %s (/engineRestart /licRestart /update /status /healthz)", a.cfg.AdminAddr)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// 这里不退出进程：管理接口不可用只影响在线升级与引擎重启，
			// 管理系统本身仍可用，退出反而把能用的服务一起带走了。
			log.Println("管理接口启动失败:", err)
		}
	}()
}

// handleEngineRestart 重启授权引擎。
func (a *app) handleEngineRestart(w http.ResponseWriter, r *http.Request) {
	if a.cfg.NoEngine {
		log.Println("收到引擎重启请求，但 IPTV_NO_ENGINE=true")
		fmt.Fprintln(w, "FAIL")
		return
	}

	log.Println("收到引擎重启请求")
	a.refreshInstalled()

	if c := a.engineChild(); c == nil {
		// 启动阶段跳过了引擎（未安装），这次是第一次拉起
		if err := a.ensureEngine(false); err != nil {
			log.Println("引擎启动失败:", err)
			fmt.Fprintln(w, "FAIL")
			return
		}
	} else if err := c.Restart(); err != nil {
		log.Println("引擎重启失败:", err)
		fmt.Fprintln(w, "FAIL")
		return
	}

	fmt.Fprintln(w, "OK")
}

// handleUpdate 应用在线升级包并重启受影响的进程。
func (a *app) handleUpdate(w http.ResponseWriter, r *http.Request) {
	log.Println("收到在线更新请求")

	applied, err := until.ApplyUpdate(a.cfg.BinDir, a.cfg.WebDir(), a.cfg.UpdataDir())
	if err != nil {
		log.Println("在线更新失败:", err)
		fmt.Fprintln(w, "FAIL: "+err.Error())
		return
	}
	if len(applied) == 0 {
		log.Println("升级包里没有可应用的文件，忽略")
		fmt.Fprintln(w, "OK")
		return
	}

	log.Println("在线更新完成:", until.JoinList(applied))
	a.setLastUpdate(applied)
	a.restartUpdated(applied)
	fmt.Fprintln(w, "OK")
}

// restartUpdated 重启被替换过二进制的子进程。
// 必须重启才生效：运行中的进程持有的是旧 inode（rename 是原子的）。
func (a *app) restartUpdated(applied []string) {
	for _, name := range applied {
		switch name {
		case "iptv":
			if c := a.apiChild(); c != nil {
				log.Println("管理系统已更新，重启生效")
				_ = c.Restart()
			}
		case "engine":
			if c := a.engineChild(); c != nil {
				log.Println("引擎已更新，重启生效")
				_ = c.Restart()
			}
		}
	}
}

// handleStatus 输出 JSON 状态快照，用于排障。
func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.refreshInstalled()
	apiVer, frontVer, engineVer := until.VersionSummary(a.cfg.BinDir, a.cfg.WebDir(), a.cfg.EngineBin())

	a.mu.Lock()
	lastUpd := a.lastUpd
	a.mu.Unlock()

	resp := map[string]interface{}{
		"version":       VERSION,
		"goArch":        until.GoArch(),
		"installed":     a.Installed(),
		"apiVersion":    apiVer,
		"frontVersion":  frontVer,
		"engineVersion": engineVer,
		"uptimeSecond":  int64(time.Since(a.startedAt).Seconds()),
		"nginxPort":     a.cfg.NginxPort,
		"apiPort":       a.cfg.APIPort,
		// apiHost 一起报出来：排障时"后端到底绑在哪"是个会被反复问到的问题，
		// 而它是决定"同网桥其它容器能不能绕过 nginx 直连"的唯一变量。
		"apiHost":   a.cfg.APIHost,
		"adminAddr": a.cfg.AdminAddr,
		"binDir":    a.cfg.BinDir,
		"configDir": a.cfg.ConfDir,
	}
	if lastUpd != "" {
		resp["lastUpdate"] = lastUpd
	}
	if c := a.apiChild(); c != nil {
		resp["api"] = c.Status()
	}
	if c := a.engineChild(); c != nil {
		resp["engine"] = c.Status()
	}
	if c := a.nginxChild(); c != nil {
		resp["nginx"] = c.Status()
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleHealthz 只回答一件事：对外这一条链路是不是活的。
func (a *app) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if c := a.apiChild(); c == nil || !c.Running() {
		http.Error(w, "management not running", http.StatusServiceUnavailable)
		return
	}
	if !a.cfg.NoNginx {
		if c := a.nginxChild(); c == nil || !c.Running() {
			http.Error(w, "nginx not running", http.StatusServiceUnavailable)
			return
		}
	}
	fmt.Fprintln(w, "ok")
}

// 退出

func (a *app) waitSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	sig := <-ch
	log.Printf("收到信号 %s，开始优雅退出...", sig)
}

// shutdown 按「nginx → 后端 → 引擎」的顺序停。
func (a *app) shutdown() {
	if c := a.nginxChild(); c != nil {
		c.Stop(5 * time.Second)
	}
	if c := a.apiChild(); c != nil {
		c.Stop(15 * time.Second)
	}
	if c := a.engineChild(); c != nil {
		c.Stop(5 * time.Second)
	}
	log.Println("升级服务已退出")
}

// summary 生成一行启动/状态摘要。
func (a *app) summary() string {
	state := "未安装"
	if a.Installed() {
		state = "已安装"
	}

	desc := func(parent string, c *until.Child) string {
		if c == nil {
			return parent + "未启动"
		}
		if c.Running() {
			return fmt.Sprintf("%s运行中(PID=%d)", parent, c.PID())
		}
		return parent + "已停止"
	}

	a.mu.Lock()
	upd := a.lastUpd
	a.mu.Unlock()

	s := fmt.Sprintf("安装状态=%s %s %s %s 对外端口=%s",
		state,
		desc("引擎=", a.engineChild()),
		desc("管理系统=", a.apiChild()),
		desc("nginx=", a.nginxChild()),
		a.cfg.NginxPort)
	if upd != "" {
		s += " 本次已应用更新=" + strings.ReplaceAll(upd, "、", ",")
	}
	return s
}
