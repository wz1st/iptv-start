package until

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkWebBundle 打一个含 index.html 的前端整包。
func mkWebBundle(t *testing.T, path, marker string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := "<html>" + marker + "</html>"
	if err := tw.WriteHeader(&tar.Header{
		Name: "webdist/index.html", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func mkIdx(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 用户要求：**先装前端、再装接口，任一项失败都要整体回退**。
// 这条钉的是"接口替换失败时前端必须回到旧版" —— 少了它，失败现场会留下
// "新前端 + 旧 api"：页面能开但新接口全 404，比没升级更难排查。
func TestApplyUpdateRollsBackWebWhenBinaryFails(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "app")
	webDir := filepath.Join(binDir, "web")
	upDir := filepath.Join(root, "updata")
	for _, d := range []string{binDir, webDir, upDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkIdx(t, filepath.Join(webDir, "index.html"), "OLD-FRONT")
	mkIdx(t, filepath.Join(binDir, "iptv"), "OLD-API")
	mkWebBundle(t, filepath.Join(upDir, webBundleName), "NEW-FRONT")
	// 升级包里的 iptv 做成目录 ⇒ replaceFile 的 rename 会失败，
	// 从而制造一个"前端已换、接口换不动"的现场。
	if err := os.MkdirAll(filepath.Join(upDir, "iptv"), 0o755); err != nil {
		t.Fatal(err)
	}

	applied, err := ApplyUpdate(binDir, webDir, upDir)
	if err == nil {
		t.Fatal("接口替换失败时应报错")
	}
	if len(applied) != 0 {
		t.Fatalf("回退后不该报告任何已应用项，实际 %v", applied)
	}
	idx, rerr := os.ReadFile(filepath.Join(webDir, "index.html"))
	if rerr != nil {
		t.Fatalf("回退后前端目录必须还在: %v", rerr)
	}
	if !strings.Contains(string(idx), "OLD-FRONT") {
		t.Fatalf("前端必须回到旧版，实际内容 %q", string(idx))
	}
	if _, serr := os.Stat(filepath.Join(binDir, "iptv")); serr != nil {
		t.Fatalf("回退后原二进制必须还在: %v", serr)
	}
}

// 前端包本身坏了 ⇒ 一步都不该动，旧站点保持原样。
func TestApplyUpdateKeepsOldSiteOnBadBundle(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "app")
	webDir := filepath.Join(binDir, "web")
	upDir := filepath.Join(root, "updata")
	for _, d := range []string{binDir, webDir, upDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkIdx(t, filepath.Join(webDir, "index.html"), "OLD-FRONT")
	mkIdx(t, filepath.Join(binDir, "iptv"), "OLD-API")
	mkIdx(t, filepath.Join(upDir, webBundleName), "NOT-A-TARBALL")

	applied, err := ApplyUpdate(binDir, webDir, upDir)
	if err == nil {
		t.Fatal("坏包应报错")
	}
	if len(applied) != 0 {
		t.Fatalf("失败时不该报告已应用项，实际 %v", applied)
	}
	idx, _ := os.ReadFile(filepath.Join(webDir, "index.html"))
	if !strings.Contains(string(idx), "OLD-FRONT") {
		t.Fatalf("坏包不该动旧站点，实际 %q", string(idx))
	}
}

// 顺序：前端整包**排在 api 之前**。这条用"api 换成功、前端目录随后要存在"
// 反过来证明不了顺序，所以直接断言执行顺序由代码结构保证 —— 这里钉的是
// "全成功时前端与 api 同时生效，且备份目录被清掉"。
func TestApplyUpdateSuccessCleansBackup(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "app")
	webDir := filepath.Join(binDir, "web")
	upDir := filepath.Join(root, "updata")
	for _, d := range []string{binDir, webDir, upDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkIdx(t, filepath.Join(webDir, "index.html"), "OLD-FRONT")
	mkIdx(t, filepath.Join(binDir, "iptv"), "OLD-API")
	mkWebBundle(t, filepath.Join(upDir, webBundleName), "NEW-FRONT")
	mkIdx(t, filepath.Join(upDir, "iptv"), "NEW-API")

	applied, err := ApplyUpdate(binDir, webDir, upDir)
	if err != nil {
		t.Fatalf("全部替换应成功: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("应报告前端与接口两项，实际 %v", applied)
	}
	idx, _ := os.ReadFile(filepath.Join(webDir, "index.html"))
	if !strings.Contains(string(idx), "NEW-FRONT") {
		t.Fatalf("前端应已更新，实际 %q", string(idx))
	}
	api, _ := os.ReadFile(filepath.Join(binDir, "iptv"))
	if !strings.Contains(string(api), "NEW-API") {
		t.Fatalf("接口应已更新，实际 %q", string(api))
	}
	// 备份目录必须清掉 —— 留在磁盘上只是白占一份站点空间。
	if Exists(filepath.Join(binDir, ".web-old")) {
		t.Error("全部成功后前端备份目录应被清理")
	}
	if entries, _ := os.ReadDir(binDir); len(entries) > 2 {
		// web + iptv，剩下的（.web-old / .rollback-*）都不该残留
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				t.Errorf("残留临时项: %s", e.Name())
			}
		}
	}
}

// 引擎与 api 是平级目标：引擎替换失败也要把已经换好的 api 与前端一起退回。
func TestApplyUpdateRollsBackWhenEngineFails(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "app")
	webDir := filepath.Join(binDir, "web")
	upDir := filepath.Join(root, "updata")
	for _, d := range []string{binDir, webDir, upDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkIdx(t, filepath.Join(webDir, "index.html"), "OLD-FRONT")
	mkIdx(t, filepath.Join(binDir, "iptv"), "OLD-API")
	mkWebBundle(t, filepath.Join(upDir, webBundleName), "NEW-FRONT")
	mkIdx(t, filepath.Join(upDir, "iptv"), "NEW-API")
	// engine 做成目录 ⇒ 替换失败，于是前面换好的都必须退回去。
	if err := os.MkdirAll(filepath.Join(upDir, "engine"), 0o755); err != nil {
		t.Fatal(err)
	}

	applied, err := ApplyUpdate(binDir, webDir, upDir)
	if err == nil {
		t.Fatal("引擎替换失败时应报错")
	}
	if len(applied) != 0 {
		t.Fatalf("回退后不该报告已应用项，实际 %v", applied)
	}
	api, _ := os.ReadFile(filepath.Join(binDir, "iptv"))
	if !strings.Contains(string(api), "OLD-API") {
		t.Fatalf("接口必须回到旧版，实际 %q", string(api))
	}
	idx, _ := os.ReadFile(filepath.Join(webDir, "index.html"))
	if !strings.Contains(string(idx), "OLD-FRONT") {
		t.Fatalf("前端必须回到旧版，实际 %q", string(idx))
	}
}

// 原本没有该文件时回退要"删掉新文件"，而不是留下一半。
func TestRestoreFileRemovesWhenNoBackup(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "iptv")
	mkIdx(t, dst, "NEW")
	if err := restoreFile(filepath.Join(dir, "missing-backup"), dst); err != nil {
		t.Fatal(err)
	}
	if Exists(dst) {
		t.Fatal("没有备份时回退应删掉该文件")
	}
}
