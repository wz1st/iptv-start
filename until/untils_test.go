package until

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 版本号

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v1.2.1", "1.2.0", 1, true},
		{"1.2.0", "v1.2.1", -1, true},
		{"v1.2.0", "1.2.0", 0, true},
		{"1.2", "1.2.0", 0, true}, // 段数不足补 0
		{"2.0.0", "1.9.9", 1, true},
		{"1.2.3-beta", "1.2.3", 0, true}, // 预发布后缀不参与比较
		{"local", "1.2.3", 0, false},     // 占位版本号无法比较
		{"", "1.2.3", 0, false},
		{"1.2.3.4", "1.2.3.4.5", 0, false}, // 段数过多视为非法
	}
	for _, c := range cases {
		got, ok := CompareVersion(c.a, c.b)
		if ok != c.ok {
			t.Errorf("CompareVersion(%q,%q) ok=%v, want %v", c.a, c.b, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersion(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// 安装态

func TestIsInstalled(t *testing.T) {
	dir := t.TempDir()

	// 三件套缺一不可：只看 install.lock 会把"安装到一半"误判成已安装
	if IsInstalled(dir) {
		t.Fatal("空目录不应判定为已安装")
	}
	write(t, filepath.Join(dir, "iptv.db"), "x")
	write(t, filepath.Join(dir, "config.yml"), "x")
	if IsInstalled(dir) {
		t.Fatal("缺少 install.lock 不应判定为已安装")
	}
	write(t, filepath.Join(dir, "install.lock"), "")
	if !IsInstalled(dir) {
		t.Fatal("三件套齐全应判定为已安装")
	}
}

// 升级包

// 注意：这里的所有"版本号"都由**可执行文件自己**回答（fakeBin 造的 shell 脚本），
// 不再有 Version 文件参与 —— 见 until/version.go 的文件头说明。
func TestShouldApplyUpdate(t *testing.T) {
	bin := t.TempDir()
	web := filepath.Join(bin, "web")
	up := t.TempDir()

	// 升级目录不存在 → 不应用
	if apply, _ := ShouldApplyUpdate(bin, web, filepath.Join(up, "nope")); apply {
		t.Error("升级目录不存在时不应应用")
	}

	// 升级包里没有可应用的产物（只有无关文件）→ 不应用
	write(t, filepath.Join(up, "README.md"), "x")
	if apply, why := ShouldApplyUpdate(bin, web, up); apply {
		t.Errorf("升级包里没有产物时不应应用（%s）", why)
	}

	// 当前二进制读不出自身版本号 → 按需应用。
	fakeBin(t, filepath.Join(up, "iptv"), "v1.1.0")
	if apply, why := ShouldApplyUpdate(bin, web, up); !apply {
		t.Errorf("当前二进制读不出版本号时应按需应用（%s）", why)
	}

	// 升级包更新 → 应用
	fakeBin(t, filepath.Join(bin, "iptv"), "v1.0.0")
	if apply, _ := ShouldApplyUpdate(bin, web, up); !apply {
		t.Error("升级包版本更高时应应用")
	}

	// 版本相同 → 不应用（容器每次启动都会跑一遍这个判断）
	fakeBin(t, filepath.Join(up, "iptv"), "v1.0.0")
	if apply, _ := ShouldApplyUpdate(bin, web, up); apply {
		t.Error("版本相同时不应应用")
	}

	// 升级包更旧 → 不应用（防止容器重建后被卷里的旧包降级覆盖）
	fakeBin(t, filepath.Join(up, "iptv"), "v0.9.0")
	if apply, _ := ShouldApplyUpdate(bin, web, up); apply {
		t.Error("升级包版本更低时不应应用")
	}

	// 逐产物比：iptv 不比当前新、但 engine 比当前新 → 仍要应用。
	// 只看某一个产物会让"只升引擎"这条在线升级路径静默失效。
	fakeBin(t, filepath.Join(up, "iptv"), "v1.0.0")
	fakeBin(t, filepath.Join(up, "engine"), "v2.0.0")
	fakeBin(t, filepath.Join(bin, "engine"), "v1.9.0")
	if apply, _ := ShouldApplyUpdate(bin, web, up); !apply {
		t.Error("任意一个产物更新时应应用（只升引擎的场景）")
	}
}

// TestShouldApplyUpdateFrontBundle 前端整包也走同一套判据：
// 版本号不解包读（BundleVersion 顺 tar 流找 index.html），与站点现版本比。
func TestShouldApplyUpdateFrontBundle(t *testing.T) {
	bin := t.TempDir()
	up := t.TempDir()
	web := filepath.Join(bin, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(web, "index.html"),
		`<html><head><meta name="app-version" content="v3.0.6"></head></html>`)

	makeBundle(t, filepath.Join(up, webBundleName), true, map[string]string{
		"index.html": `<html><head><meta name="app-version" content="v3.0.7"></head></html>`,
	})
	if apply, _ := ShouldApplyUpdate(bin, web, up); !apply {
		t.Error("前端包版本更高时应应用")
	}

	// 同一版本 → 不应用
	write(t, filepath.Join(web, "index.html"),
		`<html><head><meta name="app-version" content="v3.0.7"></head></html>`)
	if apply, why := ShouldApplyUpdate(bin, web, up); apply {
		t.Errorf("前端版本相同时不应应用（%s）", why)
	}
}

func TestApplyUpdateReplacesFilesAtomically(t *testing.T) {
	bin := t.TempDir()
	up := t.TempDir()

	write(t, filepath.Join(bin, "iptv"), "old-binary")
	fakeBin(t, filepath.Join(up, "iptv"), "v1.1.0")
	// 空文件（下载中断留下的）不能拿去替换正在运行的二进制
	write(t, filepath.Join(up, "engine"), "")
	// 不在 updateTargets 里的文件必须无视（升级包里混进 README 是常事）
	write(t, filepath.Join(up, "README.md"), "x")

	applied, err := ApplyUpdate(bin, filepath.Join(bin, "web"), up)
	if err != nil {
		t.Fatalf("应用升级包失败: %v", err)
	}
	if len(applied) != 1 || applied[0] != "iptv" {
		t.Fatalf("应用列表异常: %v", applied)
	}
	if got := ReadFileTrim(filepath.Join(bin, "iptv")); !strings.Contains(got, "v1.1.0") {
		t.Errorf("iptv 未被替换，实际 %q", got)
	}
	if Exists(filepath.Join(bin, "engine")) {
		t.Error("空文件不应被应用到运行目录")
	}
	if Exists(filepath.Join(bin, "README.md")) {
		t.Error("不在 updateTargets 里的文件不应被应用")
	}
	// 临时文件必须清理干净，不能留在运行目录里
	if Exists(filepath.Join(bin, "iptv.new")) {
		t.Error("临时文件 iptv.new 应已被 rename 掉")
	}
	// 替换后要有可执行权限（否则重启即失败）
	fi, err := os.Stat(filepath.Join(bin, "iptv"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("替换后的文件应可执行，实际权限 %v", fi.Mode().Perm())
	}
}

// 子进程监管

func TestChildRestartsAndStops(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "child.log")

	c := NewChild("测试进程", "/bin/sh", []string{"-c", "echo hello; exit 7"})
	c.LogFile = logFile
	c.MinDelay = 30 * time.Millisecond
	c.MaxDelay = 60 * time.Millisecond
	c.HealthyAfter = time.Hour // 强制走退避路径

	if err := c.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool { return c.Status().Restarts >= 2 }, "进程应被反复拉起")
	if got := c.Status().LastError; !strings.Contains(got, "退出码 7") {
		t.Errorf("应记录退出码 7，实际 %q", got)
	}

	c.Stop(3 * time.Second)
	if c.Running() {
		t.Error("Stop 之后不应仍在运行")
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("读取子进程日志失败: %v", err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Errorf("子进程输出应写入 LogFile，实际 %q", string(data))
	}
}

func TestChildRestartReplacesRunningProcess(t *testing.T) {
	c := NewChild("长跑进程", "/bin/sh", []string{"-c", "sleep 30"})
	c.MinDelay = 30 * time.Millisecond
	c.MaxDelay = 60 * time.Millisecond

	if err := c.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	old := c.PID()
	if old <= 0 {
		t.Fatal("应拿到运行中的 PID")
	}

	if err := c.Restart(); err != nil {
		t.Fatalf("重启请求失败: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return c.Running() && c.PID() != old }, "应换成新进程")

	// 旧进程必须真的没了：只 kill 父进程会留下 sleep 孤儿
	waitFor(t, 3*time.Second, func() bool { return syscall.Kill(old, 0) != nil }, "旧进程应已退出")

	c.Stop(3 * time.Second)
}

func TestChildStartReportsMissingBinary(t *testing.T) {
	c := NewChild("不存在", filepath.Join(t.TempDir(), "nope"), nil)
	c.MinDelay = 10 * time.Millisecond
	c.MaxDelay = 20 * time.Millisecond
	if err := c.Start(); err == nil {
		t.Error("二进制不存在时 Start 应返回错误")
	}
	c.Stop(time.Second)
}

// 工具函数

func TestOriginOf(t *testing.T) {
	cases := map[string]string{
		"ws://127.0.0.1:81/ws":    "http://127.0.0.1:81/",
		"ws://127.0.0.1:81":       "http://127.0.0.1:81/",
		"wss://example.com/a?b=1": "http://example.com/",
	}
	for in, want := range cases {
		if got := originOf(in); got != want {
			t.Errorf("originOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	// 不设任何环境变量时必须是可用的默认值（compose 不配也能跑）
	for _, k := range []string{
		"IPTV_BIN_DIR", "IPTV_CONFIG_DIR", "IPTV_PORT",
		"IPTV_API_PORT", "IPTV_API_HOST", "IPTV_ADMIN_ADDR",
		"IPTV_ENGINE_WS", "IPTV_LICENSE_WS",
	} {
		t.Setenv(k, "")
	}
	cfg := LoadConfig()
	if cfg.BinDir != "/app" || cfg.ConfDir != "/config" {
		t.Errorf("默认目录异常: %s / %s", cfg.BinDir, cfg.ConfDir)
	}
	// 对外端口仍由 IPTV_PORT 表示（现在由 nginx 监听）；
	// 后端挪到独立的内部端口，两者不能重合。
	if cfg.NginxPort != "80" || cfg.APIPort != "9000" {
		t.Errorf("默认端口异常: nginx=%s api=%s", cfg.NginxPort, cfg.APIPort)
	}
	if cfg.NginxPort == cfg.APIPort {
		t.Errorf("对外端口与后端端口不能相同: %s", cfg.NginxPort)
	}
	// 后端默认只绑回环：这是"nginx 是对外唯一入口"这条约束的载体，
	if cfg.APIHost != "127.0.0.1" {
		t.Errorf("后端默认应只绑回环，实际 %q", cfg.APIHost)
	}
	if cfg.AdminAddr != "127.0.0.1:82" {
		t.Errorf("默认管理接口地址异常: %s", cfg.AdminAddr)
	}
	if cfg.EngineWait <= 0 || cfg.RestartMin <= 0 || cfg.RestartMax < cfg.RestartMin {
		t.Errorf("默认时长异常: %v %v %v", cfg.EngineWait, cfg.RestartMin, cfg.RestartMax)
	}
}

// TestEngineEnvLegacyNames 改造前的 IPTV_LICENSE_* 必须继续可用，
// 否则老 compose 文件不改就跑不起来，升级时会被当成"启动器坏了"。
func TestEngineEnvLegacyNames(t *testing.T) {
	t.Setenv("IPTV_ENGINE_WS", "")
	t.Setenv("IPTV_LICENSE_WS", "ws://127.0.0.1:99/ws")
	t.Setenv("IPTV_ENGINE_WAIT", "")
	t.Setenv("IPTV_LICENSE_WAIT", "12")
	cfg := LoadConfig()
	if cfg.EngineWS != "ws://127.0.0.1:99/ws" {
		t.Errorf("旧名 IPTV_LICENSE_WS 未生效: %s", cfg.EngineWS)
	}
	if cfg.EngineWait != 12*time.Second {
		t.Errorf("旧名 IPTV_LICENSE_WAIT 未生效: %v", cfg.EngineWait)
	}

	// 新名优先
	t.Setenv("IPTV_ENGINE_WS", "ws://127.0.0.1:81/ws")
	cfg = LoadConfig()
	if cfg.EngineWS != "ws://127.0.0.1:81/ws" {
		t.Errorf("新名应优先于旧名: %s", cfg.EngineWS)
	}
}

func TestEnvDurationAcceptsSecondsAndUnits(t *testing.T) {
	t.Setenv("IPTV_TEST_DURATION", "45")
	if got := envDuration("IPTV_TEST_DURATION", time.Second); got != 45*time.Second {
		t.Errorf("纯数字应按秒解析，实际 %v", got)
	}
	t.Setenv("IPTV_TEST_DURATION", "2m")
	if got := envDuration("IPTV_TEST_DURATION", time.Second); got != 2*time.Minute {
		t.Errorf("带单位应按单位解析，实际 %v", got)
	}
	t.Setenv("IPTV_TEST_DURATION", "abc")
	if got := envDuration("IPTV_TEST_DURATION", 7*time.Second); got != 7*time.Second {
		t.Errorf("非法值应回退默认值，实际 %v", got)
	}
}

// 测试辅助

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待超时: " + msg)
}
