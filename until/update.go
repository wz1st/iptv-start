package until

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// 在线升级（启动器侧）。

// updateTargets 是升级包里会被"原子替换单个文件"的目标。
var updateTargets = []string{"iptv", "engine"}

// ShouldApplyUpdate 判断持久卷里的升级包是否值得应用。
func ShouldApplyUpdate(binDir, webDir, updataDir string) (bool, string) {
	if !Exists(updataDir) {
		return false, "升级目录不存在，跳过"
	}

	compared := false
	for _, name := range updateTargets {
		stage := filepath.Join(updataDir, name)
		if !Exists(stage) {
			continue
		}
		compared = true
		up := BinaryVersion(stage)
		if up == "" {
			// 读不出自身版本的升级包没法比较新旧。宁可应用（否则用户
			// 点了升级却永远不动），但要说清楚，日志里能看出来。
			return true, fmt.Sprintf("%s 读不出自身版本号，按需应用", name)
		}
		cur := BinaryVersion(filepath.Join(binDir, name))
		if cur == "" {
			return true, fmt.Sprintf("%s 升级包版本 %s，当前二进制不可执行，按需应用", name, up)
		}
		cmp, ok := CompareVersion(up, cur)
		if !ok {
			return true, fmt.Sprintf("%s 版本号无法比较（升级包=%s 当前=%s），按需应用", name, up, cur)
		}
		if cmp > 0 {
			return true, fmt.Sprintf("发现新版本 %s（当前 %s）", up, cur)
		}
	}

	if bundle := filepath.Join(updataDir, webBundleName); Exists(bundle) {
		compared = true
		up := BundleVersion(bundle)
		cur := WebVersion(webDir)
		switch {
		case up == "":
			return true, "前端升级包里读不到版本号，按需应用"
		case cur == "":
			return true, fmt.Sprintf("前端升级包版本 %s，当前站点无版本标记，按需应用", up)
		}
		if cmp, ok := CompareVersion(up, cur); !ok || cmp > 0 {
			return true, fmt.Sprintf("前端有新版本 %s（当前 %s）", up, cur)
		}
	}

	if !compared {
		return false, "升级包里没有可应用的产物，跳过"
	}
	return false, "升级包版本不高于当前，跳过"
}

// ApplyUpdate 把升级包里的内容原子替换到运行目录。
func ApplyUpdate(binDir, webDir, updataDir string) ([]string, error) {
	if !Exists(updataDir) {
		return nil, nil
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return nil, fmt.Errorf("运行目录不可写: %w", err)
	}

	var applied []string

	// 前端整包排在最前，且失败即中止：它是最容易出错的一步（解包/换目录），
	if bundle := filepath.Join(updataDir, webBundleName); Exists(bundle) {
		if err := ApplyWebBundle(webDir, bundle); err != nil {
			return applied, fmt.Errorf("应用前端升级包失败: %w", err)
		}
		log.Printf("已更新前端产物 %s", webDir)
		applied = append(applied, webBundleName)
	}

	for _, name := range updateTargets {
		src := filepath.Join(updataDir, name)
		if !Exists(src) {
			continue
		}
		fi, err := os.Stat(src)
		if err != nil {
			return applied, fmt.Errorf("读取 %s 失败: %w", src, err)
		}
		if fi.Size() == 0 {
			log.Printf("跳过空文件: %s", src)
			continue
		}

		dst := filepath.Join(binDir, name)
		if err := replaceFile(src, dst); err != nil {
			return applied, fmt.Errorf("替换 %s 失败: %w", name, err)
		}
		log.Printf("已更新 %s (%s)", dst, humanSize(fi.Size()))
		applied = append(applied, name)
	}
	return applied, nil
}

// replaceFile 用"同目录临时文件 + rename"做原子替换。
func replaceFile(src, dst string) error {
	tmp := dst + ".new"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := copyFileMode(src, tmp, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// VersionSummary 读三方的版本号，用于启动日志与 /status。
func VersionSummary(binDir, webDir, engineBin string) (api, front, engine string) {
	api = BinaryVersion(filepath.Join(binDir, "iptv"))
	if api == "" {
		api = "未知"
	}
	front = WebVersion(webDir)
	if front == "" {
		front = "未知"
	}
	engine = BinaryVersion(engineBin)
	if engine == "" {
		engine = "未知"
	}
	return api, front, engine
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"KB", "MB", "GB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f%s", v, u)
		}
	}
	return fmt.Sprintf("%.1fTB", v/unit)
}

// JoinList 把文件名列表拼成日志友好的字符串。
func JoinList(items []string) string {
	if len(items) == 0 {
		return "无"
	}
	return strings.Join(items, "、")
}
