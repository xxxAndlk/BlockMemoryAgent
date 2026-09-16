package agent

// meta_skill_block_test.go 覆盖 meta 技能目录收敛（A 治理）：静态技能全列、
// 经验技能只列 top-N + 检索提示、无回调时零注入。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func newSkillBlockTestService(t *testing.T) *ReactService {
	t.Helper()
	return newDedupTestService(t, nil)
}

// TestMetaSkillBlock_ConvergedLearned 经验技能只列 top-N，超出部分给计数与检索提示；
// 静态技能（yaml/builtin）不受收敛影响。
func TestMetaSkillBlock_ConvergedLearned(t *testing.T) {
	s := newSkillBlockTestService(t)
	pool := skill.NewPoolFromSkills([]*types.Skill{
		{SkillID: "read_file", Name: "读文件", Description: "读本地文件", Source: "builtin"},
		{SkillID: "learned-a", Name: "经验技能甲", Description: "场景甲", Source: "learned"},
		{SkillID: "learned-b", Name: "经验技能乙", Description: "场景乙", Source: "learned"},
	})
	s.SetSkillCatalog(pool)
	s.SetSkillCatalogSource(func() ([]SkillRecallHint, int) {
		return []SkillRecallHint{{Name: "learned-a", Title: "经验技能甲"}}, 7
	})

	block := s.metaSkillBlock()
	if !strings.Contains(block, "读文件") {
		t.Fatalf("静态技能应全列：\n%s", block)
	}
	if !strings.Contains(block, "learned-a: 经验技能甲") {
		t.Fatalf("经验技能 top-N 应列出：\n%s", block)
	}
	if strings.Contains(block, "learned-b") {
		t.Fatalf("经验技能不应全量进提示：\n%s", block)
	}
	if !strings.Contains(block, "共 7 个") || !strings.Contains(block, "list_skills(query=") {
		t.Fatalf("超出 top-N 应有计数与检索提示：\n%s", block)
	}
}

// TestMetaSkillBlock_NoCatalog 未接目录回调时经验技能零注入，静态技能照常。
func TestMetaSkillBlock_NoCatalog(t *testing.T) {
	s := newSkillBlockTestService(t)
	pool := skill.NewPoolFromSkills([]*types.Skill{
		{SkillID: "read_file", Name: "读文件", Description: "读本地文件", Source: "builtin"},
		{SkillID: "learned-a", Name: "经验技能甲", Description: "场景甲", Source: "learned"},
	})
	s.SetSkillCatalog(pool)

	block := s.metaSkillBlock()
	if strings.Contains(block, "经验技能") {
		t.Fatalf("无目录回调不应列经验技能：\n%s", block)
	}
	if !strings.Contains(block, "读文件") {
		t.Fatalf("静态技能应保留：\n%s", block)
	}
}
