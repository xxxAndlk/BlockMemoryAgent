package userprofile

// merge_test.go 验证 Merge 整理（2026-09-02 偏好与自进化期 1）：
// 人工行保护 / 冲突归档 / 去重 / 时间戳保留 / 归档裁剪 / 缺失小节补建 / 项目偏好双实例。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
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
	ctx := context.Background()
	s := NewProjectStore(t.TempDir())
	if err := s.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Append(ctx, "项目经验", "渲染帧先统一去白底"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got := s.Current(ctx).Content
	if !strings.Contains(got, "# 项目偏好") || !strings.Contains(got, "## 项目经验") {
		t.Fatalf("project template missing: %s", got)
	}
	view := s.MergeView(ctx, []string{"项目约定", "项目经验"})
	if len(view["项目经验"]) != 1 || view["项目经验"][0] != "渲染帧先统一去白底" {
		t.Fatalf("MergeView wrong: %v", view)
	}
	plan := MergePlan{
		Merged:   map[string][]string{"项目经验": {"渲染帧先去白底再合成"}},
		Archived: []string{"渲染帧先统一去白底"},
	}
	if err := s.ApplyMerge(ctx, plan, []string{"项目约定", "项目经验"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	got = s.Current(ctx).Content
	if !strings.Contains(got, "## 经验归档") || !strings.Contains(got, "渲染帧先统一去白底") {
		t.Fatalf("project archive missing: %s", got)
	}
}

// TestProjectStore_PerSession 项目偏好按 ctx 会话工作目录解析（S2）：
// ctx 注入会话目录时读写落在该目录的 .bma/project_preferences.md，未注入回落构造目录。
func TestProjectStore_PerSession(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	s := NewProjectStore(dirA)
	ctxB := tool.WithWorkDir(context.Background(), dirB)

	if err := s.Append(ctxB, "项目约定", "本项目用 pnpm"); err != nil {
		t.Fatalf("Append ctxB: %v", err)
	}
	if got := s.Current(ctxB).Content; !strings.Contains(got, "本项目用 pnpm") {
		t.Fatalf("Current ctxB missing content: %s", got)
	}
	// 默认目录侧读不到 dirB 的写入。
	if got := s.Current(context.Background()).Content; strings.Contains(got, "pnpm") {
		t.Fatalf("default dir should not see session B prefs: %s", got)
	}
	// 文件确实落在 dirB/.bma 下。
	if _, err := os.Stat(filepath.Join(dirB, ".bma", "project_preferences.md")); err != nil {
		t.Fatalf("prefs file should exist under dirB/.bma: %v", err)
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

// TestMerge_SectionLineCap 目标小节行数硬上限（2026-10-19）：merged 超 30 行时按
// 时间戳最旧优先移入归档小节（注入面有界），现行小节保持上限内。
func TestMerge_SectionLineCap(t *testing.T) {
	var merged []string
	for i := range maxAutoLinesPerSection + 5 {
		merged = append(merged, fmt.Sprintf("经验行%d", i))
	}
	s := newLoadedStore(t, "# 用户画像\n\n## 偏好\n- x（2026-01-01 10:00）\n")
	plan := MergePlan{Merged: map[string][]string{"偏好": merged}}
	if err := s.ApplyMerge(plan, []string{"偏好"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	got := s.Current().Content
	doc := ParseDocument(got)
	sec := findSection(doc, "偏好")
	if sec == nil || len(sec.Auto) != maxAutoLinesPerSection {
		t.Fatalf("section should be capped at %d auto lines: %v", maxAutoLinesPerSection, sec)
	}
	// 溢出 5 行进归档（带时间戳，可审计）。
	arc := findSection(doc, DefaultArchiveSection)
	if arc == nil || len(arc.Auto) != 5 {
		t.Fatalf("overflow lines should move to archive: %v", arc)
	}
}

// TestArchiveView_AndStripSection 归档视图（merge 冲突判据输入）与注入侧剥离：
// StripSection 剔除归档小节，其余内容逐字保留。
func TestArchiveView_AndStripSection(t *testing.T) {
	s := newLoadedStore(t, "# 用户画像\n\n## 偏好\n- 现行行（2026-01-01 10:00）\n\n## 反馈记录\n- 已废止的旧行（2026-01-01 09:00）\n")
	arch := s.ArchiveView()
	if len(arch) != 1 || arch[0] != "已废止的旧行" {
		t.Fatalf("ArchiveView wrong: %v", arch)
	}
	stripped := StripSection(s.Current().Content, DefaultArchiveSection)
	if strings.Contains(stripped, "已废止的旧行") || !strings.Contains(stripped, "现行行") {
		t.Fatalf("StripSection wrong: %s", stripped)
	}
	if strings.Contains(stripped, "## 反馈记录") {
		t.Fatalf("archive section header should be stripped: %s", stripped)
	}
	// 目标小节不存在时原样返回。
	if orig := s.Current().Content; StripSection(orig, "不存在的小节") != orig {
		t.Fatalf("StripSection should return content unchanged when section absent")
	}
}

func TestMerge_ArchiveAppendDedupAndNoResurrect(t *testing.T) {
	s := newLoadedStore(t, "# 用户画像\n\n## 偏好\n- 现行行（2026-01-01 10:00）\n\n## 反馈记录\n- 已废止旧行（2025-12-01 09:00）\n")
	plan := MergePlan{
		Merged:   map[string][]string{"偏好": {"现行行", "已废止旧行"}},
		Archived: []string{"已废止旧行", "已废止旧行"},
	}
	if err := s.ApplyMerge(plan, []string{"偏好"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	doc := ParseDocument(s.Current().Content)
	if pref := findSection(doc, "偏好"); pref != nil {
		for _, a := range pref.Auto {
			if a.Text == "已废止旧行" {
				t.Fatalf("archived line resurrected into live section: %s", s.Current().Content)
			}
		}
	}
	arc := findSection(doc, "反馈记录")
	n := 0
	for _, a := range arc.Auto {
		if a.Text == "已废止旧行" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("archive append dedup failed, got %d copies: %s", n, s.Current().Content)
	}
}

func TestMerge_ArchiveSelfHealExistingDups(t *testing.T) {
	// 历史脏数据：归档小节同文本多条；任意后续 merge 都应顺带自愈。
	s := newLoadedStore(t, "# 用户画像\n\n## 偏好\n- 现行行（2026-01-01 10:00）\n\n## 反馈记录\n- 重复旧行（2025-12-01 09:00）\n- 重复旧行（2025-12-01 09:00）\n- 另一条（2025-12-02 09:00）\n")
	plan := MergePlan{Merged: map[string][]string{"偏好": {"现行行", "新增行"}}}
	if err := s.ApplyMerge(plan, []string{"偏好"}); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	doc := ParseDocument(s.Current().Content)
	arc := findSection(doc, "反馈记录")
	n := 0
	for _, a := range arc.Auto {
		if a.Text == "重复旧行" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("archive self-heal failed, %d copies remain: %s", n, s.Current().Content)
	}
}

// TestBudgetSections_SkipsArchiveAndKeepsNewest 验证注入视图：归档小节整节剔除，
// 每小节优先保留最新自动行（2026-10-22 污染修复）。
func TestBudgetSections_SkipsArchiveAndKeepsNewest(t *testing.T) {
	content := `# 用户画像

## 偏好
- 旧偏好条目（2026-10-20 10:00）
- 新偏好条目（2026-10-22 10:00）

## 反馈记录
- 已废止的旧方案（2026-10-21 10:00）

## 技术栈
- 技术栈条目（2026-10-22 11:00）
`
	out := BudgetSections(content, 1200, "反馈记录")
	if strings.Contains(out, "已废止的旧方案") {
		t.Fatalf("归档小节不应进入注入视图:\n%s", out)
	}
	if !strings.Contains(out, "新偏好条目") || !strings.Contains(out, "技术栈条目") {
		t.Fatalf("最新自动行应保留:\n%s", out)
	}
	if n := len([]rune(out)); n > 1200 {
		t.Fatalf("注入视图应 ≤1200 runes，got %d", n)
	}
}

// TestBudgetSections_DropsOldestUnderTightBudget 验证预算紧张时按小节从最旧开始省略。
func TestBudgetSections_DropsOldestUnderTightBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("# 画像\n\n## 偏好\n\n")
	for i := 0; i < 8; i++ {
		b.WriteString("- 偏好条目" + string(rune('A'+i)) + strings.Repeat("内容", 20) + "（2026-10-2" + string(rune('0'+i%9)) + " 10:00）\n")
	}
	b.WriteString("\n## 技术栈\n\n")
	for i := 0; i < 8; i++ {
		b.WriteString("- 技术条目" + string(rune('A'+i)) + strings.Repeat("内容", 20) + "（2026-10-2" + string(rune('0'+i%9)) + " 10:00）\n")
	}
	out := BudgetSections(b.String(), 700)
	if strings.Contains(out, "偏好条目A") {
		t.Fatalf("最旧行应被省略:\n%s", out)
	}
	if !strings.Contains(out, "偏好条目H") || !strings.Contains(out, "技术条目H") {
		t.Fatalf("最新行必须保留:\n%s", out)
	}
	if !strings.Contains(out, "省略") {
		t.Fatalf("应标注省略行数:\n%s", out)
	}
	if n := len([]rune(out)); n > 700 {
		t.Fatalf("输出应 ≤700 runes, got %d", n)
	}
}

// TestAppendWithSource 验证自动行出处后缀（2026-10-23）：写入带来源会话短标签、
// 仍按自动行解析（时间戳+出处整体进 Stamp）、排序前缀仍是日期。
func TestAppendWithSource(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "profile.md"))
	if err := s.AppendWithSource("偏好", "采用16帧图集序列帧", "会话#65"); err != nil {
		t.Fatalf("AppendWithSource: %v", err)
	}
	content := s.Current().Content
	if !strings.Contains(content, "· 会话#65）") {
		t.Fatalf("自动行应带出处后缀: %s", content)
	}
	found := false
	for _, sec := range ParseDocument(content).Sections {
		for _, a := range sec.Auto {
			if a.Text == "采用16帧图集序列帧" && strings.Contains(a.Stamp, "会话#65") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("带出处的行应被解析为自动行: %s", content)
	}
}

// TestSessionSource 验证会话 ID → 短标签。
func TestSessionSource(t *testing.T) {
	if got := SessionSource("session-1792684183766702800-85936e94-65"); got != "会话#65" {
		t.Fatalf("SessionSource got %q", got)
	}
	if got := SessionSource(""); got != "" {
		t.Fatalf("空 ID 应返回空标签, got %q", got)
	}
}
