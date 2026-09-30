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
