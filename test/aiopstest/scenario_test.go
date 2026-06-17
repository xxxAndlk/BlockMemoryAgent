// Package aiopstest 模拟 doc/test/测试项目设计与接口文档.md
// 中描述的 4 个 Agent 测试场景：
//
//   场景 A：告警风暴下的自动聚合与根因定位
//   场景 B：自动诊断并请求人工审批
//   场景 C：容灾演练编排与条件回滚
//   场景 D：复盘与知识沉淀
//
// 这些测试不直接依赖 LLM（避免 CI 不稳定），而是通过项目内
// ToolExecutor 工具直接调用 Mock Server，验证 Agent "可以"
// 完成这些任务所必需的基础能力（HTTP 串联、状态轮询、参数透传、
// 异常处理）。后续可在有 LLM API 的环境下用 graph.SessionManager
// 跑端到端流程，本文件只覆盖工具链路。
package aiopstest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/internal/graph"
	"github.com/blockmemory/agent/test/aiopsmock"
)

// runTool 帮助函数
func runTool(t *testing.T, ex *graph.ToolExecutor, name string, args map[string]any) *graph.ToolResult {
	t.Helper()
	r := ex.Execute(context.Background(), name, args)
	if r.Error != "" {
		t.Logf("tool %s error: %s", name, r.Error)
	}
	return r
}

// TestScenarioA_AlertStormAndRootCause 场景 A
func TestScenarioA_AlertStormAndRootCause(t *testing.T) {
	srv := aiopsmock.NewServer()
	defer srv.Close()
	ex := graph.NewToolExecutor("")

	base := srv.URL() + "/api/v1"

	// 1. 推 8 条告警
	for i := 0; i < 8; i++ {
		r := runTool(t, ex, "HTTPPost", map[string]any{
			"url": base + "/alerts",
			"body": map[string]any{
				"source": "prometheus", "name": "high_latency",
				"severity": "critical", "status": "firing",
				"labels": map[string]any{"service": "order-service"},
			},
		})
		if !r.Success {
			t.Fatalf("alert push failed: %s", r.Output)
		}
	}

	// 2. 查询事件
	r := runTool(t, ex, "HTTPGet", map[string]any{"url": base + "/incidents?status=open"})
	if !strings.Contains(r.Output, "INC-20260616-0018") {
		t.Fatalf("incident missing in listing: %s", r.Output)
	}

	// 3. 触发根因分析
	r = runTool(t, ex, "HTTPPost", map[string]any{
		"url": base + "/incidents/INC-20260616-0018/rootcause/analyze", "body": map[string]any{},
	})
	if !r.Success {
		t.Fatalf("rca trigger failed: %s", r.Output)
	}

	// 4. 轮询 → 100ms 内收敛
	deadline := time.Now().Add(2 * time.Second)
	var done bool
	for time.Now().Before(deadline) {
		r = runTool(t, ex, "HTTPGet", map[string]any{"url": base + "/incidents/INC-20260616-0018/rootcause/result"})
		if strings.Contains(r.Output, `"completed"`) && strings.Contains(r.Output, "mysql-primary") {
			done = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !done {
		t.Fatalf("rca did not complete within deadline; last=%s", r.Output)
	}
}

// TestScenarioB_PlaybookWithApproval 场景 B
func TestScenarioB_PlaybookWithApproval(t *testing.T) {
	srv := aiopsmock.NewServer()
	defer srv.Close()
	ex := graph.NewToolExecutor("")
	base := srv.URL() + "/api/v1"

	// 1. 拉取诊断 Playbook 列表
	r := runTool(t, ex, "HTTPGet", map[string]any{"url": base + "/playbooks?scope=incident&tags=diagnosis"})
	if !strings.Contains(r.Output, "kill-slow-query") {
		t.Fatalf("playbook listing missing kill-slow-query: %s", r.Output)
	}

	// 2. 执行（要求审批）
	r = runTool(t, ex, "HTTPPost", map[string]any{
		"url": base + "/incidents/INC-20260616-0018/playbooks/kill-slow-query/execute",
		"body": map[string]any{
			"parameters":       map[string]any{"slow_query_id": 105623},
			"require_approval": true,
		},
	})
	var resp struct {
		Data struct {
			ExecID string                   `json:"execution_id"`
			Status string                   `json:"status"`
			Steps  []map[string]interface{} `json:"steps"`
		} `json:"data"`
	}
	body := r.Output[strings.Index(r.Output, "{"):]
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode exec resp: %v", err)
	}
	if resp.Data.Status != "pending_approval" {
		t.Fatalf("expected pending_approval, got %s", resp.Data.Status)
	}

	// 3. 审批 step2
	r = runTool(t, ex, "HTTPPost", map[string]any{
		"url":  base + "/executions/" + resp.Data.ExecID + "/steps/step2/approve",
		"body": map[string]any{"approved": true, "comment": "确认 KILL"},
	})
	if !strings.Contains(r.Output, `"completed"`) {
		t.Fatalf("approve did not complete exec: %s", r.Output)
	}

	// 4. 查询最终状态
	r = runTool(t, ex, "HTTPGet", map[string]any{"url": base + "/executions/" + resp.Data.ExecID})
	if !strings.Contains(r.Output, `"completed"`) {
		t.Fatalf("final state not completed: %s", r.Output)
	}
}

// TestScenarioC_ChaosDrillRollback 场景 C
func TestScenarioC_ChaosDrillRollback(t *testing.T) {
	srv := aiopsmock.NewServer()
	defer srv.Close()
	ex := graph.NewToolExecutor("")
	base := srv.URL() + "/api/v1"

	r := runTool(t, ex, "HTTPPost", map[string]any{
		"url": base + "/incidents/INC-20260616-0018/playbooks/chaos-drill-order/execute",
		"body": map[string]any{
			"parameters":       map[string]any{"target": "order-service"},
			"require_approval": false,
		},
	})
	if !r.Success {
		t.Fatalf("execute failed: %s", r.Output)
	}
	if !strings.Contains(r.Output, "rollback_traffic") || !strings.Contains(r.Output, "recover_db") {
		t.Fatalf("rollback steps not surfaced: %s", r.Output)
	}
	if !strings.Contains(r.Output, "verify_failover") || !strings.Contains(r.Output, "failed") {
		t.Fatalf("expected verify_failover failure marker: %s", r.Output)
	}
}

// TestScenarioD_PostmortemAndKnowledge 场景 D
func TestScenarioD_PostmortemAndKnowledge(t *testing.T) {
	srv := aiopsmock.NewServer()
	defer srv.Close()
	ex := graph.NewToolExecutor("")
	base := srv.URL() + "/api/v1"

	// 1. 创建复盘
	r := runTool(t, ex, "HTTPPost", map[string]any{
		"url": base + "/incidents/INC-20260616-0018/postmortem",
		"body": map[string]any{
			"timeline": []string{"09:58 告警", "10:05 升级", "10:30 KILL 慢SQL"},
			"summary":  "DB 慢查询导致订单延迟，已通过 Kill + 索引建议恢复。",
		},
	})
	if !r.Success {
		t.Fatalf("postmortem failed: %s", r.Output)
	}

	// 2. 知识库搜索
	r = runTool(t, ex, "HTTPGet", map[string]any{"url": base + "/knowledge/search?q=mysql%20%E6%85%A2%E6%9F%A5%E8%AF%A2"})
	if !strings.Contains(r.Output, "kb-001") {
		t.Fatalf("knowledge search miss: %s", r.Output)
	}
}
