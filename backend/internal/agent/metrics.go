package agent

import (
	"sync"
	"time"
)

// metricsCollector aggregates LLM call statistics.
type metricsCollector struct {
	mu           sync.Mutex
	callCount    int
	timeoutCount int
	totalDur     time.Duration
	maxDur       time.Duration
}

// metricsSnapshot is a read-only snapshot of LLM metrics.
type metricsSnapshot struct {
	CallCount     int
	TimeoutCount  int
	AvgDurationMs int
	MaxDurationMs int
}

func newMetricsCollector() *metricsCollector {
	return &metricsCollector{}
}

func (m *metricsCollector) record(agent, model string, in, out, latency int) {
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

func (m *metricsCollector) snapshot() metricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	avg := 0
	if m.callCount > 0 {
		avg = int(m.totalDur.Milliseconds() / int64(m.callCount))
	}
	return metricsSnapshot{
		CallCount:     m.callCount,
		TimeoutCount:  m.timeoutCount,
		AvgDurationMs: avg,
		MaxDurationMs: int(m.maxDur.Milliseconds()),
	}
}
