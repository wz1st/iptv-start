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
func ApplyWebBundle(webDir, tarPath string) error {
	parent := filepath.Dir(webDir)
	tmp := filepath.Join(parent, ".web-new")
	old := filepath.Join(parent, ".web-old")

	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("清理临时目录失败: %w", err)
	}
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("清理备份目录失败: %w", err)
	}
	defer os.RemoveAll(tmp)

	siteRoot, err := ExtractWebBundle(tarPath, tmp)
	if err != nil {
		return err
	}

	// 老目录不存在（例如异常镜像）时直接上位，不必先改名。
	if Exists(webDir) {
		if err := os.Rename(webDir, old); err != nil {
			return fmt.Errorf("备份原前端目录失败: %w", err)
		}
	}
	if err := os.Rename(siteRoot, webDir); err != nil {
		if Exists(old) {
			if rbErr := os.Rename(old, webDir); rbErr != nil {
				return fmt.Errorf("新前端目录上位失败(%v)，且回滚失败(%v)", err, rbErr)
			}
		}
		return fmt.Errorf("新前端目录上位失败: %w", err)
	}

	if Exists(old) {
		os.RemoveAll(old)
	}
	return nil
}
