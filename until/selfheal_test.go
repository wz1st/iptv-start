package until

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 产物自愈：运行目录里的产物被换坏、或被卷里的旧包降级时，用镜像内置副本恢复。

func mkBackupDir(t *testing.T, bin string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(bin, ImageBackupDir), 0o755); err != nil {
		t.Fatal(err)
	}
}

// 产物读不出自身版本号（这里用"文件不存在"复刻）⇒ 用副本恢复。
func TestSelfHealRestoresUnreadableBinary(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, ImageVersionsFile), "api=v4.0.0\nfront=v4.0.0\nengine=v3.0.0\n")
	mkBackupDir(t, bin)
	fakeBin(t, filepath.Join(bin, ImageBackupDir, "iptv"), "v4.0.0")

	healed, err := SelfHealBinaries(bin)
	if err != nil {
		t.Fatalf("自愈出错: %v", err)
	}
	if len(healed) != 1 {
		t.Fatalf("应恢复 1 个产物，实际 %v", healed)
	}
	if !strings.Contains(healed[0], "读不出自身版本号") {
		t.Errorf("恢复原因应说明产物读不出，实际 %q", healed[0])
	}
	if got := BinaryVersion(filepath.Join(bin, "iptv")); got != "v4.0.0" {
		t.Errorf("恢复后应能答出 v4.0.0，实际 %q", got)
	}
}

// 产物被卷里的旧包降级（答得出，但低于镜像内置）⇒ 同样恢复。
func TestSelfHealRestoresDowngradedBinary(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, ImageVersionsFile), "api=v4.0.0\nfront=v4.0.0\nengine=v3.0.0\n")
	mkBackupDir(t, bin)
	fakeBin(t, filepath.Join(bin, ImageBackupDir, "iptv"), "v4.0.0")
	fakeBin(t, filepath.Join(bin, "iptv"), "v3.0.0") // 3.x 旧包留下的

	healed, err := SelfHealBinaries(bin)
	if err != nil {
		t.Fatalf("自愈出错: %v", err)
	}
	if len(healed) != 1 || !strings.Contains(healed[0], "低于镜像内置") {
		t.Fatalf("应恢复被降级的产物，实际 %v", healed)
	}
	if got := BinaryVersion(filepath.Join(bin, "iptv")); got != "v4.0.0" {
		t.Errorf("恢复后应回到 v4.0.0，实际 %q", got)
	}
}

// **高于**镜像版本的产物（用户自己在线升级上来的）不许动。
func TestSelfHealKeepsNewerBinary(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, ImageVersionsFile), "api=v4.0.0\nfront=v4.0.0\nengine=v3.0.0\n")
	mkBackupDir(t, bin)
	fakeBin(t, filepath.Join(bin, ImageBackupDir, "iptv"), "v4.0.0")
	fakeBin(t, filepath.Join(bin, "iptv"), "v4.5.0") // 在线升级上来的，比镜像新

	healed, err := SelfHealBinaries(bin)
	if err != nil {
		t.Fatalf("自愈出错: %v", err)
	}
	if len(healed) != 0 {
		t.Errorf("高于镜像版本的产物不该被回退，实际 %v", healed)
	}
	if got := BinaryVersion(filepath.Join(bin, "iptv")); got != "v4.5.0" {
		t.Errorf("产物应保持 v4.5.0，实际 %q", got)
	}
}

// 老镜像（没有 .image 副本）⇒ 一律不动，避免误伤。
func TestSelfHealNoBackup(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, ImageVersionsFile), "api=v4.0.0\nfront=v4.0.0\nengine=v3.0.0\n")
	fakeBin(t, filepath.Join(bin, "iptv"), "v3.0.0")

	healed, err := SelfHealBinaries(bin)
	if err != nil {
		t.Fatalf("自愈出错: %v", err)
	}
	if len(healed) != 0 {
		t.Errorf("没有副本时不该动产物，实际 %v", healed)
	}
}

// **本次核心修复**：产物读不出 + 镜像清单有值 ⇒ 不得放行卷里的旧包。
// 旧口径会落到"当前二进制不可执行，按需应用"，主动把旧包怼上去。
func TestShouldApplyUpdateRejectsStaleWhenCurrentUnreadable(t *testing.T) {
	bin := t.TempDir()
	web := filepath.Join(bin, "web")
	up := t.TempDir()
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(bin, ImageVersionsFile), "api=v4.0.0\nfront=v4.0.0\nengine=v3.0.0\n")
	// /app/iptv 读不出（不存在），卷里躺着一个旧包
	fakeBin(t, filepath.Join(up, "iptv"), "v3.9.0")

	apply, why := ShouldApplyUpdate(bin, web, up)
	if apply {
		t.Errorf("产物读不出且有镜像清单时，不得放行旧包（%s）", why)
	}
	if !strings.Contains(why, "拒绝降级") {
		t.Errorf("拒绝原因应说明是降级，实际 %q", why)
	}
}

// 陈旧升级包清理：低于镜像版本的删掉，高的留着，读不出的留着。
func TestCleanStaleUpdata(t *testing.T) {
	bin := t.TempDir()
	up := t.TempDir()
	write(t, filepath.Join(bin, ImageVersionsFile), "api=v4.0.0\nfront=v4.0.0\nengine=v3.0.0\n")
	fakeBin(t, filepath.Join(up, "iptv"), "v3.0.0")            // 低于镜像 ⇒ 删
	fakeBin(t, filepath.Join(up, "engine"), "v5.0.0")          // 高于镜像 ⇒ 留
	write(t, filepath.Join(up, "webdist.tar.gz"), "not-a-tar") // 读不出 ⇒ 留

	removed := CleanStaleUpdata(bin, up)
	if len(removed) != 1 || removed[0] != "iptv" {
		t.Fatalf("只应删掉 iptv，实际 %v", removed)
	}
	if Exists(filepath.Join(up, "iptv")) {
		t.Error("陈旧包应已被删除")
	}
	if !Exists(filepath.Join(up, "engine")) {
		t.Error("高于镜像版本的包不该被删")
	}
	if !Exists(filepath.Join(up, "webdist.tar.gz")) {
		t.Error("读不出内容的包不该被删")
	}
}
