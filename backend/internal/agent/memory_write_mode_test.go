package agent

// memory_write_mode_test.go 覆盖长期记忆写入三档（2026-10-24 P0）：
//   - shadow：候选只进 evolution_log，三面现行存储（画像/项目偏好/技能）完全不变；
//   - off：不调 evolver（连候选都不生成）；
//   - enforce：确定性准入门——一次性任务选择不进全局画像，mixed/failed 不晋升项目经验。

import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/internal/userprofile"
)

// memModeFixture 构造一个三类存储都接线、evolver 返回固定候选的服务。
func memModeFixture(t *testing.T, mode string, out *EvolveOutput) (*ReactService, *userprofile.Store, *userprofile.ProjectStore, *[]string, *int) {
	t.Helper()
	s := newEvolverTestService(t)
	s.SetMemoryWriteMode(mode)
	profile := userprofile.NewStore(t.TempDir() + "/user_profile.md")
	project := userprofile.NewProjectStore(t.TempDir())
	if err := profile.Load(); err != nil {
		t.Fatalf("load profile: %v", err)
	}
	if err := project.Load(context.Background()); err != nil {
		t.Fatalf("load project prefs: %v", err)
	}
	s.userProfile = profile
	s.projectPrefs = project

	logKinds := &[]string{}
	s.evolutionLog = func(ctx context.Context, sessionID, kind, target, summary string) error {
		*logKinds = append(*logKinds, kind)
		return nil
	}
	sunk := 0
	s.skillSink = func(ctx context.Context, sessionID string, skills []EvolvedSkill, outcome string) error {
		sunk++
		return nil
	}
	called := 0
	s.evolver = func(ctx context.Context, in EvolveInput) (*EvolveOutput, error) {
		called++
		return out, nil
	}
	return s, profile, project, logKinds, &called
}

func sampleEvolveOutput() *EvolveOutput {
	return &EvolveOutput{
		UserPrefs:      []string{"以后所有输出都用中文，记住这个偏好", "用 D 版"},
		ProjectLessons: []string{"渲染动画帧前先统一去白底并做技术体检"},
		Skills: []EvolvedSkill{{
			Name: "frame-preflight", Title: "帧序列预检",
			WhenToUse: "渲染帧序列前", Steps: []string{"统一去底"}, Verify: "抽查首尾帧",
		}},
	}
}

// TestMemoryShadowWritesNothing shadow 下三面现行存储必须完全不变，候选只进审计日志。
func TestMemoryShadowWritesNothing(t *testing.T) {
	s, profile, project, logKinds, called := memModeFixture(t, MemoryWriteModeShadow, sampleEvolveOutput())
	s.evolveSession(newEvolverTestSession(6), "success")

	if *called != 1 {
		t.Fatalf("shadow 应仍生成候选（evolver 调用 1 次），got %d", *called)
	}
	if strings.TrimSpace(profile.Current().Content) != "" {
		t.Fatalf("shadow 不得改用户画像，got: %q", profile.Current().Content)
	}
	if strings.TrimSpace(project.Current(context.Background()).Content) != "" {
		t.Fatalf("shadow 不得改项目经验，got: %q", project.Current(context.Background()).Content)
	}
	var hasShadow bool
	for _, k := range *logKinds {
		if strings.HasPrefix(k, "shadow_") {
			hasShadow = true
		}
		if k == "user_pref" || k == "project_lesson" {
			t.Fatalf("shadow 不得记现行写入流水 %q（会与真实写入混淆）", k)
		}
	}
	if !hasShadow {
		t.Fatalf("shadow 候选必须落 evolution_log，got %v", *logKinds)
	}
}

// TestMemoryOffSkipsEvolver off 下不跑进化（零候选、零成本）。
func TestMemoryOffSkipsEvolver(t *testing.T) {
	s, profile, project, logKinds, called := memModeFixture(t, MemoryWriteModeOff, sampleEvolveOutput())
	s.evolveSession(newEvolverTestSession(6), "success")
	if *called != 0 {
		t.Fatalf("off 不应调用 evolver，got %d", *called)
	}
	if strings.TrimSpace(profile.Current().Content) != "" || strings.TrimSpace(project.Current(context.Background()).Content) != "" {
		t.Fatal("off 下现行记忆必须不变")
	}
	if len(*logKinds) != 0 {
		t.Fatalf("off 下不应写任何进化流水，got %v", *logKinds)
	}
}

// TestMemoryEnforceOneShotPreferenceRejected 一次性任务选择（"用 D 版"）不得写全局画像。
func TestMemoryEnforceOneShotPreferenceRejected(t *testing.T) {
	s, profile, project, _, _ := memModeFixture(t, MemoryWriteModeEnforce, sampleEvolveOutput())
	s.evolveSession(newEvolverTestSession(6), "success")

	got := profile.Current().Content
	if !strings.Contains(got, "以后所有输出都用中文") {
		t.Fatalf("带长期意图的偏好应写入画像，got: %q", got)
	}
	if strings.Contains(got, "用 D 版") {
		t.Fatalf("一次性任务选择不得写全局画像，got: %q", got)
	}
	if !strings.Contains(project.Current(context.Background()).Content, "统一去白底") {
		t.Fatalf("success 会话的项目经验应写入，got: %q", project.Current(context.Background()).Content)
	}
}

// TestMemoryEnforceMixedNoProjectLesson mixed/failed 会话只留失败档案，不晋升现行项目经验。
func TestMemoryEnforceMixedNoProjectLesson(t *testing.T) {
	for _, outcome := range []string{"mixed", "failed"} {
		s, _, project, logKinds, _ := memModeFixture(t, MemoryWriteModeEnforce, sampleEvolveOutput())
		s.evolveSession(newEvolverTestSession(6), outcome)
		if strings.Contains(project.Current(context.Background()).Content, "统一去白底") {
			t.Fatalf("%s 会话不得晋升现行项目经验，got: %q", outcome, project.Current(context.Background()).Content)
		}
		for _, k := range *logKinds {
			if k == "project_lesson" {
				t.Fatalf("%s 会话不应记 project_lesson 写入流水", outcome)
			}
		}
	}
}

// TestLongTermPreferenceGate 准入判据单测：长期意图 + 非一次性任务标记。
func TestLongTermPreferenceGate(t *testing.T) {
	yes := []string{"以后都用中文回复", "记住我的偏好是简洁", "始终先给我看锚点图", "长期用这个命名规范"}
	no := []string{"用 D 版", "这一版先用红色", "这个角色改用侧视", "把尾巴调长一点", ""}
	for _, s := range yes {
		if !isLongTermPreference(s) {
			t.Errorf("应判为长期偏好: %q", s)
		}
	}
	for _, s := range no {
		if isLongTermPreference(s) {
			t.Errorf("不应判为长期偏好: %q", s)
		}
	}
}

// TestMemoryWriteModeNormalize 模式归一：空=shadow（config 默认），未注入的直构服务=legacy enforce。
func TestMemoryWriteModeNormalize(t *testing.T) {
	var zero ReactService
	if got := zero.MemoryWriteMode(); got != MemoryWriteModeEnforce {
		t.Fatalf("未注入模式应保持 legacy enforce 语义，got %q", got)
	}
	s := &ReactService{}
	s.SetMemoryWriteMode("")
	if got := s.MemoryWriteMode(); got != MemoryWriteModeShadow {
		t.Fatalf("显式注入空串应按 shadow，got %q", got)
	}
	s.SetMemoryWriteMode("bogus")
	if got := s.MemoryWriteMode(); got != MemoryWriteModeShadow {
		t.Fatalf("非法模式应按 shadow 回落，got %q", got)
	}
	s.SetMemoryWriteMode("ENFORCE")
	if got := s.MemoryWriteMode(); got != MemoryWriteModeEnforce {
		t.Fatalf("模式应大小写归一，got %q", got)
	}
}

func TestMemoryEnforceWritesStructuredEffectiveView(t *testing.T) {
	s, _, _, _, _ := memModeFixture(t, MemoryWriteModeEnforce, sampleEvolveOutput())
	repo := store.NewMemoryLongTermStore()
	lc := NewMemoryLifecycle(repo, RetractionModeEnforce)
	s.SetMemoryLifecycle(lc)
	sess := newEvolverTestSession(6)
	sess.setWorkDir(`D:\proj\moying`)
	s.evolveSession(sess, "success")

	userBlock := lc.RenderScope(context.Background(), "", "", store.MemoryScopeUser, 2000)
	if !strings.Contains(userBlock, "以后所有输出都用中文") {
		t.Fatalf("准入的用户偏好应进入结构化有效视图:\n%s", userBlock)
	}
	projectBlock := lc.RenderScope(context.Background(), "", sess.currentWorkDir(), store.MemoryScopeProject, 2000)
	if !strings.Contains(projectBlock, "统一去白底") {
		t.Fatalf("准入的项目经验应进入结构化有效视图:\n%s", projectBlock)
	}
	items, _ := repo.ListItems(context.Background(), store.MemoryQuery{Statuses: []string{store.MemoryStatusActive}})
	if len(items) != 2 {
		t.Fatalf("只应激活两条准入记忆，got %+v", items)
	}
	for _, item := range items {
		if item.Origin != store.MemoryOriginEvolver || item.SourceSession != sess.ID {
			t.Fatalf("自动记忆来源/会话必须保留: %+v", item)
		}
	}
}
