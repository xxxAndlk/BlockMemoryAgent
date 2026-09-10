// Package server 任务 140 澄清帧测试：批量 PendingClarify 的 HTTP 线型映射
// 与 SSE awaiting_clarify 帧的 detail/questions 字段。
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// batchClarifySession 构造批量挂起中的 agent.Session：detail 长上下文 + 2 题
//（题1 单选带选项、题2 纯文本），顶层镜像第一题。
func batchClarifySession(id string) *agent.Session {
	now := time.Now()
	return &agent.Session{
		ID:     id,
		Goal:   "搭建服务",
		Status: string(enums.SessionStatusAwaitingClarify),
		PendingClarify: &agent.ClarifyRequest{
			ID:       "ask-batch-1",
			Kind:     "choice",
			Question: "数据库选哪个？",
			Detail:   "进度盘点：模块 A 已完成，模块 B 需要定案方向。",
			Options: []agent.ClarifyOption{{ID: "pg", Label: "PostgreSQL"}},
			Questions: []agent.ClarifyQuestionItem{
				{Question: "数据库选哪个？", Kind: "choice", Options: []agent.ClarifyOption{
					{ID: "pg", Label: "PostgreSQL"},
					{ID: "mysql", Label: "MySQL"},
				}},
				{Question: "配置格式用哪种？"},
			},
			CreatedAt: now,
		},
		StartedAt: now,
	}
}

// TestToServerSession_ClarifyBatchMapping 批量 PendingClarify → HTTP 线型逐字段
// 深拷贝：detail/questions 透传、选项逐项复制、改源对象不影响已转换副本。
func TestToServerSession_ClarifyBatchMapping(t *testing.T) {
	src := batchClarifySession("s1")
	got := ToServerSession(src)
	if got == nil || got.State == nil || got.State.PendingClarify == nil {
		t.Fatal("ToServerSession 应映射 PendingClarify")
	}
	pc := got.State.PendingClarify
	if pc.Detail != "进度盘点：模块 A 已完成，模块 B 需要定案方向。" {
		t.Fatalf("Detail 应透传, got %q", pc.Detail)
	}
	if len(pc.Questions) != 2 {
		t.Fatalf("Questions 应 2 题, got %d", len(pc.Questions))
	}
	if len(pc.Questions[0].Options) != 2 || pc.Questions[0].Options[1].Label != "MySQL" {
		t.Fatalf("题目选项应逐项复制, got %+v", pc.Questions[0].Options)
	}
	if pc.Question != "数据库选哪个？" || len(pc.Options) != 1 {
		t.Fatalf("顶层应镜像第一题, question=%q options=%d", pc.Question, len(pc.Options))
	}

	// 深拷贝：改源（追加题目/选项）不得影响已转换副本。
	src.PendingClarify.Questions = append(src.PendingClarify.Questions, agent.ClarifyQuestionItem{Question: "新增题"})
	src.PendingClarify.Questions[0].Options[0].Label = "被改掉"
	if len(pc.Questions) != 2 || pc.Questions[0].Options[0].Label != "PostgreSQL" {
		t.Fatal("ToServerSession 应深拷贝 PendingClarify（源改动不得泄漏）")
	}

	// 单题形态回归：questions 长度 1 时映射不变（前端按 questions<=1 走单题卡）。
	src2 := batchClarifySession("s2")
	src2.PendingClarify.Questions = src2.PendingClarify.Questions[:1]
	pc2 := ToServerSession(src2).State.PendingClarify
	if len(pc2.Questions) != 1 {
		t.Fatalf("单题应映射 1 题, got %d", len(pc2.Questions))
	}
}

// TestHandleSessionStream_AwaitingClarifyFrame SSE 流在 awaiting_clarify 轮询周期
// 推送的帧必须携带 detail 与 questions 全量题目（旧客户端顶层 question/options 继续可用）。
func TestHandleSessionStream_AwaitingClarifyFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	facade := newMockAgentForServer()
	sess := batchClarifySession("session-1")
	facade.sessions["session-1"] = sess

	mgr := NewSessionManager(facade)
	r := gin.New()
	r.GET("/api/sessions/:id/stream", mgr.HandleSessionStream)
	ts := httptest.NewServer(r)
	defer ts.Close()

	// 可取消的请求上下文：拿到 awaiting_clarify 帧后主动断开（会话恒挂起，流不自然结束）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/sessions/session-1/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()

	var clarifyFrame map[string]any
	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame) != nil {
			continue
		}
		if frame["type"] == "awaiting_clarify" {
			clarifyFrame = frame
			break
		}
	}
	cancel()

	if clarifyFrame == nil {
		t.Fatal("3s 内未收到 awaiting_clarify 帧")
	}
	if clarifyFrame["detail"] != "进度盘点：模块 A 已完成，模块 B 需要定案方向。" {
		t.Fatalf("帧应携带 detail 长上下文, got %v", clarifyFrame["detail"])
	}
	if clarifyFrame["question"] != "数据库选哪个？" {
		t.Fatalf("顶层 question 应镜像第一题, got %v", clarifyFrame["question"])
	}
	qs, ok := clarifyFrame["questions"].([]any)
	if !ok || len(qs) != 2 {
		t.Fatalf("帧应携带 questions 全量 2 题, got %v", clarifyFrame["questions"])
	}
	q1, _ := qs[0].(map[string]any)
	if q1["question"] != "数据库选哪个？" {
		t.Fatalf("题1 question 应就位, got %v", q1["question"])
	}
	opts, _ := q1["options"].([]any)
	if len(opts) != 2 {
		t.Fatalf("题1 应携带 2 个选项, got %v", q1["options"])
	}
	if _, has := clarifyFrame["question_id"]; !has {
		t.Fatal("帧应携带 question_id（旧客户端提交路由依赖）")
	}
}
