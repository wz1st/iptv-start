package until

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/mem"
	"golang.org/x/net/websocket"
)

// 文件系统

func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ReadFileTrim 读一个纯文本文件（版本号文件就是这种），
func ReadFileTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	return strings.TrimPrefix(s, "\ufeff")
}

// FixPerm 递归把目录下所有条目的权限设为 0777。
func FixPerm(path string) error {
	return filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chmod(p, 0777)
	})
}

// copyFileMode 复制文件并设置权限位，用于升级包的原子替换。
func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := out.ReadFrom(in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// 版本号

// CompareVersion 比较形如 v1.2.3 / 1.2.3 的版本号，返回 -1 / 0 / 1。
func CompareVersion(a, b string) (int, bool) {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x > y {
			return 1, true
		}
		if x < y {
			return -1, true
		}
	}
	return 0, true
}

func parseVersion(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return nil, false
	}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if i := strings.IndexAny(p, "-+"); i >= 0 {
			p = p[:i]
		}
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// 运行环境检测

// CheckRam 返回"内存是否不足"。命名沿用 iptv 侧语义：true = 不足。
func CheckRam() bool {
	vmStat, err := mem.VirtualMemory()
	if err != nil {
		// 读不到内存信息不该成为拒绝启动的理由（部分受限环境没有 /proc/meminfo）
		return false
	}
	log.Printf("可用内存: %d MB (%d GB)", vmStat.Available/1024/1024, vmStat.Available/1024/1024/1024)
	return vmStat.Available < 256*1024*1024
}

// IsPrivileged 判断容器是否以 privileged 运行。
func IsPrivileged() bool {
	if hasHostDevices() {
		return true
	}
	if hasAllCaps() {
		return true
	}
	if hasSysAdminCap() {
		return true
	}
	if canAccessRestrictedKernelInfo() {
		return true
	}
	return false
}

// parseStatusCap 从 /proc/self/status 文本里解析指定字段（CapEff / CapBnd）
func parseStatusCap(status, field string) (uint64, bool) {
	prefix := field + ":"
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		n, err := strconv.ParseUint(val, 16, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// capVolume 是"内核目前用到的全部 capability 位"的掩码（bit 0..40）。
const capVolume = uint64(0x1ffffffffff)

// allCapsSatisfied 判断 "capability 边界集" 是否未被裁剪（= privileged 的标志）。
func allCapsSatisfied(eff, bnd uint64, hasBnd bool) bool {
	if !hasBnd {
		return eff != 0 && eff&capVolume == capVolume
	}
	return bnd&capVolume == capVolume
}

// capEff 读 /proc/self/status 的 CapEff，解析成 uint64。
func capEff() (uint64, bool) {
	return readStatusCap("CapEff")
}

// capBnd 读 /proc/self/status 的 CapBnd（能力边界集）。
func capBnd() (uint64, bool) {
	return readStatusCap("CapBnd")
}

func readStatusCap(field string) (uint64, bool) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	return parseStatusCap(string(data), field)
}

// hasAllCaps: capability 边界集是否未裁剪（privileged 的标志）。
func hasAllCaps() bool {
	eff, ok := capEff()
	if !ok {
		return false
	}
	bnd, hasBnd := capBnd()
	return allCapsSatisfied(eff, bnd, hasBnd)
}

// hasSysAdminCap: CapEff 是否含 SYS_ADMIN（bit 21）。
func hasSysAdminCap() bool {
	eff, ok := capEff()
	if !ok {
		return false
	}
	return hasCapBit(eff, 21) // CAP_SYS_ADMIN = 21
}

// hasCapBit 判断 CapEff 的第 bit 位是否置位（bit 从 0 起）。
func hasCapBit(eff uint64, bit uint) bool {
	if bit >= 64 {
		return false
	}
	return eff&(uint64(1)<<bit) != 0
}

// canAccessRestrictedKernelInfo: 能否读只有特权容器才可读的内核内存。
func canAccessRestrictedKernelInfo() bool {
	_, err := os.ReadFile("/proc/kcore")
	return err == nil
}

// hasHostDevices: privileged 容器会把宿主机设备全挂进来，
func hasHostDevices() bool {
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return false
	}
	return len(entries) > 100
}

// CheckJava / CheckApktool / CheckPort 是"编译 APK"这条链路的前置条件。
func CheckJava() bool {
	log.Println("检查Java版本...")
	cmd := exec.Command("java", "-version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Println("Java版本检查失败:", err)
		return false
	}

	lines := strings.Split(string(output), "\n")
	if len(lines) == 0 {
		log.Println("无法确定 Java 版本")
		return false
	}
	javaVersionLine := lines[0]
	log.Println("Java版本信息:", javaVersionLine)

	var versionStr string
	if strings.Contains(javaVersionLine, `"`) {
		parts := strings.Split(javaVersionLine, `"`)
		if len(parts) >= 2 {
			versionStr = parts[1]
		}
	}
	if versionStr == "" {
		log.Println("无法解析Java版本")
		return false
	}

	// 形如 1.8.0_361 或 17.0.7：Java 9+ 主版本号直接是 9 以上
	versionParts := strings.Split(versionStr, ".")
	major, minor := 0, 0
	if len(versionParts) >= 2 {
		major, _ = strconv.Atoi(versionParts[0])
		minor, _ = strconv.Atoi(versionParts[1])
	} else if len(versionParts) == 1 {
		major, _ = strconv.Atoi(versionParts[0])
	}
	if major > 1 || (major == 1 && minor >= 8) {
		return true
	}
	log.Println("Java版本低于 1.8")
	return false
}

func CheckApktool() bool {
	log.Println("检查Apktool版本...")
	cmd := exec.Command("apktool", "-version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Println("Apktool检查失败:", err)
		return false
	}
	lines := strings.Split(string(output), "\n")
	if len(lines) == 0 {
		log.Println("无法确定 Apktool 版本")
		return false
	}
	log.Println("Apktool版本:", strings.TrimSpace(lines[0]))
	return true
}

// CheckPort 试听一下端口是否空闲：能 Listen 说明没被占用。
func CheckPort(port string) bool {
	log.Println("检查端口占用...")
	listener, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		log.Println("端口" + port + "被占用...")
		return false
	}
	listener.Close()
	return true
}

// CheckNginx 校验 nginx 配置语法（nginx -t -c <conf>）。
func CheckNginx(bin, conf string) bool {
	log.Println("检查 nginx 配置...")
	cmd := exec.Command(bin, "-t", "-c", conf)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Println("nginx 配置校验失败:", strings.TrimSpace(string(output)))
		return false
	}
	log.Println("nginx 配置校验通过")
	return true
}

// 引擎就绪判定

// WaitEngineReady 等引擎真正开始服务。
func WaitEngineReady(rawURL string, timeout time.Duration, alive func() bool) error {
	origin := originOf(rawURL)
	start := time.Now()
	nextLog := 5 * time.Second

	for {
		if pingWS(rawURL, origin) {
			log.Printf("引擎启动完成（耗时 %.1fs）", time.Since(start).Seconds())
			return nil
		}
		if alive != nil && !alive() {
			return errors.New("引擎进程已退出")
		}
		if time.Since(start) >= timeout {
			return fmt.Errorf("等待引擎就绪超时(%s)", timeout)
		}
		if time.Since(start) >= nextLog {
			log.Printf("等待引擎就绪... 已等待 %.0fs", time.Since(start).Seconds())
			nextLog += 5 * time.Second
		}
		time.Sleep(time.Second)
	}
}

func pingWS(rawURL, origin string) bool {
	ws, err := websocket.Dial(rawURL, "", origin)
	if err != nil {
		return false
	}
	defer ws.Close()
	if _, err := ws.Write([]byte("ping")); err != nil {
		return false
	}
	return true
}

// originOf 从 ws://host:port/xxx 推出 WebSocket 握手要带的 Origin。
func originOf(rawURL string) string {
	s := rawURL
	s = strings.TrimPrefix(s, "ws://")
	s = strings.TrimPrefix(s, "wss://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "http://127.0.0.1:81/"
	}
	return "http://" + s + "/"
}

// 杂项

// GoArch 返回当前架构名（用于日志里说清"这份二进制是给哪个架构的"）。
func GoArch() string { return runtime.GOARCH }

// HTTPGetText 取一个短文本接口的返回体，失败返回空串。
func HTTPGetText(url string, timeout time.Duration) string {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	buf := make([]byte, 0, 256)
	tmp := make([]byte, 128)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > 4096 {
				break
			}
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}
