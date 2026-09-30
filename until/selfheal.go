package until

import (
	"fmt"
	"path/filepath"
)

// 产物自愈（启动器侧）。
//
// 为什么需要它：`/app` 是容器的**可写层**，`docker restart` **抹不掉**。
// 产物一旦被写坏或被降级，重启也好不了，只有 `docker rm -f` 重建容器才会
// 回到镜像层那份。所以镜像里给每个产物留一份原始副本（`/app/.image/<name>`），
// 启动器每次启动都拿它当标准答案核对运行目录，必要时覆盖回去 ——
// 这就是定制分支"默认使用镜像内置产物"的实现本体。

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
