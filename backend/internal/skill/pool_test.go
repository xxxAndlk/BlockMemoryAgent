package skill

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestPool_RegisterGetAll 验证注册/查询/全量/摘除的基本语义。
func TestPool_RegisterGetAll(t *testing.T) {
	p := NewPool()
	p.Register(nil)                             // nil 静默忽略
	p.Register(&types.Skill{SkillID: "a"})      // 正常注册
	p.Register(&types.Skill{SkillID: ""})       // 缺 ID 静默忽略
	if p.Get("a") == nil {
		t.Fatalf("expected skill a to be registered")
	}
	if p.Get("missing") != nil {
		t.Fatalf("expected nil for unknown id")
	}
	if got := len(p.All()); got != 1 {
		t.Fatalf("expected 1 skill in pool, got %d", got)
	}
	p.Register(&types.Skill{SkillID: "a", Description: "v2"}) // 同 ID 覆盖
	if got := p.Get("a").Description; got != "v2" {
		t.Fatalf("expected overwrite, got %q", got)
	}
	p.Remove("a")
	if got := len(p.All()); got != 0 {
		t.Fatalf("expected empty pool after Remove, got %d", got)
	}
}

// TestPool_FindByNameOrID 验证先按 SkillID 再按 Name 的匹配顺序。
func TestPool_FindByNameOrID(t *testing.T) {
	p := NewPoolFromSkills([]*types.Skill{
		{SkillID: "pdf_extract", Name: "pdf-extract", Description: "d"},
	})
	if got := p.FindByNameOrID("pdf_extract"); got == nil {
		t.Fatalf("expected match by SkillID")
	}
	if got := p.FindByNameOrID("pdf-extract"); got == nil {
		t.Fatalf("expected match by Name")
	}
	if got := p.FindByNameOrID("nope"); got != nil {
		t.Fatalf("expected nil for unknown key")
	}
}

// TestPool_Names 验证 Names 按字典序去重输出，缺 Name 回退 SkillID。
func TestPool_Names(t *testing.T) {
	p := NewPoolFromSkills([]*types.Skill{
		{SkillID: "b_id", Name: "b", Description: "d"},
		{SkillID: "a_id", Name: "a", Description: "d"},
		{SkillID: "c_id", Description: "d"}, // 缺 Name → 回退 SkillID
		{SkillID: "dup_id", Name: "a", Description: "d"},
	})
	got := p.Names()
	want := "a,b,c_id"
	if strings.Join(got, ",") != want {
		t.Fatalf("expected %q, got %q", want, strings.Join(got, ","))
	}
}

// TestPool_SourceTags 验证 yaml/builtin 加载路径都会写入 Source 标记。
func TestPool_SourceTags(t *testing.T) {
	p := BuiltinPool()
	if got := p.Get("read_file").Source; got != "builtin" {
		t.Fatalf("expected builtin source, got %q", got)
	}
}

// TestPool_NamesExceptSource 验证按 Source 排除（meta 提示词剥离 learned 用）：
// 排除项不入列表、缺 Name 回退 SkillID、字典序稳定、Source 为空视为非排除项。
func TestPool_NamesExceptSource(t *testing.T) {
	p := NewPoolFromSkills([]*types.Skill{
		{SkillID: "a", Name: "alpha", Source: "yaml"},
		{SkillID: "b", Name: "beta", Source: "learned"},
		{SkillID: "c", Name: "gamma", Source: "learned"},
		{SkillID: "d", Source: "builtin"}, // 缺 Name 回退 SkillID
		{SkillID: "e", Name: "eps", Source: "learned"},
	})
	got := p.NamesExceptSource("learned")
	want := []string{"alpha", "d"}
	if len(got) != len(want) {
		t.Fatalf("NamesExceptSource = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NamesExceptSource = %v, want %v（须字典序）", got, want)
		}
	}
	// 全排除时返回空集而非 nil 崩。
	if got := p.NamesExceptSource("yaml"); len(got) != 4 {
		t.Fatalf("expected 4 non-yaml names, got %v", got)
	}
}
