// Package watchdog 实现 v3 §4.4 设计的"上下文长度看门狗"。
//
// 设计意图：主 Agent 在每一轮调度循环中调用 Watchdog.Check，
// 评估当前活跃 Agent 的上下文规模。依据软/硬阈值划分四级决策
// （OK / Warn / Compress / Evict）：超过软阈值时仅记录警告并建议压缩，
// 超过硬阈值时返回 Evict 信号由上层强制切换或销毁该 Agent。
//
// 并发安全：Watchdog 内部用 sync.RWMutex 保护决策历史，
// Check / History / LastNonOK 均可被多 goroutine 并发调用。
package watchdog

import (
	"fmt"  // 格式化 Decision.Reason 字符串
	"sync" // RWMutex 保护 decisions 切片
	"time" // 记录 Decision.OccurredAt 时间戳
)

// Estimator 根据文本长度粗略估算 token 数（4 字符 ≈ 1 token）。
//
// 职责：用 "4 字符 ≈ 1 token" 的经验值把字符串长度映射为 token 估计值，
// 供 Watchdog.Check 判断上下文规模。
//
// 参数：
//   - text：待评估的上下文文本（通常为系统提示 + 历史 + 当前输入拼接）。
//
// 返回：估算的 token 数；空串返回 0。
//
// 副作用：无。
//
// 并发安全：纯函数，可并发调用。
//
// 注意：v3 文档显式提示当前估算"不准确"，但相对值能够稳定区分
// "正常 / 接近阈值 / 已超阈值"，已足够 Watchdog 决策使用。
func Estimator(text string) int {
	// 空文本直接返回 0，避免后续无意义计算
	if text == "" {
		return 0
	}
	// 取字节数（非 rune 数）；中文每 rune 约 3 字节，折算后约 1.5 token/rune，
	// 英文按 4 char ≈ 1 token，整体误差在 Watchdog 决策可接受范围内
	bytes := len(text)
	// +1 保证非空文本至少记 1 token，避免整数除法下溢为 0
	return bytes/4 + 1
}

// Level 表示 Watchdog 一次检查所判定的触发级别（OK/Warn/Compress/Evict）。
type Level int

const (
	LevelOK       Level = iota // 上下文余量充足，无需任何动作
	LevelWarn                  // 已接近软阈值（≥80% soft），记录警告并建议监控
	LevelCompress              // 已超过软阈值，建议触发上下文压缩
	LevelEvict                 // 已超过硬阈值，必须切换或销毁该 Agent
)

// String 返回 Level 的可读字符串，用于日志和调试输出。
//
// 返回：OK / WARN / COMPRESS / EVICT，未知值返回 UNKNOWN。
//
// 并发安全：纯函数。
func (l Level) String() string {
	switch l {
	case LevelOK:
		return "OK"
	case LevelWarn:
		return "WARN"
	case LevelCompress:
		return "COMPRESS"
	case LevelEvict:
		return "EVICT"
	default:
		return "UNKNOWN"
	}
}

// Decision 是 Watchdog.Check 一次检查的完整输出，会被序列化用于日志和面板。
type Decision struct {
	AgentID    string    `json:"agent_id"`    // 被检查的 Agent 标识
	Tokens     int       `json:"tokens"`      // 本次估算的 token 数
	Level      Level     `json:"level"`       // 触发级别 OK/Warn/Compress/Evict
	Reason     string    `json:"reason"`      // 人读原因说明（含数值比较）
	Suggested  string    `json:"suggested"`   // 建议动作: compress / monitor / evict / none
	OccurredAt time.Time `json:"occurred_at"` // 决策发生时间
}

// Config 是 Watchdog 的软/硬阈值配置。
type Config struct {
	SoftLimit int // 软阈值：达到后建议压缩（LevelCompress）
	HardLimit int // 硬阈值：达到后必须驱逐（LevelEvict）
}

// DefaultConfig 返回默认阈值配置。
//
// 设计意图：原 v3 §4.4 举例 3000 token 在实际使用中过低——一次 ReadFile 输出
// （如 CLAUDE.md ≈ 3700 token）就会触发硬驱逐，导致会话被强制结束、
// 写文件等关键动作来不及执行。默认基于 32k 上下文窗口设置 soft=16000 / hard=25600，
// 兼容一般 LLM 上下文窗口（≥128k）下的多轮工具调用。
//
// 返回：soft=16000、hard=25600 的 Config。
func DefaultConfig() Config {
	return ConfigForWindow(32000)
}

// ConfigForWindow 根据给定的上下文窗口总 token 数推导软/硬阈值。
// 比例：soft = 50% contextWindow，hard = 80% contextWindow，
// 保证硬阈值始终比软阈值大至少 600 token 的安全余量。
func ConfigForWindow(contextWindow int) Config {
	if contextWindow <= 0 {
		contextWindow = 32000
	}
	soft := contextWindow * 50 / 100
	hard := contextWindow * 80 / 100
	if hard <= soft+600 {
		hard = soft + 600
	}
	return Config{SoftLimit: soft, HardLimit: hard}
}

// Watchdog 是上下文长度看门狗的核心结构，持有阈值配置与决策历史。
//
// 并发安全：所有方法均通过 mu 保护 decisions，可被多 goroutine 并发调用。
type Watchdog struct {
	cfg       Config       // 软/硬阈值配置
	mu        sync.RWMutex // 保护 decisions 切片的读写锁
	decisions []Decision   // 历史决策缓冲（最新追加到尾部，超出 max 丢弃最旧）
	max       int          // 历史决策最大保留数，超出则丢弃最旧
}

// New 创建一个 Watchdog 实例。
//
// 职责：根据传入 cfg 构造 Watchdog，并对非法阈值做兜底修正。
//
// 参数：
//   - cfg：阈值配置；SoftLimit<=0 时回退到默认值，HardLimit<=SoftLimit 时
//     自动设为 SoftLimit+600，保证硬阈值严格大于软阈值。
//
// 返回：初始化好的 *Watchdog，max 固定为 200。
//
// 副作用：可能修改 cfg 的局部副本（按值传入，不影响调用方）。
func New(cfg Config) *Watchdog {
	cfg = normalizeConfig(cfg)
	// max=200 限制历史决策条数，防止长会话内存无限增长
	return &Watchdog{cfg: cfg, max: 200}
}

// SetConfig 运行时更新 Watchdog 阈值。
// 用于 Runtime.SetAgentConfig 注入用户配置的 context_window 后同步调整软/硬阈值。
// 并发安全：持写锁更新 cfg。
func (w *Watchdog) SetConfig(cfg Config) {
	cfg = normalizeConfig(cfg)
	w.mu.Lock()
	w.cfg = cfg
	w.mu.Unlock()
}

// normalizeConfig 对阈值做兜底修正。
func normalizeConfig(cfg Config) Config {
	if cfg.SoftLimit <= 0 {
		cfg.SoftLimit = DefaultConfig().SoftLimit
	}
	if cfg.HardLimit <= cfg.SoftLimit {
		cfg.HardLimit = cfg.SoftLimit + 600
	}
	return cfg
}

// Check 评估指定 Agent 的当前上下文规模并返回决策。
//
// 职责：估算 token 数，按 soft/hard 阈值划分 OK/Warn/Compress/Evict 四级，
// 将决策追加到历史并返回。
//
// 参数：
//   - agentID：被检查 Agent 的标识，仅写入 Decision.AgentID，不做查表。
//   - contextText：该 Agent 当前上下文的完整文本（提示+历史+输入）。
//
// 返回：填好的 Decision（含级别、原因、建议动作）。
//
// 副作用：追加一条决策到内部历史，超出 max 时丢弃最旧条目。
//
// 并发安全：写 decisions 时持写锁，可并发调用。
func (w *Watchdog) Check(agentID string, contextText string) Decision {
	// 估算当前上下文 token 数
	tokens := Estimator(contextText)
	// 组装决策结构体，时间戳取当前时刻
	d := Decision{
		AgentID:    agentID,
		Tokens:     tokens,
		OccurredAt: time.Now(),
	}
	// 按从严到宽的顺序判断：先硬阈值、再软阈值、再 80% 软阈值警告
	switch {
	case tokens >= w.cfg.HardLimit:
		// 超过硬阈值：必须驱逐/切换，由上层 graph 据此终止该 Agent
		d.Level = LevelEvict
		d.Reason = fmt.Sprintf("tokens=%d ≥ hard=%d", tokens, w.cfg.HardLimit)
		d.Suggested = "evict"
	case tokens >= w.cfg.SoftLimit:
		// 超过软阈值但未达硬阈值：建议压缩上下文
		d.Level = LevelCompress
		d.Reason = fmt.Sprintf("tokens=%d ≥ soft=%d", tokens, w.cfg.SoftLimit)
		d.Suggested = "compress"
	case tokens >= int(float64(w.cfg.SoftLimit)*0.8):
		// 达到软阈值的 80%：进入预警区，建议持续监控
		d.Level = LevelWarn
		d.Reason = fmt.Sprintf("tokens=%d, approaching soft=%d", tokens, w.cfg.SoftLimit)
		d.Suggested = "monitor"
	default:
		// 余量充足：无需动作
		d.Level = LevelOK
		d.Suggested = "none"
	}

	// 写入历史前加写锁，避免与并发 History/LastNonOK 冲突
	w.mu.Lock()
	w.decisions = append(w.decisions, d)
	// 超过上限时切片丢弃最旧条目，保留最近 max 条
	if len(w.decisions) > w.max {
		w.decisions = w.decisions[len(w.decisions)-w.max:]
	}
	w.mu.Unlock()
	return d
}

// History 返回决策历史的完整副本，用于面板展示或调试。
//
// 返回：新分配的 []Decision，调用方可任意修改而不影响内部状态。
//
// 并发安全：持读锁拷贝，可并发调用。
func (w *Watchdog) History() []Decision {
	w.mu.RLock()
	defer w.mu.RUnlock()
	// 预分配等长切片并 copy，确保返回的是独立副本
	out := make([]Decision, len(w.decisions))
	copy(out, w.decisions)
	return out
}

// LastNonOK 返回最近一次非 OK 级别的决策，用于快速查询是否出现过告警。
//
// 返回：指向内部决策副本的指针；若历史全为 OK 或为空则返回 nil。
//
// 并发安全：持读锁遍历，可并发调用。
func (w *Watchdog) LastNonOK() *Decision {
	w.mu.RLock()
	defer w.mu.RUnlock()
	// 从尾部向前遍历，命中第一条非 OK 即返回（即时间最近的一条告警）
	for i := len(w.decisions) - 1; i >= 0; i-- {
		if w.decisions[i].Level != LevelOK {
			// 取值拷贝，避免返回内部切片元素的指针
			d := w.decisions[i]
			return &d
		}
	}
	return nil
}
