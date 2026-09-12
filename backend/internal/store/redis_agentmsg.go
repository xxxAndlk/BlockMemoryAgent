package store

// redis_agentmsg.go Agent 消息热层（编排页对话视图数据源）：
// 运行期每条入史消息逐条 RPUSH，TTL 24h、cap 最近 500 条；PG agent_messages
// 在子 Agent 终态/暂停全量落库（权威），Redis 承担运行期热读与崩溃缓冲。
// 读侧（agent.agentMessagesQueryResult）热层 miss/不足时回退 PG 切片。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// agentMsgHotTTL 热层 key 过期时间（每次 Append 重置）。
	agentMsgHotTTL = 24 * time.Hour
	// agentMsgHotCap 热层单 Agent 容量上限（LTRIM 保留最新 N 条）。
	agentMsgHotCap = 500
)

// AgentMsgEntry 是热层中的单条 Agent 消息（字段与 agent_messages 表对齐 + at 时间戳）。
type AgentMsgEntry struct {
	Seq        int    `json:"seq"`
	At         string `json:"at"` // RFC3339Nano
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolCalls  string `json:"tool_calls,omitempty"` // JSON 字符串（[]ToolCall），空省略
	Reasoning  string `json:"reasoning,omitempty"`
}

// AgentMsgRedisStore 负责消息热层读写。client 为 nil 时全部方法 no-op（降级 PG 直读）。
type AgentMsgRedisStore struct {
	client *redis.Client
}

// agentMsgKey 生成热层 key：sess:{sessionID}:agent:{agentID}:msgs（sessionID 取 "/" 前段）。
func agentMsgKey(agentID string) string {
	return fmt.Sprintf("sess:%s:agent:%s:msgs", sessionFromAgentID(agentID), agentID)
}

// sessionFromAgentID 从 agentID 派生 sessionID（MetaAgent==sessionID，子 Agent 取 "/" 前段）。
func sessionFromAgentID(agentID string) string {
	for i, r := range agentID {
		if r == '/' {
			return agentID[:i]
		}
	}
	return agentID
}

// AppendMsg 追加一条消息（RPUSH + 重置 TTL + LTRIM 截断，同一 pipeline 三次往返合一）。
func (s *AgentMsgRedisStore) AppendMsg(ctx context.Context, agentID string, e AgentMsgEntry) error {
	if s == nil || s.client == nil || agentID == "" {
		return nil
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	key := agentMsgKey(agentID)
	pipe := s.client.Pipeline()
	pipe.RPush(ctx, key, data)
	pipe.Expire(ctx, key, agentMsgHotTTL)
	pipe.LTrim(ctx, key, -agentMsgHotCap, -1)
	_, err = pipe.Exec(ctx)
	return err
}

// ClearMsg 清空某 Agent 的热层（复活重跑前调用：新 run 的 seq 从 0 重编号，
// 与旧 run 重叠会让对话页增量游标永久失效）。
func (s *AgentMsgRedisStore) ClearMsg(ctx context.Context, agentID string) error {
	if s == nil || s.client == nil || agentID == "" {
		return nil
	}
	return s.client.Del(ctx, agentMsgKey(agentID)).Err()
}

// TailMsg 取热层尾部 limit 条（seq 升序）。
func (s *AgentMsgRedisStore) TailMsg(ctx context.Context, agentID string, limit int) ([]AgentMsgEntry, error) {
	if s == nil || s.client == nil || agentID == "" || limit <= 0 {
		return nil, nil
	}
	raw, err := s.client.LRange(ctx, agentMsgKey(agentID), int64(-limit), -1).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentMsgs(raw), nil
}

// BeforeMsg 取 seq < beforeSeq 的最近 limit 条（升序）；beforeSeq<=0 等价 TailMsg。
// 实现为读全量热层（cap 500）后内存切片——热层有界，切片成本可忽略。
func (s *AgentMsgRedisStore) BeforeMsg(ctx context.Context, agentID string, beforeSeq, limit int) ([]AgentMsgEntry, error) {
	if s == nil || s.client == nil || agentID == "" || limit <= 0 {
		return nil, nil
	}
	if beforeSeq <= 0 {
		return s.TailMsg(ctx, agentID, limit)
	}
	all, err := s.TailMsg(ctx, agentID, agentMsgHotCap)
	if err != nil {
		return nil, err
	}
	return beforeWindow(all, beforeSeq, limit), nil
}

// AfterMsg 取 seq > afterSeq 的最早 limit 条（升序，对话页 3s 增量轮询用）。
func (s *AgentMsgRedisStore) AfterMsg(ctx context.Context, agentID string, afterSeq, limit int) ([]AgentMsgEntry, error) {
	if s == nil || s.client == nil || agentID == "" || limit <= 0 {
		return nil, nil
	}
	all, err := s.TailMsg(ctx, agentID, agentMsgHotCap)
	if err != nil {
		return nil, err
	}
	return afterWindow(all, afterSeq, limit), nil
}

// beforeWindow 在升序切片中取 seq < beforeSeq 的最近 limit 条（保持升序返回）。
func beforeWindow(all []AgentMsgEntry, beforeSeq, limit int) []AgentMsgEntry {
	out := make([]AgentMsgEntry, 0, limit)
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		if all[i].Seq < beforeSeq {
			out = append(out, all[i])
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// afterWindow 在升序切片中取 seq > afterSeq 的最早 limit 条。
func afterWindow(all []AgentMsgEntry, afterSeq, limit int) []AgentMsgEntry {
	out := make([]AgentMsgEntry, 0, limit)
	for _, e := range all {
		if e.Seq > afterSeq {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func decodeAgentMsgs(raw []string) []AgentMsgEntry {
	out := make([]AgentMsgEntry, 0, len(raw))
	for _, r := range raw {
		var e AgentMsgEntry
		if json.Unmarshal([]byte(r), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}
