package skill

import (
	"context" // 测试上下文
	"strings" // 子串检查
	"testing" // Go 测试框架

	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakeLLM 是测试用的伪 LLM，实现 LLMClient 接口。
type fakeLLM struct {
	resp string // 固定返回的文本
	err  error  // 固定返回的错误
}

// Generate 实现 LLMClient，忽略输入 prompt，直接返回预设的 resp/err。
func (f *fakeLLM) Generate(_ context.Context, _ string) (string, error) {
	return f.resp, f.err
}

// TestPool_FilterByDomain 验证按领域筛选能命中 code 相关 Skill，且不会混入无关领域。
func TestPool_FilterByDomain(t *testing.T) {
	p := BuiltinPool() // 使用内置 Skill 池
	got := p.FilterByDomain("code")
	// 至少应命中一个 code 领域 Skill。
	if len(got) == 0 {
		t.Fatalf("expected at least one code-domain skill, got 0")
	}
	// 每个命中项的 domain 必须包含 "code" 或是通配 "*"，否则视为泄漏。
	for _, s := range got {
		if !strings.Contains(s.Domain, "code") && !strings.Contains(s.Domain, "*") {
			t.Errorf("skill %s leaked into code domain (domain=%s)", s.SkillID, s.Domain)
		}
	}
}

// TestPool_AssembleSet_NoLLMReturnsAllOrTrim 验证无 LLM 时，AssembleSet 按 maxKeep 截断返回。
func TestPool_AssembleSet_NoLLMReturnsAllOrTrim(t *testing.T) {
	p := NewPoolFromSkills([]*types.Skill{
		{SkillID: "a", Description: "desc a", Domain: "x"},
		{SkillID: "b", Description: "desc b", Domain: "x"},
		{SkillID: "c", Description: "desc c", Domain: "x"},
		{SkillID: "d", Description: "desc d", Domain: "x"},
	})
	// maxKeep=3，无 LLM，应返回 3 个 Skill。
	set := p.AssembleSet(context.Background(), nil, "agentX", "x", "goal", 3)
	if len(set.Skills) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(set.Skills))
	}
}

// TestPool_AssembleSet_WithLLM 验证有 LLM 时，AssembleSet 尊重 LLM 返回的 ID 列表。
func TestPool_AssembleSet_WithLLM(t *testing.T) {
	p := NewPoolFromSkills([]*types.Skill{
		{SkillID: "alpha", Description: "alpha", Domain: "x"},
		{SkillID: "beta", Description: "beta", Domain: "x"},
		{SkillID: "gamma", Description: "gamma", Domain: "x"},
		{SkillID: "delta", Description: "delta", Domain: "x"},
	})
	llm := &fakeLLM{resp: `["alpha","gamma"]`}
	set := p.AssembleSet(context.Background(), llm, "agentX", "x", "goal", 2)
	if len(set.Skills) != 2 {
		t.Fatalf("expected 2 picks, got %d", len(set.Skills))
	}
	ids := []string{set.Skills[0].SkillID, set.Skills[1].SkillID}
	if !contains(ids, "alpha") || !contains(ids, "gamma") {
		t.Fatalf("LLM picks not honored: %v", ids)
	}
}

// TestSelectOne 验证 SelectOne 在 LLM 输出 skill_id 时命中，输出 NONE 时返回空串。
func TestSelectOne(t *testing.T) {
	set := &types.SkillSet{
		Skills: []*types.Skill{
			{SkillID: "read_file", Description: "读取文件"},
			{SkillID: "run_command", Description: "执行命令"},
		},
	}
	llm := &fakeLLM{resp: "run_command"}
	if got := SelectOne(context.Background(), llm, set, "执行 ls"); got != "run_command" {
		t.Errorf("expected run_command, got %q", got)
	}
	llm.resp = "NONE"
	if got := SelectOne(context.Background(), llm, set, "做不了的事情"); got != "" {
		t.Errorf("expected empty when NONE, got %q", got)
	}
}

// TestRegistry_BindAndAdd 验证 Registry 的绑定与动态追加 Skill 能力。
func TestRegistry_BindAndAdd(t *testing.T) {
	pool := BuiltinPool()     // 内置池提供 read_file / write_file 等 Skill
	reg := NewRegistry(pool)  // 用内置池创建注册表
	reg.Bind(&types.SkillSet{ // 绑定一个仅含 read_file 的 SkillSet
		OwnerAgent: "a1",
		Skills:     []*types.Skill{pool.Get("read_file")},
	})
	// 绑定后应能按 Agent ID 取回，且 Skills 数量为 1。
	if got := reg.GetForAgent("a1"); got == nil || len(got.Skills) != 1 {
		t.Fatalf("bind failed: %#v", got)
	}
	// 追加 write_file 应成功。
	if !reg.AddSkillToAgent("a1", "write_file") {
		t.Fatalf("expected AddSkillToAgent to succeed")
	}
	// 追加后 Skills 数量应为 2。
	if got := reg.GetForAgent("a1"); len(got.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(got.Skills))
	}
}

// contains 判断字符串切片 ss 中是否包含元素 s。
func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
