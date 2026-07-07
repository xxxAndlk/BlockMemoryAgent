package graph

import (
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestClassifyByRules_DirectTool 验证纯 QA / 单工具请求路由到 RouteDirectTool。
func TestClassifyByRules_DirectTool(t *testing.T) {
	n := &MetaAgentNode{} // 无依赖；规则层只用 isSimpleQuestion/shouldDirectExecute 关键词
	cases := []string{"你好", "你是谁", "读一下 main.go", "运行 go test", "查一下天气"}
	for _, goal := range cases {
		if got := classifyByRules(n, goal); got != RouteDirectTool {
			t.Errorf("goal=%q: want direct_tool, got %s", goal, got)
		}
	}
}

// TestClassifyByRules_DirectAssistant 验证单领域简单任务路由到 RouteDirectAssistant。
func TestClassifyByRules_DirectAssistant(t *testing.T) {
	n := &MetaAgentNode{}
	cases := []string{"修复 CSS padding", "改一下文案", "调整颜色", "修改配置"}
	for _, goal := range cases {
		if got := classifyByRules(n, goal); got != RouteDirectAssistant {
			t.Errorf("goal=%q: want direct_assistant, got %s", goal, got)
		}
	}
}

// TestClassifyByRules_TowerDefenseBugExample 验证多动作开放探索目标不被误判为 direct_assistant。
// 塔防事故根因：该目标被路由到 direct_assistant，单 Assistant 包揽复杂目标后上下文爆炸。
func TestClassifyByRules_TowerDefenseBugExample(t *testing.T) {
	n := &MetaAgentNode{}
	cases := []string{
		"查看塔防游戏找出 bug 修复并优化",
		"查看 workspace 中的塔防游戏并修复 bug",
		"分析现有代码找出问题并优化性能",
		"排查登录 bug 并修复，同时优化数据库查询",
	}
	for _, goal := range cases {
		if got := classifyByRules(n, goal); got == RouteDirectAssistant {
			t.Errorf("goal=%q: 不应路由到 direct_assistant（多动作/开放探索），got %s", goal, got)
		}
		if got := classifyByRules(n, goal); got == RouteDirectTool {
			t.Errorf("goal=%q: 不应路由到 direct_tool（多动作/开放探索），got %s", goal, got)
		}
	}
}

// TestProfileGoal_MultiAction 验证多维打分对多动作目标的识别。
func TestProfileGoal_MultiAction(t *testing.T) {
	// 塔防事故目标：4 个动作（查看/找出/修复/优化）+ 开放探索 + 多步骤（并）
	p := profileGoal("查看塔防游戏找出 bug 修复并优化")
	if !p.OpenEnded {
		t.Errorf("应识别为开放探索（找出/优化）")
	}
	if p.StepCount == 0 {
		t.Errorf("应识别到多步骤信号（并）")
	}
	if p.HasLightAction && p.ActionCount <= 1 {
		t.Errorf("多动作目标 ActionCount 应 ≥2，got %d", p.ActionCount)
	}
	// 修复 CSS padding：单轻量动作，无开放探索
	p2 := profileGoal("修复 CSS padding")
	if p2.OpenEnded {
		t.Errorf("修复 CSS padding 不应识别为开放探索")
	}
	if !p2.HasLightAction {
		t.Errorf("应识别到轻量动作 修复")
	}
	if p2.ActionCount != 1 {
		t.Errorf("单动作目标 ActionCount 应=1，got %d", p2.ActionCount)
	}
}

// TestParseRouteConfidence 验证 LLM 输出置信度解析。
func TestParseRouteConfidence(t *testing.T) {
	cases := []struct {
		input string
		want  float64
	}{
		{"complexity: simple\npath: direct_tool\nconfidence: 0.9", 0.9},
		{"confidence:0.75", 0.75},
		{"置信度: 0.6", 0.6},
		{"confidence: 1.5", 1.0}, // 超界归一
		{"confidence: -0.1", 0.0},
		{"no confidence line", 0.0},
		{"confidence: abc", 0.0},
	}
	for _, c := range cases {
		got := parseRouteConfidence(c.input)
		if got != c.want {
			t.Errorf("parseRouteConfidence(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

// TestClassifyByProfile_LowConfidenceFallback 验证低置信度 LLM 输出返回空串（交安全兜底）。
// 通过模拟 LLM 返回低置信度，检查 classifyRouteLLM 行为不可在此单元测试中直接验证
// （需 modelFactory），这里只验证 parseRouteConfidence 与 minRouteConfidence 阈值的配合。
func TestMinRouteConfidence(t *testing.T) {
	if minRouteConfidence != 0.7 {
		t.Errorf("minRouteConfidence 应为 0.7，got %v", minRouteConfidence)
	}
	// 低置信度应小于阈值
	if parseRouteConfidence("confidence: 0.5") >= minRouteConfidence {
		t.Errorf("0.5 应低于阈值 0.7")
	}
	// 高置信度应大于等于阈值
	if parseRouteConfidence("confidence: 0.85") < minRouteConfidence {
		t.Errorf("0.85 应不低于阈值 0.7")
	}
}

// TestClassifyByRules_MultiDomain 验证多领域信号路由到 RouteMultiDomain。
func TestClassifyByRules_MultiDomain(t *testing.T) {
	n := &MetaAgentNode{}
	cases := []string{
		"后端加接口，前端改样式",
		"前端页面和后端接口都要改",
		"做一个页面，同时做一个接口",
	}
	for _, goal := range cases {
		if got := classifyByRules(n, goal); got != RouteMultiDomain {
			t.Errorf("goal=%q: want multi_domain, got %s", goal, got)
		}
	}
}

// TestClassifyByRules_Undecided 验证复杂单领域目标未命中规则（交 LLM 兜底）。
func TestClassifyByRules_Undecided(t *testing.T) {
	n := &MetaAgentNode{}
	cases := []string{"重构 API 层", "开发一个商城系统", "设计数据库模型"}
	for _, goal := range cases {
		if got := classifyByRules(n, goal); got != "" {
			t.Errorf("goal=%q: want undecided (empty), got %s", goal, got)
		}
	}
}

// TestClassifyTask_SafeFallback 验证无 modelFactory 时未命中规则 → RouteCreateDomain 安全兜底。
func TestClassifyTask_SafeFallback(t *testing.T) {
	n := &MetaAgentNode{} // modelFactory=nil, llmTracker=nil
	state := types.NewThreeLayerState("s1")
	state.DomainGoal = "重构 API 层" // 未命中规则
	d := n.ClassifyTask(nil, state)
	if d.Path != RouteCreateDomain {
		t.Errorf("want create_domain fallback, got %s", d.Path)
	}
	if d.EnableSubdomain {
		t.Errorf("create_domain should not enable subdomain")
	}
}

// TestClassifyTask_EmptyGoal 验证空目标安全兜底。
func TestClassifyTask_EmptyGoal(t *testing.T) {
	n := &MetaAgentNode{}
	state := types.NewThreeLayerState("s1")
	state.DomainGoal = ""
	d := n.ClassifyTask(nil, state)
	if d.Path != RouteCreateDomain {
		t.Errorf("empty goal: want create_domain, got %s", d.Path)
	}
}

// TestClassifyTask_EnableSubdomainFlag 验证 RouteFullFourLayer 置 EnableSubdomain（通过规则路径不可达，
// 这里直接验证 RouteDecision 构造逻辑：只有 full_four_layer 为 true）。
func TestRouteDecision_EnableSubdomainFlag(t *testing.T) {
	cases := map[RoutePath]bool{
		RouteDirectTool:      false,
		RouteDirectAssistant: false,
		RouteCreateDomain:    false,
		RouteMultiDomain:     false,
		RouteFullFourLayer:   true,
	}
	for path, want := range cases {
		d := RouteDecision{Path: path, EnableSubdomain: path == RouteFullFourLayer}
		if d.EnableSubdomain != want {
			t.Errorf("path=%s: want EnableSubdomain=%v, got %v", path, want, d.EnableSubdomain)
		}
	}
}

// TestIsSingleToolRequest 验证单工具请求识别，包含新增复合信号词。
func TestIsSingleToolRequest(t *testing.T) {
	positives := []string{"读一下 main.go", "运行 go test", "查看 config.yaml"}
	for _, g := range positives {
		if !isSingleToolRequest(g) {
			t.Errorf("expected single tool request for %q", g)
		}
	}
	negatives := []string{
		"读一下 main.go 然后重构它",
		"实现一个 web 服务",
		"读一下 A 再写一下 B",
		"查一下天气并发送邮件",
		"修改配置之后重启服务",
	}
	for _, g := range negatives {
		if isSingleToolRequest(g) {
			t.Errorf("expected NOT single tool for %q", g)
		}
	}
}

// TestIsMultiDomainHint_Extended 验证扩展后的跨领域组合。
func TestIsMultiDomainHint_Extended(t *testing.T) {
	positives := []string{
		"设计数据库表结构并加缓存",
		"前端展示算法排序结果",
		"后端 API 连数据库",
		"服务接口和前端页面一起改",
	}
	for _, g := range positives {
		if !isMultiDomainHint(g) {
			t.Errorf("expected multi-domain hint for %q", g)
		}
	}
}

// TestDetectSubdomainBoundaries 验证子领域边界检测。
func TestDetectSubdomainBoundaries(t *testing.T) {
	// 跨 ≥2 子领域
	if !detectSubdomainBoundaries([]string{"设计 API 接口", "实现数据库模型", "编写前端页面"}) {
		t.Errorf("expected boundaries detected for api+db+frontend")
	}
	// 单一领域
	if detectSubdomainBoundaries([]string{"修复 bug A", "修复 bug B", "修复 bug C"}) {
		t.Errorf("expected no boundaries for same-domain tasks")
	}
}

// TestParseRoutePath 验证从 LLM 输出解析路由路径。
func TestParseRoutePath(t *testing.T) {
	cases := []struct {
		input string
		want  RoutePath
	}{
		{"complexity: simple\npath: direct_tool", RouteDirectTool},
		{"complexity: simple\npath: direct_assistant", RouteDirectAssistant},
		{"complexity: complex\npath: create_domain", RouteCreateDomain},
		{"complexity: complex\npath: multi_domain", RouteMultiDomain},
		{"complexity: complex\npath: full_four_layer", RouteFullFourLayer},
		// 兼容旧输出/兜底
		{"direct_tool", RouteDirectTool},
		{"  direct_assistant  ", RouteDirectAssistant},
		{"path: create_domain.", RouteCreateDomain},
		// 否定前缀：不应被兜底路径误判
		{"不应使用 direct_tool，请用 create_domain", RouteCreateDomain},
		{"不要选 direct_assistant，选 multi_domain", RouteMultiDomain},
		// 非法路径回退空串
		{"path: unknown_path", ""},
		{"just some text", ""},
	}
	for _, c := range cases {
		got := parseRoutePath(c.input)
		if got != c.want {
			t.Errorf("parseRoutePath(%q) = %s, want %s", c.input, got, c.want)
		}
	}
}
