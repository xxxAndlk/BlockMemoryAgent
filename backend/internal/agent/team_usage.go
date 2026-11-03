package agent

// team_usage.go 会话级团队 token 用量与预算（B-04/B-10）。
// 用量来自 LiveEventTokenUsage（顶层与全部子 Agent 均经 handleLiveEvent 归集），
// Dispatcher 在派发新子 Agent 前查询预算闸门；0 表示不设预算。

import "sync"

// teamTokenUsage 单会话累计用量（并发安全）。
type teamTokenUsage struct {
	mu  sync.Mutex
	in  int64
	out int64
}

// SetTeamTokenBudget 设置会话累计 token 预算；<=0 表示关闭（默认）。
func (s *ReactService) SetTeamTokenBudget(n int64) {
	if s == nil {
		return
	}
	s.teamTokenBudget = n
}

// TeamTokenBudget 返回当前预算（0=关闭）。
func (s *ReactService) TeamTokenBudget() int64 {
	if s == nil {
		return 0
	}
	return s.teamTokenBudget
}

// recordSessionTokens 累加一次 LLM 调用的 token 用量。
func (s *ReactService) recordSessionTokens(sessionID string, in, out int64) {
	if s == nil || sessionID == "" || (in == 0 && out == 0) {
		return
	}
	v, ok := s.teamUsage.Load(sessionID)
	if !ok {
		v, _ = s.teamUsage.LoadOrStore(sessionID, &teamTokenUsage{})
	}
	u := v.(*teamTokenUsage)
	u.mu.Lock()
	u.in += in
	u.out += out
	u.mu.Unlock()
}

// TeamTokenUsage 返回会话累计 (input, output) token。
func (s *ReactService) TeamTokenUsage(sessionID string) (in, out int64) {
	if s == nil || sessionID == "" {
		return 0, 0
	}
	v, ok := s.teamUsage.Load(sessionID)
	if !ok {
		return 0, 0
	}
	u := v.(*teamTokenUsage)
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.in, u.out
}

// TeamBudgetExceeded 报告会话累计 token 是否已达预算上限（0=永不超限）。
func (s *ReactService) TeamBudgetExceeded(sessionID string) bool {
	if s == nil || s.teamTokenBudget <= 0 {
		return false
	}
	in, out := s.TeamTokenUsage(sessionID)
	return in+out >= s.teamTokenBudget
}

// ResetTeamTokenUsage 清空会话累计用量（会话硬删除路径调用，防内存滞留）。
func (s *ReactService) ResetTeamTokenUsage(sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.teamUsage.Delete(sessionID)
}
