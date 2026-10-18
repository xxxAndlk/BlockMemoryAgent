package agent

// evolver_test.go 覆盖会话进化入口（2026-09-02 设计 §10）：
// 事件数闸门 / nil evolver 降级 / LLM 失败降级 / 三类产出落库 / 技能召回注入。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/internal/userprofile"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

func newEvolverTestSession(events int) *reactInternalSession {
	s := &reactInternalSession{
		ID:     "session-evolve-test",
		Goal:   "做一个塔防游戏",
		Result: "已完成并验证",
	}
	for i := range events {
		s.Events = append(s.Events, internalEvent{
			Type: eventkind.Message, Agent: "MetaAgent",
			Message: strings.Repeat("事件内容", i+1),
		})
	}
	s.Messages = []Message{
		{Role: string(enums.ChatRoleUser), Content: "用中文沟通，偏好简洁输出"},
		{Role: string(enums.ChatRoleAssistant), Content: "好的"},
	}
	return s
}

func newEvolverTestService(t *testing.T) *ReactService {
	t.Helper()
	return &ReactService{store: newReactSessionStore(nil)}
}

// TestEvolveSessionGateOnEventCount 事件数 < minEvolveEvents 不进进化（防噪音）。
func TestEvolveSessionGateOnEventCount(t *testing.T) {
	s := newEvolverTestService(t)
	called := false
	s.evolver = func(ctx context.Context, in EvolveInput) (*EvolveOutput, error) {
		called = true
		return &EvolveOutput{}, nil
	}
	s.evolveSession(newEvolverTestSession(minEvolveEvents-1), "success")
	if called {
		t.Fatalf("evolver should not be called below %d events", minEvolveEvents)
	}
}

// TestEvolveSessionNilEvolverFallsBack evolver 未接线回退纯画像提取（旧语义）。
func TestEvolveSessionNilEvolverFallsBack(t *testing.T) {
	s := newEvolverTestService(t)
	profile := userprofile.NewStore(t.TempDir() + "/user_profile.md")
	if err := profile.Load(); err != nil {
		t.Fatalf("load profile: %v", err)
	}
	s.userProfile = profile
	extracted := false
	s.profileExtractor = func(ctx context.Context, text string) ([]string, error) {
		extracted = true
		return []string{"偏好简洁输出"}, nil
	}
	s.evolveSession(newEvolverTestSession(6), "success")
	if !extracted {
		t.Fatal("nil evolver should fall back to profile extraction")
	}
	if !strings.Contains(profile.Current().Content, "偏好简洁输出") {
		t.Fatalf("profile should contain extracted preference, got: %q", profile.Current().Content)
	}
}

// TestEvolveSessionDegradeOnLLMError LLM 失败回退画像提取（静默降级）。
func TestEvolveSessionDegradeOnLLMError(t *testing.T) {
	s := newEvolverTestService(t)
	profile := userprofile.NewStore(t.TempDir() + "/user_profile.md")
	if err := profile.Load(); err != nil {
		t.Fatalf("load profile: %v", err)
	}
	s.userProfile = profile
	s.profileExtractor = func(ctx context.Context, text string) ([]string, error) {
		return []string{"降级提取"}, nil
	}
	s.evolver = func(ctx context.Context, in EvolveInput) (*EvolveOutput, error) {
		return nil, context.DeadlineExceeded
	}
	s.evolveSession(newEvolverTestSession(6), "success")
	if !strings.Contains(profile.Current().Content, "降级提取") {
		t.Fatalf("degraded extraction should reach profile, got: %q", profile.Current().Content)
	}
}

// TestEvolveSessionThreeOutputs 一次调用三类沉淀全落（Merge 不可用降级直写归档小节）。
func TestEvolveSessionThreeOutputs(t *testing.T) {
	s := newEvolverTestService(t)
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

	var sunkSkills []EvolvedSkill
	var sunkOutcome string
	s.skillSink = func(ctx context.Context, sessionID string, skills []EvolvedSkill, outcome string) error {
		sunkSkills, sunkOutcome = skills, outcome
		return nil
	}
	var logKinds []string
	s.evolutionLog = func(ctx context.Context, sessionID, kind, target, summary string) error {
		logKinds = append(logKinds, kind)
		return nil
	}
	s.evolver = func(ctx context.Context, in EvolveInput) (*EvolveOutput, error) {
		if in.Outcome != "failed" {
			t.Errorf("outcome should pass through, got %q", in.Outcome)
		}
		if in.Goal == "" || in.UserMessages == "" || in.EventsDigest == "" {
			t.Errorf("evolve input incomplete: goal=%q msgs=%d digest=%d", in.Goal, len(in.UserMessages), len(in.EventsDigest))
		}
		return &EvolveOutput{
			UserPrefs:      []string{"偏好简洁输出"},
			ProjectLessons: []string{"渲染动画帧前先统一去白底"},
			Skills: []EvolvedSkill{{
				Name: "frame-render-dedup", Title: "动画帧渲染去重",
				WhenToUse: "批量渲染动画帧出现重复底色时",
				Steps:     []string{"统一预处理"}, Pitfalls: []string{"逐帧去底慢"}, Verify: "抽查首尾帧",
			}},
		}, nil
	}

	s.evolveSession(newEvolverTestSession(6), "failed")

	if got := profile.Current().Content; !strings.Contains(got, "偏好简洁输出") || !strings.Contains(got, "## 反馈记录") {
		t.Fatalf("user prefs should land in archive section, got: %q", got)
	}
	if got := project.Current(context.Background()).Content; !strings.Contains(got, "渲染动画帧前先统一去白底") || !strings.Contains(got, "## 经验归档") {
		t.Fatalf("project lessons should land in archive section, got: %q", got)
	}
	if len(sunkSkills) != 1 || sunkSkills[0].Name != "frame-render-dedup" {
		t.Fatalf("skill sink should receive 1 skill, got %+v", sunkSkills)
	}
	if sunkOutcome != "failed" {
		t.Fatalf("skill sink outcome = %q, want failed", sunkOutcome)
	}
	for _, want := range []string{"user_pref", "project_lesson"} {
		found := false
		for _, k := range logKinds {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("evolution log missing kind %q, got %v", want, logKinds)
		}
	}
}

// TestInjectSkillRecall 召回注入：未接线/无命中零注入，命中注一行提示块。
func TestInjectSkillRecall(t *testing.T) {
	s := newEvolverTestService(t)
	goal := "帮我渲染一批动画帧"
	if got := s.injectSkillRecall(context.Background(), goal); got != goal {
		t.Fatalf("nil recall should return goal unchanged, got %q", got)
	}
	s.skillRecall = func(ctx context.Context, task string) []SkillRecallHint {
		return nil
	}
	if got := s.injectSkillRecall(context.Background(), goal); got != goal {
		t.Fatalf("no hits should return goal unchanged, got %q", got)
	}
	s.skillRecall = func(ctx context.Context, task string) []SkillRecallHint {
		return []SkillRecallHint{{Name: "frame-render-dedup", Title: "动画帧渲染去重"}}
	}
	got := s.injectSkillRecall(context.Background(), goal)
	if !strings.HasPrefix(got, "【相关经验】") {
		t.Fatalf("hint block should prefix goal, got %q", got)
	}
	if !strings.Contains(got, "frame-render-dedup") || !strings.Contains(got, "动画帧渲染去重") || !strings.Contains(got, goal) {
		t.Fatalf("hint block incomplete: %q", got)
	}
}

// ---- 写盘产物捕获（技能进化"带真实代码"供给） ----

func TestCaptureWriteArtifact(t *testing.T) {
	st := newReactSessionStore(nil)
	s := &reactInternalSession{ID: "session-artifact-test"}
	pyCode := "import os\nprint('hi')\n"
	writeArgs := `{"path":"scripts/check.py","content":` + mustJSONString(pyCode) + `}`

	// 非写文件工具：跳过。
	st.captureWriteArtifact(s, "RunCommand", writeArgs)
	if len(s.writeArtifacts) != 0 {
		t.Fatalf("non-write tool must not capture, got %d", len(s.writeArtifacts))
	}
	// 非脚本扩展名：跳过。
	st.captureWriteArtifact(s, "WriteFile", `{"path":"README.md","content":"# x"}`)
	if len(s.writeArtifacts) != 0 {
		t.Fatal("non-script ext must not capture")
	}
	// node_modules 路径：跳过。
	st.captureWriteArtifact(s, "WriteFile", `{"path":"node_modules/x/index.js","content":"console.log(1)"}`)
	if len(s.writeArtifacts) != 0 {
		t.Fatal("node_modules path must not capture")
	}
	// 超行：跳过（照抄半截会误导模型）。
	longCode := strings.Repeat("print(1)\n", 61)
	st.captureWriteArtifact(s, "WriteFile", `{"path":"scripts/long.py","content":`+mustJSONString(longCode)+`}`)
	if len(s.writeArtifacts) != 0 {
		t.Fatal("over-long script must not capture")
	}
	// 正常捕获 + 别名工具名。
	st.captureWriteArtifact(s, "write_file", writeArgs)
	if len(s.writeArtifacts) != 1 || s.writeArtifacts[0].Path != "scripts/check.py" || s.writeArtifacts[0].Ext != "py" {
		t.Fatalf("capture = %+v", s.writeArtifacts)
	}
	// 同路径后写覆盖。
	st.captureWriteArtifact(s, "WriteFile", `{"path":"scripts/check.py","content":"print('v2')\n"}`)
	if len(s.writeArtifacts) != 1 || !strings.Contains(s.writeArtifacts[0].Content, "v2") {
		t.Fatalf("same path must be overwritten, got %+v", s.writeArtifacts)
	}
	// 容量淘汰最旧：check.py 是最早的产物，超容时先被淘汰。
	for _, p := range []string{"scripts/a.py", "scripts/b.py", "scripts/c.py"} {
		st.captureWriteArtifact(s, "WriteFile", `{"path":"`+p+`","content":"print(1)\n"}`)
	}
	if len(s.writeArtifacts) != writeArtifactMaxCount {
		t.Fatalf("artifact count = %d, want %d", len(s.writeArtifacts), writeArtifactMaxCount)
	}
	if s.writeArtifacts[0].Path != "scripts/a.py" {
		t.Fatalf("oldest must be evicted first, got %s", s.writeArtifacts[0].Path)
	}
	// 渲染段：含路径与代码原文，空产物返回空串。
	text := artifactsText(s)
	if !strings.Contains(text, "scripts/a.py") || !strings.Contains(text, "print(1)") {
		t.Fatalf("artifactsText = %q", text)
	}
	if artifactsText(&reactInternalSession{}) != "" {
		t.Fatal("empty artifacts must render empty section")
	}
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
