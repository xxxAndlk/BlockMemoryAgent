// Package soul 实现 v3 §6.2 / §6.3 设计的人格与温度策略。
//
// soul.md 是用户级人格定义，可在 Agent 启动时加载，并支持运行时
// 热重载（通过 SoulLoader.Reload）。Temperature 调节根据任务类型
// 选择确定性（路由 / 总结 = 0）或创意（撰写 / 头脑风暴 ≈ 0.8）。
package soul

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Persona 人格内容（来自 soul.md）
type Persona struct {
	Path      string // 加载来源路径
	Content   string // 全文
}

// Loader 人格加载器（线程安全 + 可重载）
type Loader struct {
	mu      sync.RWMutex
	path    string
	current atomic.Pointer[Persona]
}

// NewLoader 创建加载器
func NewLoader(path string) *Loader {
	return &Loader{path: path}
}

// Load 立即加载（首次启动调用）
func (l *Loader) Load() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		// 文件不存在不视为致命错误，使用空人格
		l.current.Store(&Persona{Path: l.path, Content: ""})
		return err
	}
	l.current.Store(&Persona{Path: l.path, Content: string(data)})
	return nil
}

// Reload 显式重载（可由用户在线触发）
func (l *Loader) Reload() error {
	return l.Load()
}

// Current 当前人格
func (l *Loader) Current() *Persona {
	if p := l.current.Load(); p != nil {
		return p
	}
	return &Persona{Path: l.path}
}

// Name 返回人格标识（文件名或 default）
func (l *Loader) Name() string {
	if l.path == "" {
		return "default"
	}
	base := filepath.Base(l.path)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if base == "" {
		return "default"
	}
	return base
}

// Inject 把人格内容拼接到 systemPrompt 之前
//
// 调用方传入角色原始 system prompt，本函数负责把人格作为前置段落
// 注入。若人格为空则返回原 prompt 不变。
func (l *Loader) Inject(systemPrompt string) string {
	p := l.Current()
	content := strings.TrimSpace(p.Content)
	if content == "" {
		return systemPrompt
	}
	if systemPrompt == "" {
		return content
	}
	return content + "\n\n---\n\n" + systemPrompt
}

// TaskKind 调节 temperature 时使用的任务类别
type TaskKind string

const (
	KindRouting    TaskKind = "routing"     // 路由 / 选择 → 0
	KindSummarize  TaskKind = "summarize"   // 总结 / 压缩 → 0
	KindCode       TaskKind = "code"        // 代码 / 数学 → 0.1~0.2
	KindAnalysis   TaskKind = "analysis"    // 分析 / 推理 → 0.3
	KindCreative   TaskKind = "creative"    // 创意 / 文案 → 0.7~0.9
	KindGeneric    TaskKind = "generic"     // 默认 → 配置温度
)

// Temperature 返回任务类别对应的推荐温度
//
// base 是当前角色配置文件里的默认温度，作为 KindGeneric 的回退。
func Temperature(kind TaskKind, base float64) float64 {
	switch kind {
	case KindRouting, KindSummarize:
		return 0.0
	case KindCode:
		return 0.15
	case KindAnalysis:
		return 0.3
	case KindCreative:
		return 0.8
	case KindGeneric:
		fallthrough
	default:
		return base
	}
}

// InferKind 根据自然语言任务描述启发式推断 TaskKind，仅作为
// 没有显式声明时的回退；显式声明优先级更高。
func InferKind(text string) TaskKind {
	lower := strings.ToLower(text)
	switch {
	case containsAny(lower, []string{"路由", "选择 skill", "select skill", "判断", "决定"}):
		return KindRouting
	case containsAny(lower, []string{"总结", "摘要", "压缩", "summary", "summarize"}):
		return KindSummarize
	case containsAny(lower, []string{"代码", "code", "bug", "重构", "math"}):
		return KindCode
	case containsAny(lower, []string{"创意", "头脑风暴", "brainstorm", "文案", "营销"}):
		return KindCreative
	case containsAny(lower, []string{"分析", "排查", "诊断", "为什么", "why"}):
		return KindAnalysis
	}
	return KindGeneric
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
