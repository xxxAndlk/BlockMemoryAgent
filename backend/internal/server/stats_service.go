package server

import (
	"time"

	"github.com/blockmemory/agent/backend/internal/server/eventkind"
)

// StatsService 封装会话级统计聚合逻辑（时间线、活动流等），
// 使 APIHandler 不再直接遍历 SessionManager 内部事件。
type StatsService struct {
	sessionMgr *SessionManager
}

// NewStatsService 从 SessionManager 构造统计服务。
func NewStatsService(mgr *SessionManager) *StatsService {
	return &StatsService{sessionMgr: mgr}
}

// Timeline 返回最近 points 个小时的 calls / tokens 聚合。
func (s *StatsService) Timeline(points int) []map[string]any {
	data := make([]map[string]any, points)
	now := time.Now()
	for i := 0; i < points; i++ {
		data[i] = map[string]any{
			"time":   now.Add(-time.Duration(points-1-i) * time.Hour).Format("15:00"),
			"calls":  0,
			"tokens": 0,
		}
	}

	if s.sessionMgr == nil {
		return data
	}
	for _, sess := range s.sessionMgr.ListSessions() {
		for _, ev := range sess.Events {
			if ev.Kind != eventkind.TokenUsage {
				continue
			}
			hourIdx := points - 1 - int(now.Sub(ev.Timestamp).Hours())
			if hourIdx < 0 || hourIdx >= points {
				continue
			}
			data[hourIdx]["calls"] = data[hourIdx]["calls"].(int) + 1
			data[hourIdx]["tokens"] = data[hourIdx]["tokens"].(int) + ev.InputTokens + ev.OutputTokens
		}
	}
	return data
}

// Activity 返回最近 limit 条会话活动流，按事件倒序。
func (s *StatsService) Activity(limit int) []map[string]any {
	var activities []map[string]any
	if s.sessionMgr == nil {
		return activities
	}

	for _, sess := range s.sessionMgr.ListSessions() {
		for i := len(sess.Events) - 1; i >= 0 && len(activities) < limit; i-- {
			ev := sess.Events[i]
			if ev.Kind == "" {
				continue
			}
			activities = append(activities, map[string]any{
				"session_id": sess.ID,
				"agent":      ev.Agent,
				"kind":       ev.Kind,
				"content":    ev.Message,
				"time":       ev.Timestamp.Format("15:04:05"),
			})
		}
	}
	return activities
}
