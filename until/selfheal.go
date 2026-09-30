package until

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// 产物自愈（启动器侧）。
//
// 为什么需要它：`/app` 是容器的**可写层**。ApplyUpdate 会把持久卷里的升级包
// 写进 `/app`，而 `docker restart` **抹不掉**可写层 —— 产物一旦被写坏或被
// 降级，重启也好不了，只有 `docker rm -f` 重建容器才会回到镜像层那份。
// 所以镜像里给每个产物留一份原始副本（`/app/.image/<name>`），启动器每次
// 启动都拿它当标准答案核对运行目录，必要时覆盖回去。

// ImageBackupDir 是镜像内置产物副本所在目录（相对 BinDir）。
const ImageBackupDir = ".image"

// healTargets 是需要自愈的产物。
// 不含 start：启动器自己不在升级包里，也没法给自己做手术。
var healTargets = []string{"iptv", "engine"}

// SelfHealBinaries 核对运行目录里的产物，必要时用镜像内置副本恢复。
// 返回被恢复产物的说明（空 = 无需处理）。
//
// 触发恢复的两种情况：
//   - 产物读不出自身版本号（不存在 / 不可执行 / 被换成不认识 -version 的东西）
//   - 产物版本**低于**镜像内置版本（被卷里的旧包降级了）
//
// **高于**镜像版本的一律不动：那是用户自己在线升级上来的，比镜像新。
func SelfHealBinaries(binDir string) ([]string, error) {
	imgAPI, _, imgEngine := ImageVersions(binDir)
	var healed []string

	for _, name := range healTargets {
		src := filepath.Join(binDir, ImageBackupDir, name)
		if !Exists(src) {
			continue // 老镜像里没有副本，不敢乱动
		}
		dst := filepath.Join(binDir, name)
		imgVer := imageVerFor(name, imgAPI, imgEngine)
		cur := BinaryVersion(dst)

		var reason string
		switch {
		case cur == "":
			reason = fmt.Sprintf("%s 读不出自身版本号", name)
		case imgVer != "":
			if cmp, ok := CompareVersion(cur, imgVer); ok && cmp < 0 {
				reason = fmt.Sprintf("%s 当前 %s 低于镜像内置 %s", name, cur, imgVer)
			}
		}
		if reason == "" {
			continue
		}
		if err := replaceFile(src, dst); err != nil {
			return healed, fmt.Errorf("用镜像副本恢复 %s 失败: %w", name, err)
		}
		healed = append(healed, reason+"（已恢复）")
	}
	return healed, nil
}

// CleanStaleUpdata 删除持久卷里"低于镜像内置版本"的陈旧升级包。
//
// 它们唯一的作用就是下次启动时把产物再降级一次；自愈能救回来，但每次都白跑
// 一遍、日志也吵。只删**能读出版本且明确更低**的：读不出的一律留着 —— 那是
// 用户自己放的，我们没本事判断它有没有用。
func CleanStaleUpdata(binDir, updataDir string) []string {
	if !Exists(updataDir) {
		return nil
	}
	imgAPI, imgFront, imgEngine := ImageVersions(binDir)
	var removed []string

	for _, name := range updateTargets {
		p := filepath.Join(updataDir, name)
		if !Exists(p) {
			continue
		}
		imgVer := imageVerFor(name, imgAPI, imgEngine)
		if imgVer == "" {
			continue
		}
		up := BinaryVersion(p)
		if up == "" {
			continue
		}
		if cmp, ok := CompareVersion(up, imgVer); ok && cmp < 0 {
			if err := os.Remove(p); err != nil {
				log.Printf("清理陈旧升级包 %s 失败: %v", p, err)
				continue
			}
			removed = append(removed, name)
		}
	}

	if imgFront == "" {
		return removed
	}
	bundle := filepath.Join(updataDir, webBundleName)
	if !Exists(bundle) {
		return removed
	}
	up := BundleVersion(bundle)
	if up == "" {
		return removed
	}
	if cmp, ok := CompareVersion(up, imgFront); ok && cmp < 0 {
		if err := os.Remove(bundle); err != nil {
			log.Printf("清理陈旧前端升级包失败: %v", err)
			return removed
		}
		removed = append(removed, webBundleName)
	}
	return removed
}
