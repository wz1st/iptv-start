package until

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// 版本号：问产物自己

// fakeBin 造一个"能回答 -version 的可执行文件"。
func fakeBin(t *testing.T, path, version string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+version+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestBinaryVersionReadsFromExecutable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "iptv")
	fakeBin(t, bin, "v3.0.7")
	if got := BinaryVersion(bin); got != "v3.0.7" {
		t.Errorf("应读出 v3.0.7，实际 %q", got)
	}

	// 文件不存在 / 不可执行 → 空串（不臆造版本号）
	if got := BinaryVersion(filepath.Join(t.TempDir(), "nope")); got != "" {
		t.Errorf("不存在的文件应回空串，实际 %q", got)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, []byte("not an exe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := BinaryVersion(plain); got != "" {
		t.Errorf("不可执行的文件应回空串，实际 %q", got)
	}
}

func TestParseWebVersion(t *testing.T) {
	cases := map[string]string{
		`<head><meta name="app-version" content="v3.0.7"></head>`:               "v3.0.7",
		`<meta content="v3.0.7" name="app-version" />`:                          "v3.0.7",
		"<meta name='app-version' content='v3.0.8'>":                            "v3.0.8",
		`<meta http-equiv="X" content="v1">`:                                    "",
		`<html>nothing</html>`:                                                  "",
		"<meta\n  name=\"app-version\"\n  content=\"v9.9.9\">":                  "v9.9.9",
		`<meta name="app-version" content="v3.0.7"><meta name="a" content="b">`: "v3.0.7",
	}
	for html, want := range cases {
		if got := parseWebVersion(html); got != want {
			t.Errorf("parseWebVersion(%q) = %q, want %q", html, got, want)
		}
	}
}

func TestWebVersionAndBundleVersion(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "index.html"), `<html><head><meta name="app-version" content="v3.0.7"></head></html>`)
	if got := WebVersion(dir); got != "v3.0.7" {
		t.Errorf("WebVersion = %q, want v3.0.7", got)
	}

	// 升级包里的版本号要能"不解包"读出来
	bundle := filepath.Join(t.TempDir(), "webdist.tar.gz")
	makeBundle(t, bundle, true, map[string]string{
		"index.html": `<html><head><meta name="app-version" content="v3.0.8"></head></html>`,
	})
	if got := BundleVersion(bundle); got != "v3.0.8" {
		t.Errorf("BundleVersion = %q, want v3.0.8", got)
	}

	// 包里没有版本标记 → 空串
	noTag := filepath.Join(t.TempDir(), "notag.tar.gz")
	makeBundle(t, noTag, true, map[string]string{"index.html": "<html></html>"})
	if got := BundleVersion(noTag); got != "" {
		t.Errorf("无标记应回空串，实际 %q", got)
	}
}

// 前端整包在线升级

// makeBundle 打一个 webdist.tar.gz。nest=true 时按 CI 的形态带顶层目录。
func makeBundle(t *testing.T, path string, nest bool, files map[string]string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	prefix := ""
	if nest {
		prefix = "webdist/"
		if err := tw.WriteHeader(&tar.Header{Name: prefix, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		hdr := &tar.Header{
			Name:     prefix + name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractWebBundleFindsSiteRoot(t *testing.T) {
	// CI 打的包带顶层 webdist/ 目录
	nested := filepath.Join(t.TempDir(), "webdist.tar.gz")
	makeBundle(t, nested, true, map[string]string{
		"index.html":          "<html>new</html>",
		"assets/index.aaa.js": "js",
	})
	root, err := ExtractWebBundle(nested, t.TempDir())
	if err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	if filepath.Base(root) != "webdist" {
		t.Errorf("应识别出 webdist 作为站点根，实际 %s", root)
	}
	if got := ReadFileTrim(filepath.Join(root, "index.html")); got != "<html>new</html>" {
		t.Errorf("index.html 内容异常: %q", got)
	}
	if !Exists(filepath.Join(root, "assets", "index.aaa.js")) {
		t.Error("子目录文件未解出")
	}

	// 解平的包（顶层就是 index.html）也要能认
	flat := filepath.Join(t.TempDir(), "flat.tar.gz")
	makeBundle(t, flat, false, map[string]string{"index.html": "flat"})
	root, err = ExtractWebBundle(flat, t.TempDir())
	if err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	if got := ReadFileTrim(filepath.Join(root, "index.html")); got != "flat" {
		t.Errorf("解平包站点根识别错误: %q", root)
	}
}

func TestExtractWebBundleRejectsTraversalAndMissingIndex(t *testing.T) {
	// 越界路径必须拒绝：升级包来自网络，不能因为发布方写错就往 /app 外面写
	evil := filepath.Join(t.TempDir(), "evil.tar.gz")
	makeBundle(t, evil, false, map[string]string{
		"index.html":  "x",
		"../../pwned": "boom",
	})
	if _, err := ExtractWebBundle(evil, t.TempDir()); err == nil {
		t.Error("含 ../ 的包应被拒绝")
	}

	// 没有 index.html 的包不能当站点根
	noidx := filepath.Join(t.TempDir(), "noidx.tar.gz")
	makeBundle(t, noidx, true, map[string]string{"assets/a.js": "js"})
	if _, err := ExtractWebBundle(noidx, t.TempDir()); err == nil {
		t.Error("缺少 index.html 的包应报错")
	}
}

func TestApplyWebBundleReplacesWholeDir(t *testing.T) {
	bin := t.TempDir()
	web := filepath.Join(bin, "web")
	if err := os.MkdirAll(filepath.Join(web, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(web, "index.html"), "old")
	write(t, filepath.Join(web, "assets", "index.old.js"), "old-js")

	bundle := filepath.Join(t.TempDir(), "webdist.tar.gz")
	makeBundle(t, bundle, true, map[string]string{
		"index.html":          "new",
		"assets/index.new.js": "new-js",
	})

	if err := ApplyWebBundle(web, bundle); err != nil {
		t.Fatalf("应用前端升级包失败: %v", err)
	}

	if got := ReadFileTrim(filepath.Join(web, "index.html")); got != "new" {
		t.Errorf("index.html 未替换，实际 %q", got)
	}
	if !Exists(filepath.Join(web, "assets", "index.new.js")) {
		t.Error("新资源未落地")
	}
	// 整目录替换：旧 hash 的资源必须消失，否则每升一次就多攒一批死文件
	if Exists(filepath.Join(web, "assets", "index.old.js")) {
		t.Error("旧资源应被整目录替换掉")
	}
	if Exists(filepath.Join(bin, ".web-new")) || Exists(filepath.Join(bin, ".web-old")) {
		t.Error("临时/备份目录应已清理")
	}
}

func TestApplyWebBundleKeepsOldSiteOnBadBundle(t *testing.T) {
	bin := t.TempDir()
	web := filepath.Join(bin, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(web, "index.html"), "old")

	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	makeBundle(t, bad, true, map[string]string{"assets/a.js": "js"}) // 缺 index.html

	if err := ApplyWebBundle(web, bad); err == nil {
		t.Fatal("缺 index.html 的包应报错")
	}
	// 关键：站点不能被换坏 —— 老目录还在，仍能对外服务
	if got := ReadFileTrim(filepath.Join(web, "index.html")); got != "old" {
		t.Errorf("失败时不应动老目录，实际 index.html=%q", got)
	}
	if Exists(filepath.Join(bin, ".web-old")) {
		t.Error("失败时不应留下备份目录")
	}
}
