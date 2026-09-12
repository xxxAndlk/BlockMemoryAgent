package subagent

// reuse_guard_test.go 测试热驻复用守卫：命名包含拦截、【新领域声明】逃生口、
// spec 文件重叠拦截、无匹配放行，及纯函数（domainNameOverlap/pathsOverlap）。

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

func TestDomainNameOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"jiujie-port80", "jiujie", true}, // 事故原案：新名包含旧名
		{"jiujie", "jiujie-port80", true}, // 方向无关
		{"Jiujie-Port80", "jiujie", true}, // case-insensitive
		{"jiujie", "jiujie", true},        // 同名（idle 槽重派）
		{"game-core", "game-ui", false},   // token 前缀共享不算
		{"finance", "jiujie", false},      // 无关
		{"", "jiujie", false},             // 空名
		{"a", "abc", false},               // 短侧 < 2 rune 防单字符误命中
		{"九劫部署", "九劫", true},            // 中文包含
	}
	for _, c := range cases {
		if got := domainNameOverlap(c.a, c.b); got != c.want {
			t.Errorf("domainNameOverlap(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestPathsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"/srv/jiujie/app.conf", "/srv/jiujie/app.conf", true},
		{"D:/srv/app.conf", "d:\\srv\\app.conf", true}, // 大小写 + 反斜杠归一
		{"./app.conf", "/srv/jiujie/app.conf", true},   // 相对 vs 绝对（后缀）
		{"/srv/jiujie/app.conf", "app.conf", true},     // 后缀
		{"/srv/a/app.conf", "/srv/b/app.conf", false},  // 同名不同目录
		{"/srv/nginx.conf", "/srv/app.conf", false},
		{"", "/srv/app.conf", false},
	}
	for _, c := range cases {
		if got := pathsOverlap(c.a, c.b); got != c.want {
			t.Errorf("pathsOverlap(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// dispatchIdleDomain 派发单个热驻 domain 并等其进 Idle，返回 subAgentID。
func dispatchIdleDomain(t *testing.T, toolsReg *tool.Registry, domain, responsibility string) string {
	t.Helper()
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "做某事",
		"domain":         domain,
		"responsibility": responsibility,
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch %s failed: err=%v res=%+v", domain, err, res)
	}
	return subAgentIDOf(res)
}

// waitTreeIdle 等指定子 Agent 树节点转 Idle。
func waitTreeIdle(t *testing.T, tr *orchestrator.Tree, subID string) {
	t.Helper()
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})
}

func TestCheckIdleDomainReuse_NameContainmentRejects(t *testing.T) {
	provider := &scriptProvider{lines: []string{"domain result"}}
	_, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)
	subID := dispatchIdleDomain(t, toolsReg, "jiujie", "负责九劫服务器部署")
	waitTreeIdle(t, tr, subID)

	// 事故原案重放：jiujie 已热驻 idle，新建 jiujie-port80 应被守卫拦截。
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "把九劫服务改到 80 端口",
		"domain":         "jiujie-port80",
		"responsibility": "负责九劫服务器部署",
	})
	if err != nil {
		t.Fatalf("dispatch err: %v", err)
	}
	if res.Success {
		t.Fatal("near-name new domain should be rejected by reuse guard")
	}
	if !strings.Contains(res.Error, "复用守卫") || !strings.Contains(res.Error, subID) || !strings.Contains(res.Error, "reuse_agent_id") {
		t.Fatalf("rejection should guide reuse, got: %q", res.Error)
	}
}

func TestCheckIdleDomainReuse_NewDomainDeclarationEscapes(t *testing.T) {
	provider := &scriptProvider{lines: []string{"domain result", "domain result"}}
	_, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)
	subID := dispatchIdleDomain(t, toolsReg, "jiujie", "负责九劫服务器部署")
	waitTreeIdle(t, tr, subID)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "搭建独立监控面板。【新领域声明：与 jiujie 部署域无关，属全新模块】",
		"domain":         "jiujie-monitor",
		"responsibility": "负责监控面板",
	})
	if err != nil || !res.Success {
		t.Fatalf("declared new domain should pass, err=%v res=%+v", err, res)
	}
}

func TestCheckIdleDomainReuse_UnrelatedDomainPasses(t *testing.T) {
	provider := &scriptProvider{lines: []string{"domain result", "domain result"}}
	_, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)
	subID := dispatchIdleDomain(t, toolsReg, "jiujie", "负责九劫服务器部署")
	waitTreeIdle(t, tr, subID)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "写财务周报脚本",
		"domain":         "finance",
		"responsibility": "负责财务报表",
	})
	if err != nil || !res.Success {
		t.Fatalf("unrelated domain should pass, err=%v res=%+v", err, res)
	}
}

func TestCheckIdleDomainReuse_FileOverlapRejects(t *testing.T) {
	provider := &scriptProvider{lines: []string{"domain result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)
	subID := dispatchIdleDomain(t, toolsReg, "deploy", "负责部署")
	waitTreeIdle(t, tr, subID)

	// 命名无包含（ops-config vs deploy），但 spec 文件与 deploy Agent 近期写入重叠。
	d.lastWrites.Store(subID, &fileWriteState{recs: []fileWriteRecord{
		{path: "D:/srv/jiujie/app.conf", at: time.Now()},
	}})
	kv := newTestKVMemory(true)
	d.sharedMem = kv
	kv.items[specKeyFor("s1", "ops-config")] = tool.EncodeSpecMD("s1", tool.Spec{
		Goal:       "改配置",
		Acceptance: []string{"配置生效"},
		Files:      []string{"D:/srv/jiujie/app.conf"},
	}, nil)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "调整服务配置",
		"domain":         "ops-config",
		"responsibility": "负责服务配置",
	})
	if err != nil {
		t.Fatalf("dispatch err: %v", err)
	}
	if res.Success {
		t.Fatal("file-overlapping new domain should be rejected by reuse guard")
	}
	if !strings.Contains(res.Error, "近期写入重叠") || !strings.Contains(res.Error, subID) {
		t.Fatalf("rejection should cite file overlap and slot id, got: %q", res.Error)
	}
}

func TestCheckIdleDomainReuse_DisabledHotResidentNoop(t *testing.T) {
	// 热驻关闭：守卫零行为变化（checkIdleDomainReuse 直接放行）。
	d := &Dispatcher{}
	if msg := d.checkIdleDomainReuse(dispatchCtx(), "s1", "jiujie-port80", "task"); msg != "" {
		t.Fatalf("hot resident disabled should pass, got %q", msg)
	}
}

// --- 同名热驻 Idle 槽隐式复用（2026-08-27 派发死循环根治） ---

// TestImplicitReuse_SameNameIdleRoutesToSlot：MetaAgent 漏传 reuse_agent_id 且空
// responsibility 的同名重派，不再落进 "responsibility required"/"同名活跃实例"
// 双错循环，而是自动路由到热驻槽（等价隐式 reuse_agent_id）。
func TestImplicitReuse_SameNameIdleRoutesToSlot(t *testing.T) {
	provider := &scriptProvider{lines: []string{"domain result", "domain result"}}
	_, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)
	subID := dispatchIdleDomain(t, toolsReg, "jiujie", "负责九劫服务器部署")
	waitTreeIdle(t, tr, subID)

	// 事故原案重放：domain 精确同名 + responsibility 缺省（复用沿用槽内冻结值）。
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id": "domain",
		"task":    "补验收尾任务",
		"domain":  "jiujie",
	})
	if err != nil || !res.Success {
		t.Fatalf("same-name idle re-dispatch should implicitly reuse, err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Output, subID) {
		t.Fatalf("implicit reuse should target original slot %s, got %q", subID, res.Output)
	}
	count := 0
	for _, n := range tr.Snapshot() {
		if n.Role == "domain" && n.Domain == "jiujie" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("implicit reuse created duplicate node: %d nodes for domain jiujie, want 1", count)
	}
}

// TestImplicitReuse_BatchSameNameItemRoutesToSlot：批量派发单入口同样享受隐式复用，
// 同名项路由热驻槽、无关新项正常新建。
func TestImplicitReuse_BatchSameNameItemRoutesToSlot(t *testing.T) {
	provider := &scriptProvider{lines: []string{"domain result", "domain result", "domain result"}}
	_, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)
	subID := dispatchIdleDomain(t, toolsReg, "deploy", "负责部署")
	waitTreeIdle(t, tr, subID)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agents", map[string]any{
		"tasks": []any{
			map[string]any{"role_id": "domain", "task": "部署后续调整", "domain": "deploy"},
			map[string]any{"role_id": "domain", "task": "写财务周报脚本", "domain": "finance", "responsibility": "负责财务报表"},
		},
	})
	if err != nil || !res.Success {
		t.Fatalf("batch with same-name idle item should succeed, err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Output, subID) {
		t.Fatalf("reuse item should map to original slot id %s, got %q", subID, res.Output)
	}
}
