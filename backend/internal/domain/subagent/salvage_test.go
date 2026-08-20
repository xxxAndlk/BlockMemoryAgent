package subagent

// salvage_test.go 验证 TODO #20 第二/三层：
//   - 失败打捞：runSubAgent 失败路径（含循环守卫终止）提取摘要写共享槽位 + 追加进父 mailbox；
//   - 同域重派继承：withPriorSalvage 对 Failed/Cancelled 兄弟注入【前序探索摘要】；
//   - buildSharedPrefix 跳过打捞槽位（不向无关子 Agent 通用注入）。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// loopCallProvider 每次 Generate 都返回同一个"文本 + 工具调用"消息（无限循环），
// 驱动子 Agent 反复调用同一工具直到循环守卫（连读 ×3）命中；文本部分使失败打捞有内容可提取。
type loopCallProvider struct {
	resp *blades.Message
}

func (p *loopCallProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	return &blades.ModelResponse{Message: p.resp}, nil
}
func (p *loopCallProvider) Name() string { return "loop-call" }

func readFileToolCall(path string) *blades.Message {
	raw, _ := json.Marshal(map[string]any{"path": path})
	return blades.AssistantMessage(
		blades.TextPart{Text: "我正在读取文件并分析其结构。"},
		blades.ToolPart{Name: "ReadFile", Request: string(raw)},
	)
}

// newSalvageTestEnv 构造带 tree + FileSharedMemoryStore 的 Dispatcher 测试环境。
func newSalvageTestEnv(t *testing.T, provider agent.ModelProvider) (*Dispatcher, *mailbox.Mailbox, *orchestrator.Tree, *tool.Registry, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}, SystemPrompt: "通用领域纪律"},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(dir, nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: provider}, toolsReg, mb, agent.NopMemoryPipeline{})
	tr := orchestrator.NewTree("s1", nil)
	d.WithTree(func(string) *orchestrator.Tree { return tr })
	d.WithSharedMemory(tool.NewFileSharedMemoryStore(dir))
	d.RegisterCallTool(toolsReg)
	return d, mb, tr, toolsReg, dir
}

// TestDispatcher_LoopGuardFailsChildWithSalvage 端到端：子 Agent 反复 ReadFile 同参
// 命中连读守卫 -> ErrLoopExit -> runSubAgent 失败路径 -> 树 Failed + 父 mailbox 收
// "被循环守卫终止"失败消息（含【失败打捞】摘要）+ 共享槽位 s1:salvage:配置 可读。
func TestDispatcher_LoopGuardFailsChildWithSalvage(t *testing.T) {
	d, mb, tr, toolsReg, dir := newSalvageTestEnv(t, &loopCallProvider{resp: readFileToolCall("a.txt")})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"domain":         "配置",
		"task":           "读 a.txt 并报告",
		"responsibility": "负责配置相关文件；不碰其他模块",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}

	// 等父 mailbox 收到失败消息（含循环守卫文案 + 打捞摘要）。
	var msg *mailbox.Message
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs := mb.Drain("s1")
		for _, m := range msgs {
			if strings.Contains(m.Body, "被守卫终止") {
				msg = m
				break
			}
		}
		if msg != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if msg == nil {
		t.Fatal("parent mailbox never got loop-guard failure message")
	}
	if !strings.Contains(msg.Body, "被守卫终止") {
		t.Fatalf("expected guard-termination text, got: %s", msg.Body)
	}
	// 结构化失败标记（TODO #23）：守卫终止不可重试。
	if !strings.HasPrefix(msg.Body, "[failure kind=loop_guard retryable=false]") {
		t.Fatalf("expected structured failure marker prefix, got: %s", msg.Body)
	}

	// 树节点 Failed。
	found := false
	for _, n := range tr.Snapshot() {
		if n.ParentID == "s1" && n.Domain == "配置" {
			found = true
			if n.Status != orchestrator.StatusFailed {
				t.Fatalf("tree node should be Failed, got status=%v", n.Status)
			}
		}
	}
	if !found {
		t.Fatal("domain node not registered in tree")
	}

	// 打捞槽位 <parentID>:salvage:<domain> 可读（fallback = 末条 assistant 文本截断）。
	val, err := d.sharedMem.Get(context.Background(), "s1:salvage:配置")
	if err != nil || strings.TrimSpace(val) == "" {
		t.Fatalf("salvage slot should be readable, err=%v val=%q", err, val)
	}
	if !strings.Contains(val, "我正在读取文件") {
		t.Fatalf("salvage slot should hold last assistant text fallback, got: %q", val)
	}
	if !strings.Contains(msg.Body, "失败打捞") {
		t.Fatalf("failure message should carry salvage summary, got: %s", msg.Body)
	}
}

// TestSalvageFailure_WritesSlotWithExtractedFacts 打捞提取成功：槽位写入 LLM 提取的事实。
func TestSalvageFailure_WritesSlotWithExtractedFacts(t *testing.T) {
	_, _, _, _, dir := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	// 独立构造：注入 fake 提取器 + 共享槽位断言。
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{
			ModelConfig:  types.AgentModelConfig{Provider: "mock"},
			SystemPrompt: "通用领域纪律",
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(dir, nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "ok"}}, toolsReg, mb, agent.NopMemoryPipeline{})
	shared := tool.NewFileSharedMemoryStore(dir)
	d.WithSharedMemory(shared)
	d.WithSalvageExtractor(&mockFactExtractor{facts: []string{"已读: config.js", "卡点: 路径引用错误"}})

	roleDef := reg.Get("domain")
	history := []agent.ReactMessage{{Role: "assistant", Content: "我已读完 config.js，卡在 index.html 引用路径"}}
	result := agent.ReactResult{History: history}
	salvage := d.salvageFailure(context.Background(), "s1", "s1/domain-1", *roleDef, "配置", result, "")

	if !strings.Contains(salvage, "已读: config.js") || !strings.Contains(salvage, "卡点") {
		t.Fatalf("salvage should contain extracted facts, got: %s", salvage)
	}
	val, _ := shared.Get(context.Background(), "s1:salvage:配置")
	if !strings.Contains(val, "已读: config.js") {
		t.Fatalf("slot should hold extracted facts, got: %q", val)
	}
}

// TestSalvageFailure_ExtractorErrorFallsBack 提取失败回退末条文本截断，不阻塞主流程。
func TestSalvageFailure_ExtractorErrorFallsBack(t *testing.T) {
	_, _, _, _, dir := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(dir, nil, nil)
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "ok"}}, toolsReg, mailbox.New(), agent.NopMemoryPipeline{})
	shared := tool.NewFileSharedMemoryStore(dir)
	d.WithSharedMemory(shared)
	d.WithSalvageExtractor(&mockFactExtractor{err: errors.New("llm down")})

	roleDef := reg.Get("domain")
	history := []agent.ReactMessage{{Role: "assistant", Content: "末条部分产出文本"}}
	salvage := d.salvageFailure(context.Background(), "s1", "s1/domain-1", *roleDef, "配置", agent.ReactResult{History: history}, "")

	if !strings.Contains(salvage, "末条部分产出文本") {
		t.Fatalf("fallback should be last assistant text, got: %q", salvage)
	}
	val, _ := shared.Get(context.Background(), "s1:salvage:配置")
	if !strings.Contains(val, "末条部分产出文本") {
		t.Fatalf("slot should hold fallback text, got: %q", val)
	}
}

// TestSalvageFailure_LeafWritesRoleSlot 叶子派发（domain 空）以 role:<roleID> 为 scope 写槽位，
// 供同角色重派带回前序摘要（2026-08-19：心跳误杀叶子重派不再从零重跑）。
func TestSalvageFailure_LeafWritesRoleSlot(t *testing.T) {
	_, _, _, _, dir := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	cfg := &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{SystemPrompt: "meta", ModelConfig: types.AgentModelConfig{Provider: "mock"}},
		DomainAgent: config.DomainAgentConfig{ModelConfig: types.AgentModelConfig{Provider: "mock"}},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(dir, nil, nil)
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "ok"}}, toolsReg, mailbox.New(), agent.NopMemoryPipeline{})
	shared := tool.NewFileSharedMemoryStore(dir)
	d.WithSharedMemory(shared)

	roleDef := reg.Get("domain")
	history := []agent.ReactMessage{{Role: "assistant", Content: "探索成果"}}
	salvage := d.salvageFailure(context.Background(), "s1", "s1/domain-1", *roleDef, "", agent.ReactResult{History: history}, "")
	if !strings.Contains(salvage, "探索成果") {
		t.Fatalf("salvage should return text even without domain, got: %q", salvage)
	}
	val, err := shared.Get(context.Background(), "s1:salvage:role.domain")
	if err != nil || !strings.Contains(val, "探索成果") {
		t.Fatalf("leaf salvage should write role-scoped slot, err=%v val=%q", err, val)
	}
}

// TestWithPriorSalvage_AppendsToSameDomainReDispatch 同父同 domain 存在 Failed 兄弟：
// 新任务文本带【前序探索摘要】；无失败兄弟 / 摘要为空时任务零变化。
func TestWithPriorSalvage_AppendsToSameDomainReDispatch(t *testing.T) {
	d, _, tr, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})

	// 注册一个 Failed 兄弟节点（同父同 domain）。
	tr.Register(orchestrator.Node{ID: "s1/domain-1", ParentID: "s1", Role: "domain", Domain: "配置", Status: orchestrator.StatusRunning})
	tr.Finish("s1/domain-1", "failed", errors.New("timeout"))
	_ = d.sharedMem.Set(context.Background(), "s1:salvage:配置", "已读: config.js 结构\n卡点: 渲染接口签名未确认")

	task := d.withPriorSalvage(dispatchCtx(), "s1", "配置", "domain", "实现 config.js 渲染")
	if !strings.Contains(task, "【前序探索摘要】") || !strings.Contains(task, "config.js") {
		t.Fatalf("re-dispatch task should carry prior salvage, got: %s", task)
	}

	// 其他 domain（无失败兄弟）：任务零变化。
	other := d.withPriorSalvage(dispatchCtx(), "s1", "渲染", "domain", "实现渲染引擎")
	if other != "实现渲染引擎" {
		t.Fatalf("task without failed sibling should be unchanged, got: %s", other)
	}
	// 叶子派发（domain 空）无同角色失败前任：零变化。
	empty := d.withPriorSalvage(dispatchCtx(), "s1", "", "code_assistant", "任务")
	if empty != "任务" {
		t.Fatalf("leaf without failed sibling should be unchanged, got: %s", empty)
	}
	// 摘要槽位为空：零变化。
	tr.Register(orchestrator.Node{ID: "s1/domain-2", ParentID: "s1", Role: "domain", Domain: "测试", Status: orchestrator.StatusRunning})
	tr.Finish("s1/domain-2", "failed", errors.New("timeout"))
	nofail := d.withPriorSalvage(dispatchCtx(), "s1", "测试", "domain", "写测试")
	if nofail != "写测试" {
		t.Fatalf("task with failed sibling but empty salvage should be unchanged, got: %s", nofail)
	}
}

// TestBuildSharedPrefix_SkipsSalvageSlot 打捞槽位不参与通用共享注入（防跨域污染）。
func TestBuildSharedPrefix_SkipsSalvageSlot(t *testing.T) {
	d, _, _, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	_ = d.sharedMem.Set(context.Background(), "s1:salvage:配置", "已读: config.js")
	_ = d.sharedMem.Set(context.Background(), "s1:file_tree", "tree: a.txt")

	prefix := d.buildSharedPrefix(context.Background(), "s1")
	if !strings.Contains(prefix, "tree: a.txt") {
		t.Fatalf("normal shared slot should inject, got: %s", prefix)
	}
	if strings.Contains(prefix, "config.js") {
		t.Fatalf("salvage slot must not leak into generic shared prefix, got: %s", prefix)
	}
}

// TestExecuteChild_RegistersInTree 验证 TODO #21 吸收 #2 开放动作：同步子 Agent
// （verifyloop ExecuteChild）入权威树——Register Running -> Finish Done，TUI 可见可取消。
func TestExecuteChild_RegistersInTree(t *testing.T) {
	d, _, tr, _, _ := newSalvageTestEnv(t, &mockProvider{text: "done"})
	out, err := d.ExecuteChild(dispatchCtx(), "s1", "code_assistant", "写 config.js")
	if err != nil {
		t.Fatalf("ExecuteChild: %v", err)
	}
	if out != "done" {
		t.Fatalf("expected 'done', got %q", out)
	}
	var node *orchestrator.Node
	for i, n := range tr.Snapshot() {
		if n.ParentID == "s1" && n.Role == "code_assistant" {
			node = &tr.Snapshot()[i]
		}
	}
	if node == nil {
		t.Fatal("ExecuteChild node not registered in tree")
	}
	if node.Status != orchestrator.StatusDone {
		t.Fatalf("node should be Done after sync child, got status=%v", node.Status)
	}
	if node.Task != "写 config.js" {
		t.Fatalf("node task mismatch, got: %s", node.Task)
	}
}

// TestExecuteChild_FailureFinishesFailed 同步子 Agent 失败：节点 Finish Failed。
func TestExecuteChild_FailureFinishesFailed(t *testing.T) {
	d, _, tr, _, _ := newSalvageTestEnv(t, &countedProvider{failCalls: 99, text: "x"})
	_, err := d.ExecuteChild(dispatchCtx(), "s1", "code_assistant", "会失败的任务")
	if err == nil {
		t.Fatal("expected ExecuteChild error for failing child")
	}
	for _, n := range tr.Snapshot() {
		if n.ParentID == "s1" && n.Role == "code_assistant" {
			if n.Status != orchestrator.StatusFailed {
				t.Fatalf("node should be Failed, got status=%v", n.Status)
			}
			return
		}
	}
	t.Fatal("failed ExecuteChild node not found in tree")
}
