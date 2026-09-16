package subagent

// skills_tool_test.go 覆盖 list_skills 的 query 过滤与输出条数封顶（A 目录收敛配套）：
// meta 持全池，技能库膨胀后需要能按关键词检索而不是一次性灌出全部目录。

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// newListSkillsTestTool 构造注入技能池的 list_skills 测试夹具（agent ctx 标识 meta 持全池）。
func newListSkillsTestTool(t *testing.T, skills []*types.Skill) (*listSkillsTool, context.Context) {
	t.Helper()
	d := NewDispatcher(nil, nil, nil, nil, agent.NopMemoryPipeline{})
	d.WithSkillPool(skill.NewPoolFromSkills(skills))
	ctx := agent.WithAgentID(context.Background(), "meta")
	return &listSkillsTool{dispatcher: d}, ctx
}

func TestListSkills_QueryFilterAndCap(t *testing.T) {
	var pool []*types.Skill
	for i := 0; i < 50; i++ {
		pool = append(pool, &types.Skill{
			SkillID:     "batch-skill-" + strconv.Itoa(i),
			Name:        "批量技能" + strconv.Itoa(i),
			Description: "常用工艺",
		})
	}
	pool = append(pool, &types.Skill{SkillID: "canvas-render", Name: "画布渲染", Description: "渲染 2D 画布动画"})
	tool, ctx := newListSkillsTestTool(t, pool)

	// 无 query：输出封顶（<=40 条）+ 提示存在更多。
	res := tool.Execute(ctx, map[string]any{})
	if !res.Success {
		t.Fatalf("list_skills failed: %s", res.Error)
	}
	if got := strings.Count(res.Output, "\n- "); got > listSkillsMaxOutput {
		t.Fatalf("输出超过封顶 %d 条：%d", listSkillsMaxOutput, got)
	}
	if !strings.Contains(res.Output, "缩小范围") {
		t.Fatalf("封顶时应提示用 query 缩小范围：\n%s", res.Output)
	}

	// query 命中标题：只出匹配项。
	res = tool.Execute(ctx, map[string]any{"query": "画布"})
	if !strings.Contains(res.Output, "画布渲染") || strings.Contains(res.Output, "批量技能") {
		t.Fatalf("query 过滤未生效：\n%s", res.Output)
	}

	// query 无命中：返回空提示而非空串。
	res = tool.Execute(ctx, map[string]any{"query": "不存在的技能xyz"})
	if !strings.Contains(res.Output, "没有匹配") {
		t.Fatalf("无命中应有提示：%q", res.Output)
	}
}

func TestListSkills_NoPool(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, agent.NopMemoryPipeline{})
	ctx := agent.WithAgentID(context.Background(), "meta")
	res := (&listSkillsTool{dispatcher: d}).Execute(ctx, map[string]any{})
	if !res.Success || !strings.Contains(res.Output, "未启用") {
		t.Fatalf("未注入池时应给出未启用提示：%+v", res)
	}
}
