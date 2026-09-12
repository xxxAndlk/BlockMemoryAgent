package agent

// message_log.go 编排页对话视图的消息热层写入：ReActAgent 每条入史消息经
// MessageLogger 热写 Redis（best-effort，2s 超时，失败仅记日志——绝不影响
// ReAct 主循环）。读取侧见 query_react.go agentMessagesQueryResult。

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/blockmemory/agent/backend/internal/store"
)

// MessageLogger 是 Agent 消息热写接口。实现必须 best-effort：错误内部消化。
type MessageLogger interface {
	Log(agentID string, seq int, msg ReactMessage)
	// Clear 清空某 Agent 的热层（复活重跑前调用）。新 run 的 history 从 0 重新编号，
	// 与旧 run 的 seq 重叠会让增量游标（after_seq）永久失效、面板出现重复 seq 条目；
	// PG 侧 agent_messages 本就是终态全量覆盖语义，清理后两源一致。
	Clear(agentID string)
}

// redisMessageLogger 把消息序列化后写入 store.AgentMsgRedisStore。
type redisMessageLogger struct {
	msgs *store.AgentMsgRedisStore
}

// NewMessageLogger 创建 Redis 热层消息记录器；msgs 为 nil 时返回 nil（调用方判空跳过）。
func NewMessageLogger(msgs *store.AgentMsgRedisStore) MessageLogger {
	if msgs == nil {
		return nil
	}
	return &redisMessageLogger{msgs: msgs}
}

func (l *redisMessageLogger) Log(agentID string, seq int, msg ReactMessage) {
	if l == nil || agentID == "" {
		return
	}
	calls := ""
	if len(msg.ToolCalls) > 0 {
		if b, err := json.Marshal(msg.ToolCalls); err == nil {
			calls = string(b)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := l.msgs.AppendMsg(ctx, agentID, store.AgentMsgEntry{
		Seq:        seq,
		At:         time.Now().Format(time.RFC3339Nano),
		Role:       msg.Role,
		Content:    sanitizeUTF8(msg.Content),
		ToolCallID: msg.ToolCallID,
		ToolCalls:  calls,
		Reasoning:  sanitizeUTF8(msg.ReasoningContent),
	})
	if err != nil {
		log.Printf("[agent] msg hot-log failed: agent=%s seq=%d err=%v", agentID, seq, err)
	}
}

func (l *redisMessageLogger) Clear(agentID string) {
	if l == nil || agentID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.msgs.ClearMsg(ctx, agentID); err != nil {
		log.Printf("[agent] msg hot-clear failed: agent=%s err=%v", agentID, err)
	}
}
