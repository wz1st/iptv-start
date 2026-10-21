package until

import (
	"errors"
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
//
// 产物读不出自身版本号时（被换坏 / 不可执行），基准**直接取镜像内置版本** ——
// 否则会判成"当前二进制不可执行，按需应用"，等于主动让旧包把产物再降一级。
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
			switch {
			case cur == "":
				// 产物读不出（不存在 / 不可执行 / 被换坏）。**必须**拿镜像内置版本
				// 当基准：否则会落到下面"当前二进制不可执行，按需应用"那句，把卷里
				// 的旧包怼上去 —— 正是 3.x 装 4.x 时 `-host` 崩溃的成因。
				cur = base
			default:
				if cmp, ok := CompareVersion(base, cur); ok && cmp > 0 {
					cur = base
				}
			}
		}
		if cur == "" {
			// 走到这里说明镜像里连版本清单都没有（老镜像），只能按需应用。
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
			switch {
			case cur == "":
				// 站点没有版本标记（被换坏 / 被清空）⇒ 同样拿镜像内置版本兜住，
				// 免得落到下面"当前站点无版本标记，按需应用"去降级。
				cur = imgFront
			default:
				if cmp, ok := CompareVersion(imgFront, cur); ok && cmp > 0 {
					cur = imgFront
				}
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
//
// ## 顺序与回退：这是一笔事务，不是一串独立操作
//
// 用户明确要求：**先装前端、再装接口（api）**，其中任何一项失败都要整体回退。
//
// 顺序这么定是有原因的：前端整包是"整目录换"，是链路上最容易坏的一步
// （解包、旧目录改名、新目录上位）；而 api 是单个文件替换。万一真在换 api
// 的中途失败，已经换好的前端就与旧 api 配成一对 —— 新前端已经在调新接口，
// 症状是"页面能开、功能大面积报错"，比干脆没升级更难排查。所以前端那一步
// 的旧目录会一直留着，直到所有替换都成功才删。
//
// 回退动作（按替换的逆序）：
//   - 前端失败 ⇒ 把旧前端目录改名换回来；
//   - 前端成功、api 失败 ⇒ 先把 api 换回去（replaceFile 本就原子，单文件
//     失败时目标文件仍是旧的），再把前端换回来；
//   - 全成功 ⇒ 删掉前端备份目录与二进制备份文件。
//
// 引擎（engine）也在同一笔事务里：它与 api 是平级的替换目标，中途失败同样
// 回退，不给"api 升了、引擎没升"的半截状态。
//
// 返回的 applied 只含**真正换成功且没被回退**的目标 —— 调用方据此决定
// 要不要重启进程；回退后必须返回空列表，否则会拿一个已经换掉的进程去重启。
func ApplyUpdate(binDir, webDir, updataDir string) ([]string, error) {
	if !Exists(updataDir) {
		return nil, nil
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return nil, fmt.Errorf("运行目录不可写: %w", err)
	}

	var applied []string

	// 二进制的备份目录：替换前把旧文件挪进来，回退时挪回去。
	backupDir, err := os.MkdirTemp(binDir, ".rollback-")
	if err != nil {
		// 备份建不出来就不敢开始：没有退路的事不能做。
		return nil, fmt.Errorf("创建回退备份目录失败: %w", err)
	}
	defer os.RemoveAll(backupDir)

	// rollBack 把已经动过的部分按逆序还原。返回拼接后的错误（全部都要说，
	// 只报最后一个会让排障以为只坏了一处）。
	rollBack := func(webOld string, binNames []string) error {
		var msgs []string
		// 逆序：先二进制（它们是单文件 rename，回退代价最小），再前端目录。
		for i := len(binNames) - 1; i >= 0; i-- {
			name := binNames[i]
			if err := restoreFile(filepath.Join(backupDir, name), filepath.Join(binDir, name)); err != nil {
				msgs = append(msgs, fmt.Sprintf("回滚 %s 失败: %v", name, err))
			}
		}
		if err := rollbackWebBundle(webDir, webOld); err != nil {
			msgs = append(msgs, fmt.Sprintf("回滚前端失败: %v", err))
		}
		if len(msgs) == 0 {
			return nil
		}
		return errors.New(strings.Join(msgs, "；"))
	}

	// ---- 第 1 步：前端整包 ----
	webOld := ""
	if bundle := filepath.Join(updataDir, webBundleName); Exists(bundle) {
		old, err := applyWebBundleKeepOld(webDir, bundle)
		if err != nil {
			// 整包应用内部已做过一次"上位失败就换回旧目录"，这里再兜一层
			// 覆盖"解包成功但改名后异常"的少数情况。
			if rbErr := rollbackWebBundle(webDir, old); rbErr != nil {
				return nil, fmt.Errorf("应用前端升级包失败(%v)，且回滚失败(%v)", err, rbErr)
			}
			return nil, fmt.Errorf("应用前端升级包失败: %w", err)
		}
		webOld = old
		log.Printf("已更新前端产物 %s（旧目录暂留，全部替换成功后清理）", webDir)
		applied = append(applied, webBundleName)
	}

	// ---- 第 2 步：二进制（api / engine）----
	var binDone []string
	for _, name := range updateTargets {
		src := filepath.Join(updataDir, name)
		if !Exists(src) {
			continue
		}
		fi, err := os.Stat(src)
		if err != nil {
			return rollBackEarly(applied, webOld, binDone, rollBack,
				fmt.Errorf("读取 %s 失败: %w", src, err))
		}
		if fi.Size() == 0 {
			log.Printf("跳过空文件: %s", src)
			continue
		}

		dst := filepath.Join(binDir, name)
		// 先备份旧文件再替换：没有旧文件（首次安装/异常镜像）就记空串，
		// 回退时对应地"删掉新文件"即可。
		if err := backupFile(dst, filepath.Join(backupDir, name)); err != nil {
			return rollBackEarly(applied, webOld, binDone, rollBack,
				fmt.Errorf("备份 %s 失败: %w", name, err))
		}
		if err := replaceFile(src, dst); err != nil {
			// 关键：replaceFile 失败时 dst **已经不在了**（上一步已挪进备份目录），
			// 所以这个目标必须一起交给回退 —— 只回退"已成功的那几个"会让它
			// 永远消失，容器里就少了 api。
			return rollBackEarly(applied, webOld, append(binDone, name), rollBack,
				fmt.Errorf("替换 %s 失败: %w", name, err))
		}
		log.Printf("已更新 %s (%s)", dst, humanSize(fi.Size()))
		binDone = append(binDone, name)
		applied = append(applied, name)
	}

	// 全部替换成功 ⇒ 前端备份可以清掉了（留在磁盘上只是白占一份空间）。
	if webOld != "" {
		_ = os.RemoveAll(webOld)
	}
	return applied, nil
}

// rollBackEarly 统一"回退 + 汇报"的出口。
//
// 返回值里 applied 被清空：已经动过的部分都被换回去了，调用方绝不能拿
// 这份列表去重启进程（那会拿旧二进制去跑，看起来像重启失败）。
func rollBackEarly(applied []string, webOld string, binDone []string,
	rollBack func(string, []string) error, cause error) ([]string, error) {
	if rbErr := rollBack(webOld, binDone); rbErr != nil {
		return nil, fmt.Errorf("%v；%v（已尝试回退，请检查容器内 /app 与 /config/updata）", cause, rbErr)
	}
	log.Printf("升级失败并已回退到升级前状态：%v", cause)
	return nil, cause
}

// backupFile 把 dst 挪到 backupPath（dst 不存在时静默成功，回退即"删掉新的"）。
func backupFile(dst, backupPath string) error {
	if !Exists(dst) {
		return nil
	}
	_ = os.Remove(backupPath)
	return os.Rename(dst, backupPath)
}

// restoreFile 把备份换回原位；备份为空（原先没有该文件）时删掉现在的文件，
// 让目录回到"本来没有这个文件"的状态。
func restoreFile(backupPath, dst string) error {
	if !Exists(backupPath) {
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return replaceFile(backupPath, dst)
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
