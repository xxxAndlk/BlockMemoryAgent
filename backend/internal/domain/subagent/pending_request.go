// pending_request.go 实现 send_message 协作问答的请求-回复配对注册表
//（2026-09-28 P0：此前配对靠双方模型沿用 thread_id 自觉，ReplyTo 被误填成
// 发送方 agentID，回复永远不来也无任何告警——实测 meta 发进度询问零回复）。
//
// 机制：request/escalate 投递成功即登记（消息 ID 为键）；reply 到达时按 ReplyTo
// 显式销账，未填 ReplyTo 时按 (回复方, 被回复方, thread) 自动配对最近一条；
// patrol 每 tick 扫超期项，给提问方的父 Agent 投 escalate 告警。
package subagent

import (
	"strings" // purgeSession 的会话前缀匹配
	"sync"
	"time"
)

// pendingRequest 一条待应答的协作询问。
type pendingRequest struct {
	MsgID     string    // 请求消息 ID（配对键）
	From      string    // 提问方 agentID
	To        string    // 被问方 agentID
	ThreadID  string    // 问答链 ID（首问 ID）
	Subject   string    // 摘要（超时告警文案用）
	CreatedAt time.Time // 登记时间（自动配对取最近）
	Deadline  time.Time // 超时升级死线
}

// pendingRequestRegistry 进程内待应答注册表（按消息 ID 索引）。
// 进程重启丢失：恢复路径对未读 request 经 RegisterRestoredPending 重建（deadline 顺延）；
// 已读未答的条目随进程蒸发——重建成本（跨表配对 SQL）远高于收益，回复到达时
// 自动配对兜底不依赖注册表存在。
type pendingRequestRegistry struct {
	mu   sync.Mutex
	byID map[string]*pendingRequest
}

func newPendingRequestRegistry() *pendingRequestRegistry {
	return &pendingRequestRegistry{byID: make(map[string]*pendingRequest)}
}

// add 登记待应答请求（同 ID 覆盖：恢复重建与重投安全）。
func (r *pendingRequestRegistry) add(req *pendingRequest) {
	if req == nil || req.MsgID == "" {
		return
	}
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[req.MsgID] = req
}

// complete 按消息 ID 销账（reply 的 ReplyTo 命中）；返回被销账项，未命中/重复返回 nil。
func (r *pendingRequestRegistry) complete(msgID string) *pendingRequest {
	if msgID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	req, ok := r.byID[msgID]
	if ok {
		delete(r.byID, msgID)
		return req
	}
	return nil
}

// matchAuto 自动配对：replier（回复方=reply 发送者）答复 target（被回复方=reply 收件人）
// 时，找"From==target 且 To==replier"的未答请求，threadID 非空时要求同链；取最近一条。
// 命中即销账（一答销一问）。未命中返回 nil（reply 照常投递，只是不销账）。
func (r *pendingRequestRegistry) matchAuto(replier, target, threadID string) *pendingRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *pendingRequest
	for _, req := range r.byID {
		if req.From != target || req.To != replier {
			continue
		}
		if threadID != "" && req.ThreadID != threadID {
			continue
		}
		if best == nil || req.CreatedAt.After(best.CreatedAt) {
			best = req
		}
	}
	if best != nil {
		delete(r.byID, best.MsgID)
	}
	return best
}

// expire 弹出全部超期项并销账（调用方负责告警）；按 Deadline 升序返回。
func (r *pendingRequestRegistry) expire(now time.Time) []*pendingRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*pendingRequest
	for id, req := range r.byID {
		if now.After(req.Deadline) {
			out = append(out, req)
			delete(r.byID, id)
		}
	}
	return out
}

// purgeSession 会话硬删除时清理会话内全部条目（From/To 任一在会话内）。
func (r *pendingRequestRegistry) purgeSession(sessionID string) {
	if sessionID == "" {
		return
	}
	inSession := func(id string) bool {
		return id == sessionID || strings.HasPrefix(id, sessionID+"/")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, req := range r.byID {
		if inSession(req.From) || inSession(req.To) {
			delete(r.byID, id)
		}
	}
}
