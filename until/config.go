package until

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config 汇总启动器运行期的全部可调项。
type Config struct {
	// BinDir 是可执行文件目录。默认 /app，也就是镜像里那份 ——
	// 这一点是本启动器与旧版最本质的差别，原因见 update.go 顶部注释。
	BinDir string
	// ConfDir 是数据目录（持久卷），默认 /config。
	ConfDir string
	// NginxPort 是 nginx 监听端口，默认 80（由 compose 映射到外网）。
	// 它才是"对外端口"；iptv-api 不再直接对外。
	NginxPort string
	// APIPort 是管理系统（iptv-api）监听端口，默认 9000。
	// 刻意与对外端口分开：nginx 才是唯一入口，后端不该有第二条可达路径。
	APIPort string
	// APIHost 是 iptv-api 的绑定地址，默认 127.0.0.1（只绑回环）。
	APIHost string
	// AdminAddr 是管理接口监听地址。默认只绑 127.0.0.1：
	AdminAddr string
	// EngineWS 是授权引擎的 WebSocket 地址（引擎由 /app/engine 提供，固定 81 端口）。
	EngineWS string
	// EngineWait 是等待引擎就绪的上限。
	EngineWait time.Duration
	// RestartMin/Max 是子进程崩溃重启的退避区间。
	RestartMin time.Duration
	RestartMax time.Duration
	// NoEngine 为真时不拉起引擎（IPTV_NO_ENGINE=true，仅用于排障）。
	NoEngine bool
	// NoNginx 为真时不拉起 nginx（IPTV_NO_NGINX=true，仅供本地/排障：
	// 此时前端站点不可达，只能直接访问 APIPort 调后端接口）。
	NoNginx bool
	// SkipPrivCheck 为真时跳过 privileged 检测（本地开发用）。
	SkipPrivCheck bool
	// SkipPreCheck 为真时跳过 java / apktool / 端口预检。
	SkipPreCheck bool
}

// LoadConfig 从环境变量装载配置，缺省即默认值。
func LoadConfig() *Config {
	c := &Config{
		BinDir:  envStr("IPTV_BIN_DIR", "/app"),
		ConfDir: envStr("IPTV_CONFIG_DIR", "/config"),
		// IPTV_PORT 保持旧名：它一直表示"对外暴露的那个端口"，
		// 只是现在由 nginx 而非 iptv 监听。
		NginxPort: envStr("IPTV_PORT", "80"),
		APIPort:   envStr("IPTV_API_PORT", "9000"),
		APIHost:   envStr("IPTV_API_HOST", "127.0.0.1"),
		AdminAddr: envStr("IPTV_ADMIN_ADDR", "127.0.0.1:82"),
		// IPTV_LICENSE_WS / IPTV_LICENSE_WAIT 是改造前的名字，
		// 继续认（老 compose 文件不改也能起来），新名字优先。
		EngineWS:   envStr2("IPTV_ENGINE_WS", "IPTV_LICENSE_WS", "ws://127.0.0.1:81/ws"),
		EngineWait: envDuration2("IPTV_ENGINE_WAIT", "IPTV_LICENSE_WAIT", 30*time.Second),

		RestartMin: envDuration("IPTV_RESTART_MIN", time.Second),
		RestartMax: envDuration("IPTV_RESTART_MAX", 30*time.Second),

		NoEngine:      envBool("IPTV_NO_ENGINE", false),
		NoNginx:       envBool("IPTV_NO_NGINX", false),
		SkipPrivCheck: envBool("IPTV_SKIP_PRIV_CHECK", false),
		SkipPreCheck:  envBool("IPTV_SKIP_PRECHECK", false),
	}
	c.BinDir = filepath.Clean(c.BinDir)
	c.ConfDir = filepath.Clean(c.ConfDir)
	return c
}

// WebBin 是管理系统（iptv-api）二进制。
func (c *Config) WebBin() string { return filepath.Join(c.BinDir, "iptv") }

// NginxBin 是 nginx 主程序。
func (c *Config) NginxBin() string { return envStr("IPTV_NGINX_BIN", "/usr/sbin/nginx") }

// NginxConf 是 nginx 主配置。
func (c *Config) NginxConf() string { return envStr("IPTV_NGINX_CONF", "/etc/nginx/nginx.conf") }

// NginxLog 是 nginx 的 stdout/stderr 落盘位置。
func (c *Config) NginxLog() string { return filepath.Join(c.ConfDir, "nginx.log") }

// WebDir 是前端静态站点根目录（nginx 的 root）。
func (c *Config) WebDir() string { return envStr("IPTV_WEB_DIR", "/app/web") }

// EngineBin 是本次要运行的授权引擎路径。
func (c *Config) EngineBin() string {
	plain := filepath.Join(c.BinDir, "engine")
	if Exists(plain) {
		return plain
	}
	return filepath.Join(c.BinDir, "engine_"+runtime.GOARCH)
}

// 这里曾有 WebVersionFile / EngineVersionFile / FrontVersionFile 三个方法，

// EngineLog 是引擎日志。后台"引擎日志"页读的就是它，故仍放在 /config 下。
func (c *Config) EngineLog() string { return filepath.Join(c.ConfDir, "engine.log") }

// IsInstalled 判断系统是否已完成安装。
func IsInstalled(confDir string) bool {
	return Exists(filepath.Join(confDir, "iptv.db")) &&
		Exists(filepath.Join(confDir, "config.yml")) &&
		Exists(filepath.Join(confDir, "install.lock"))
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envStr2 读两个候选环境变量，前者优先（用于"新名优先、旧名兼容"）。
func envStr2(primary, legacy, def string) string {
	if v := strings.TrimSpace(os.Getenv(primary)); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(legacy)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envDuration(key string, def time.Duration) time.Duration {
	return parseDuration(os.Getenv(key), def)
}

// envDuration2 同 envStr2，但针对时长。
func envDuration2(primary, legacy string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(primary)); v != "" {
		return parseDuration(v, def)
	}
	return parseDuration(os.Getenv(legacy), def)
}

// parseDuration 允许纯数字（按秒理解）或带单位的 30s / 2m。
func parseDuration(v string, def time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return def
		}
		return time.Duration(n) * time.Second
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	return def
}
