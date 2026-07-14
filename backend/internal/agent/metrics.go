// Package agent 聚合 LLM 调用相关的统计指标，
// 包括调用次数、超时次数以及平均/最大耗时。
package agent

import (
	// sync 提供互斥锁，用于在并发访问时保护指标数据。
	"sync"
	// time 提供 Duration 与毫秒转换，用于统计 LLM 调用耗时。
	"time"
)

// metricsCollector 聚合 LLM 调用相关的统计指标，包括调用次数、超时次数以及耗时分布。
// 所有字段均受 mu 互斥锁保护，避免并发读写导致数据竞争。
type metricsCollector struct {
	// mu 用于保护下面所有字段的并发访问。
	mu sync.Mutex
	// callCount 记录 LLM 调用总次数（无论成功或超时都会递增）。
	callCount int
	// timeoutCount 记录被判定为超时的调用次数（latency < 0 时递增）。
	timeoutCount int
	// totalDur 累计所有调用的耗时总和，用于计算平均耗时。
	totalDur time.Duration
	// maxDur 记录截至目前遇到的最大单次调用耗时。
	maxDur time.Duration
}

// metricsSnapshot 是 metricsCollector 在某一时刻的只读快照，
// 供外部读取并以毫秒为单位展示平均耗时与最大耗时。
type metricsSnapshot struct {
	// CallCount 表示快照生成时的总调用次数。
	CallCount int
	// TimeoutCount 表示快照生成时的超时调用次数。
	TimeoutCount int
	// AvgDurationMs 表示平均调用耗时，单位为毫秒。
	AvgDurationMs int
	// MaxDurationMs 表示最大单次调用耗时，单位为毫秒。
	MaxDurationMs int
}

// newMetricsCollector 创建并返回一个空的 metricsCollector 实例。
func newMetricsCollector() *metricsCollector {
	// 返回零值结构体，各计数器与耗时默认初始化为零。
	return &metricsCollector{}
}

// record 记录一次 LLM 调用的结果与耗时。
//
// 参数说明：
//   - agent: 发起调用的 Agent 标识（目前仅作语义记录，不参与统计）。
//   - model: 使用的模型标识（目前仅作语义记录，不参与统计）。
//   - in: 输入 token 数量（目前仅作语义记录，不参与统计）。
//   - out: 输出 token 数量（目前仅作语义记录，不参与统计）。
//   - latency: 调用耗时（毫秒）；负值表示超时。
func (m *metricsCollector) record(agent, model string, in, out, latency int) {
	// 加锁，保证并发调用 record 时内部计数器操作的原子性与一致性。
	m.mu.Lock()
	// 函数返回前自动解锁，避免在发生 panic 或提前返回时遗漏解锁。
	defer m.mu.Unlock()

	// 总调用次数加一：无论成功还是超时，都算一次调用。
	m.callCount++
	// 如果 latency 为负值，说明上游将该调用标记为超时，需要单独统计超时次数。
	if latency < 0 {
		m.timeoutCount++
	}

	// 将毫秒整数转换为 time.Duration，便于后续累加和求平均值。
	dur := time.Duration(latency) * time.Millisecond
	// 对于超时场景，latency 为负数；这里取绝对值，使耗时保持非负并参与平均/最大耗时统计。
	if latency < 0 {
		dur = time.Duration(-latency) * time.Millisecond
	}
	// 将本次调用耗时累加到总耗时，用于后续计算平均值。
	m.totalDur += dur
	// 如果本次耗时刷新历史最大值，则更新 maxDur。
	if dur > m.maxDur {
		m.maxDur = dur
	}
}

// snapshot 返回当前指标数据的只读快照。
// 返回的 metricsSnapshot 中，耗时字段均以毫秒为单位。
func (m *metricsCollector) snapshot() metricsSnapshot {
	// 加锁，防止在读取过程中有其他 goroutine 修改指标字段。
	m.mu.Lock()
	// 函数返回前自动解锁。
	defer m.mu.Unlock()

	// avg 用于存储平均耗时（毫秒）；默认值为 0，避免除零错误。
	avg := 0
	// 只有在已有调用记录时才计算平均值，否则保持 0。
	if m.callCount > 0 {
		// 总耗时除以调用次数得到平均耗时；Milliseconds 返回 int64，再转回 int。
		avg = int(m.totalDur.Milliseconds() / int64(m.callCount))
	}
	// 将当前受锁保护的内部字段复制到返回值中，
	// 调用方获得快照后无需再持有锁即可安全读取。
	return metricsSnapshot{
		CallCount:     m.callCount,
		TimeoutCount:  m.timeoutCount,
		AvgDurationMs: avg,
		MaxDurationMs: int(m.maxDur.Milliseconds()),
	}
}
