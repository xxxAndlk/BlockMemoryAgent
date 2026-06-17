// Package watchdog 实现 v3 §4.4 设计的"上下文长度看门狗"。
//
// 主 Agent 在每一轮调度循环中调用 Watchdog.Check 评估
// 当前活跃 Agent 的上下文规模；当超过软阈值时记录警告，
// 超过硬阈值时强制触发压缩或切换信号。
package watchdog

import (
	"fmt"
	"sync"
	"time"
)

// Estimator token 估算函数（4 字符 ≈ 1 token 的粗略经验值）
//
// 注意：v3 文档显式提示当前估算"不准确"，但相对值能够稳定区分
// "正常 / 接近阈值 / 已超阈值"，已足够 Watchdog 决策使用。
func Estimator(text string) int {
	if text == "" {
		return 0
	}
	// 中文每个 rune 约占 1.5 token，英文按 4 char ≈ 1 token
	bytes := len(text)
	return bytes/4 + 1
}

// Level Watchdog 触发级别
type Level int

const (
	LevelOK       Level = iota // 上下文余量充足
	LevelWarn                   // 接近软阈值，记录警告
	LevelCompress               // 超过软阈值，建议压缩
	LevelEvict                  // 超过硬阈值，必须切换 / 销毁
)

// String 用于日志
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

// Decision 一次检查的输出
type Decision struct {
	AgentID    string    `json:"agent_id"`
	Tokens     int       `json:"tokens"`
	Level      Level     `json:"level"`
	Reason     string    `json:"reason"`
	Suggested  string    `json:"suggested"` // 建议动作: compress / switch / evict / none
	OccurredAt time.Time `json:"occurred_at"`
}

// Config 阈值配置
type Config struct {
	SoftLimit int // 软阈值（建议压缩）
	HardLimit int // 硬阈值（必须驱逐）
}

// DefaultConfig 默认阈值。
//
// 原 v3 §4.4 举例 3000 token 在实际使用中过低——一次 ReadFile 输出
// （如 CLAUDE.md ≈ 3700 token）就会触发硬驱逐，导致会话被强制结束、
// 写文件等关键动作来不及执行。提高到 soft=12000 / hard=20000，
// 兼容一般 LLM 上下文窗口（≥128k）下的多轮工具调用。
func DefaultConfig() Config {
	return Config{
		SoftLimit: 12000,
		HardLimit: 20000,
	}
}

// Watchdog 主逻辑
type Watchdog struct {
	cfg     Config
	mu      sync.RWMutex
	decisions []Decision
	max     int // 历史决策最大保留数
}

// New 创建 Watchdog
func New(cfg Config) *Watchdog {
	if cfg.SoftLimit <= 0 {
		cfg.SoftLimit = DefaultConfig().SoftLimit
	}
	if cfg.HardLimit <= cfg.SoftLimit {
		cfg.HardLimit = cfg.SoftLimit + 600
	}
	return &Watchdog{cfg: cfg, max: 200}
}

// Check 评估某个 Agent 的当前上下文规模并返回决策
func (w *Watchdog) Check(agentID string, contextText string) Decision {
	tokens := Estimator(contextText)
	d := Decision{
		AgentID:    agentID,
		Tokens:     tokens,
		OccurredAt: time.Now(),
	}
	switch {
	case tokens >= w.cfg.HardLimit:
		d.Level = LevelEvict
		d.Reason = fmt.Sprintf("tokens=%d ≥ hard=%d", tokens, w.cfg.HardLimit)
		d.Suggested = "evict"
	case tokens >= w.cfg.SoftLimit:
		d.Level = LevelCompress
		d.Reason = fmt.Sprintf("tokens=%d ≥ soft=%d", tokens, w.cfg.SoftLimit)
		d.Suggested = "compress"
	case tokens >= int(float64(w.cfg.SoftLimit)*0.8):
		d.Level = LevelWarn
		d.Reason = fmt.Sprintf("tokens=%d, approaching soft=%d", tokens, w.cfg.SoftLimit)
		d.Suggested = "monitor"
	default:
		d.Level = LevelOK
		d.Suggested = "none"
	}

	w.mu.Lock()
	w.decisions = append(w.decisions, d)
	if len(w.decisions) > w.max {
		w.decisions = w.decisions[len(w.decisions)-w.max:]
	}
	w.mu.Unlock()
	return d
}

// History 历史决策（用于面板/调试）
func (w *Watchdog) History() []Decision {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]Decision, len(w.decisions))
	copy(out, w.decisions)
	return out
}

// LastNonOK 返回最后一次非 OK 决策（无则返回 nil）
func (w *Watchdog) LastNonOK() *Decision {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for i := len(w.decisions) - 1; i >= 0; i-- {
		if w.decisions[i].Level != LevelOK {
			d := w.decisions[i]
			return &d
		}
	}
	return nil
}
