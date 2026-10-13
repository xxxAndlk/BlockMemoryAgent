package subagent

// dispatcher_tools_grant_test.go 覆盖派发级工具授权（二期）：
//   - tools/tools_mode 参数解析与 ⊆ 父可分配集校验（越界项忽略回告）；
//   - heldTools 登记/清理生命周期（运行期间可见、子 Agent 结束后回收）；
//   - effectiveToolAllowlist 的 extend/only/无覆盖三态与子 Agent Schema 内容；
//   - 执行硬门（roleToolGate）按 scope 叠加授权：only 收窄、extend 并集、预挂载豁免不回归；
//   - map_sub_agents 逐项透传 tools/tools_mode；
//   - 派前能力校验（checkRoleTaskFit）按授权后的有效工具面判定。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// gateReleaseProvider 阻塞型 mock：Generate 阻塞直到 release 关闭，用于在子 Agent
// 运行期间稳定观察派发级登记（授权），避免"完成即清理"的断言竞态。
type gateReleaseProvider struct{ release chan struct{} }

func (p *gateReleaseProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
}

func (p *gateReleaseProvider) Name() string { return "mock" }

// newGrantTestEnv 构造派发级授权测试环境：meta + domain + scout（白名单收窄）+
// code_assistant（全量）角色；工具注册表带插件可见性天花板（复用 hintCeiling：
// canvas_draw 全角色可见、computer_use_click 仅 meta）与两个插件工具。
func newGrantTestEnv(t *testing.T, provider agent.ModelProvider) (*Dispatcher, *tool.Registry, *mailbox.Mailbox) {
	t.Helper()
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{
			SystemPrompt: "meta",
			ModelConfig:  types.AgentModelConfig{Provider: "mock"},
		},
		DomainAgent: config.DomainAgentConfig{
			ModelConfig: types.AgentModelConfig{Provider: "mock"},
		},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
			{ID: "scout", Type: enums.RoleTypeFixed, CanBeCalled: true, SpecExempt: true, SystemPrompt: "scout",
				Tools: []string{"ReadFile", "ListDir", "SearchInFiles"}},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	toolsReg.SetPluginVisibility(hintCeiling)
	toolsReg.Register(&hintPluginTool{name: "canvas_draw"})
	toolsReg.Register(&hintPluginTool{name: "computer_use_click"})
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: provider}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.WithPluginVisibility(hintCeiling)
	d.RegisterCallTool(toolsReg)
	return d, toolsReg, mb
}

// subIDFromOutput 从派发结果文本中提取子 Agent ID（拒绝附言以"。"分隔）。
func subIDFromOutput(out string) string {
	id := strings.TrimSpace(out)
	if i := strings.Index(id, "。"); i >= 0 {
		id = id[:i]
	}
	return id
}

// TestToolsGrant_EffectiveToolAllowlist 有效白名单三态：无覆盖=角色白名单原样；
// extend=角色白名单 ∪ 授权集（去重）；only=仅授权集。
func TestToolsGrant_EffectiveToolAllowlist(t *testing.T) {
	d, _, _ := newGrantTestEnv(t, &mockProvider{text: "done"})
	rd := &types.RoleDefinition{ID: "scout", Tools: []string{"ReadFile", "ListDir"}}

	// 无覆盖：逐字一致。
	got := d.effectiveToolAllowlist("s1/scout-1", rd)
	if strings.Join(got, ",") != "ReadFile,ListDir" {
		t.Fatalf("无覆盖应原样返回角色白名单: %v", got)
	}

	// extend：并集去重。
	d.heldTools.Store("s1/scout-2", heldToolGrant{tools: []string{"canvas_draw", "ReadFile"}, mode: ToolGrantModeExtend})
	got = d.effectiveToolAllowlist("s1/scout-2", rd)
	if strings.Join(got, ",") != "ReadFile,ListDir,canvas_draw" {
		t.Fatalf("extend 应为角色白名单∪授权集（去重）: %v", got)
	}

	// only：仅授权集。
	d.heldTools.Store("s1/scout-3", heldToolGrant{tools: []string{"canvas_draw"}, mode: ToolGrantModeOnly})
	got = d.effectiveToolAllowlist("s1/scout-3", rd)
	if strings.Join(got, ",") != "canvas_draw" {
		t.Fatalf("only 应仅含授权集: %v", got)
	}

	// 角色白名单为空（全量暴露）：extend 不缩小（原样返回空表=不过滤），only 仍收窄。
	full := &types.RoleDefinition{ID: "code_assistant"}
	d.heldTools.Store("s1/code-1", heldToolGrant{tools: []string{"canvas_draw"}, mode: ToolGrantModeExtend})
	if got := d.effectiveToolAllowlist("s1/code-1", full); len(got) != 0 {
		t.Fatalf("空白名单角色 extend 应保持全量暴露: %v", got)
	}
	d.heldTools.Store("s1/code-2", heldToolGrant{tools: []string{"canvas_draw"}, mode: ToolGrantModeOnly})
	if got := d.effectiveToolAllowlist("s1/code-2", full); strings.Join(got, ",") != "canvas_draw" {
		t.Fatalf("空白名单角色 only 应收窄为授权集: %v", got)
	}
}

// TestToolsGrant_SchemaContent 子 Agent Schema 内容随授权模式变化：
// only 收窄为授权集；extend 为角色白名单∪授权；无覆盖不变。
func TestToolsGrant_SchemaContent(t *testing.T) {
	d, toolsReg, _ := newGrantTestEnv(t, &mockProvider{text: "done"})
	rd := &types.RoleDefinition{ID: "scout", Tools: []string{"ReadFile", "ListDir"}}
	schemaNames := func(scope string) map[string]bool {
		out := map[string]bool{}
		for _, tt := range agent.NewToolRegistryAdapterForRole(toolsReg, scope, d.effectiveToolAllowlist(scope, rd), rd.ID, d.pluginVisibility).Schema() {
			out[tt.Name()] = true
		}
		return out
	}

	// 无覆盖：白名单内工具可见，白名单外内置工具不可见。
	base := schemaNames("s1/scout-1")
	if !base["ReadFile"] || !base["ListDir"] || base["WriteFile"] || base["canvas_draw"] {
		t.Fatalf("无覆盖 Schema 应为角色白名单: %v", base)
	}

	// extend：授权插件工具并入。
	d.heldTools.Store("s1/scout-2", heldToolGrant{tools: []string{"canvas_draw"}, mode: ToolGrantModeExtend})
	ext := schemaNames("s1/scout-2")
	if !ext["ReadFile"] || !ext["canvas_draw"] || ext["WriteFile"] {
		t.Fatalf("extend Schema 应为白名单∪授权: %v", ext)
	}

	// only：仅授权集（收窄）。
	d.heldTools.Store("s1/scout-3", heldToolGrant{tools: []string{"canvas_draw"}, mode: ToolGrantModeOnly})
	only := schemaNames("s1/scout-3")
	if !only["canvas_draw"] || only["ReadFile"] || only["WriteFile"] {
		t.Fatalf("only Schema 应仅含授权集: %v", only)
	}
}

// TestResolveChildTools_Bounds 授权校验：未注册项、子角色不可见插件项、
// 父未持有的内置项均被忽略并列入 rejected；合法项进入授权集。
func TestResolveChildTools_Bounds(t *testing.T) {
	d, _, _ := newGrantTestEnv(t, &mockProvider{text: "done"})

	// meta 父：全注册工具可分配；插件工具受子角色可见性天花板约束。
	child := &types.RoleDefinition{ID: "code_assistant"}
	grants, rejected := d.resolveChildTools("meta", child, []string{"canvas_draw", "computer_use_click", "ghost_tool", "RunCommand"})
	if !containsString(grants, "canvas_draw") || !containsString(grants, "RunCommand") {
		t.Fatalf("天花板内插件工具与已注册内置工具应授予: %v", grants)
	}
	joined := strings.Join(rejected, ";")
	if !strings.Contains(joined, "computer_use_click") || !strings.Contains(joined, "ghost_tool") {
		t.Fatalf("越界插件与未注册项应列入 rejected: %v", rejected)
	}

	// 非 meta 父（scout 白名单收窄）：父未持有的内置工具不可下放。
	scoutChild := &types.RoleDefinition{ID: "scout"}
	grants, rejected = d.resolveChildTools("s1/scout-1", scoutChild, []string{"ReadFile", "RunCommand"})
	if !containsString(grants, "ReadFile") || containsString(grants, "RunCommand") {
		t.Fatalf("父未持有工具不应下放: grants=%v rejected=%v", grants, rejected)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0], "RunCommand") {
		t.Fatalf("RunCommand 应被拒绝: %v", rejected)
	}

	// 父自身携带的派发级授权可继续下放（extend/only 授权集都是父的有效工具面）。
	d.heldTools.Store("s1/scout-9", heldToolGrant{tools: []string{"RunCommand"}, mode: ToolGrantModeExtend})
	grants, _ = d.resolveChildTools("s1/scout-9", scoutChild, []string{"RunCommand"})
	if !containsString(grants, "RunCommand") {
		t.Fatalf("父的授权集应可继续下放: %v", grants)
	}
}

// TestDispatch_ToolsGrant 派发携带 tools：越界项回告父 Agent，授权集登记进 heldTools；
// 子 Agent 结束后授权随清理点回收（生命周期同 heldSkills）。
func TestDispatch_ToolsGrant(t *testing.T) {
	provider := &gateReleaseProvider{release: make(chan struct{})}
	d, toolsReg, mb := newGrantTestEnv(t, provider)

	ctx := agent.WithAgentID(context.Background(), "meta")
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "画一个页面",
		"verify_kind": "none",
		"tools":       []any{"canvas_draw", "RunCommand", "computer_use_click", "ghost_tool"},
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch: %v %s", err, res.Error)
	}
	// 越界项回告：插件天花板外（computer_use_click 仅 meta）与未注册项（ghost_tool）。
	if !strings.Contains(res.Output, "tools 授权越界忽略") ||
		!strings.Contains(res.Output, "computer_use_click") ||
		!strings.Contains(res.Output, "ghost_tool") {
		t.Fatalf("越界授权应回告: %s", res.Output)
	}
	subID := subIDFromOutput(res.Output)
	grants, mode, ok := d.HeldToolGrant(subID)
	if !ok || mode != ToolGrantModeExtend {
		t.Fatalf("授权应登记为 extend: ok=%v mode=%q", ok, mode)
	}
	if !containsString(grants, "canvas_draw") || !containsString(grants, "RunCommand") || containsString(grants, "ghost_tool") {
		t.Fatalf("授权集内容不符: %v", grants)
	}

	// 放行子 Agent 完成，结束后授权条目被清理。
	close(provider.release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(mb.Drain("meta")) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := d.HeldToolGrant(subID); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("子 Agent 结束后 heldTools 条目应被清理")
}

// TestDispatch_ToolsGrantOnlyNarrows tools_mode=only：有效白名单收窄为授权集；
// 派前能力校验按授权后的有效工具面判定（授权 RunCommand 后命令类任务放行）。
func TestDispatch_ToolsGrantOnlyNarrows(t *testing.T) {
	provider := &gateReleaseProvider{release: make(chan struct{})}
	defer close(provider.release)
	d, toolsReg, _ := newGrantTestEnv(t, provider)
	ctx := agent.WithAgentID(context.Background(), "meta")

	// 无授权基线：scout 无 RunCommand，命令类任务被派前校验拒绝。
	res, _ := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id": "scout",
		"task":    "请执行命令统计该目录文件数",
	})
	if res.Success || !strings.Contains(res.Error, "能力不匹配") {
		t.Fatalf("无授权时应被能力校验拒绝: %+v", res)
	}

	// 授权 RunCommand（only 模式）：能力校验按有效工具面放行，授权登记为 only。
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id":    "scout",
		"task":       "请执行命令统计该目录文件数",
		"tools":      []any{"RunCommand"},
		"tools_mode": "only",
	})
	if err != nil || !res.Success {
		t.Fatalf("授权后应放行: %v %s", err, res.Error)
	}
	subID := subIDFromOutput(res.Output)
	grants, mode, ok := d.HeldToolGrant(subID)
	if !ok || mode != ToolGrantModeOnly || !containsString(grants, "RunCommand") {
		t.Fatalf("only 授权登记不符: ok=%v mode=%q grants=%v", ok, mode, grants)
	}

	// tools_mode 非法值拒绝。
	res, _ = toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id":    "scout",
		"task":       "定位 X",
		"tools_mode": "bogus",
	})
	if res.Success || !strings.Contains(res.Error, "tools_mode") {
		t.Fatalf("非法 tools_mode 应拒绝: %+v", res)
	}
}

// TestRoleToolGate_ToolsGrant 执行硬门 scope 化：only 模式下授权外内置工具被拒、
// 授权内放行；extend 模式角色白名单∪授权均放行；预挂载豁免不回归。
func TestRoleToolGate_ToolsGrant(t *testing.T) {
	d, toolsReg, _ := newGrantTestEnv(t, &mockProvider{text: "done"})
	toolsReg.Register(&hintPluginTool{name: "probe-grant"})
	toolsReg.Register(&hintPluginTool{name: "probe-other"})
	// 硬门闭包（复刻 bootstrap 装配口径）：角色静态面 + 派发级授权覆盖。
	roleTools := map[string][]string{"scout": {"probe-base"}}
	toolsReg.Register(&hintPluginTool{name: "probe-base"})
	toolsReg.Register(&hintPluginTool{name: "probe-pre"})
	toolsReg.SetRoleToolGateResolver(func(roleID, scope string) []string {
		base := roleTools[roleID]
		if grants, mode, ok := d.HeldToolGrant(scope); ok && len(grants) > 0 {
			if mode == ToolGrantModeOnly {
				return grants
			}
			return mergeToolGrant(base, grants, mode)
		}
		return base
	})

	dispatch := func(scope, name string) *tool.Result {
		ctx := tool.WithAgentID(tool.WithRoleID(context.Background(), "scout"), scope)
		res, err := toolsReg.Dispatch(ctx, name, nil)
		if err != nil {
			t.Fatalf("dispatch %s: %v", name, err)
		}
		return res
	}
	denied := func(res *tool.Result) bool { return strings.Contains(res.Error, "硬门拒绝") }

	// only 模式：授权内放行，授权外（含角色静态面）拒绝。
	d.heldTools.Store("s1/scout-only", heldToolGrant{tools: []string{"probe-grant"}, mode: ToolGrantModeOnly})
	if res := dispatch("s1/scout-only", "probe-grant"); !res.Success {
		t.Fatalf("only 模式授权内工具应放行: %+v", res)
	}
	if res := dispatch("s1/scout-only", "probe-other"); !denied(res) {
		t.Fatalf("only 模式授权外工具应被硬门拒绝: %+v", res)
	}
	if res := dispatch("s1/scout-only", "probe-base"); !denied(res) {
		t.Fatalf("only 模式角色静态面也被收窄: %+v", res)
	}

	// extend 模式：角色静态面与授权集均放行，其余拒绝。
	d.heldTools.Store("s1/scout-ext", heldToolGrant{tools: []string{"probe-grant"}, mode: ToolGrantModeExtend})
	if res := dispatch("s1/scout-ext", "probe-base"); !res.Success {
		t.Fatalf("extend 模式角色静态面应放行: %+v", res)
	}
	if res := dispatch("s1/scout-ext", "probe-grant"); !res.Success {
		t.Fatalf("extend 模式授权集应放行: %+v", res)
	}
	if res := dispatch("s1/scout-ext", "probe-other"); !denied(res) {
		t.Fatalf("extend 模式白名单外工具应拒绝: %+v", res)
	}

	// 预挂载豁免不回归：配置预挂的顶层工具不经授权也可执行。
	toolsReg.MountPreApprovedForScope("s1/scout-only", "scout", []string{"probe-pre"})
	if res := dispatch("s1/scout-only", "probe-pre"); !res.Success {
		t.Fatalf("预挂载工具应豁免硬门: %+v", res)
	}

	// 无授权 scope：回落现状（角色静态面）。
	if res := dispatch("s1/scout-none", "probe-base"); !res.Success {
		t.Fatalf("无授权应回落角色白名单: %+v", res)
	}
}

// TestMapSubAgents_ToolsGrantPassthrough map_sub_agents 逐项透传 tools/tools_mode：
// 每个子 Agent 的 scope 都登记同一授权。
func TestMapSubAgents_ToolsGrantPassthrough(t *testing.T) {
	provider := &gateReleaseProvider{release: make(chan struct{})}
	defer close(provider.release)
	d, toolsReg, _ := newGrantTestEnv(t, provider)

	res, err := toolsReg.Dispatch(agent.WithAgentID(context.Background(), "meta"), "map_sub_agents", map[string]any{
		"role_id":       "scout",
		"task_template": "定位 {{item}} 的定义",
		"items":         []any{"foo", "bar"},
		"tools":         []any{"canvas_draw"},
		"tools_mode":    "only",
	})
	if err != nil || !res.Success {
		t.Fatalf("map dispatch: %v %s", err, res.Error)
	}
	// 两个子 scope 均应登记 only 授权（登记在 dispatchOne 内同步完成）。
	count := 0
	d.heldTools.Range(func(_, v any) bool {
		g := v.(heldToolGrant)
		count++
		if g.mode != ToolGrantModeOnly || !containsString(g.tools, "canvas_draw") {
			t.Errorf("map 透传授权不符: %+v", g)
		}
		return true
	})
	if count != 2 {
		t.Fatalf("应登记 2 个子 scope 授权，实际 %d", count)
	}
}

// TestToolsGrant_ModeValidation 并发安全占位：heldToolGrant 读写经 sync.Map，
// 本用例压一遍并发 Store/Load/Delete 防数据竞争（配合 -race）。
func TestToolsGrant_ConcurrentAccess(t *testing.T) {
	d, _, _ := newGrantTestEnv(t, &mockProvider{text: "done"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := strings.Repeat("x", n+1)
			d.heldTools.Store(id, heldToolGrant{tools: []string{"ReadFile"}, mode: ToolGrantModeExtend})
			_, _, _ = d.HeldToolGrant(id)
			_ = d.effectiveToolAllowlist(id, &types.RoleDefinition{ID: "scout", Tools: []string{"ReadFile"}})
			d.heldTools.Delete(id)
		}(i)
	}
	wg.Wait()
}
