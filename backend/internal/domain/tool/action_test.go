package tool

// action_test.go 覆盖 Action 授权规则与事务管线的负面场景（计划 §6.7）。
//
// 这些用例刻意覆盖"旧实现会放行/会丢"的路径：
//   - RunCommand 用 Node 脚本改两个文件，只有一个获授权时实际变化被识别并回滚；
//   - 同一路径但语义目标不同的越界（语义目标未上线时记为越界，不虚报通过）；
//   - dirty/untracked 文件在回滚后完整保留；
//   - 检查点失败时 enforce 不写生产文件；
//   - 验收目录/检查点目录被模型写入时拒绝；
//   - 插件 unknown capability 不被当成只读；
//   - shadow 只记录，不改变旧行为。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/checkpoint"
	"github.com/blockmemory/agent/backend/internal/team"
)

// memSink 内存 Action 事件落点（测试用）。
type memSink struct {
	mu     sync.Mutex
	events []ActionEvent
}

func (s *memSink) Append(_ context.Context, ev ActionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *memSink) phases(actionID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, 8)
	for _, ev := range s.events {
		if actionID == "" || ev.ActionID == actionID {
			out = append(out, ev.Phase)
		}
	}
	return out
}

func (s *memSink) has(actionID, phase string) bool {
	for _, p := range s.phases(actionID) {
		if p == phase {
			return true
		}
	}
	return false
}

// contractFor 构造带写范围的合同。
func contractFor(t *testing.T, writePaths ...string) *team.LogicalTaskContract {
	t.Helper()
	c := &team.LogicalTaskContract{
		LogicalTaskID: "lt-test", SessionID: "s-test", Revision: 1, Goal: "测试",
		Status:   team.TaskStatusActive,
		WriteSet: []team.ResourceRule{},
	}
	for _, p := range writePaths {
		c.WriteSet = append(c.WriteSet, team.ResourceRule{
			Kind: team.RuleKindPath, Value: p, Effect: team.RuleEffectAllow, Source: team.RuleSourceUser,
		})
	}
	if err := team.NormalizeContract(c); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if err := team.ValidateContract(c); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return c
}

func writeTemp(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTemp(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return string(data)
}

// TestAuthorizerDenyPrecedence deny 优先，且 ExcludedSet 恒 deny。
func TestAuthorizerDenyPrecedence(t *testing.T) {
	c := contractFor(t, "src/a.css")
	c.ExcludedSet = []team.ResourceRule{{Kind: team.RuleKindPath, Value: "src/a.css", Effect: team.RuleEffectDeny}}
	if err := team.NormalizeContract(c); err != nil {
		t.Fatal(err)
	}
	auth := Authorizer{}.Authorize([]ResourceRequest{{Kind: team.RuleKindPath, Value: "src/a.css", Write: true}},
		ActionPolicy{Mode: ActionModeEnforce, Contract: c})
	if auth.Decision != "deny" {
		t.Fatalf("excluded path must be denied even when in write set, got %+v", auth)
	}
}

// TestAuthorizerEmptyWriteSetIsAskNotAllow 空 WriteSet 不等于全允许。
func TestAuthorizerEmptyWriteSetIsAskNotAllow(t *testing.T) {
	c := contractFor(t) // 无写范围
	auth := Authorizer{}.Authorize([]ResourceRequest{{Kind: team.RuleKindPath, Value: "src/a.css", Write: true}},
		ActionPolicy{Mode: ActionModeEnforce, Contract: c})
	if auth.Decision == "allow" {
		t.Fatalf("empty write set must not authorize writes: %+v", auth)
	}
	if !auth.EscalationNeeded {
		t.Fatal("empty write set must require contract escalation")
	}
}

// TestAuthorizerProtectedDirs 验收规则/检查点目录禁止写入（I-VERIFY-2）。
func TestAuthorizerProtectedDirs(t *testing.T) {
	c := contractFor(t, ".bma/verification/guards.md", ".bma/checkpoints/x")
	policy := ActionPolicy{
		Mode:          ActionModeEnforce,
		Contract:      c,
		ProtectedDirs: []string{".bma/verification", ".bma/checkpoints"},
	}
	for _, p := range []string{".bma/verification/guards.md", ".bma/checkpoints/x"} {
		auth := Authorizer{}.Authorize([]ResourceRequest{{Kind: team.RuleKindPath, Value: p, Write: true}}, policy)
		if auth.Decision != "deny" {
			t.Fatalf("%s must be denied even when in write set, got %+v", p, auth)
		}
	}
}

// TestAuthorizerSemanticTargetMismatch 同一路径但语义目标不同 → 不算授权。
func TestAuthorizerSemanticTargetMismatch(t *testing.T) {
	c := contractFor(t, "src/App.vue")
	target := team.SemanticTarget{ID: "hdr-name", File: "src/App.vue", Kind: "template_node"}
	c.Targets = []team.SemanticTarget{target}
	if err := team.NormalizeContract(c); err != nil {
		t.Fatal(err)
	}
	// 语义目标 ID 匹配 → allow
	ok := Authorizer{}.Authorize([]ResourceRequest{{
		Kind: team.RuleKindSemanticTarget, Value: "hdr-name", Write: true, Semantic: &target,
	}}, ActionPolicy{Mode: ActionModeEnforce, Contract: c})
	if ok.Decision != "allow" {
		t.Fatalf("authorized semantic target must be allowed, got %+v", ok)
	}
	// 同路径不同语义 ID → 不匹配，进入 ask/deny（不得虚报通过）
	other := team.SemanticTarget{ID: "hdr-time", File: "src/App.vue"}
	bad := Authorizer{}.Authorize([]ResourceRequest{{
		Kind: team.RuleKindSemanticTarget, Value: "hdr-time", Write: true, Semantic: &other,
	}}, ActionPolicy{Mode: ActionModeEnforce, Contract: c})
	if bad.Decision == "allow" {
		t.Fatalf("unlisted semantic target must not be allowed: %+v", bad)
	}
}

// TestPipelineEnforceRollsBackUnauthorizedFile 事故第 15 轮回放：
// 一个 Action（RunCommand / Node 脚本）改了两个文件，只有其中一个获授权 →
// 实际变更被真实 diff 识别，整个 Action 按事务语义回滚（all-or-nothing）。
func TestPipelineEnforceRollsBackUnauthorizedFile(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "src/allowed.vue", "<template>orig</template>\n")
	writeTemp(t, root, "src/shared.vue", ".label{min-width:4.2em}\n")
	store := checkpoint.NewStore(t.TempDir())
	sink := &memSink{}
	p := NewActionPipeline(ActionPipelineConfig{Mode: ActionModeEnforce, Store: store, Events: sink})

	c := contractFor(t, "src/allowed.vue")
	ctx := WithLogicalTask(context.Background(), c)
	ctx = WithSessionID(ctx, "s-test")
	ctx = WithTrustModeFunc(ctx, func() string { return TrustModeFullAuto })

	res, out := p.Run(ctx, "RunCommand", nil, map[string]any{"command": "node edit.js"}, root, func() *Result {
		// 模拟 Node 脚本：改授权文件 + 改未授权共享文件
		if err := os.WriteFile(filepath.Join(root, "src/allowed.vue"), []byte("<template>new</template>\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "src/shared.vue"), []byte(".label{min-width:5em}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return &Result{Tool: "RunCommand", Success: true, Output: "done"}
	})
	if out.Decision != "allow" {
		t.Fatalf("RunCommand with full-auto should pass authorization phase, got %+v", out)
	}
	if res.Success {
		t.Fatal("unauthorized mutation must fail the action")
	}
	if !out.RolledBack {
		t.Fatalf("outcome must record rollback: %+v", out)
	}
	if got := readTemp(t, root, "src/shared.vue"); got != ".label{min-width:4.2em}\n" {
		t.Fatalf("unauthorized file must be restored, got %q", got)
	}
	// Action 事务是 all-or-nothing（计划 §6.6③「越界文件变更回滚本 Action」）：
	// 该 Action 内获授权的改动也一并回滚，保证模型必须重新以范围内动作重做。
	if got := readTemp(t, root, "src/allowed.vue"); got != "<template>orig</template>\n" {
		t.Fatalf("whole action must roll back atomically, got %q", got)
	}
	if !strings.Contains(res.Error, "src/shared.vue") {
		t.Fatalf("failure message must name the violating file, got %q", res.Error)
	}
	phases := sink.phases(out.ActionID)
	for _, want := range []string{ActionPhaseRequested, ActionPhaseCheckpoint, ActionPhaseExecuted,
		ActionPhaseMutated, ActionPhaseScopeViolate, ActionPhaseRolledBack} {
		if !sink.has(out.ActionID, want) {
			t.Fatalf("missing phase %q in %v", want, phases)
		}
	}
}

// TestPipelineShadowDoesNotRollback shadow 只记录，不改变旧行为。
func TestPipelineShadowDoesNotRollback(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "src/shared.vue", "orig\n")
	store := checkpoint.NewStore(t.TempDir())
	sink := &memSink{}
	p := NewActionPipeline(ActionPipelineConfig{Mode: ActionModeShadow, Store: store, Events: sink})
	c := contractFor(t, "src/other.vue")
	ctx := WithLogicalTask(context.Background(), c)
	ctx = WithSessionID(ctx, "s-test")

	res, out := p.Run(ctx, "WriteFile", nil, map[string]any{"path": "src/shared.vue"}, root, func() *Result {
		_ = os.WriteFile(filepath.Join(root, "src/shared.vue"), []byte("clobbered\n"), 0o644)
		return &Result{Tool: "WriteFile", Success: true}
	})
	if !res.Success {
		t.Fatal("shadow mode must not block execution")
	}
	if out.RolledBack {
		t.Fatal("shadow mode must not roll back")
	}
	if out.ShadowWould == "" {
		t.Fatal("shadow mode must record what enforce would do")
	}
	if got := readTemp(t, root, "src/shared.vue"); got != "clobbered\n" {
		t.Fatalf("shadow mode must leave disk as-is, got %q", got)
	}
	if !sink.has(out.ActionID, ActionPhaseScopeViolate) {
		t.Fatal("shadow mode must still record scope violation")
	}
}

// TestPipelinePreservesDirtyAndUntracked 回滚后用户已有 dirty/untracked 文件保留（R-10/R-11）。
func TestPipelinePreservesDirtyAndUntracked(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "src/dirty.vue", "user-wip\n")
	writeTemp(t, root, "src/untracked.css", ".u{}\n")
	writeTemp(t, root, "src/target.css", ".t{1}\n")
	store := checkpoint.NewStore(t.TempDir())
	p := NewActionPipeline(ActionPipelineConfig{Mode: ActionModeEnforce, Store: store})
	c := contractFor(t, "src/target.css")
	ctx := WithLogicalTask(context.Background(), c)
	ctx = WithSessionID(ctx, "s-test")
	ctx = WithTrustModeFunc(ctx, func() string { return TrustModeFullAuto })

	_, out := p.Run(ctx, "RunCommand", nil, map[string]any{"command": "node x.js"}, root, func() *Result {
		if err := os.WriteFile(filepath.Join(root, "src/dirty.vue"), []byte("clobbered\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return &Result{Tool: "RunCommand", Success: true}
	})
	if !out.RolledBack {
		t.Fatalf("expected rollback: %+v", out)
	}
	if got := readTemp(t, root, "src/dirty.vue"); got != "user-wip\n" {
		t.Fatalf("user dirty content must be restored to its pre-action state, got %q", got)
	}
	if got := readTemp(t, root, "src/untracked.css"); got != ".u{}\n" {
		t.Fatalf("untracked file must survive rollback, got %q", got)
	}
}

// TestPipelineCheckpointFailureFailsClosed 检查点失败时 enforce 不写生产文件（I-EXEC-3）。
func TestPipelineCheckpointFailureFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "src/a.css", "orig\n")
	// 用一个不可写的检查点目录模拟 Capture 失败。
	badDir := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(badDir, 0o500); err != nil {
		t.Fatal(err)
	}
	store := checkpoint.NewStore(badDir)
	if store == nil {
		t.Fatal("store required")
	}
	// 在 Windows 上目录权限不一定拦住写入：再用"索引目录是文件"制造确定性失败。
	if err := os.RemoveAll(filepath.Join(badDir, "blobs")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "blobs"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewActionPipeline(ActionPipelineConfig{Mode: ActionModeEnforce, Store: store})
	c := contractFor(t, "src/a.css")
	ctx := WithLogicalTask(context.Background(), c)
	ctx = WithSessionID(ctx, "s-test")
	ctx = WithTrustModeFunc(ctx, func() string { return TrustModeFullAuto })

	executed := false
	res, out := p.Run(ctx, "WriteFile", nil, map[string]any{"path": "src/a.css"}, root, func() *Result {
		executed = true
		_ = os.WriteFile(filepath.Join(root, "src/a.css"), []byte("new\n"), 0o644)
		return &Result{Tool: "WriteFile", Success: true}
	})
	if executed {
		t.Fatal("enforce must not execute when checkpoint creation fails")
	}
	if res.Success {
		t.Fatalf("result must fail closed, got %+v", res)
	}
	if got := readTemp(t, root, "src/a.css"); got != "orig\n" {
		t.Fatalf("production file must be untouched, got %q", got)
	}
	if out.Executed {
		t.Fatal("outcome must record not executed")
	}
}

// TestPipelineProtectedDirWritesDenied 验收目录/检查点目录写入被拒绝（R-22）。
func TestPipelineProtectedDirWritesDenied(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, ".bma/verification/guards.md", "original-guard\n")
	store := checkpoint.NewStore(t.TempDir())
	p := NewActionPipeline(ActionPipelineConfig{
		Mode: ActionModeEnforce, Store: store,
		Protected: []string{".bma/verification", ".bma/checkpoints"},
	})
	c := contractFor(t, ".bma/verification/guards.md")
	ctx := WithLogicalTask(context.Background(), c)
	ctx = WithSessionID(ctx, "s-test")
	ctx = WithTrustModeFunc(ctx, func() string { return TrustModeFullAuto })

	executed := false
	res, _ := p.Run(ctx, "WriteFile", nil, map[string]any{"path": ".bma/verification/guards.md"}, root, func() *Result {
		executed = true
		return &Result{Tool: "WriteFile", Success: true}
	})
	if executed {
		t.Fatal("protected directory writes must be blocked before execution")
	}
	if res.Success {
		t.Fatal("protected write must fail")
	}
	if got := readTemp(t, root, ".bma/verification/guards.md"); got != "original-guard\n" {
		t.Fatalf("guard file must stay untouched, got %q", got)
	}
}

// TestCapabilitiesUnknownForPluginTools 插件工具未声明能力时必须按 unknown 处理（R-21）。
func TestCapabilitiesUnknownForPluginTools(t *testing.T) {
	caps := capabilitiesOf(nil, "host_computer_use__screenshot")
	if len(caps) != 1 || caps[0] != CapUnknown {
		t.Fatalf("plugin tool without declaration must be unknown, got %v", caps)
	}
	if !CapabilitiesMutateFiles(caps) {
		t.Fatal("unknown capability must be treated as possibly mutating")
	}
	// 内置读工具不应被当成写入
	readCaps := capabilitiesOf(nil, "ReadFile")
	if CapabilitiesMutateFiles(readCaps) {
		t.Fatalf("ReadFile must be read-only, got %v", readCaps)
	}
	// forged__ 工具按 shell 处理
	forged := capabilitiesOf(nil, "forged__my_tool")
	if !CapabilitiesMutateFiles(forged) {
		t.Fatalf("forged tool must be treated as mutating, got %v", forged)
	}
}

// TestPipelineOffIsTransparent off 模式零行为（性能与兼容纪律）。
func TestPipelineOffIsTransparent(t *testing.T) {
	p := NewActionPipeline(ActionPipelineConfig{Mode: ActionModeOff, Store: checkpoint.NewStore(t.TempDir())})
	if p.Enabled() {
		t.Fatal("off mode must not be enabled")
	}
	root := t.TempDir()
	writeTemp(t, root, "src/a.css", "orig\n")
	res, out := p.Run(context.Background(), "WriteFile", nil, nil, root, func() *Result {
		_ = os.WriteFile(filepath.Join(root, "src/a.css"), []byte("new\n"), 0o644)
		return &Result{Tool: "WriteFile", Success: true}
	})
	if !res.Success || out.ActionID == "" {
		t.Fatalf("off mode must execute transparently: %+v %+v", res, out)
	}
	if got := readTemp(t, root, "src/a.css"); got != "new\n" {
		t.Fatalf("off mode must not roll back, got %q", got)
	}
}

// TestRuleGlobMatching glob 规则（含 ** 跨目录）。
func TestRuleGlobMatching(t *testing.T) {
	cases := []struct {
		pattern, target string
		want            bool
	}{
		{"src/**/*.css", "src/a/b/c.css", true},
		{"src/*.css", "src/a.css", true},
		{"src/*.css", "src/a/b.css", false},
		{"src/**", "src/a/b/c.vue", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.target); got != c.want {
			t.Fatalf("globMatch(%q,%q) = %v, want %v", c.pattern, c.target, got, c.want)
		}
	}
}

// TestMutationManifestEvidenceIsRealDiff 变更清单来自真实 diff，不采信模型声明。
func TestMutationManifestEvidenceIsRealDiff(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "src/a.css", "v1\n")
	writeTemp(t, root, "src/b.css", "v1\n")
	store := checkpoint.NewStore(t.TempDir())
	p := NewActionPipeline(ActionPipelineConfig{Mode: ActionModeEnforce, Store: store})
	c := contractFor(t, "src/a.css", "src/b.css")
	ctx := WithLogicalTask(context.Background(), c)
	ctx = WithSessionID(ctx, "s-test")
	ctx = WithTrustModeFunc(ctx, func() string { return TrustModeFullAuto })

	_, out := p.Run(ctx, "WriteFile", nil, map[string]any{"path": "src/a.css"}, root, func() *Result {
		// 模型声称只改 a.css，实际只改 a.css（b.css 不动）
		_ = os.WriteFile(filepath.Join(root, "src/a.css"), []byte("v2\n"), 0o644)
		return &Result{Tool: "WriteFile", Success: true, Output: "done"}
	})
	if out.Manifest == nil {
		t.Fatal("manifest must be recorded")
	}
	if len(out.Manifest.Modified) != 1 || out.Manifest.Modified[0] != "src/a.css" {
		t.Fatalf("manifest must reflect real diff, got %+v", out.Manifest)
	}
	payload, _ := json.Marshal(out.Manifest)
	if !strings.Contains(string(payload), "src/a.css") {
		t.Fatalf("manifest json must contain path: %s", payload)
	}
}
