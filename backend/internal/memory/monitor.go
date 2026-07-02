package memory

import (
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// Monitor 监控与自愈组件：跟踪 Agent 间的切换记录，检测切换频率异常与震荡纠缠。
// 当检测到异常时，由 AutoArbitrator 上抛 EventEscalation 交由 MetaAgent 仲裁。
// 并发安全：本结构未加锁，调用方需自行串行调用 RecordSwitch / Detect* 等方法。
type Monitor struct {
	switchHistory []SwitchRecord // 切换历史记录，按时间顺序追加
	maxHistory    int            // 历史记录上限，超出时按 FIFO 裁剪
	threshold     int            // 单位时间内的最大切换次数阈值
	window        time.Duration  // 检测窗口长度，用于频率统计
}

// SwitchRecord Agent 切换记录：一次 Agent 间控制权转移的结构化留痕。
type SwitchRecord struct {
	FromAgent string    // 源 Agent ID
	ToAgent   string    // 目标 Agent ID
	Timestamp time.Time // 切换发生时间
	Reason    string    // 切换原因描述
}

// NewMonitor 创建监控器，使用默认配置：历史上限 100、阈值 10 次/5 分钟。
// 参数：无。
// 返回：初始化好的 *Monitor。
// 副作用：无。
func NewMonitor() *Monitor {
	// 预分配空切片，避免后续 append 触发 nil 检查。
	return &Monitor{
		switchHistory: make([]SwitchRecord, 0),
		maxHistory:    100,             // 最多保留 100 条历史，控制内存占用
		threshold:     10,              // 5 分钟内最多 10 次切换
		window:        5 * time.Minute, // 检测窗口
	}
}

// RecordSwitch 记录一次 Agent 切换。
// 参数：fromAgent 源 Agent；toAgent 目标 Agent；reason 切换原因。
// 返回：无。
// 副作用：追加记录到 switchHistory；超过上限时按 FIFO 裁剪。
// 注意：本方法未加锁，调用方需保证串行调用。
func (m *Monitor) RecordSwitch(fromAgent, toAgent, reason string) {
	// 组装一条切换记录，时间戳取当前时刻。
	record := SwitchRecord{
		FromAgent: fromAgent,
		ToAgent:   toAgent,
		Timestamp: time.Now(),
		Reason:    reason,
	}
	// 追加到历史尾部，保持时间顺序。
	m.switchHistory = append(m.switchHistory, record)

	// 保持历史记录在限制范围内：超出上限时丢弃最旧记录。
	if len(m.switchHistory) > m.maxHistory {
		// 切片保留最近 maxHistory 条，等价于 FIFO 出队。
		m.switchHistory = m.switchHistory[len(m.switchHistory)-m.maxHistory:]
	}
}

// CheckSwitchFrequency 检查切换频率是否异常。
// 在最近 window 时长内统计切换次数，与 threshold 比较。
//
// 参数：无。
// 返回：(是否超阈值, 当前次数/阈值的比值)。比值 >1 表示已超出。
// 副作用：无。
func (m *Monitor) CheckSwitchFrequency() (bool, float64) {
	// 计算窗口起点：当前时间减去窗口长度。
	cutoff := time.Now().Add(-m.window)
	// 统计窗口内的切换次数。
	count := 0
	for _, r := range m.switchHistory {
		// 仅统计在窗口内的记录。
		if r.Timestamp.After(cutoff) {
			count++
		}
	}

	// 超过阈值时返回 true 与比值，便于仲裁器决策。
	if count > m.threshold {
		return true, float64(count) / float64(m.threshold)
	}
	// 未超阈值时返回 false 与当前比值（用于趋势监控）。
	return false, float64(count) / float64(m.threshold)
}

// DetectEntanglement 检测震荡纠缠。
// 震荡纠缠定义：两个 Agent 在短周期内来回切换超过阈值次数（默认 4 次）。
// 当历史不足 6 条时不做检测，避免误报。
//
// 参数：无。
// 返回：检测到纠缠时返回 *EntanglementReport，否则返回 nil。
// 副作用：无。
func (m *Monitor) DetectEntanglement() *EntanglementReport {
	// 历史过少时不做判断，避免噪声。
	if len(m.switchHistory) < 6 {
		return nil
	}

	// 检查最近 N 次切换中是否存在 A->B->A->B 模式
	// 取最近 20 条记录进行分析，平衡敏感度与噪声。
	recent := m.switchHistory
	if len(recent) > 20 {
		recent = recent[len(recent)-20:]
	}

	// 统计 Agent 对之间的切换次数（基于 ToAgent 序列）。
	pairSwitches := make(map[string]int)
	for i := 1; i < len(recent); i++ {
		// 用相邻两次 ToAgent 拼接作为 pair 键，刻画往返模式。
		pair := recent[i-1].ToAgent + "->" + recent[i].ToAgent
		pairSwitches[pair]++
	}

	// 检测高频往返对：同一对 Agent 之间往返 4 次以上视为纠缠。
	for pair, count := range pairSwitches {
		if count >= 4 {
			// 命中纠缠，返回报告，严重度按往返倍数估算。
			return &EntanglementReport{
				Pair:        pair,
				SwitchCount: count,
				Severity:    count / 4, // 1 次往返=1 级，线性递增
				Timestamp:   time.Now(),
			}
		}
	}

	// 未检测到纠缠，返回 nil。
	return nil
}

// EntanglementReport 震荡纠缠报告：描述一次检测到的 Agent 间震荡。
type EntanglementReport struct {
	Pair        string    // 发生震荡的 Agent 对标识（A->B 形式）
	SwitchCount int       // 该对的切换次数
	Severity    int       // 1-5，严重程度（当前按 count/4 线性计算）
	Timestamp   time.Time // 报告生成时间
}

// String 报告字符串表示：用于日志与升级事件 payload。
// 参数：无。返回：可读字符串。副作用：无。
func (r *EntanglementReport) String() string {
	// 格式化输出关键字段，便于人工排查与事件溯源。
	return fmt.Sprintf("Entanglement detected: %s (switches: %d, severity: %d)",
		r.Pair, r.SwitchCount, r.Severity)
}

// AutoArbitrator 自动仲裁器：基于 Monitor 检测结果生成升级事件。
// 当切换频率或震荡纠缠触发阈值时，构造 EventEscalation 交由 MetaAgent 处理。
type AutoArbitrator struct {
	monitor     *Monitor // 关联的监控器
	broadcaster interface {
		Broadcast(topicID string, ev types.UIEvent)
	} // 可选广播器接口（保留扩展）
}

// NewAutoArbitrator 创建自动仲裁器。
// 参数：monitor 关联的监控器实例。
// 返回：组装好的 *AutoArbitrator（broadcaster 默认为 nil）。
// 副作用：无。
func NewAutoArbitrator(monitor *Monitor) *AutoArbitrator {
	// 仅注入 monitor，broadcaster 留空，后续可扩展注入。
	return &AutoArbitrator{monitor: monitor}
}

// CheckAndArbitrate 检查并执行仲裁：依次检测频率与纠缠，命中即生成升级事件。
// 参数：topicID 当前话题 ID，用于事件 payload。
// 返回：(升级事件指针, 是否触发仲裁)。未触发时事件为 nil。
// 副作用：无（仅读取 monitor 状态并构造事件对象）。
func (a *AutoArbitrator) CheckAndArbitrate(topicID string) (*types.Event, bool) {
	// 1. 检查切换频率：超阈值时生成升级事件。
	overThreshold, ratio := a.monitor.CheckSwitchFrequency()
	if overThreshold {
		// 频率越界，构造升级事件并返回。
		return a.createEscalationEvent(topicID, fmt.Sprintf("switch frequency exceeded threshold (ratio: %.2f)", ratio)), true
	}

	// 2. 检测震荡纠缠：命中时生成升级事件。
	report := a.monitor.DetectEntanglement()
	if report != nil {
		// 纠缠报告转字符串作为升级原因。
		return a.createEscalationEvent(topicID, report.String()), true
	}

	// 未触发任何异常，返回 nil 与 false。
	return nil, false
}

// createEscalationEvent 创建升级事件：构造 EventEscalation 类型的事件对象。
// 参数：topicID 话题 ID；reason 升级原因描述。
// 返回：填充好的 *types.Event，状态为 EventPending，优先级 10。
// 副作用：无。
func (a *AutoArbitrator) createEscalationEvent(topicID, reason string) *types.Event {
	// 组装升级事件，ID 使用纳秒时间戳保证唯一性。
	return &types.Event{
		ID:          fmt.Sprintf("esc_%d", time.Now().UnixNano()),          // 唯一 ID
		Type:        enums.EventEscalation,                                 // 升级事件类型
		SourceAgent: "monitor",                                             // 来源标记为监控器
		Payload:     map[string]any{"reason": reason, "topic_id": topicID}, // 携带原因与话题
		Priority:    10,                                                    // 高优先级，需 MetaAgent 优先处理
		CreatedAt:   time.Now(),                                            // 创建时间
		Status:      enums.EventPending,                                    // 初始状态为待处理
	}
}

// Stats 监控统计：聚合监控器运行的关键指标，供外部观测与诊断使用。
type Stats struct {
	TotalSwitches     int           // 累计切换次数
	Entanglements     int           // 累计检测到的纠缠次数（保留字段，当前未累加）
	Escalations       int           // 累计升级次数（保留字段，当前未累加）
	AvgSwitchInterval time.Duration // 平均切换间隔
	LastCheck         time.Time     // 最近一次统计时间
}

// GetStats 获取监控统计：基于当前 switchHistory 计算切换总数与平均间隔。
// 参数：无。
// 返回：填充好的 Stats（部分字段为保留位，当前未累加）。
// 副作用：无。
func (m *Monitor) GetStats() Stats {
	// 初始化统计对象，TotalSwitches 取历史长度。
	stats := Stats{
		TotalSwitches: len(m.switchHistory),
		LastCheck:     time.Now(),
	}

	// 历史不少于 2 条时计算平均切换间隔。
	if len(m.switchHistory) >= 2 {
		var totalInterval time.Duration
		// 累加相邻两次切换的时间差。
		for i := 1; i < len(m.switchHistory); i++ {
			totalInterval += m.switchHistory[i].Timestamp.Sub(m.switchHistory[i-1].Timestamp)
		}
		// 总间隔除以间隔数得到平均值。
		stats.AvgSwitchInterval = totalInterval / time.Duration(len(m.switchHistory)-1)
	}

	// 返回统计快照。
	return stats
}

// Reset 重置监控状态：清空所有切换历史。
// 参数：无。返回：无。
// 副作用：清空 switchHistory，后续检测从零开始。
func (m *Monitor) Reset() {
	// 重新分配空切片，丢弃全部历史。
	m.switchHistory = make([]SwitchRecord, 0)
}
