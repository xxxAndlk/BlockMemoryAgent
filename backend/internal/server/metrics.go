package server

import (
	"sync"
	"time"
)

// MetricsCollector 聚合 LLM 调用统计（调用次数、超时次数、平均 / 最大耗时）。
// 设计意图：把原本散落在 SessionManager.LLMStats() 中的聚合逻辑抽到独立的
// 状态收集器，便于在进度回调中实时累加，也为后续按 agent/model 维度扩展留口。
type MetricsCollector struct {
	mu           sync.Mutex
	callCount    int
	timeoutCount int
	totalDur     time.Duration
	maxDur       time.Duration
}

// MetricsSnapshot 是 MetricsCollector 的只读快照，用于 /api/health 等导出点。
type MetricsSnapshot struct {
	CallCount     int
	TimeoutCount  int
	AvgDurationMs int
	MaxDurationMs int
}

// NewMetricsCollector 创建一个新的 LLM 指标收集器。
func NewMetricsCollector() *MetricsCollector {
	return &MetricsCollector{}
}

// Record 记录一次 LLM 调用的指标。
//
// 参数：
//   - agent: 调用方 Agent 名称（保留用于后续按 agent 聚合）。
//   - model: 使用的模型标识（保留用于后续按 model 聚合）。
//   - in: 输入 token 估算数（保留用于后续扩展）。
//   - out: 输出 token 估算数（保留用于后续扩展）。
//   - latency: 调用耗时（毫秒）。负值表示本次调用为超时调用，其绝对值作为耗时参与统计。
//
// 并发安全：内部使用互斥锁。
func (m *MetricsCollector) Record(agent, model string, in, out, latency int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.callCount++
	if latency < 0 {
		m.timeoutCount++
	}

	dur := time.Duration(latency) * time.Millisecond
	if latency < 0 {
		dur = time.Duration(-latency) * time.Millisecond
	}
	m.totalDur += dur
	if dur > m.maxDur {
		m.maxDur = dur
	}
}

// Snapshot 返回当前聚合指标的只读副本。
func (m *MetricsCollector) Snapshot() MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	avg := 0
	if m.callCount > 0 {
		avg = int(m.totalDur.Milliseconds() / int64(m.callCount))
	}
	return MetricsSnapshot{
		CallCount:     m.callCount,
		TimeoutCount:  m.timeoutCount,
		AvgDurationMs: avg,
		MaxDurationMs: int(m.maxDur.Milliseconds()),
	}
}
