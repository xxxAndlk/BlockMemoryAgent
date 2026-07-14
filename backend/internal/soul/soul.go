// Package soul 实现 v3 §6.2 / §6.3 设计的人格与温度策略。
//
// soul.md 是用户级人格定义，可在 Agent 启动时加载，并支持运行时
// 热重载（通过 SoulLoader.Reload）。Temperature 调节根据任务类型
// 选择确定性（路由 / 总结 = 0）或创意（撰写 / 头脑风暴 ≈ 0.8）。
package soul

// 标准库依赖：os 读取 soul.md 文件；filepath 处理路径名提取；
// strings 做大小写归一与子串匹配；sync 提供 RWMutex；atomic 支持无锁读。
import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Persona 表示一次加载得到的人格内容（来自 soul.md）。
//
// 字段为值类型且不可变，加载完成后通过 atomic.Pointer 原子替换，
// 保证并发读无需加锁。
type Persona struct {
	Path    string // 加载来源路径（用于调试与日志追溯）
	Content string // 人格全文（原始 markdown 文本）
}

// Loader 是人格加载器，提供线程安全的读取与运行时热重载能力。
//
// 设计要点：
//   - mu 保护 path 字段（当前实现 path 仅在构造时设置，仍保留锁以备扩展）；
//   - current 用 atomic.Pointer 实现 RCU 风格的读多写少场景，避免读路径加锁；
//   - 写路径（Load/Reload）通过 Store 原子发布新 Persona，旧读者持有旧快照不受影响。
type Loader struct {
	mu          sync.RWMutex            // 保留以扩展静态字段；当前 path 不可变
	path        string                  // soul.md 文件路径，空串表示无文件
	current     atomic.Pointer[Persona] // 当前人格快照，可能为 nil（未加载）
	classifier  TaskKindClassifier      // 任务类别分类策略
	temperature TemperaturePolicy       // 温度策略
}

// NewLoader 创建加载器。
//
// 仅记录路径，不触发实际 IO；调用方需显式调用 Load 完成首次加载。
// 即便文件缺失，后续 Current() 仍会返回空 Persona，保证服务可启动。
func NewLoader(path string) *Loader {
	// 记录 soul.md 路径；current 为 nil，待 Load/Current 填充。
	// 分类与温度策略默认使用当前逻辑，保持行为不变。
	return &Loader{
		path:        path,
		classifier:  KeywordClassifier{},
		temperature: NewDefaultTemperaturePolicy(),
	}
}

// Load 立即加载 soul.md 并原子发布新 Persona。
//
// 职责：读取磁盘文件 → 解析为字符串 → 通过 atomic.Store 发布。
// 参数：无。
// 返回：文件 IO 错误（如权限/不存在），调用方按需处理。
// 副作用：更新 current 指针。
//
// 并发安全：原子写，与并发读互不阻塞。
func (l *Loader) Load() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return err
	}
	// 成功读取：发布带全文的 Persona 快照。
	l.current.Store(&Persona{Path: l.path, Content: string(data)})
	return nil
}

// Reload 显式重载 soul.md，可由用户在线触发热更新。
//
// 当前实现直接复用 Load 的逻辑；独立方法保留语义入口，
// 便于将来加入变更检测、事件通知等扩展而不破坏调用方。
// 并发安全：等同 Load。
func (l *Loader) Reload() error {
	// 复用 Load 完成磁盘读取与原子发布。
	return l.Load()
}

// Current 返回当前人格快照。
//
// 若尚未 Load（指针为 nil），返回一个仅含 Path 的空 Persona，
// 避免 nil 解引用；调用方应容忍 Content 为空的情况。
// 返回值不可被修改（如需修改请深拷贝）。
// 并发安全：原子读，无锁。
func (l *Loader) Current() *Persona {
	// 原子加载当前快照；非 nil 直接返回。
	if p := l.current.Load(); p != nil {
		return p
	}
	// 未加载场景：返回占位 Persona，保证非 nil。
	return &Persona{Path: l.path}
}

// Name 返回人格标识，取自文件名（去扩展名），缺失则返回 "default"。
//
// 用于日志、metrics 标签与多角色场景下的人格区分。
// 并发安全：仅读取不可变字段，无锁。
func (l *Loader) Name() string {
	// 无路径：返回默认标识。
	if l.path == "" {
		return "default"
	}
	// 提取文件名（含扩展名）。
	base := filepath.Base(l.path)
	// 去除扩展名，仅保留主体。
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	// 去扩展后若为空（如文件名全是 ".md"），回退默认值。
	if base == "" {
		return "default"
	}
	return base
}

// Inject 把人格内容拼接到 systemPrompt 之前，作为人格前置段落。
//
// 职责：取当前 Persona 内容 → TrimSpace → 与原 system prompt 拼接。
// 参数：
//
//	systemPrompt - 角色原始系统提示词，允许为空。
//
// 返回：拼接后的系统提示词；若人格为空则原样返回 systemPrompt。
// 副作用：无（纯函数，除读取快照外不改变状态）。
// 并发安全：通过 Current() 原子读，无锁。
//
// 拼接格式：使用 "\n\n---\n\n" 作为分隔符，既视觉清晰又便于 LLM 区分段落。
func (l *Loader) Inject(systemPrompt string) string {
	// 获取当前人格快照。
	p := l.Current()
	// 去除首尾空白，避免空行污染最终 prompt。
	content := strings.TrimSpace(p.Content)
	// 人格为空：直接返回原 prompt，不做任何注入。
	if content == "" {
		return systemPrompt
	}
	// 原始 prompt 为空：仅返回人格内容。
	if systemPrompt == "" {
		return content
	}
	// 两者都存在：人格在前，分隔符后接原 prompt。
	return content + "\n\n---\n\n" + systemPrompt
}

// Classify 使用 Loader 持有的分类策略推断任务类别。
func (l *Loader) Classify(text string) TaskKind {
	return l.classifier.Classify(text)
}

// Temperature 使用 Loader 持有的温度策略返回推荐温度。
func (l *Loader) Temperature(kind TaskKind, base float64) float64 {
	return l.temperature.Temperature(kind, base)
}

// SetClassifier 设置任务类别分类策略；传入 nil 时恢复默认。
func (l *Loader) SetClassifier(c TaskKindClassifier) {
	if c == nil {
		c = KeywordClassifier{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.classifier = c
}

// SetTemperaturePolicy 设置温度策略；传入 nil 时恢复默认。
func (l *Loader) SetTemperaturePolicy(tp TemperaturePolicy) {
	if tp == nil {
		tp = NewDefaultTemperaturePolicy()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.temperature = tp
}

// TaskKind 表示调节 temperature 时使用的任务类别。
//
// 用字符串枚举而非 int，便于日志可读与 YAML 配置直接映射。
type TaskKind string

// 任务类别常量：注释中给出对应温度推荐值，与 Temperature 实现保持一致。
const (
	KindRouting   TaskKind = "routing"   // 路由 / 选择 → 0
	KindSummarize TaskKind = "summarize" // 总结 / 压缩 → 0
	KindCode      TaskKind = "code"      // 代码 / 数学 → 0.1~0.2
	KindAnalysis  TaskKind = "analysis"  // 分析 / 推理 → 0.3
	KindCreative  TaskKind = "creative"  // 创意 / 文案 → 0.7~0.9
	KindGeneric   TaskKind = "generic"   // 默认 → 配置温度
)

// 包级默认策略实例，保证旧有包级函数行为不变。
var (
	defaultClassifier        TaskKindClassifier = KeywordClassifier{}
	defaultTemperaturePolicy TemperaturePolicy  = NewDefaultTemperaturePolicy()
)

// Temperature 返回任务类别对应的推荐温度。
//
// 参数：
//
//	kind - 任务类别枚举值；
//	base - 当前角色配置文件里的默认温度，仅作为 KindGeneric 的回退。
//
// 返回：0~1 之间的推荐 temperature。
// 副作用：无。并发安全：纯函数。
func Temperature(kind TaskKind, base float64) float64 {
	return defaultTemperaturePolicy.Temperature(kind, base)
}

// InferKind 根据自然语言任务描述启发式推断 TaskKind。
//
// 仅作为没有显式声明时的回退；调用方若显式指定 TaskKind，应优先使用显式值。
// 参数：
//
//	text - 任务描述文本（中英文均可，内部统一转小写匹配）。
//
// 返回：推断出的 TaskKind；无匹配时返回 KindGeneric。
// 副作用：无。并发安全：纯函数。
func InferKind(text string) TaskKind {
	return defaultClassifier.Classify(text)
}
