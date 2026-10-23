package until

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// 前端整包在线升级（启动器侧）。

// webBundleName 是前端升级包在 /config/updata 里的落地名，
// 与 iptv-api/until/updataUntil.go 里 stageUpdata 的目标名逐字一致。
const webBundleName = "webdist.tar.gz"

// webIndexName 是"这棵树能不能当站点根"的判据。
// 缺了它 nginx 的 try_files 会全部回落 404，等于把站点换坏。
const webIndexName = "index.html"

// ErrBundleNoIndex：解出来的目录里没有 index.html。
// 单独定义成哨兵错误，是为了让调用方能把它和"磁盘满/权限错"区分开报。
var ErrBundleNoIndex = errors.New("前端升级包内缺少 " + webIndexName)

// ExtractWebBundle 把 tar.gz 解到 dstDir，返回站点根目录（含 index.html 的那层）。
func ExtractWebBundle(tarPath, dstDir string) (string, error) {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", fmt.Errorf("创建解包目录失败: %w", err)
	}

	f, err := os.Open(tarPath)
	if err != nil {
		return "", fmt.Errorf("打开升级包失败: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("不是有效的 gzip 包: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("读取升级包失败: %w", err)
		}

		rel, err := safeRelPath(hdr.Name)
		if err != nil {
			return "", err
		}
		if rel == "" {
			continue
		}
		dst := filepath.Join(dstDir, rel)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return "", fmt.Errorf("创建目录 %s 失败: %w", rel, err)
			}
		case tar.TypeReg:
			if err := writeTarFile(dst, tr, hdr); err != nil {
				return "", err
			}
		default:
			// 符号链接/设备文件一律忽略：前端产物里本来就没有，
			// 允许它们等于给"用链接指向 /etc"开一条路。
			continue
		}
	}

	return findSiteRoot(dstDir)
}

// writeTarFile 落一个普通文件（自动补父目录）。
func writeTarFile(dst string, r io.Reader, hdr *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	mode := os.FileMode(hdr.Mode) & 0o777
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("写入 %s 失败: %w", filepath.Base(dst), err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", filepath.Base(dst), err)
	}
	return f.Close()
}

// safeRelPath 把包内路径规范化成"相对于解包目录"的安全路径。
// 返回空串表示这条目不用管（比如 tar 里的 "." 或 "./"）。
func safeRelPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	// 统一分隔符：Windows 上打的包可能带反斜杠。
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("升级包内含绝对路径 %q，拒绝解包", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == "/" {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("升级包内含越界路径 %q，拒绝解包", name)
	}
	return clean, nil
}

// findSiteRoot 找出解包结果里的站点根：优先 dstDir/webdist，其次 dstDir 本身。
func findSiteRoot(dstDir string) (string, error) {
	nested := filepath.Join(dstDir, "webdist")
	if Exists(filepath.Join(nested, webIndexName)) {
		return nested, nil
	}
	if Exists(filepath.Join(dstDir, webIndexName)) {
		return dstDir, nil
	}
	return "", ErrBundleNoIndex
}

// ApplyWebBundle 把前端升级包应用到 webDir（生产上是 /app/web）。
//
// 语义是"用完即清"：成功后备份目录立刻删掉。**参与整笔升级事务的调用方不要用
// 它**，用 applyWebBundleKeepOld —— 前端与二进制必须当成一笔事务，前端这步
// 的旧目录要留到二进制也换完才能清。
func ApplyWebBundle(webDir, tarPath string) error {
	old, err := applyWebBundleKeepOld(webDir, tarPath)
	if err != nil {
		return err
	}
	if old != "" {
		os.RemoveAll(old)
	}
	return nil
}

// applyWebBundleKeepOld 应用前端整包，但**保留**旧内容不删，返回备份目录路径。
//
// 为什么多一个“保留旧内容”的版本：整包升级与二进制升级必须当成**一个事务**
// 做 —— 前端换成功了、api 替换失败时，不能留下“新前端 + 旧 api”的组合
// （前端可能已经在调新接口，表现为页面能开但一堆功能报错）。所以前端这一步
// 成功后要把旧内容留着，等二进制也换完了再删；中途失败由调用方回滚。
// 没有旧内容（首次安装/异常镜像）时返回空串，表示“没什么可回退的”。
//
// ## 为什么全程只做「文件级」操作，绝不 rename 目录
//
// 早先的实现是 `rename(webDir, .web-old)` + `rename(新目录, webDir)`。
// 这套在生产上**必然失败**：
//
//	rename /app/web /app/.web-old: invalid cross-device link
//
// 报的是 EXDEV（cross-device link）。成因有两类，根子却是一个：
//
//  1. /app/web 是**独立挂载点**（volume / bind mount）时，它与父目录 /app
//     不在同一个设备上，rename(2) 跨设备直接失败；
//  2. /app/web 来自**镜像层**（overlayfs 的 lower dir）时，把它 rename 到
//     容器可写层（upper dir）需要 overlayfs 的 redirect_dir 特性；
//     内核没开这个特性时报的也是 EXDEV。
//
// 第 2 类尤其隐蔽：开发机（较新内核，默认开 redirect_dir）跑得好好的，
// 生产机（老内核或别的存储驱动）就必挂 —— 而**开发机上根本测不出来**。
//
// 还有一个更隐蔽的坑：第二次 `rename(新目录, webDir)` 在 webDir 已存在时，
// 会把新目录**移进** webDir 里（变成 /app/web/webdist）—— 不报错，
// 但 nginx 找不到 index.html，站点直接废掉。
//
// 所以这里的规矩是：**一个目录都不 rename**。目录结构靠 MkdirAll 重建，
// 文件靠 copy + 同目录 rename 替换（同一目录 ⇒ 必然同设备 ⇒ 必然成功）。
// 备份也靠 copy 而不是 rename：copy 跨设备不受限。
//
// 代价是“整目录换”变成了“逐文件换”，中途失败可能留下半新半旧的站点；
// 但完整备份的存在让回退能把它还原回去，事务语义要求的前端优先、
// 失败整体回退都保住了。
func applyWebBundleKeepOld(webDir, tarPath string) (string, error) {
	parent := filepath.Dir(webDir)
	tmp := filepath.Join(parent, webNewDirName)
	backup := filepath.Join(parent, webOldDirName)

	if err := os.RemoveAll(tmp); err != nil {
		return "", fmt.Errorf("清理临时目录失败: %w", err)
	}
	if err := os.RemoveAll(backup); err != nil {
		return "", fmt.Errorf("清理备份目录失败: %w", err)
	}
	defer os.RemoveAll(tmp)

	siteRoot, err := ExtractWebBundle(tarPath, tmp)
	if err != nil {
		return "", err
	}

	// webDir 不存在（首次安装/异常镜像）时直接建出来写入。
	if err := os.MkdirAll(webDir, 0o755); err != nil {
		return "", fmt.Errorf("创建前端目录失败: %w", err)
	}

	// ---- 第 1 步：完整备份旧站点（copy，跨设备安全）----
	// 备份必须**先完整做完**才动新内容：备份不全的话，回退就会把站点还原成
	// 缺文件的样子，比不回退更糟。
	oldFiles, err := listFiles(webDir)
	if err != nil {
		return "", err
	}
	// 备份目录**总是**建出来，哪怕旧站点本来就是空的。
	//
	// 原因：备份目录是"旧状态"的唯一载体。旧站点为空时若不建备份，
	// 回退阶段会因为"没有旧内容"而直接跳过 —— 于是"空站点 → 升级 → 接口
	// 替换失败"这条路径会把**新版文件留在原地**，比不回退更糟（前端已经
	// 在调新接口，配的却是旧 api）。有了空备份，回退就会把站点清回空态。
	if err := os.MkdirAll(backup, 0o755); err != nil {
		return "", fmt.Errorf("创建备份目录失败: %w", err)
	}
	if len(oldFiles) > 0 {
		if err := copyTree(webDir, backup); err != nil {
			return "", fmt.Errorf("备份原前端内容失败: %w", err)
		}
	}

	// ---- 第 2 步：写入新站点（文件级，同目录 rename 原子替换）----
	newFiles, err := listFiles(siteRoot)
	if err != nil {
		return backup, err
	}
	if err := installTree(siteRoot, webDir); err != nil {
		// 写新内容失败 ⇒ 还原成旧站点，绝不留在"半新半旧"的状态。
		if rbErr := rollbackWebBundle(webDir, backup); rbErr != nil {
			return backup, fmt.Errorf("写入新前端失败(%v)，且回滚失败(%v)", err, rbErr)
		}
		return backup, fmt.Errorf("写入新前端失败: %w", err)
	}

	// ---- 第 3 步：删掉新版本里没有的旧文件 ----
	// 不删的话，每次升级都在站点根下攒一批旧 hash 的死文件（前端产物是
	// 带 hash 的不可变资源，文件名一变就再也不会被请求到）。
	if err := pruneExtraFiles(webDir, newFiles); err != nil {
		if rbErr := rollbackWebBundle(webDir, backup); rbErr != nil {
			return backup, fmt.Errorf("清理旧前端文件失败(%v)，且回滚失败(%v)", err, rbErr)
		}
		return backup, fmt.Errorf("清理旧前端文件失败: %w", err)
	}

	return backup, nil
}

// 前端升级过程中的临时/备份目录名（都在 webDir 的父目录，即 /app 下）。
const (
	webNewDirName = ".web-new"
	webOldDirName = ".web-old"
)

// listFiles 列出一棵目录树里所有普通文件的**相对路径**（用 / 分隔）。
//
// 相对路径是"这套文件集合"的唯一标识：安装与回退都靠它判断某个文件
// 属于新版还是旧版。
func listFiles(root string) (map[string]bool, error) {
	files := map[string]bool{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		files[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("读取目录失败(%s): %w", root, err)
	}
	return files, nil
}

// copyTree 把 src 目录树**逐文件**复制到 dst。
//
// 用 copy 而不是 rename 的原因：copy 跨设备不受限，rename 不行。
// 目录靠 MkdirAll 重建，文件靠 copyFileMode 写 —— 全程不碰目录 rename。
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil // 符号链接等一律跳过
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := copyFileMode(p, target, mode); err != nil {
			return fmt.Errorf("复制 %s 失败: %w", rel, err)
		}
		return nil
	})
}

// installTree 把 src 目录树的内容装进 dst（dst 已存在，覆盖同名文件）。
//
// 每个文件用 replaceFile：同目录临时文件 + rename —— rename 发生在**同一
// 个目录**里，必然同设备，所以绝不会撞 EXDEV；而且是原子的，
// nginx 不会读到写了一半的文件。
func installTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil // 符号链接等一律跳过
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := replaceFileWithMode(p, target, 0o644); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", rel, err)
		}
		return nil
	})
}

// pruneExtraFiles 删除 webDir 下不在 keep 集合里的文件（含其空掉的目录）。
//
// keep 是"新版本应该有的文件"集合。删的是旧 hash 资源这类新版已不存在的文件。
func pruneExtraFiles(webDir string, keep map[string]bool) error {
	cur, err := listFiles(webDir)
	if err != nil {
		return err
	}
	var stale []string
	for rel := range cur {
		if !keep[rel] {
			stale = append(stale, rel)
		}
	}
	for _, rel := range stale {
		if err := os.Remove(filepath.Join(webDir, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除旧文件 %s 失败: %w", rel, err)
		}
	}
	// 顺带清掉空目录（新版没有的旧目录）。
	pruneEmptyDirs(webDir)
	return nil
}

// pruneEmptyDirs 自底向上删空目录。
func pruneEmptyDirs(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name())
		pruneEmptyDirs(p)
		if sub, err := os.ReadDir(p); err == nil && len(sub) == 0 {
			_ = os.Remove(p)
		}
	}
}

// rollbackWebBundle 把保留的旧前端内容换回来。
//
// old 是 applyWebBundleKeepOld 返回的**备份目录**（在 webDir 的父目录下），
// 里面是旧站点的完整副本。
//
// 回退分两步，同样**只用文件级操作**（绝不 rename 目录，见
// applyWebBundleKeepOld 的注释）：
//  1. 把旧版文件逐个写回 webDir（同目录 tmp + rename，原子）；
//  2. 把新版多出来的文件删掉（不属于旧版文件集合的）。
//
// 第 2 步不能省：只写回旧文件的话，新版新增的文件会留在站点里 ——
// 比如新版加了几个 js 资源、而旧版没有，回退后这些文件仍然在，
// 页面可能引用到与旧版不匹配的东西。
func rollbackWebBundle(webDir, old string) error {
	if old == "" || !Exists(old) {
		return nil // 本来就没有旧内容，没什么可回退
	}
	oldFiles, err := listFiles(old)
	if err != nil {
		return err
	}
	if len(oldFiles) == 0 {
		// 备份是空的 ⇒ 旧站点本来就是空的，那就把现在的内容全清掉。
		if err := pruneExtraFiles(webDir, map[string]bool{}); err != nil {
			return fmt.Errorf("回滚前端失败: %w", err)
		}
		return nil
	}
	if err := installTree(old, webDir); err != nil {
		return fmt.Errorf("回滚前端失败: %w", err)
	}
	if err := pruneExtraFiles(webDir, oldFiles); err != nil {
		return fmt.Errorf("回滚前端失败: %w", err)
	}
	return nil
}
