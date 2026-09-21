package subagent

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

func identityTestDispatcher(t *testing.T, sessID string, nodes ...orchestrator.Node) *Dispatcher {
	t.Helper()
	d := NewDispatcher(nil, nil, nil, nil, nil)
	tr := orchestrator.NewTree(sessID, nil)
	for _, n := range nodes {
		tr.Register(n)
	}
	return d.WithTree(func(sessionID string) *orchestrator.Tree {
		if sessionID == sessID {
			return tr
		}
		return nil
	})
}

// TestRuntimeIdentityBlock_DomainSeesUpPeersAndChildren domain 身份块：上级+同级 domain+下级叶子。
func TestRuntimeIdentityBlock_DomainSeesUpPeersAndChildren(t *testing.T) {
	sess := "s1"
	d := identityTestDispatcher(t, sess,
		orchestrator.Node{ID: "s1/domain-1", ParentID: "s1", Role: "domain", Domain: "核心层", Task: "实现核心逻辑"},
		orchestrator.Node{ID: "s1/domain-2", ParentID: "s1", Role: "domain", Domain: "命令层", Task: "实现命令解析"},
		orchestrator.Node{ID: "s1/domain-4", ParentID: "s1", Role: "domain", Domain: "已完结", Task: "早期任务"},
		orchestrator.Node{ID: "s1/domain-3/code_assistant-5", ParentID: "s1/domain-3", Role: "code_assistant", Task: "写解析器"},
		orchestrator.Node{ID: "s1/domain-3/scout-6", ParentID: "s1/domain-3", Role: "scout", Task: "定位符号"},
	)
	tr := d.treeFn(sess)
	tr.Finish("s1/domain-4", "", nil) // 终态同级：不得列入

	block := d.runtimeIdentityBlock(sess, "s1", "s1/domain-3")
	for _, want := range []string{
		"你的实例 id：s1/domain-3",
		"你的上级：s1",
		"s1/domain-1（核心层",
		"s1/domain-2（命令层",
		"s1/domain-3/code_assistant-5",
		"s1/domain-3/scout-6",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("domain 身份块缺少 %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "s1/domain-4") {
		t.Fatalf("domain 身份块不得包含已终结同级:\n%s", block)
	}
	// 下级段与同级段必须都有标签。
	if !strings.Contains(block, "- 同级 domain") || !strings.Contains(block, "- 你的下级执行者") {
		t.Fatalf("domain 身份块须含同级/下级两段:\n%s", block)
	}
}

// TestRuntimeIdentityBlock_LeafSeesParentAndGroupOnly 叶子身份块：只含上级+同组同级，
// 不得含祖父 meta 与其他 domain 节点。
func TestRuntimeIdentityBlock_LeafSeesParentAndGroupOnly(t *testing.T) {
	sess := "s1"
	d := identityTestDispatcher(t, sess,
		orchestrator.Node{ID: "s1/domain-9", ParentID: "s1", Role: "domain", Domain: "存储层", Task: "存储实现"},
		orchestrator.Node{ID: "s1/domain-9/code_assistant-5", ParentID: "s1/domain-9", Role: "code_assistant", Task: "写模块A"},
		orchestrator.Node{ID: "s1/domain-9/scout-6", ParentID: "s1/domain-9", Role: "scout", Task: "定位B"},
		orchestrator.Node{ID: "s1/domain-8/code_assistant-7", ParentID: "s1/domain-8", Role: "code_assistant", Task: "他域叶子"},
		orchestrator.Node{ID: "s1/domain-8", ParentID: "s1", Role: "domain", Domain: "他域", Task: "他域任务"},
	)

	block := d.runtimeIdentityBlock(sess, "s1/domain-9", "s1/domain-9/code_assistant-5")
	for _, want := range []string{
		"你的实例 id：s1/domain-9/code_assistant-5",
		"你的上级：s1/domain-9",
		"s1/domain-9/scout-6",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("叶子身份块缺少 %q:\n%s", want, block)
		}
	}
	// 可见性红线：不得出现祖父 meta（s1 单独成行级 id）、他域 domain、他域叶子、自己。
	for _, forbid := range []string{"- s1\n", "s1/domain-8", "code_assistant-7", "code_assistant-5（代码助手"} {
		if strings.Contains(block, forbid) {
			t.Fatalf("叶子身份块越界包含 %q:\n%s", forbid, block)
		}
	}
	if !strings.Contains(block, "- 同组并行执行者") {
		t.Fatalf("叶子身份块须含同组段:\n%s", block)
	}
}

func TestRuntimeIdentityBlock_NoPeersNoChildren(t *testing.T) {
	d := identityTestDispatcher(t, "s1",
		orchestrator.Node{ID: "s1/domain-9", ParentID: "s1", Role: "domain", Domain: "独苗", Task: "独自任务"},
	)
	block := d.runtimeIdentityBlock("s1", "s1", "s1/domain-9")
	if !strings.Contains(block, "同级 domain：当前无") || !strings.Contains(block, "下级执行者：当前无") {
		t.Fatalf("无同级/下级时应明示当前无:\n%s", block)
	}
}

func TestRuntimeIdentityBlock_NilTreeFn(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	block := d.runtimeIdentityBlock("s1", "s1", "s1/domain-1")
	if !strings.Contains(block, "你的实例 id：s1/domain-1") || !strings.Contains(block, "同级 domain：当前无") {
		t.Fatalf("treeFn 为 nil 时身份块仍须含自己/上级 id 并安全回退:\n%s", block)
	}
}

// TestVisibleTargets_Matrix 可见性矩阵硬校验的目标集合：
// meta 不限（nil）；domain=上级+同级+下级；叶子=上级+同组（不含他域叶子/孙级/meta 直发）。
func TestVisibleTargets_Matrix(t *testing.T) {
	sess := "s1"
	d := identityTestDispatcher(t, sess,
		orchestrator.Node{ID: "s1/domain-1", ParentID: "s1", Role: "domain", Domain: "核心", Task: "核心任务"},
		orchestrator.Node{ID: "s1/domain-2", ParentID: "s1", Role: "domain", Domain: "命令", Task: "命令任务"},
		orchestrator.Node{ID: "s1/domain-1/code_assistant-5", ParentID: "s1/domain-1", Role: "code_assistant", Task: "叶子A"},
		orchestrator.Node{ID: "s1/domain-1/scout-6", ParentID: "s1/domain-1", Role: "scout", Task: "叶子B"},
	)

	// meta：nil = 不校验。
	if got := d.visibleTargets(sess, "s1"); got != nil {
		t.Fatalf("meta 应返回 nil（不校验），got %v", got)
	}
	// domain-1：上级 s1 + 同级 domain-2 + 下级两个叶子。
	dt := d.visibleTargets(sess, "s1/domain-1")
	for _, want := range []string{"s1", "s1/domain-2", "s1/domain-1/code_assistant-5", "s1/domain-1/scout-6"} {
		if !dt[want] {
			t.Fatalf("domain-1 可见集缺 %q: %v", want, dt)
		}
	}
	for _, forbid := range []string{"s1/domain-3", "s1/domain-2/code_assistant-9"} {
		if dt[forbid] {
			t.Fatalf("domain-1 可见集不得含 %q", forbid)
		}
	}
	// 叶子：上级 domain-1 + 同组 scout-6；不得含祖父 s1、同级 domain、他域叶子。
	lf := d.visibleTargets(sess, "s1/domain-1/code_assistant-5")
	if !lf["s1/domain-1"] || !lf["s1/domain-1/scout-6"] {
		t.Fatalf("叶子可见集缺上级/同组: %v", lf)
	}
	for _, forbid := range []string{"s1", "s1/domain-2", "s1/domain-1/code_assistant-5" /*自己*/, "s1/domain-2/x-1"} {
		if lf[forbid] {
			t.Fatalf("叶子可见集不得含 %q", forbid)
		}
	}
}

func TestParentIDOfAgentID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"s1/domain-3", "s1"},
		{"s1/domain-1/code_assistant-5", "s1/domain-1"},
		{"s1", ""},
		{"session-42", ""},
	}
	for _, c := range cases {
		if got := parentIDOfAgentID(c.in); got != c.want {
			t.Fatalf("parentIDOfAgentID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
