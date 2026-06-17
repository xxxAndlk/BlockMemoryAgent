package memory

import (
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// Monitor 监控与自愈组件
type Monitor struct {
	switchHistory []SwitchRecord
	maxHistory    int
	threshold     int // 单位时间内的最大切换次数阈值
	window        time.Duration
}

// SwitchRecord Agent 切换记录
type SwitchRecord struct {
	FromAgent string
	ToAgent   string
	Timestamp time.Time
	Reason    string
}

// NewMonitor 创建监控器
func NewMonitor() *Monitor {
	return &Monitor{
		switchHistory: make([]SwitchRecord, 0),
		maxHistory:    100,
		threshold:     10,              // 5 分钟内最多 10 次切换
		window:        5 * time.Minute, // 检测窗口
	}
}

// RecordSwitch 记录一次 Agent 切换
func (m *Monitor) RecordSwitch(fromAgent, toAgent, reason string) {
	record := SwitchRecord{
		FromAgent: fromAgent,
		ToAgent:   toAgent,
		Timestamp: time.Now(),
		Reason:    reason,
	}
	m.switchHistory = append(m.switchHistory, record)

	// 保持历史记录在限制范围内
	if len(m.switchHistory) > m.maxHistory {
		m.switchHistory = m.switchHistory[len(m.switchHistory)-m.maxHistory:]
	}
}

// CheckSwitchFrequency 检查切换频率是否异常
func (m *Monitor) CheckSwitchFrequency() (bool, float64) {
	cutoff := time.Now().Add(-m.window)
	count := 0
	for _, r := range m.switchHistory {
		if r.Timestamp.After(cutoff) {
			count++
		}
	}

	if count > m.threshold {
		return true, float64(count) / float64(m.threshold)
	}
	return false, float64(count) / float64(m.threshold)
}

// DetectEntanglement 检测震荡纠缠
// 震荡纠缠: 两个 Agent 在短周期内来回切换超过阈值次数
func (m *Monitor) DetectEntanglement() *EntanglementReport {
	if len(m.switchHistory) < 6 {
		return nil
	}

	// 检查最近 N 次切换中是否存在 A->B->A->B 模式
	recent := m.switchHistory
	if len(recent) > 20 {
		recent = recent[len(recent)-20:]
	}

	// 统计 Agent 对之间的切换次数
	pairSwitches := make(map[string]int)
	for i := 1; i < len(recent); i++ {
		pair := recent[i-1].ToAgent + "->" + recent[i].ToAgent
		pairSwitches[pair]++
	}

	// 检测高频往返对
	for pair, count := range pairSwitches {
		if count >= 4 { // 同一对 Agent 之间往返 4 次以上
			return &EntanglementReport{
				Pair:        pair,
				SwitchCount: count,
				Severity:    count / 4,
				Timestamp:   time.Now(),
			}
		}
	}

	return nil
}

// EntanglementReport 震荡纠缠报告
type EntanglementReport struct {
	Pair        string
	SwitchCount int
	Severity    int // 1-5, 严重程度
	Timestamp   time.Time
}

// String 报告字符串表示
func (r *EntanglementReport) String() string {
	return fmt.Sprintf("Entanglement detected: %s (switches: %d, severity: %d)",
		r.Pair, r.SwitchCount, r.Severity)
}

// AutoArbitrator 自动仲裁器
type AutoArbitrator struct {
	monitor     *Monitor
	broadcaster interface{ Broadcast(topicID string, ev types.UIEvent) }
}

// NewAutoArbitrator 创建自动仲裁器
func NewAutoArbitrator(monitor *Monitor) *AutoArbitrator {
	return &AutoArbitrator{monitor: monitor}
}

// CheckAndArbitrate 检查并执行仲裁
func (a *AutoArbitrator) CheckAndArbitrate(topicID string) (*types.Event, bool) {
	// 1. 检查切换频率
	overThreshold, ratio := a.monitor.CheckSwitchFrequency()
	if overThreshold {
		return a.createEscalationEvent(topicID, fmt.Sprintf("switch frequency exceeded threshold (ratio: %.2f)", ratio)), true
	}

	// 2. 检测震荡纠缠
	report := a.monitor.DetectEntanglement()
	if report != nil {
		return a.createEscalationEvent(topicID, report.String()), true
	}

	return nil, false
}

// createEscalationEvent 创建升级事件
func (a *AutoArbitrator) createEscalationEvent(topicID, reason string) *types.Event {
	return &types.Event{
		ID:          fmt.Sprintf("esc_%d", time.Now().UnixNano()),
		Type:        types.EventEscalation,
		SourceAgent: "monitor",
		Payload:     map[string]any{"reason": reason, "topic_id": topicID},
		Priority:    10,
		CreatedAt:   time.Now(),
		Status:      types.EventPending,
	}
}

// Stats 监控统计
type Stats struct {
	TotalSwitches      int
	Entanglements      int
	Escalations        int
	AvgSwitchInterval  time.Duration
	LastCheck          time.Time
}

// GetStats 获取监控统计
func (m *Monitor) GetStats() Stats {
	stats := Stats{
		TotalSwitches: len(m.switchHistory),
		LastCheck:     time.Now(),
	}

	if len(m.switchHistory) >= 2 {
		var totalInterval time.Duration
		for i := 1; i < len(m.switchHistory); i++ {
			totalInterval += m.switchHistory[i].Timestamp.Sub(m.switchHistory[i-1].Timestamp)
		}
		stats.AvgSwitchInterval = totalInterval / time.Duration(len(m.switchHistory)-1)
	}

	return stats
}

// Reset 重置监控状态
func (m *Monitor) Reset() {
	m.switchHistory = make([]SwitchRecord, 0)
}
