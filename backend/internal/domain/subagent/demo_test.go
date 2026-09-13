package subagent

// 录播演示阶段（demo.go）单元测试：
// 确认/评审答复解析、演示任务书与归因任务契约、演示产物机读块解析、
// 名单排除 test_assistant（验收+演示节点）、RunWrap 演示通过/打回两条端到端走向。

import (
	"context" // context 构造派发/提问上下文
	"strings" // strings 断言交付文本包含关系
	"testing" // 测试框架
	"time"    // time 构造树节点时间

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator" // 权威树（名单断言）
	"github.com/blockmemory/agent/backend/internal/domain/tool"         // WithSessionID / SetAskUserHook
)

// TestParseDemoConfirm 验证「是否进入演示」答复解析：跳过词优先（防误判），
// 进入词放行，自由文本/空默认跳过（fail-open）。
func TestParseDemoConfirm(t *testing.T) {
	cases := []struct {
		answer string
		want   bool
	}{
		{"进入演示", true},
		{"好的，开始吧", true},
		{"yes", true},
		{"确认", true},
		{"跳过演示", false},
		{"不用了", false},
		{"不需要演示", false}, // 含"演示"但跳过词优先
		{"否", false},
		{"随便你", false}, // 自由文本默认跳过
		{"", false},
	}
	for _, c := range cases {
		if got := parseDemoConfirm(c.answer); got != c.want {
			t.Fatalf("parseDemoConfirm(%q)=%v, want %v", c.answer, got, c.want)
		}
	}
}

// TestParseDemoReview 验证演示评审答复解析：驳回词优先（防"不通过"误判通过），
// 批准词放行，自由文本按打回处理（原话作意见），空答复判打回（fail-open 在调用侧）。
func TestParseDemoReview(t *testing.T) {
	// 批准词。
	for _, a := range []string{"演示通过", "可以，满意", "pass", "ok，没问题"} {
		if approved, fb := parseDemoReview(a); !approved || fb != "" {
			t.Fatalf("parseDemoReview(%q) 应判通过, got approved=%v fb=%q", a, approved, fb)
		}
	}
	// 驳回词（原话作意见）。
	for _, a := range []string{"演示打回", "不通过", "视频有问题", "reject"} {
		if approved, fb := parseDemoReview(a); approved || fb != a {
			t.Fatalf("parseDemoReview(%q) 应判打回且原话作意见, got approved=%v fb=%q", a, approved, fb)
		}
	}
	// 自由文本按打回处理（原话作意见）。
	if approved, fb := parseDemoReview("第 2 页按钮点不动"); approved || fb != "第 2 页按钮点不动" {
		t.Fatalf("自由文本应按打回处理, got approved=%v fb=%q", approved, fb)
	}
	// 空答复：无批准词判打回（调用侧已先按 fail-open 拦截，这里只钉住解析语义）。
	if approved, _ := parseDemoReview(""); approved {
		t.Fatal("空答复不应判通过")
	}
}

// TestBuildDemoTask 验证演示任务书契约：接力理由（防 relay 熔断）、验收报告路径引用、
// 录像手段优先级、产物落盘目录与机读契约块。
func TestBuildDemoTask(t *testing.T) {
	task := buildDemoTask("sess-demo", 2, ".bma/acceptance/sess-demo-r2.md")
	for _, want := range []string{
		"【接力理由】",
		".bma/acceptance/sess-demo-r2.md",
		"模拟人类完整操作交付物",
		".bma/demo/",
		"Playwright recordVideo",
		"ffmpeg gdigrab",
		"host_computer_use",
		"【演示产物】",
		"【演示摘要】",
	} {
		if !strings.Contains(task, want) {
			t.Fatalf("演示任务书缺 %q:\n%s", want, task)
		}
	}
	// 报告路径缺省时按会话/轮次合成期望路径。
	if task := buildDemoTask("sess-demo", 1, ""); !strings.Contains(task, "sess-demo-r1.md") {
		t.Fatalf("缺省应合成验收报告路径:\n%s", task)
	}
}

// TestParseDemoOutput 验证演示节点答复末尾机读块解析。
func TestParseDemoOutput(t *testing.T) {
	path, summary := parseDemoOutput("演示正文……\n【演示产物】.bma/demo/demo.webm\n【演示摘要】逐页点击操作演示\n")
	if path != ".bma/demo/demo.webm" {
		t.Fatalf("产物路径解析错误: %q", path)
	}
	if !strings.Contains(summary, "逐页点击操作演示") {
		t.Fatalf("摘要解析错误: %q", summary)
	}
	if path, _ := parseDemoOutput("没有任何机读块"); path != "" {
		t.Fatalf("缺机读块应返回空路径, got %q", path)
	}
}

// TestBuildAttributionTask 验证归因任务书：含打回意见原文 + 验收报告路径 + 名单 ID；
// 其要求的机读块与验收同款，可被 parseAcceptanceReport 解析。
func TestBuildAttributionTask(t *testing.T) {
	roster := []acceptanceRosterEntry{
		{ID: "sess-x/web-1", Role: "web_dev", Domain: "web", Task: "搭页面", Status: "done", Summary: "完成"},
	}
	task := buildAttributionTask("第 2 页按钮点不动", ".bma/acceptance/sess-x-r1.md", roster)
	for _, want := range []string{
		"【接力理由】",
		"第 2 页按钮点不动",
		".bma/acceptance/sess-x-r1.md",
		"sess-x/web-1",
		"【验收结论】FAIL",
		"【错误清单】",
	} {
		if !strings.Contains(task, want) {
			t.Fatalf("归因任务书缺 %q:\n%s", want, task)
		}
	}
	// 归因答复复用 parseAcceptanceReport 解析（机读契约同款）。
	answer := "归因正文：按钮由 web-1 实现。\n【验收结论】FAIL\n【错误清单】\n1. [agent:sess-x/web-1] 演示发现第 2 页按钮点击无响应\n"
	hasVerdict, passed, items := parseAcceptanceReport(answer)
	if !hasVerdict || passed || len(items) != 1 || items[0].AgentID != "sess-x/web-1" {
		t.Fatalf("归因答复解析错误: hasVerdict=%v passed=%v items=%v", hasVerdict, passed, items)
	}
}

// TestCollectAcceptanceRoster_ExcludesTestAssistant 验证名单按角色排除全部
// test_assistant 节点（验收 acceptance 与演示 demo 节点都不进错误归因名单）。
func TestCollectAcceptanceRoster_ExcludesTestAssistant(t *testing.T) {
	tree := orchestrator.NewTree("sess-r", nil)
	tree.Register(orchestrator.Node{ID: "sess-r/web-1", Role: "web_dev", Domain: "web", Task: "搭页面", Started: time.Now()})
	tree.Finish("sess-r/web-1", "完成", nil)
	tree.Register(orchestrator.Node{ID: "sess-r/tester-1", Role: "test_assistant", Domain: acceptanceDomain, Task: "验收", Started: time.Now()})
	tree.Finish("sess-r/tester-1", "PASS", nil)
	tree.Register(orchestrator.Node{ID: "sess-r/demo-1", Role: "test_assistant", Domain: demoDomain, Task: "录演示", Started: time.Now()})
	tree.Finish("sess-r/demo-1", "产物", nil)
	roster, _ := collectAcceptanceRoster(tree)
	if len(roster) != 1 || roster[0].ID != "sess-r/web-1" {
		t.Fatalf("名单应只剩执行节点 sess-r/web-1, got %+v", roster)
	}
}

// TestRun_DemoPass 验证演示通过端到端：验收 PASS → 用户确认进入 → 演示节点产出
// 【演示产物】→ 评审放行，终答附演示视频路径；树上恰好 1 个 demo 节点。
func TestRun_DemoPass(t *testing.T) {
	mgr, tree, _ := newAcceptanceTestManager(t, "验收正文\n【验收结论】PASS\n【演示产物】.bma/demo/demo.webm\n【演示摘要】逐页操作演示\n")
	workDir := t.TempDir()
	if err := SaveTesterConfig(workDir, TesterConfig{Mode: TesterModeOn, MaxRounds: 2}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	answers := []string{"进入演示", "演示通过"}
	var idx int
	var reviewArtifacts []tool.AskUserArtifact
	mgr.d.tools.SetAskUserHook(func(ctx context.Context, question string, opts tool.AskUserOptions) (string, error) {
		a := answers[idx]
		if idx == 1 {
			reviewArtifacts = opts.Artifacts // 评审卡应内嵌演示产物
		}
		idx++
		return a, nil
	})
	ctx := tool.WithSessionID(context.Background(), "sess-acc")
	out := mgr.RunWrap(ctx, "sess-acc", workDir, "做个页面", "最终答复原文")
	if !strings.Contains(out, "验收通过") || !strings.Contains(out, "演示视频：.bma/demo/demo.webm") {
		t.Fatalf("演示通过终答应附演示视频路径, got %q", out)
	}
	if idx != 2 {
		t.Fatalf("应恰好提问 2 次（确认+评审）, got %d", idx)
	}
	if len(reviewArtifacts) != 1 || reviewArtifacts[0].Kind != "video" || reviewArtifacts[0].Path != ".bma/demo/demo.webm" {
		t.Fatalf("评审卡应内嵌演示视频产物, got %+v", reviewArtifacts)
	}
	demos := 0
	for _, n := range tree.Snapshot() {
		if n.Role == "test_assistant" && n.Domain == demoDomain {
			demos++
		}
	}
	if demos != 1 {
		t.Fatalf("应恰好派发 1 个 demo 节点, got %d", demos)
	}
}

// TestRun_DemoRejectUnattributable 验证演示打回但无法归因（树上无执行节点兜底）：
// 意见记入【未验证项】按现状交付，不阻塞。
func TestRun_DemoRejectUnattributable(t *testing.T) {
	mgr, _, _ := newAcceptanceTestManager(t, "验收正文\n【验收结论】PASS\n【演示产物】.bma/demo/demo.webm\n【演示摘要】逐页操作演示\n")
	workDir := t.TempDir()
	if err := SaveTesterConfig(workDir, TesterConfig{Mode: TesterModeOn, MaxRounds: 2}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	// 评审只点选项文案（无实质意见）→ 应追加第三次纯文本提问收意见原文。
	answers := []string{"进入演示", "演示打回", "第 2 页按钮无响应"}
	var idx int
	mgr.d.tools.SetAskUserHook(func(ctx context.Context, question string, opts tool.AskUserOptions) (string, error) {
		a := answers[idx]
		idx++
		return a, nil
	})
	ctx := tool.WithSessionID(context.Background(), "sess-acc")
	out := mgr.RunWrap(ctx, "sess-acc", workDir, "做个页面", "最终答复原文")
	if idx != 3 {
		t.Fatalf("选项文案无实质意见应追加提问收打回意见, 提问次数 got %d", idx)
	}
	if !strings.Contains(out, "【未验证项】") || !strings.Contains(out, "第 2 页按钮无响应") {
		t.Fatalf("无法归因的打回意见应记入【未验证项】, got %q", out)
	}
}
