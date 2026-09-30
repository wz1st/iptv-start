package until

import (
	"os"
	"path/filepath"
	"testing"
)

// 版本号：问产物自己。（原 webbundle_test.go 里的这几条与升级包无关，
// 随升级包测试一起错删会丢掉 fakeBin —— imageversion/selfheal 的用例都要它。）

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

// WebVersion 读的是站点根目录下 index.html 的 meta 标记。
func TestWebVersion(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "index.html"),
		`<html><head><meta name="app-version" content="v3.0.7"></head></html>`)
	if got := WebVersion(dir); got != "v3.0.7" {
		t.Errorf("WebVersion = %q, want v3.0.7", got)
	}
	// 没有 index.html / 没有标记 → 空串
	if got := WebVersion(t.TempDir()); got != "" {
		t.Errorf("无 index.html 应回空串，实际 %q", got)
	}
}
