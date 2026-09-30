package until

import (
	"os"
	"path/filepath"
	"strings"
)

// 启动器侧：读版本号 + 用镜像内置副本自愈产物。
//
// 在线升级（api / 引擎 / 前端整包）已在定制分支整体删除：
// 定制包不发主线的公共发布位，版本更新一律换镜像。
// 因此这里不再有 ShouldApplyUpdate / ApplyUpdate，只剩下面几件配套小事。

// imageVerFor 按产物名取镜像内置的对应版本号。
func imageVerFor(name, imgAPI, imgEngine string) string {
	switch name {
	case "iptv":
		return imgAPI
	case "engine":
		return imgEngine
	}
	return ""
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

// JoinList 把文件名列表拼成日志友好的字符串。
func JoinList(items []string) string {
	if len(items) == 0 {
		return "无"
	}
	return strings.Join(items, "、")
}
