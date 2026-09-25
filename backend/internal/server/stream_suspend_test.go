package server

import (
	"bufio"      // SSE 帧逐行读取
	"context"    // 可取消的流式请求
	"encoding/json" // 帧 JSON 解码
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// startSSE 打开一个 SSE 测试流，返回帧通道与取消函数。
// 帧通道在连接结束（服务端关流/客户端取消）时关闭。
func startSSE(t *testing.T, session *agent.Session) (<-chan map[string]any, context.CancelFunc) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	facade := newMockAgentForServer()
	facade.sessions[session.ID] = session

	mgr := NewSessionManager(facade)
	r := gin.New()
	r.GET("/api/sessions/:id/stream", mgr.HandleSessionStream)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/sessions/"+session.ID+"/stream", nil)
	if err != nil {
		cancel()
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("stream request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	frames := make(chan map[string]any, 64)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var frame map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame) != nil {
				continue
			}
			frames <- frame
		}
	}()
	return frames, cancel
}

func suspendedSession(id string, st enums.SessionStatus) *agent.Session {
	return &agent.Session{
		ID:        id,
		Goal:      "联网搜索",
		Status:    string(st),
		StartedAt: time.Now(),
	}
}

// TestHandleSessionStream_AwaitingChildNotTerminal 挂起等子（awaiting_child）不是终态：
// SSE 必须继续推流，绝不能推 done 关连接——否则前端收 done 即关 EventSource、停面板定时器
// 且不再重连，对话栏从挂起那刻起永久收不到子完成唤醒后的最终答复（2026-09-25 用户实证：
// 头部徽标冻在「挂起等待子」、正文停在子完成卡，侧栏列表另路刷新却显示「完成」）。
func TestHandleSessionStream_AwaitingChildNotTerminal(t *testing.T) {
	frames, cancel := startSSE(t, suspendedSession("session-ac", enums.SessionStatusAwaitingChild))
	defer cancel()

	// 3 个轮询周期（150ms × 3）内应只见到初始快照与状态帧，不得出现 done。
	deadline := time.After(600 * time.Millisecond)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("流在挂起等子期间被服务端关闭（收到 EOF 而非 done 帧）")
			}
			if frame["type"] == "done" {
				t.Fatalf("awaiting_child 不是终态，不得推 done: %v", frame)
			}
		case <-deadline:
			return // 未见 done 即通过
		}
	}
}

// TestHandleSessionStream_PausedOnChildNotTerminal 暂停于子（paused_on_child）同理：
// 用户发消息即可续跑，SSE 需保持连接。
func TestHandleSessionStream_PausedOnChildNotTerminal(t *testing.T) {
	frames, cancel := startSSE(t, suspendedSession("session-poc", enums.SessionStatusPausedOnChild))
	defer cancel()

	deadline := time.After(600 * time.Millisecond)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("流在 paused_on_child 期间被服务端关闭（收到 EOF 而非 done 帧）")
			}
			if frame["type"] == "done" {
				t.Fatalf("paused_on_child 不是终态，不得推 done: %v", frame)
			}
		case <-deadline:
			return
		}
	}
}

// TestHandleSessionStream_TerminalSendsDone 真终态（completed）仍要推 done 收口：
// 前端据此停面板定时器、跑对账重取与浏览器通知，收不到就会永远转圈。
func TestHandleSessionStream_TerminalSendsDone(t *testing.T) {
	frames, cancel := startSSE(t, suspendedSession("session-done", enums.SessionStatusCompleted))
	defer cancel()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("流已关闭但未收到 done 帧")
			}
			if frame["type"] == "done" {
				if frame["status"] != string(enums.SessionStatusCompleted) {
					t.Fatalf("done.status 应为 completed, got %v", frame["status"])
				}
				return
			}
		case <-deadline:
			t.Fatal("3s 内未收到终态 done 帧")
		}
	}
}
