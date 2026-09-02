package userprofile

// merge_test.go 验证 Merge 整理（2026-09-02 偏好与自进化期 1）：
// 人工行保护 / 冲突归档 / 去重 / 时间戳保留 / 归档裁剪 / 缺失小节补建 / 项目偏好双实例。

import (
	"fmt"
	"strings"
	"testing"
)

func newLoadedStore(t *testing.T, content string) *Store {
	t.Helper()
	s := NewStore(t.TempDir() + "/profile.md")
	if err := s.Save(content); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return s
}

func TestMerge_HumanLineProtected(t *testing.T) {
	s := newLoadedStore(t, "# 用户画像\n\n## 偏好\n- 人工行不许动\n- 旧自动行（2026-01-01 10:00）\n")
	plan := MergePlan{
		Merged:   map[string][]string{"偏好": {"新偏好"}},
		Archived: []string{"旧自动行"},
	}
	if err := s.ApplyMerge(plan, []string{"偏好"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	got := s.Current().Content
	if !strings.Contains(got, "## 偏好") || !strings.Contains(got, "人工行不许动") {
		t.Fatalf("human line lost: %s", got)
	}
	if !strings.Contains(got, "新偏好") {
		t.Fatalf("merged line missing: %s", got)
	}
	// 人工行在前、自动行在后。
	if strings.Index(got, "人工行不许动") > strings.Index(got, "新偏好") {
		t.Fatalf("human line should render before auto lines: %s", got)
	}
}

func TestMerge_ConflictArchivedWithOriginalStamp(t *testing.T) {
	s := newLoadedStore(t, "# 用户画像\n\n## 偏好\n- 用 npm（2026-01-01 10:00）\n")
	plan := MergePlan{
		Merged:   map[string][]string{"偏好": {"用 pnpm 不用 npm"}},
		Archived: []string{"用 npm"},
	}
	if err := s.ApplyMerge(plan, []string{"偏好"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	got := s.Current().Content
	if !strings.Contains(got, "用 pnpm 不用 npm") {
		t.Fatalf("new preference missing: %s", got)
	}
	if !strings.Contains(got, "## 反馈记录") || !strings.Contains(got, "用 npm（2026-01-01 10:00）") {
		t.Fatalf("archive missing or stamp not preserved: %s", got)
	}
}

func TestMerge_DedupAndTimestampPreserved(t *testing.T) {
	s := newLoadedStore(t, "# 用户画像\n\n## 偏好\n- 直接给结论（2026-01-01 10:00）\n")
	plan := MergePlan{
		Merged: map[string][]string{"偏好": {"直接给结论", "直接给结论", "中文回答"}},
	}
	if err := s.ApplyMerge(plan, []string{"偏好"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	got := s.Current().Content
	if strings.Count(got, "直接给结论") != 1 {
		t.Fatalf("dedup failed: %s", got)
	}
	if !strings.Contains(got, "直接给结论（2026-01-01 10:00）") {
		t.Fatalf("old timestamp should be preserved for unchanged line: %s", got)
	}
}

func TestMerge_ArchiveCapped(t *testing.T) {
	var lines []string
	for i := range defaultArchiveCap + 5 {
		lines = append(lines, fmt.Sprintf("被替换的旧行%d", i))
	}
	s := newLoadedStore(t, "# 用户画像\n\n## 偏好\n- x（2026-01-01 10:00）\n")
	plan := MergePlan{
		Merged:   map[string][]string{"偏好": {"新行"}},
		Archived: lines,
	}
	if err := s.ApplyMerge(plan, []string{"偏好"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	got := s.Current().Content
	if strings.Contains(got, "被替换的旧行0") {
		t.Fatalf("oldest archive entry should be dropped: %s", got)
	}
	if !strings.Contains(got, fmt.Sprintf("被替换的旧行%d", defaultArchiveCap+4)) {
		t.Fatalf("newest archive entry missing: %s", got)
	}
}

func TestMerge_CreatesMissingSection(t *testing.T) {
	s := newLoadedStore(t, "# 用户画像\n\n## 偏好\n- x（2026-01-01 10:00）\n")
	plan := MergePlan{Merged: map[string][]string{"技术栈": {"Go + Vue"}}}
	if err := s.ApplyMerge(plan, []string{"偏好", "技术栈", "沟通风格"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	got := s.Current().Content
	if !strings.Contains(got, "## 技术栈") || !strings.Contains(got, "Go + Vue") {
		t.Fatalf("missing section not created: %s", got)
	}
}

func TestMerge_UnknownSectionIgnored(t *testing.T) {
	s := newLoadedStore(t, "# 用户画像\n\n## 偏好\n- x（2026-01-01 10:00）\n")
	plan := MergePlan{Merged: map[string][]string{"越界小节": {"不应落盘"}}}
	if err := s.ApplyMerge(plan, []string{"偏好"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	if strings.Contains(s.Current().Content, "越界小节") {
		t.Fatalf("unknown section leaked: %s", s.Current().Content)
	}
}

func TestMerge_ProjectStoreUsesProjectTemplateAndArchive(t *testing.T) {
	s := NewProjectStore(t.TempDir() + "/project_preferences.md")
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Append("项目经验", "渲染帧先统一去白底"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got := s.Current().Content
	if !strings.Contains(got, "# 项目偏好") || !strings.Contains(got, "## 项目经验") {
		t.Fatalf("project template missing: %s", got)
	}
	view := s.MergeView([]string{"项目约定", "项目经验"})
	if len(view["项目经验"]) != 1 || view["项目经验"][0] != "渲染帧先统一去白底" {
		t.Fatalf("MergeView wrong: %v", view)
	}
	plan := MergePlan{
		Merged:   map[string][]string{"项目经验": {"渲染帧先去白底再合成"}},
		Archived: []string{"渲染帧先统一去白底"},
	}
	if err := s.ApplyMerge(plan, []string{"项目约定", "项目经验"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	got = s.Current().Content
	if !strings.Contains(got, "## 经验归档") || !strings.Contains(got, "渲染帧先统一去白底") {
		t.Fatalf("project archive missing: %s", got)
	}
}

func TestMergeView_MissingSectionsPresentAsEmpty(t *testing.T) {
	s := newLoadedStore(t, "# 用户画像\n\n## 偏好\n- a（2026-01-01 10:00）\n")
	view := s.MergeView([]string{"偏好", "技术栈", "沟通风格"})
	if _, ok := view["技术栈"]; !ok {
		t.Fatalf("missing section key absent: %v", view)
	}
	if len(view["偏好"]) != 1 {
		t.Fatalf("偏好 view wrong: %v", view)
	}
}

func TestMerge_EmptyPlanNoop(t *testing.T) {
	orig := "# 用户画像\n\n## 偏好\n- x（2026-01-01 10:00）\n"
	s := newLoadedStore(t, orig)
	if err := s.ApplyMerge(MergePlan{}, []string{"偏好"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	if s.Current().Content != orig {
		t.Fatalf("empty plan should be noop, got: %s", s.Current().Content)
	}
}
