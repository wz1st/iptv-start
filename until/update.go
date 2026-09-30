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
//
// 比较基准取 **max(镜像内置版本, 当前产物版本)**，而不是只看当前产物：
// 3.x → 4.x 升级时，持久卷里可能躺着 3.x 时代点过的旧升级包，
// 若照应用，它会盖掉 4.x 镜像里自带的、能认 `-host` 的新 api
// ⇒ 新启动器 + 旧 api，api 一启动就 `flag provided but not defined: -host` 秒退。
// 所以：**升级包版本不高于镜像内置版本的一律不应用**，并在日志里说明原因。
func ShouldApplyUpdate(binDir, webDir, updataDir string) (bool, string) {
	if !Exists(updataDir) {
		return false, "升级目录不存在，跳过"
	}

	imgAPI, imgFront, imgEngine := ImageVersions(binDir)

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
		// 基准换成"镜像内置版本"与"当前产物版本"里较高的那个。
		base := imageVerFor(name, imgAPI, imgEngine)
		if base != "" {
			if cmp, ok := CompareVersion(base, cur); ok && cmp > 0 {
				cur = base
			}
		}
		if cur == "" {
			return true, fmt.Sprintf("%s 升级包版本 %s，当前二进制不可执行，按需应用", name, up)
		}
		cmp, ok := CompareVersion(up, cur)
		if !ok {
			return true, fmt.Sprintf("%s 版本号无法比较（升级包=%s 当前=%s），按需应用", name, up, cur)
		}
		if cmp < 0 {
			// 防降级：旧包会盖掉镜像里更新的产物。
			return false, fmt.Sprintf("%s 升级包 %s 低于当前 %s，拒绝降级，跳过", name, up, cur)
		}
		if cmp > 0 {
			return true, fmt.Sprintf("发现新版本 %s（当前 %s）", up, cur)
		}
	}

	if bundle := filepath.Join(updataDir, webBundleName); Exists(bundle) {
		compared = true
		up := BundleVersion(bundle)
		cur := WebVersion(webDir)
		if imgFront != "" {
			if cmp, ok := CompareVersion(imgFront, cur); ok && cmp > 0 {
				cur = imgFront
			}
		}
		switch {
		case up == "":
			return true, "前端升级包里读不到版本号，按需应用"
		case cur == "":
			return true, fmt.Sprintf("前端升级包版本 %s，当前站点无版本标记，按需应用", up)
		}
		cmp, ok := CompareVersion(up, cur)
		if !ok {
			return true, fmt.Sprintf("前端版本号无法比较（升级包=%s 当前=%s），按需应用", up, cur)
		}
		if cmp < 0 {
			return false, fmt.Sprintf("前端升级包 %s 低于当前 %s，拒绝降级，跳过", up, cur)
		}
		if cmp > 0 {
			return true, fmt.Sprintf("前端有新版本 %s（当前 %s）", up, cur)
		}
	}

	if !compared {
		return false, "升级包里没有可应用的产物，跳过"
	}
	return false, "升级包版本不高于当前，跳过"
}

// imageVerFor 按升级目标名取镜像内置的对应版本号。
func imageVerFor(name, imgAPI, imgEngine string) string {
	switch name {
	case "iptv":
		return imgAPI
	case "engine":
		return imgEngine
	}
	return ""
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
//
// 三方各自的版本号都优先"问产物自己"（`<bin> -version` / 前端 index.html 的
// meta 标记）。**问不出来时不再显示"未知"，而是回落到镜像内置的版本清单**：
// "未知"对排障毫无信息量，而清单能答出"这个容器内置的是哪一版"。
// 只有当产物答不出 **且** 镜像里也没有清单（老镜像）时，才退回"未知"。
func VersionSummary(binDir, webDir, engineBin string) (api, front, engine string) {
	imgAPI, imgFront, imgEngine := ImageVersions(binDir)

	api = BinaryVersion(filepath.Join(binDir, "iptv"))
	if api == "" {
		api = fallbackVer(imgAPI)
	}
	front = WebVersion(webDir)
	if front == "" {
		front = fallbackVer(imgFront)
	}
	engine = BinaryVersion(engineBin)
	if engine == "" {
		engine = fallbackVer(imgEngine)
	}
	return api, front, engine
}

// VersionFallbackNote 报告哪些组件**用了镜像清单兜底**（"api" / "front" / "engine"）。
//
// 值本身刻意不带标记（与正常读取完全一致，前端与脚本无需适配），
// 但排障时需要知道"这个版本号是产物自己说的，还是清单兜的"，
// 所以把来源单独作为一行日志报出来。
func VersionFallbackNote(binDir, webDir, engineBin string) []string {
	imgAPI, imgFront, imgEngine := ImageVersions(binDir)
	var fell []string
	if imgAPI != "" && BinaryVersion(filepath.Join(binDir, "iptv")) == "" {
		fell = append(fell, "管理系统")
	}
	if imgFront != "" && WebVersion(webDir) == "" {
		fell = append(fell, "前端")
	}
	if imgEngine != "" && BinaryVersion(engineBin) == "" {
		fell = append(fell, "引擎")
	}
	return fell
}

// fallbackVer 是清单值空时的最后兜底。
func fallbackVer(v string) string {
	if v == "" {
		return "未知"
	}
	return v
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
