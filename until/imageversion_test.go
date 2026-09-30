package until

import (
	"os"
	"path/filepath"
	"testing"
)

// 镜像内置版本清单 + 版本汇总的兜底行为。

func TestImageVersionsParsesManifest(t *testing.T) {
	dir := t.TempDir()
	content := "# 注释行不该被当成键\n" +
		"\n" +
		"api=v4.0.0\n" +
		"front = v4.0.0 \n" + // 键值两侧空格要容忍
		"engine=v3.0.0\n" +
		"unknownkey=x\n" // 未识别的键忽略
	if err := os.WriteFile(filepath.Join(dir, ImageVersionsFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	api, front, engine := ImageVersions(dir)
	if api != "v4.0.0" || front != "v4.0.0" || engine != "v3.0.0" {
		t.Errorf("清单解析结果 api=%q front=%q engine=%q", api, front, engine)
	}
}

func TestImageVersionsMissingFile(t *testing.T) {
	api, front, engine := ImageVersions(t.TempDir())
	if api != "" || front != "" || engine != "" {
		t.Errorf("清单不存在时应全部回空串，实际 %q/%q/%q", api, front, engine)
	}
}

// 产物答得出 → 用产物的答案，清单不参与。
func TestVersionSummaryPrefersBinary(t *testing.T) {
	binDir := t.TempDir()
	webDir := t.TempDir()
	write(t, filepath.Join(binDir, ImageVersionsFile), "api=v_img\nfront=v_img\nengine=v_img\n")

	fakeBin(t, filepath.Join(binDir, "iptv"), "v_real_api")
	write(t, filepath.Join(webDir, "index.html"),
		`<html><head><meta name="app-version" content="v_real_front"></head></html>`)
	fakeBin(t, filepath.Join(binDir, "engine"), "v_real_engine")

	api, front, engine := VersionSummary(binDir, webDir, filepath.Join(binDir, "engine"))
	if api != "v_real_api" || front != "v_real_front" || engine != "v_real_engine" {
		t.Errorf("产物能答时应用产物版本，实际 api=%q front=%q engine=%q", api, front, engine)
	}
}

// 产物答不出 → 回落到镜像清单（**这才是本次要修的行为**：不再显示"未知"）。
func TestVersionSummaryFallsBackToImageManifest(t *testing.T) {
	binDir := t.TempDir()
	webDir := t.TempDir()
	write(t, filepath.Join(binDir, ImageVersionsFile),
		"api=v4.0.0\nfront=v4.0.0\nengine=v3.0.0\n")

	// 三个产物都不存在（或不可执行）⇒ 都答不出
	api, front, engine := VersionSummary(binDir, webDir, filepath.Join(binDir, "engine"))
	if api != "v4.0.0" {
		t.Errorf("api 读不出时应回落到镜像清单 v4.0.0，实际 %q", api)
	}
	if front != "v4.0.0" {
		t.Errorf("front 读不出时应回落到镜像清单 v4.0.0，实际 %q", front)
	}
	if engine != "v3.0.0" {
		t.Errorf("engine 读不出时应回落到镜像清单 v3.0.0，实际 %q", engine)
	}
}

// 产物答不出 **且** 没有清单（老镜像）→ 才退回"未知"。
func TestVersionSummaryUnknownWhenNoManifest(t *testing.T) {
	binDir := t.TempDir()
	webDir := t.TempDir()
	api, front, engine := VersionSummary(binDir, webDir, filepath.Join(binDir, "engine"))
	if api != "未知" || front != "未知" || engine != "未知" {
		t.Errorf("既无产物答案又无清单时应回\"未知\"，实际 %q/%q/%q", api, front, engine)
	}
}

// 部分产物答得出、部分答不出 ⇒ 只对答不出的那段用清单兜底。
func TestVersionSummaryPartialFallback(t *testing.T) {
	binDir := t.TempDir()
	webDir := t.TempDir()
	write(t, filepath.Join(binDir, ImageVersionsFile), "api=v_img\nfront=v_img\nengine=v_img\n")

	fakeBin(t, filepath.Join(binDir, "iptv"), "v_real_api")
	// front、engine 缺失 ⇒ 用清单
	api, front, engine := VersionSummary(binDir, webDir, filepath.Join(binDir, "engine"))
	if api != "v_real_api" || front != "v_img" || engine != "v_img" {
		t.Errorf("部分兜底结果应为 v_real_api/v_img/v_img，实际 %q/%q/%q", api, front, engine)
	}
}

// 兜底来源报告：值不带标记，但要用一行日志说明"哪些是清单兜的"。
func TestVersionFallbackNote(t *testing.T) {
	binDir := t.TempDir()
	webDir := t.TempDir()
	write(t, filepath.Join(binDir, ImageVersionsFile), "api=v_img\nfront=v_img\nengine=v_img\n")

	// 全答不出 ⇒ 三段都报
	fell := VersionFallbackNote(binDir, webDir, filepath.Join(binDir, "engine"))
	if len(fell) != 3 {
		t.Errorf("全部读不出时应报 3 个组件，实际 %v", fell)
	}

	// 只有 api 答得出 ⇒ 不再报管理系统
	fakeBin(t, filepath.Join(binDir, "iptv"), "v_real_api")
	if fell := VersionFallbackNote(binDir, webDir, filepath.Join(binDir, "engine")); len(fell) != 2 {
		t.Errorf("api 读得出时应报 2 个组件，实际 %v", fell)
	}

	// 没有清单（老镜像）⇒ 不报（因为不是"用清单兜的"）
	empty := t.TempDir()
	if fell := VersionFallbackNote(empty, empty, filepath.Join(empty, "engine")); len(fell) != 0 {
		t.Errorf("无清单时不应报兜底，实际 %v", fell)
	}
}
