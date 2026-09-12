package subagent

// 交付验收闭环（acceptance.go）单元测试：
// 机读块解析、tester.yaml 读写三态、ShouldRun 三态（auto 用 mock 轻量调用）、
// Run 轮次熔断（mock 连续 FAIL 断言 max_rounds 后停止）。

import (
	"context" // context 构造派发/判定上下文
	"errors"  // errors 构造 mock 轻量调用失败
	"strings" // strings 断言交付文本包含关系
	"testing" // 测试框架
	"time"    // time 测试等待节拍

	"github.com/blockmemory/agent/backend/internal/agent"               // NopMemoryPipeline / WithAgentID
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator" // 权威树（名单/状态读取断言）
	"github.com/blockmemory/agent/backend/internal/domain/role"         // 角色注册表
	"github.com/blockmemory/agent/backend/internal/domain/tool"         // 工具注册表与 WithSessionID
	"github.com/blockmemory/agent/backend/internal/mailbox"             // 邮箱（抄送断言）
	"github.com/blockmemory/agent/backend/pkg/config"                   // RoleConfigFile
	"github.com/blockmemory/agent/backend/pkg/enums"                    // RoleTypeFixed
	"github.com/blockmemory/agent/backend/pkg/types"                    // RoleDefinition
)

// TestParseAcceptanceReport_PASS 验证【验收结论】PASS 机读块解析。
func TestParseAcceptanceReport_PASS(t *testing.T) {
	hasVerdict, passed, items := parseAcceptanceReport("验收正文……\n【验收结论】PASS\n")
	if !hasVerdict || !passed {
		t.Fatalf("expected PASS verdict, got hasVerdict=%v passed=%v", hasVerdict, passed)
	}
	if len(items) != 0 {
		t.Fatalf("PASS 不应有错误条目, got %v", items)
	}
}

// TestParseAcceptanceReport_FAIL 验证 FAIL + 【错误清单】的 [agent:x] 条目提取。
func TestParseAcceptanceReport_FAIL(t *testing.T) {
	text := "验收正文：点了三个按钮。\n【验收结论】FAIL\n【错误清单】\n" +
		"1. [agent:sess-1/domain-3] 首页提交按钮点击无响应（预期跳转，实际无反应）\n" +
		"2. [agent:meta] 需求第二项未交付\n"
	hasVerdict, passed, items := parseAcceptanceReport(text)
	if !hasVerdict || passed {
		t.Fatalf("expected FAIL verdict, got hasVerdict=%v passed=%v", hasVerdict, passed)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d: %v", len(items), items)
	}
	if items[0].AgentID != "sess-1/domain-3" || !strings.Contains(items[0].Desc, "提交按钮") {
		t.Fatalf("item[0] 解析错误: %+v", items[0])
	}
	if items[1].AgentID != "meta" {
		t.Fatalf("item[1] 解析错误: %+v", items[1])
	}
}

// TestParseAcceptanceReport_MissingBlock 验证缺【验收结论】段的容错（无效验收）。
func TestParseAcceptanceReport_MissingBlock(t *testing.T) {
	hasVerdict, _, _ := parseAcceptanceReport("测试报告正文，没有任何机读块。[agent:x] 不应误命中。")
	if hasVerdict {
		t.Fatal("缺【验收结论】段应判 hasVerdict=false")
	}
}

// TestParseAcceptanceReport_FailNoList 验证 FAIL 但无【错误清单】时 items 为空。
func TestParseAcceptanceReport_FailNoList(t *testing.T) {
	hasVerdict, passed, items := parseAcceptanceReport("【验收结论】FAIL\n（没写清单）")
	if !hasVerdict || passed {
		t.Fatalf("expected FAIL verdict, got hasVerdict=%v passed=%v", hasVerdict, passed)
	}
	if len(items) != 0 {
		t.Fatalf("无【错误清单】段不应提取条目, got %v", items)
	}
}

// TestLoadTesterConfig_Default 验证缺文件/空目录回落默认值 {off, "", 2}。
func TestLoadTesterConfig_Default(t *testing.T) {
	cfg := LoadTesterConfig(t.TempDir())
	if cfg.Mode != TesterModeOff || cfg.AutoPrompt != "" || cfg.MaxRounds != defaultTesterMaxRounds {
		t.Fatalf("缺文件应回落默认 {off,\"\",2}, got %+v", cfg)
	}
	if cfg := LoadTesterConfig(""); cfg.Mode != TesterModeOff {
		t.Fatalf("空 workDir 应回落 off, got %+v", cfg)
	}
}

// TestLoadTesterConfig_Modes 验证三态读写与非法值归一化。
func TestLoadTesterConfig_Modes(t *testing.T) {
	dir := t.TempDir()
	// on 档位往返。
	if err := SaveTesterConfig(dir, TesterConfig{Mode: TesterModeOn, MaxRounds: 3}); err != nil {
		t.Fatalf("save on: %v", err)
	}
	cfg := LoadTesterConfig(dir)
	if cfg.Mode != TesterModeOn || cfg.MaxRounds != 3 {
		t.Fatalf("on 往返失败: %+v", cfg)
	}
	// auto 档位带 auto_prompt。
	if err := SaveTesterConfig(dir, TesterConfig{Mode: TesterModeAuto, AutoPrompt: "涉及页面/UI 的交付", MaxRounds: 1}); err != nil {
		t.Fatalf("save auto: %v", err)
	}
	cfg = LoadTesterConfig(dir)
	if cfg.Mode != TesterModeAuto || cfg.AutoPrompt != "涉及页面/UI 的交付" || cfg.MaxRounds != 1 {
		t.Fatalf("auto 往返失败: %+v", cfg)
	}
	// 非法 mode 与 max_rounds<=0 归一化。
	if err := SaveTesterConfig(dir, TesterConfig{Mode: "banana", MaxRounds: 0}); err != nil {
		t.Fatalf("save invalid: %v", err)
	}
	cfg = LoadTesterConfig(dir)
	if cfg.Mode != TesterModeOff || cfg.MaxRounds != defaultTesterMaxRounds {
		t.Fatalf("非法值应归一化为 {off,2}, got %+v", cfg)
	}
}

// TestShouldRun 验证三态判定：off/on 直判；auto 用 mock 轻量调用覆盖
// 命中（YES）/不命中（NO）/空描述/调用失败四种走向。
func TestShouldRun(t *testing.T) {
	m := &AcceptanceManager{}
	ctx := context.Background()
	if m.ShouldRun(ctx, TesterConfig{Mode: TesterModeOff, MaxRounds: 2}, "做个页面") {
		t.Fatal("off 不应执行")
	}
	if !m.ShouldRun(ctx, TesterConfig{Mode: TesterModeOn, MaxRounds: 2}, "做个页面") {
		t.Fatal("on 应执行")
	}
	// auto：lightCall 未接线 → 不执行。
	if m.ShouldRun(ctx, TesterConfig{Mode: TesterModeAuto, AutoPrompt: "UI 交付", MaxRounds: 2}, "做个页面") {
		t.Fatal("auto 且 lightCall 未接线不应执行")
	}
	// auto：描述为空 → 不执行。
	m.lightCall = func(ctx context.Context, prompt string) (string, error) { return "YES", nil }
	if m.ShouldRun(ctx, TesterConfig{Mode: TesterModeAuto, MaxRounds: 2}, "做个页面") {
		t.Fatal("auto 且描述为空不应执行")
	}
	// auto：命中 / 不命中。
	if !m.ShouldRun(ctx, TesterConfig{Mode: TesterModeAuto, AutoPrompt: "UI 交付", MaxRounds: 2}, "做个页面") {
		t.Fatal("auto 命中（YES）应执行")
	}
	m.lightCall = func(ctx context.Context, prompt string) (string, error) { return "NO", nil }
	if m.ShouldRun(ctx, TesterConfig{Mode: TesterModeAuto, AutoPrompt: "UI 交付", MaxRounds: 2}, "做个页面") {
		t.Fatal("auto 不命中（NO）不应执行")
	}
	// auto：判定调用失败 → 不执行（降级安全方向）。
	m.lightCall = func(ctx context.Context, prompt string) (string, error) { return "", errors.New("boom") }
	if m.ShouldRun(ctx, TesterConfig{Mode: TesterModeAuto, AutoPrompt: "UI 交付", MaxRounds: 2}, "做个页面") {
		t.Fatal("auto 判定失败不应执行")
	}
}

// newAcceptanceTestManager 构造验收测试 harness：真实 Dispatcher + 固定文本 mock provider +
// 内存权威树/邮箱；waitTick 短节拍避免测试等待。
func newAcceptanceTestManager(t *testing.T, providerText string) (*AcceptanceManager, *orchestrator.Tree, *mailbox.Mailbox) {
	t.Helper()
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{
			SystemPrompt: "meta",
			ModelConfig:  types.AgentModelConfig{Provider: "mock"},
		},
		FixedRoles: []types.RoleDefinition{
			{ID: "test_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "tester", SpecExempt: true},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: providerText}}, toolsReg, mb, agent.NopMemoryPipeline{})
	tree := orchestrator.NewTree("sess-acc", nil)
	d.WithTree(func(sessionID string) *orchestrator.Tree { return tree })
	mgr := NewAcceptanceManager(d, nil)
	mgr.waitTick = 5 * time.Millisecond // 测试节拍：不等 30s 生产节拍
	return mgr, tree, mb
}

// TestRunWrap_OffPassthrough 验证 off 档（缺 tester.yaml）终答原样交付。
func TestRunWrap_OffPassthrough(t *testing.T) {
	mgr, _, _ := newAcceptanceTestManager(t, "【验收结论】PASS")
	out := mgr.RunWrap(context.Background(), "sess-acc", t.TempDir(), "做个页面", "最终答复原文")
	if out != "最终答复原文" {
		t.Fatalf("off 档应原样交付, got %q", out)
	}
}

// TestRun_MaxRoundsBreaker 验证轮次熔断：mock tester 连续 FAIL（错误归属 meta 不可复活），
// 达 max_rounds 后停止循环，终答附【未验证项】；树上 tester 节点数 == max_rounds。
func TestRun_MaxRoundsBreaker(t *testing.T) {
	mgr, tree, _ := newAcceptanceTestManager(t, "验收正文\n【验收结论】FAIL\n【错误清单】\n1. [agent:meta] 首页按钮无响应\n")
	workDir := t.TempDir()
	if err := SaveTesterConfig(workDir, TesterConfig{Mode: TesterModeOn, MaxRounds: 2}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	ctx := tool.WithSessionID(context.Background(), "sess-acc")
	out := mgr.RunWrap(ctx, "sess-acc", workDir, "做个页面", "最终答复原文")
	if !strings.Contains(out, "最终答复原文") {
		t.Fatalf("交付文本应保留原文, got %q", out)
	}
	if !strings.Contains(out, "【未验证项】") || !strings.Contains(out, "[meta] 首页按钮无响应") {
		t.Fatalf("熔断后应附【未验证项】段与错误条目, got %q", out)
	}
	// 熔断断言：验收派发恰好 max_rounds 次，不多不少。
	testers := 0
	for _, n := range tree.Snapshot() {
		if n.Role == "test_assistant" && n.Domain == acceptanceDomain {
			testers++
		}
	}
	if testers != 2 {
		t.Fatalf("max_rounds=2 应恰好派发 2 次 tester, got %d", testers)
	}
}

// TestRun_Pass 验证一轮 PASS：终答附验收通过标记，不进入返工。
func TestRun_Pass(t *testing.T) {
	mgr, tree, mb := newAcceptanceTestManager(t, "验收正文\n【验收结论】PASS\n")
	workDir := t.TempDir()
	if err := SaveTesterConfig(workDir, TesterConfig{Mode: TesterModeOn, MaxRounds: 2}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	ctx := tool.WithSessionID(context.Background(), "sess-acc")
	out := mgr.RunWrap(ctx, "sess-acc", workDir, "做个页面", "最终答复原文")
	if !strings.Contains(out, "验收通过") {
		t.Fatalf("PASS 应附验收通过标记, got %q", out)
	}
	if strings.Contains(out, "【未验证项】") {
		t.Fatalf("PASS 不应附【未验证项】, got %q", out)
	}
	// PASS 只派一次 tester。
	testers := 0
	for _, n := range tree.Snapshot() {
		if n.Role == "test_assistant" {
			testers++
		}
	}
	if testers != 1 {
		t.Fatalf("PASS 应恰好派发 1 次 tester, got %d", testers)
	}
	// PASS 不向 Meta 抄送返工邮件（tester 完成通知属正常回传，过滤掉）。
	for _, msg := range mb.Peek("sess-acc") {
		if msg.Subject == "验收返工已派回（你无需动作）" {
			t.Fatalf("PASS 不应有返工抄送邮件, got %+v", msg)
		}
	}
}
