package until

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestApplyUpdateEngineOnlyIgnoresStaleWeb 复现并锁死 2026-10-26 线上事故：
// 待安装区里除了本轮引擎产物 engine，还残留着上一轮管理系统升级留下的
// iptv / webdist.tar.gz。升级引擎时绝不能再把它们换上去，且应用成功后
// 待安装区必须清空（否则下次启动会被当成"待应用的升级包"再换一遍）。
func TestApplyUpdateEngineOnlyIgnoresStaleWeb(t *testing.T) {
	binDir := t.TempDir()
	webDir := t.TempDir()
	upDir := t.TempDir()

	// 当前运行目录里已有的产物（被替换的参照物）。
	curIptv := filepath.Join(binDir, "iptv")
	curEngine := filepath.Join(binDir, "engine")
	if err := os.WriteFile(curIptv, []byte("current-iptv-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(curEngine, []byte("current-engine-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 待安装区：本轮引擎产物 + 上一轮残留的管理系统/前端产物。
	stageEngine := filepath.Join(upDir, "engine")
	stageIptv := filepath.Join(upDir, "iptv")
	stageWeb := filepath.Join(upDir, "webdist.tar.gz")
	if err := os.WriteFile(stageEngine, []byte("new-engine-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stageIptv, []byte("stale-iptv-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stageWeb, []byte("stale-web-tarball"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 本轮登记的产物清单（kind=engine，只含 engine）：apply 阶段据此隔离范围。
	rec := stagedApplyRecord{
		Kind:  "engine",
		Files: map[string]string{"engine": fileHash(stageEngine)},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upDir, "staged.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	applied, err := ApplyUpdate(binDir, webDir, upDir)
	if err != nil {
		t.Fatalf("ApplyUpdate 失败: %v", err)
	}
	if len(applied) != 1 || applied[0] != "engine" {
		t.Fatalf("期望只应用 engine，实际: %v", applied)
	}

	// engine 应被替换成新内容。
	if got, _ := os.ReadFile(curEngine); string(got) != "new-engine-binary" {
		t.Fatalf("engine 未被替换: %q", got)
	}
	// iptv 必须保持原样（残留文件不能换上去）。
	if got, _ := os.ReadFile(curIptv); string(got) != "current-iptv-binary" {
		t.Fatalf("iptv 被错误替换: %q", got)
	}
	// 前端目录不该出现新文件（webdist 未被解包）。
	if entries, _ := os.ReadDir(webDir); len(entries) != 0 {
		t.Fatalf("前端目录被改动，多出文件: %v", entries)
	}
	// 待安装区应被清空（本轮产物 + 清单）。
	if entries, _ := os.ReadDir(upDir); len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("待安装区未被清空，残留: %v", names)
	}
}

// TestApplyUpdateWebStillAppliesAll 反向保证：管理系统链路仍按登记清单
// 同时更新 iptv + 前端，且成功后清空待安装区。
func TestApplyUpdateWebStillAppliesAll(t *testing.T) {
	binDir := t.TempDir()
	webDir := t.TempDir()
	upDir := t.TempDir()

	curIptv := filepath.Join(binDir, "iptv")
	curEngine := filepath.Join(binDir, "engine")
	os.WriteFile(curIptv, []byte("old-iptv"), 0o755)
	os.WriteFile(curEngine, []byte("old-engine"), 0o755)

	stageIptv := filepath.Join(upDir, "iptv")
	stageWeb := filepath.Join(upDir, "webdist.tar.gz")
	// 待安装区里还残留着上一轮引擎升级留下的 engine：管理系统升级绝不能把它换上去。
	stageEngine := filepath.Join(upDir, "engine")
	os.WriteFile(stageEngine, []byte("stale-engine-from-prior-round"), 0o755)
	os.WriteFile(stageIptv, []byte("new-iptv"), 0o755)
	makeBundle(t, stageWeb, true, map[string]string{"index.html": "new-web-snapshot"})

	rec := stagedApplyRecord{
		Kind:  "web",
		Files: map[string]string{"iptv": fileHash(stageIptv), "webdist.tar.gz": fileHash(stageWeb)},
	}
	b, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(upDir, "staged.json"), b, 0o644)

	applied, err := ApplyUpdate(binDir, webDir, upDir)
	if err != nil {
		t.Fatalf("ApplyUpdate 失败: %v", err)
	}
	got := map[string]bool{}
	for _, n := range applied {
		got[n] = true
	}
	if !got["iptv"] || !got["webdist.tar.gz"] {
		t.Fatalf("管理系统链路应同时应用 iptv 与 webdist.tar.gz，实际: %v", applied)
	}
	if got["engine"] {
		t.Fatalf("管理系统链路不应应用 engine: %v", applied)
	}
	// 残留的 engine 产物必须保持原样（不能换上去）。
	if got, _ := os.ReadFile(curEngine); string(got) != "old-engine" {
		t.Fatalf("engine 被错误替换: %q", got)
	}
	if entries, _ := os.ReadDir(upDir); len(entries) != 0 {
		t.Fatalf("待安装区未被清空: %v", entries)
	}
}

func fileHash(p string) string {
	b, _ := os.ReadFile(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
