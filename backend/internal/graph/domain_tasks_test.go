package graph

import (
	"strings"
	"testing"
)

func TestAnalyzeTasksByRules_AvoidsThinkOnlyTasks(t *testing.T) {
	n := &DomainAgentNode{}

	cases := []struct {
		goal string
		want []string
	}{
		{
			goal: "修复登录接口返回 500 的问题",
			want: []string{"ReadFile", "WriteFile", "RunCommand"},
		},
		{
			goal: "实现一个用户注册功能",
			want: []string{"ReadFile", "WriteFile", "RunCommand"},
		},
		{
			goal: "优化查询性能",
			want: []string{"ReadFile", "WriteFile", "RunCommand"},
		},
	}

	for _, c := range cases {
		tasks := n.analyzeTasksByRules(c.goal)
		if len(tasks) == 0 {
			t.Fatalf("goal=%q: expected non-empty tasks", c.goal)
		}
		joined := strings.Join(tasks, " ")
		for _, w := range c.want {
			if !strings.Contains(joined, w) {
				t.Errorf("goal=%q: expected tasks to contain %q, got %v", c.goal, w, tasks)
			}
		}
		for _, task := range tasks {
			for _, forbidden := range []string{"分析根因", "设计方案", "需求分析", "测试验证"} {
				if strings.TrimSpace(task) == forbidden {
					t.Errorf("goal=%q: rule fallback generated think-only task %q", c.goal, task)
				}
			}
		}
	}
}

func TestParseTaskListFromResp_AllowsExecutableLines(t *testing.T) {
	resp := "1. 用 pytest 运行测试验证模块正确性\n2. 用 ReadFile 查看配置文件\n3. 需求分析"
	tasks := parseTaskListFromResp(resp)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 executable tasks, got %d: %v", len(tasks), tasks)
	}
	if !strings.Contains(tasks[0], "pytest") {
		t.Errorf("expected pytest task to survive filtering, got %q", tasks[0])
	}
}
