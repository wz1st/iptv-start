package until

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// 跨设备（EXDEV）场景的专项判据。
//
// 生产上 /app/web 常是**独立挂载点**（volume / bind mount），或是来自
// overlayfs 镜像层的目录。这两种情况下把 webDir 整个 rename 走会报
// "invalid cross-device link"（EXDEV），在线升级的前端这一步**从来没成功过**。
//
// 这条测试用两个不同设备（/tmp 与 /dev/shm，tmpfs vs overlay 天然不同设备）
// 复现那个失败条件，钉死"新版实现不再依赖目录 rename"。
//
// /dev/shm 不存在或与 /tmp 同设备时跳过（换台机器也能跑）。

// crossDeviceDirs 找两个**确实不同设备**的目录。
func crossDeviceDirs(t *testing.T) (string, string, bool) {
	t.Helper()
	a := t.TempDir()
	shm := "/dev/shm"
	if _, err := os.Stat(shm); err != nil {
		return "", "", false
	}
	b, err := os.MkdirTemp(shm, "iptv-xdev-")
	if err != nil {
		return "", "", false
	}
	t.Cleanup(func() { os.RemoveAll(b) })

	same, err := sameDevice(a, b)
	if err != nil {
		return "", "", false
	}
	if same {
		return "", "", false
	}
	return a, b, true
}

// sameDevice 判断两个路径是否在同一个设备上。
//
// 判据用"能否跨目录 rename"来间接判定：跨设备 rename 会报 EXDEV。
// 不用 Stat 结构体的 Dev 字段 —— 那个字段在 linux/bsd 上类型不同，
// 跨平台编译会炸。
//
// 返回值：true = 同设备，false = 跨设备。
//
// ★ 这里的 true/false 千万别写反：写反的话，跨设备机器上 crossDeviceDirs
// 会判定成"同设备"从而**跳过整个测试** —— 于是这个 EXDEV 专项测试在真正
// 需要它的环境里一次都没跑过，而在不需要它的环境里反倒跑了起来。
// （这种"假绿"最难发现：测试通过，但通过的是另一条分支。）
func sameDevice(a, b string) (bool, error) {
	probe := filepath.Join(b, ".probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		return false, err
	}
	defer os.Remove(probe)
	// 把 b 里的文件 rename 到 a —— 同设备才行。
	err := os.Rename(probe, filepath.Join(a, ".probe-moved"))
	if err == nil {
		os.Remove(filepath.Join(a, ".probe-moved"))
		return true, nil // rename 成功 ⇒ 同设备
	}
	if isCrossDeviceErr(err) {
		return false, nil // EXDEV ⇒ 跨设备
	}
	// 其它原因（权限不足、目标已存在…）不能当成跨设备 —— 保守判同设备，
	// 让测试跳过，而不是拿一个没验证过的组合去跑。
	return true, nil
}

// isCrossDeviceErr 判断错误是不是"跨设备"那一种。
//
// 认两种表述：syscall.EXDEV（linux/bsd/windows 都有这个 Errno）与错误文本里的
// "cross-device"（有些平台不把它包成 EXDEV，只在文本里说）。
func isCrossDeviceErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EXDEV) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "cross-device")
}

// webDir 在独立挂载点（跨设备）时，应用前端整包必须成功。
func TestApplyWebBundleWorksAcrossDevices(t *testing.T) {
	webParent, other, ok := crossDeviceDirs(t)
	if !ok {
		t.Skip("本机找不到可用的跨设备目录组合，跳过")
	}
	_ = other

	// webDir 放在"另一个设备"上（模拟 /app/web 是独立挂载点）。
	web := filepath.Join(webParent, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(web, "index.html"), "old")
	if err := os.MkdirAll(filepath.Join(web, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(web, "assets", "index.old.js"), "old-js")

	bundle := filepath.Join(t.TempDir(), "webdist.tar.gz")
	makeBundle(t, bundle, true, map[string]string{
		"index.html":          "new",
		"assets/index.new.js": "new-js",
	})

	// 旧实现在这里会失败：rename(webDir, .web-old) 跨设备 ⇒ EXDEV。
	// 现在的实现全程只做文件级操作，必须成功。
	if err := ApplyWebBundle(web, bundle); err != nil {
		t.Fatalf("跨设备场景下应用前端升级包应成功，实际失败: %v", err)
	}
	if got := ReadFileTrim(filepath.Join(web, "index.html")); got != "new" {
		t.Errorf("index.html 未替换，实际 %q", got)
	}
	if !Exists(filepath.Join(web, "assets", "index.new.js")) {
		t.Error("新资源未落地")
	}
	if Exists(filepath.Join(web, "assets", "index.old.js")) {
		t.Error("旧资源应被清掉")
	}
}

// 跨设备场景下，接口替换失败必须把前端**还原**成旧版。
func TestApplyUpdateRollsBackWebAcrossDevices(t *testing.T) {
	webParent, _, ok := crossDeviceDirs(t)
	if !ok {
		t.Skip("本机找不到可用的跨设备目录组合，跳过")
	}
	binDir := filepath.Join(webParent, "app")
	webDir := filepath.Join(binDir, "web")
	upDir := filepath.Join(t.TempDir(), "updata")
	for _, d := range []string{binDir, webDir, upDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkIdx(t, filepath.Join(webDir, "index.html"), "OLD-FRONT")
	mkIdx(t, filepath.Join(binDir, "iptv"), "OLD-API")
	mkWebBundle(t, filepath.Join(upDir, webBundleName), "NEW-FRONT")
	// 升级包里的 iptv 做成目录 ⇒ 替换失败，制造"前端已换、接口换不动"的现场。
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
		t.Fatalf("回退后前端文件必须还在: %v", rerr)
	}
	if !strings.Contains(string(idx), "OLD-FRONT") {
		t.Fatalf("前端必须回到旧版，实际内容 %q", string(idx))
	}
}

// 前端静态资源不该带执行位：站点里每个文件都是 0755 的话，权限位不对，
// 安全扫描会报，而且"能执行"对 html/js 毫无意义。
func TestApplyWebBundleSetsStaticFilePerm(t *testing.T) {
	root := t.TempDir()
	web := filepath.Join(root, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "webdist.tar.gz")
	makeBundle(t, bundle, true, map[string]string{
		"index.html":          "new",
		"assets/index.new.js": "new-js",
	})

	if err := ApplyWebBundle(web, bundle); err != nil {
		t.Fatalf("应用前端升级包失败: %v", err)
	}
	for _, rel := range []string{"index.html", filepath.Join("assets", "index.new.js")} {
		fi, err := os.Stat(filepath.Join(web, rel))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 != 0 {
			t.Errorf("静态资源 %s 不该带执行位，实际 %v", rel, fi.Mode().Perm())
		}
	}
}

// 回退必须把"新版新增的文件"也清掉 —— 只写回旧文件的话，
// 新版多出来的 js 会留在站点里，与旧版页面不匹配。
func TestRollbackRemovesFilesAddedByNewVersion(t *testing.T) {
	root := t.TempDir()
	web := filepath.Join(root, "web")
	backup := filepath.Join(root, ".web-old")
	for _, d := range []string{web, backup} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 旧站点：index.html + 一个旧资源
	write(t, filepath.Join(web, "index.html"), "OLD")
	if err := os.MkdirAll(filepath.Join(web, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(web, "assets", "old.js"), "OLDJS")
	// 备份里是旧版内容
	write(t, filepath.Join(backup, "index.html"), "OLD")
	if err := os.MkdirAll(filepath.Join(backup, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(backup, "assets", "old.js"), "OLDJS")
	// webDir 里混进了新版新增的文件
	write(t, filepath.Join(web, "assets", "new.js"), "NEWJS")

	if err := rollbackWebBundle(web, backup); err != nil {
		t.Fatalf("回退应成功: %v", err)
	}
	if got := ReadFileTrim(filepath.Join(web, "index.html")); got != "OLD" {
		t.Errorf("index.html 应回到旧版，实际 %q", got)
	}
	if Exists(filepath.Join(web, "assets", "new.js")) {
		t.Error("新版新增的文件在回退后应被清掉")
	}
	if got := ReadFileTrim(filepath.Join(web, "assets", "old.js")); got != "OLDJS" {
		t.Errorf("旧资源内容应保留，实际 %q", got)
	}
}
