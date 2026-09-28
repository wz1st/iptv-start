package until

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 读取"产物自己的版本号"。

// binaryVersionTimeout 是读版本号的上限。它是启动路径上的动作，卡住就等于
// 容器起不来，所以宁可判成"读不出来"也不能无限等。
const binaryVersionTimeout = 15 * time.Second

// BinaryVersion 执行 `<bin> -version` 取版本号。
func BinaryVersion(bin string) string {
	if !Exists(bin) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), binaryVersionTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, "-version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// webVersionTag 是前端产物里声明自己版本的方式：
var (
	webMetaTagRe = regexp.MustCompile(`(?is)<meta[^>]*\bname=["']app-version["'][^>]*>`)
	webContentRe = regexp.MustCompile(`(?is)\bcontent=["']([^"']+)["']`)
)

// WebVersion 从站点根目录的 index.html 里读出前端版本号。
func WebVersion(webDir string) string {
	return parseWebVersion(ReadFileTrim(filepath.Join(webDir, "index.html")))
}

// parseWebVersion 从 index.html 文本里抽版本号（抽成纯函数便于单测）。
func parseWebVersion(html string) string {
	tag := webMetaTagRe.FindString(html)
	if tag == "" {
		return ""
	}
	if m := webContentRe.FindStringSubmatch(tag); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// BundleVersion 从**不解包**地读前端升级包（webdist.tar.gz）里的版本号。
func BundleVersion(tarPath string) string {
	f, err := os.Open(tarPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return ""
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return ""
		}
		if err != nil {
			return ""
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		rel, err := safeRelPath(hdr.Name)
		if err != nil || filepath.Base(rel) != webIndexName {
			continue
		}
		// index.html 只有几 KB，限制一下防止读到畸形头里的巨大 Size。
		data, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return ""
		}
		return parseWebVersion(string(data))
	}
}
