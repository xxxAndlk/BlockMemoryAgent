package tool

// external_agent_test.go 覆盖 CallExternalAgent 契约：跑通/非零退出/超时树杀/未注册/
// 未启用/未安装/占位符渲染（不经 shell）/无占位符追加/超时封顶/Schema 动态清单。
// 假 CLI 用 os.Args[0] helper 进程模式（go test 二进制自举，无外部依赖）。

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHelperExternalAgent 是 helper 进程入口（BMA_EXT_AGENT_HELPER=1 时执行）：
// go test 以 -test.run=TestHelperExternalAgent -- <args...> 启动自身，"--" 之后的
// argv 即外部 Agent 收到的渲染后参数。行为约定：每参数输出一行 ARG:<v>；
// sleep=<sec> 睡眠（测超时树杀）；"fail" 输出 boom-output 后 exit 3（测非零退出）。
func TestHelperExternalAgent(t *testing.T) {
	if os.Getenv("BMA_EXT_AGENT_HELPER") != "1" {
		return
	}
	for _, a := range helperExtraArgs() {
		switch {
		case strings.HasPrefix(a, "sleep="):
			n, _ := strconv.Atoi(strings.TrimPrefix(a, "sleep="))
			time.Sleep(time.Duration(n) * time.Second)
		case a == "fail":
			fmt.Println("boom-output")
			os.Exit(3)
		default:
			fmt.Println("ARG:" + a)
		}
	}
	os.Exit(0)
}

// helperExtraArgs 取测试二进制 "--" 分隔符之后的 argv。
func helperExtraArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

// helperDef 构造指向本测试二进制（helper 模式）的启用条目。
func helperDef() ExternalAgentDef {
	return ExternalAgentDef{
		ID:         "fake",
		Enabled:    true,
		Command:    os.Args[0],
		Args:       []string{"-test.run=TestHelperExternalAgent", "--", "{{task}}"},
		TimeoutSec: 60,
	}
}

func extTestRegistry(t *testing.T, defs []ExternalAgentDef) *Registry {
	t.Helper()
	t.Setenv("BMA_EXT_AGENT_HELPER", "1")
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetExternalAgentDefs(defs)
	return r
}

func extDispatch(ctx context.Context, r *Registry, args map[string]any) *Result {
	res, _ := r.Dispatch(ctx, "CallExternalAgent", args)
	return res
}

// 跑通 + 占位符渲染为独立 argv 元素（含空格任务文本整体到达 = 不经 shell 分词）。
func TestCallExternalAgent_Run(t *testing.T) {
	r := extTestRegistry(t, []ExternalAgentDef{helperDef()})
	res := extDispatch(context.Background(), r, map[string]any{"agent": "fake", "task": "hello world 你好"})
	if !res.Success {
		t.Fatalf("expected success, got %+v", res)
	}
	if !strings.Contains(res.Output, "ARG:hello world 你好") {
		t.Fatalf("task should arrive as one argv element, output=%q", res.Output)
	}
	if strings.Contains(res.Output, "ARG:hello\n") || strings.Contains(res.Output, "ARG:world") {
		t.Fatal("task must not be split by shell word-splitting")
	}
}

// 非零退出：execution_failed 且输出保留。
func TestCallExternalAgent_NonZeroExit(t *testing.T) {
	r := extTestRegistry(t, []ExternalAgentDef{helperDef()})
	res := extDispatch(context.Background(), r, map[string]any{"agent": "fake", "task": "fail"})
	if res.Success {
		t.Fatalf("expected failure, got %+v", res)
	}
	if res.Category != "execution_failed" {
		t.Fatalf("expected execution_failed, got %q", res.Category)
	}
	if !strings.Contains(res.Output, "boom-output") {
		t.Fatalf("output should be preserved, got %q", res.Output)
	}
}

// 超时：Error 含"超时"，进程树终止 promptly（不等到 sleep 结束）。
func TestCallExternalAgent_Timeout(t *testing.T) {
	r := extTestRegistry(t, []ExternalAgentDef{helperDef()})
	start := time.Now()
	res := extDispatch(context.Background(), r, map[string]any{"agent": "fake", "task": "sleep=30", "timeout_sec": float64(1)})
	if res.Success {
		t.Fatalf("expected failure, got %+v", res)
	}
	if !strings.Contains(res.Error, "超时") {
		t.Fatalf("error should mention timeout, got %q", res.Error)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("timeout should terminate promptly, took %v", elapsed)
	}
}

// 未注册：validation_rejected 且错误列出可用项。
func TestCallExternalAgent_Unregistered(t *testing.T) {
	r := extTestRegistry(t, []ExternalAgentDef{helperDef()})
	res := extDispatch(context.Background(), r, map[string]any{"agent": "nope", "task": "x"})
	if res.Success || res.Category != "validation_rejected" {
		t.Fatalf("unexpected: %+v", res)
	}
	if !strings.Contains(res.Error, "未注册的外部 Agent: nope") || !strings.Contains(res.Error, "fake") {
		t.Fatalf("error should list available agents, got %q", res.Error)
	}
}

// 未启用条目按同口径拒绝（可用清单不含 disabled 条目）；别名 call_external_agent 同路由。
func TestCallExternalAgent_DisabledAndAlias(t *testing.T) {
	off := helperDef()
	off.ID = "off"
	off.Enabled = false
	r := extTestRegistry(t, []ExternalAgentDef{off, helperDef()})
	res := extDispatch(context.Background(), r, map[string]any{"agent": "off", "task": "x"})
	if res.Success || res.Category != "validation_rejected" {
		t.Fatalf("unexpected: %+v", res)
	}
	if !strings.Contains(res.Error, "未注册的外部 Agent: off") {
		t.Fatalf("got %q", res.Error)
	}
	res2, _ := r.Dispatch(context.Background(), "call_external_agent", map[string]any{"agent": "nope", "task": "x"})
	if res2 == nil || res2.Success || !strings.Contains(res2.Error, "未注册") {
		t.Fatalf("alias should route to the tool, got %+v", res2)
	}
}

// 未安装 CLI：validation_rejected，错误含"未安装: command"而非裸 exec 错误。
func TestCallExternalAgent_NotInstalled(t *testing.T) {
	d := ExternalAgentDef{ID: "ghost", Enabled: true, Command: "bma-no-such-cli-xyz", Args: []string{"{{task}}"}, TimeoutSec: 60}
	r := extTestRegistry(t, []ExternalAgentDef{d})
	res := extDispatch(context.Background(), r, map[string]any{"agent": "ghost", "task": "x"})
	if res.Success || res.Category != "validation_rejected" {
		t.Fatalf("unexpected: %+v", res)
	}
	if !strings.Contains(res.Error, "未安装: bma-no-such-cli-xyz") {
		t.Fatalf("got %q", res.Error)
	}
}

// 无 {{task}} 占位符：任务文本追加为最后一个 argv（执行侧兜底）。
func TestCallExternalAgent_NoPlaceholderAppends(t *testing.T) {
	d := helperDef()
	d.Args = []string{"-test.run=TestHelperExternalAgent", "--"}
	r := extTestRegistry(t, []ExternalAgentDef{d})
	res := extDispatch(context.Background(), r, map[string]any{"agent": "fake", "task": "appended"})
	if !res.Success || !strings.Contains(res.Output, "ARG:appended") {
		t.Fatalf("task should be appended as last argv: %+v", res)
	}
}

// 超时归一化：调用级覆盖条目默认、封顶 7200、<=0 回落（条目默认失效再兜底 1800）。
func TestClampExternalTimeout(t *testing.T) {
	cases := []struct{ param, def, want int }{
		{0, 1800, 1800}, {-5, 1800, 1800}, {0, 0, 1800}, {60, 1800, 60},
		{99999, 1800, 7200}, {7200, 300, 7200},
	}
	for _, c := range cases {
		if got := clampExternalTimeout(c.param, c.def); got != c.want {
			t.Fatalf("clamp(%d,%d)=%d want %d", c.param, c.def, got, c.want)
		}
	}
}

// Destructive()=true（审批链接入契约）。
func TestCallExternalAgent_Destructive(t *testing.T) {
	tl := &externalAgentTool{reg: newExternalAgentRegistry()}
	if !tl.Destructive() {
		t.Fatal("CallExternalAgent must be destructive")
	}
}

// Schema 描述动态内嵌当前启用清单；disabled 条目不出现；空注册表显示空清单。
func TestCallExternalAgent_SchemaDesc(t *testing.T) {
	r := extTestRegistry(t, []ExternalAgentDef{
		{ID: "alpha", Enabled: true, Command: "a", TimeoutSec: 60, Description: "测试甲"},
		{ID: "beta", Enabled: false, Command: "b", TimeoutSec: 60},
	})
	desc := r.externalAgentSchemaDesc()
	if !strings.Contains(desc, "alpha") || !strings.Contains(desc, "测试甲") {
		t.Fatalf("enabled entry missing from schema desc: %q", desc)
	}
	if strings.Contains(desc, "beta") {
		t.Fatalf("disabled entry must not appear: %q", desc)
	}
	r.SetExternalAgentDefs(nil)
	if !strings.Contains(r.externalAgentSchemaDesc(), "空") {
		t.Fatalf("empty registry desc: %q", r.externalAgentSchemaDesc())
	}
	// ExternalAgentDefs 快照含 disabled（管理 API 数据源契约）。
	r.SetExternalAgentDefs([]ExternalAgentDef{{ID: "x", Enabled: false, Command: "c", TimeoutSec: 60}})
	if got := r.ExternalAgentDefs(); len(got) != 1 || got[0].Enabled {
		t.Fatalf("snapshot should keep disabled entries: %+v", got)
	}
}
