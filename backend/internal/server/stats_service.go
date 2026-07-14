package server

import (
	"time" // 时间聚合与格式化

	"github.com/blockmemory/agent/backend/internal/server/eventkind" // 事件类型常量
)

// StatsService 封装会话级统计聚合逻辑（时间线、活动流等），
// 使 APIHandler 不再直接遍历 SessionManager 内部事件。
type StatsService struct {
	sessionMgr *SessionManager // 用于拉取会话列表与事件
}

// NewStatsService 从 SessionManager 构造统计服务。
// 参数 mgr：会话管理器。
// 返回值：*StatsService。
func NewStatsService(mgr *SessionManager) *StatsService {
	return &StatsService{sessionMgr: mgr}
}

// Timeline 返回最近 points 个小时的 calls / tokens 聚合。
// 参数 points：时间点数（小时）。
// 返回值：每个小时一个 map，含 time / calls / tokens。
func (s *StatsService) Timeline(points int) []map[string]any {
	data := make([]map[string]any, points)
	now := time.Now()
	// 初始化每个时间点，按倒序填充最近 points 个小时。
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
	// 遍历所有会话的 token_usage 事件，累加到对应小时。
	for _, sess := range s.sessionMgr.ListSessions() {
		for _, ev := range sess.Events {
			if ev.Kind != eventkind.TokenUsage {
				continue // 只统计 token_usage 事件
			}
			hourIdx := points - 1 - int(now.Sub(ev.Timestamp).Hours())
			if hourIdx < 0 || hourIdx >= points {
				continue // 超出时间范围的事件忽略
			}
			data[hourIdx]["calls"] = data[hourIdx]["calls"].(int) + 1
			data[hourIdx]["tokens"] = data[hourIdx]["tokens"].(int) + ev.InputTokens + ev.OutputTokens
		}
	}
	return data
}

// Activity 返回最近 limit 条会话活动流，按事件倒序。
// 参数 limit：最大返回条数。
// 返回值：活动流切片。
func (s *StatsService) Activity(limit int) []map[string]any {
	var activities []map[string]any
	if s.sessionMgr == nil {
		return activities
	}

	// 逐个会话遍历事件，从最新事件向前取，直到凑够 limit 条。
	for _, sess := range s.sessionMgr.ListSessions() {
		for i := len(sess.Events) - 1; i >= 0 && len(activities) < limit; i-- {
			ev := sess.Events[i]
			if ev.Kind == "" {
				continue // 无 kind 的事件不展示
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
