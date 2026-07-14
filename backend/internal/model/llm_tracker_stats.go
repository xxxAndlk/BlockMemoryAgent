package model

import (
	"fmt"
	"strings"
	"time"
)

// Stats 获取 LLM 调用追踪器的超时与耗时统计（兼容原接口）。
//
// 参数：无（接收者为 *LLMCallTracker）。
//
// 返回：
//   - callCount: 累计调用次数
//   - timeoutCount: 连续超时次数
//   - avgDur: 平均耗时
//   - maxDur: 最大耗时
//
// 并发安全：读锁保护。
func (t *LLMCallTracker) Stats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	// 加读锁，防止并发修改导致统计值撕裂
	t.mu.RLock()
	// 函数退出时释放读锁，避免死锁
	defer t.mu.RUnlock()

	// 默认平均耗时为 0，避免除零 panic
	avg := time.Duration(0)
	// 当至少有一次调用时，才计算平均值
	if t.callCount > 0 {
		// 总耗时除以调用次数得到平均耗时
		avg = t.totalDur / time.Duration(t.callCount)
	}
	// 返回四个统计值
	return t.callCount, t.timeoutCount, avg, t.maxDur
}

// StatsString 返回统计摘要的可读字符串（兼容原接口）。
//
// 返回：单行可读字符串，包含调用数/超时数/平均耗时/最大耗时/慢速模式/token 总量。
// 副作用：调用 Stats 与 TokenTotals（均会自行加锁）。
// 并发安全：间接通过子方法加锁。
func (t *LLMCallTracker) StatsString() string {
	// 取基础统计：调用次数、超时次数、平均耗时、最大耗时
	calls, timeouts, avg, max := t.Stats()
	// 取 token 累计值
	totalIn, totalOut := t.TokenTotals()
	// 按固定格式拼接成单行字符串，耗时精确到毫秒
	return fmt.Sprintf(
		"calls=%d, timeouts=%d, avg=%v, max=%v, skip=%v, inputTokens=%d, outputTokens=%d",
		calls, timeouts, avg.Round(time.Millisecond), max.Round(time.Millisecond), t.slowMode, totalIn, totalOut,
	)
}

// TokenTotals 获取所有记录的总输入/输出 token 数。
//
// 返回：
//   - inputTokens: 所有记录的输入 token 之和
//   - outputTokens: 所有记录的输出 token 之和
//
// 并发安全：读锁保护。
func (t *LLMCallTracker) TokenTotals() (inputTokens, outputTokens int) {
	// 加读锁保护 records 遍历
	t.mu.RLock()
	// 退出时释放读锁
	defer t.mu.RUnlock()
	// 遍历全部调用记录，累加输入与输出 token
	for _, r := range t.records {
		inputTokens += r.InputTokens
		outputTokens += r.OutputTokens
	}
	// 使用命名返回值隐式返回累加结果
	return
}

// Records 获取所有调用记录的副本。
//
// 设计意图：返回副本，避免外部通过修改切片元素来污染内部状态。
// 返回：[]CallRecord 拷贝。
// 并发安全：读锁保护。
func (t *LLMCallTracker) Records() []CallRecord {
	// 加读锁保护内部 records 切片
	t.mu.RLock()
	// 退出时释放读锁
	defer t.mu.RUnlock()
	// 创建与内部切片等长的新切片
	out := make([]CallRecord, len(t.records))
	// 将内部数据深拷贝到新切片
	copy(out, t.records)
	// 返回副本，外部修改不影响内部状态
	return out
}

// TokenTotalsByAgent 按 caller 子串过滤后累加 input/output token。
//
// 设计意图：watchdog 需要获取"当前 block 内各 Agent"的真实 token 总量，
// 而非全局 TokenTotals。CallRecord.Caller 形如 "DomainAgent[xxx]" / "助手[临时助手]"，
// 调用方传入 agentName 子串（如 "临时助手" 或 block 内全部 agent 名）做匹配。
// 传入空 caller 子串时退化为全量 TokenTotals。
//
// 参数：
//   - callerSub: 调用方标识子串，空字符串表示不过滤。
//
// 返回：(inputTokens, outputTokens) 累加值。
// 并发安全：读锁保护。
//
// ⚠ 子串匹配风险：若一个 agent 名是另一个的子串（如 "agent_2" 是 "agent_20" 的子串），
// 传 "agent_2" 会同时命中两者，导致重复计数。调用方应传足够具体的名（如完整
// "DomainAgent[xxx]" / "助手[临时助手]"），或改用精确匹配（解析 Caller 字段后 == 比较）。
// 当前调用方 meta_watchdog.go 传 block.Agents 元素（实例 ID）与 Caller 格式（agentName）
// 不一致，实际命中数为 0；此处保留子串匹配以兼容其他调用方，待后续统一格式。
func (t *LLMCallTracker) TokenTotalsByAgent(callerSub string) (inputTokens, outputTokens int) {
	// 加读锁保护 records
	t.mu.RLock()
	// 退出时释放读锁
	defer t.mu.RUnlock()
	// 若过滤子串为空，则不过滤，直接全量累加
	if callerSub == "" {
		// 遍历所有记录累加 token
		for _, r := range t.records {
			inputTokens += r.InputTokens
			outputTokens += r.OutputTokens
		}
		// 返回全量累加值
		return
	}
	// 否则按子串过滤后累加
	for _, r := range t.records {
		// 判断 Caller 字段是否包含指定子串
		if strings.Contains(r.Caller, callerSub) {
			// 命中则累加该记录的 token
			inputTokens += r.InputTokens
			outputTokens += r.OutputTokens
		}
	}
	// 返回过滤后的累加值
	return
}
