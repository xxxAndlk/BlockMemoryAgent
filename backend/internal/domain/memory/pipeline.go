package memory

// 导入所需标准库与项目内部包。
import (
	"context" // context 用于持久化存储接口的上下文传递
	"fmt"     // fmt 用于格式化错误信息
	"log/slog" // slog 用于摘要失败时记录警告
	"strings"  // strings 用于事件摘要拼装
	"sync"     // sync 提供读写锁，保证并发安全
	"time"     // time 用于为事件填充发生时间

	"github.com/blockmemory/agent/backend/internal/agent" // agent 包提供 MemoryEvent、MemoryPipeline、ReactMessage 等类型
	"github.com/blockmemory/agent/backend/pkg/types"      // types 包提供 RoleDefinition 类型
)

// DefaultEventLimit 定义当 Assemble 未配置事件数量上限时，默认注入上下文的近期事件条数。
const DefaultEventLimit = 20

// DefaultMaxEventsPerAgent 定义每个 agent 在内存中最多保留的事件条数默认值。
// 该值必须大于 DefaultEventLimit，保证 Assemble 在容量裁减后仍能取满注入上限。
const DefaultMaxEventsPerAgent = 200

// eventSummarizeThreshold 是触发轻量模型摘要的事件条数阈值：事件数 <= 该值时直接拼装，
// 避免短任务为 3-5 条事件也额外调一次轻量模型。超过阈值时走摘要路径压缩 token。
const eventSummarizeThreshold = 8

// EventSummarizer 把一组格式化后的事件文本摘要成更短的上下文片段。
// bootstrap 侧用 modelFactory.CallLightweightWithRetry 实现，注入到 Pipeline。
// 为 nil 时关闭摘要路径，injectEvents 直接拼装原始事件。
type EventSummarizer func(ctx context.Context, events []string) (string, error)

// Pipeline 实现了 agent.MemoryPipeline 接口，作为一个基于内存的事件流。
// 每个智能体（agent）的事件按插入顺序保存在内存中；如果配置了 Store，事件还可以被持久化。
type Pipeline struct {
	mu     sync.RWMutex                   // mu 保护 events  map，避免并发读写导致数据竞争
	events map[string][]agent.MemoryEvent // events 按 agentID 分组保存内存中的事件列表
	store  Store                          // store 是可选的持久化存储接口；为 nil 时只在内存中保留事件
	limit  int                            // limit 控制 Assemble 时最多向上下文注入多少条近期事件
	// maxEventsPerAgent 控制每个 agent 在内存中最多保留的事件条数，超出时丢弃最旧事件
	maxEventsPerAgent int
	// summarizer 是可选的轻量模型摘要器：事件数超过 eventSummarizeThreshold 时调用，
	// 把原始事件列表压成短摘要注入上下文，显著降低长任务的 token 占用。
	summarizer EventSummarizer
}

// Store 抽象了事件流的持久化能力，实现者负责把事件保存到磁盘或数据库。
type Store interface {
	// SaveEvent 将指定 agent 的单条事件持久化保存。
	SaveEvent(ctx context.Context, agentID string, event agent.MemoryEvent) error
	// LoadEvents 从持久化存储中加载指定 agent 的最多 limit 条事件。
	LoadEvents(ctx context.Context, agentID string, limit int) ([]agent.MemoryEvent, error)
}

// NewPipeline 创建一个基于内存的事件管道。
// 参数 store 可为 nil：传入 nil 时事件仅在内存中保留，不会持久化。
func NewPipeline(store Store) *Pipeline {
	return &Pipeline{
		events:            make(map[string][]agent.MemoryEvent), // 初始化空的 agentID -> 事件列表映射
		store:             store,                                // 保存外部传入的持久化存储实现
		limit:             DefaultEventLimit,                    // 默认使用 DefaultEventLimit 作为注入上限
		maxEventsPerAgent: DefaultMaxEventsPerAgent,             // 默认每个 agent 最多保留 DefaultMaxEventsPerAgent 条事件
	}
}

// WithLimit 配置 Assemble 注入上下文时使用的近期事件数量。
// 参数 n 为非正数时会回退到 DefaultEventLimit，避免错误配置导致无法注入事件。
func (p *Pipeline) WithLimit(n int) *Pipeline {
	if n <= 0 {
		// n 不合法时回退到默认值，保证后续逻辑始终有可用上限
		n = DefaultEventLimit
	}
	p.limit = n
	// 返回自身以支持链式调用，例如 NewPipeline(nil).WithLimit(10)
	return p
}

// WithMaxEventsPerAgent 配置每个 agent 在内存中最多保留的事件条数。
// 参数 n 不大于 DefaultEventLimit 时回退到 DefaultMaxEventsPerAgent，
// 保证容量上限始终大于 Assemble 注入上限，避免注入逻辑取不满近期事件。
func (p *Pipeline) WithMaxEventsPerAgent(n int) *Pipeline {
	if n <= DefaultEventLimit {
		// n 不合法或过小时回退到默认值，维持 maxEventsPerAgent > DefaultEventLimit 的不变式
		n = DefaultMaxEventsPerAgent
	}
	p.maxEventsPerAgent = n
	// 返回自身以支持链式调用，例如 NewPipeline(nil).WithMaxEventsPerAgent(500)
	return p
}

// WithSummarizer 注入轻量模型摘要器，事件数超过阈值时把原始事件压成短摘要注入上下文。
// 传 nil 关闭摘要路径（默认关闭）。摘要失败时降级为直接拼装，不影响主流程。
func (p *Pipeline) WithSummarizer(s EventSummarizer) *Pipeline {
	p.summarizer = s
	return p
}

// Assemble 把 agent 的近期事件作为一条 system 角色上下文消息注入到历史记录中。
// 返回的新切片不会修改传入的 history 参数，调用方可以安全复用原切片。
func (p *Pipeline) Assemble(_ types.RoleDefinition, agentID string, history []agent.ReactMessage) []agent.ReactMessage {
	return p.injectEvents(agentID, history)
}

// injectEvents 完成实际的事件注入工作：先按 agentID 取事件，再截取最近 limit 条，
// 格式化为文本后拼装成一条 system 消息并追加到 history 末尾（不破坏前缀缓存）。
func (p *Pipeline) injectEvents(agentID string, history []agent.ReactMessage) []agent.ReactMessage {
	if agentID == "" {
		// agentID 为空无法定位事件列表，直接返回原始历史，不做任何注入
		return history
	}

	p.mu.RLock()
	// 获取该 agent 在内存中的全部事件；这里只是读指针，不需要深拷贝
	events := p.events[agentID]
	p.mu.RUnlock()

	if len(events) == 0 {
		// 没有事件时无需生成上下文消息，避免插入空内容
		return history
	}

	// 计算需要展示的事件起始下标，保证最多只取 limit 条最新的
	start := 0
	if len(events) > p.limit {
		start = len(events) - p.limit
	}
	// summary 收集每个事件格式化后的简短文本
	var summary []string
	for _, ev := range events[start:] {
		// 对单条事件按类型格式化并追加到 summary
		summary = append(summary, formatEvent(ev))
	}

	// 注入文本：事件数超过阈值且配置了摘要器时，调轻量模型压缩；
	// 否则直接 join 原始事件文本。摘要失败降级为直接 join，不影响主流程。
	body := joinNonEmpty("\n", summary)
	if p.summarizer != nil && len(summary) > eventSummarizeThreshold {
		// 给摘要调用设 15 秒超时：轻量模型（deepseek-v4-flash 等）在事件数 10-20 条时
		// 经常 5-10s 才返回，5s 全部超时降级 raw join，反而把 token 撑爆。15s 平衡主循环卡顿与摘要命中率。
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		summarized, err := p.summarizer(ctx, summary)
		cancel()
		if err != nil {
			// 摘要失败：记录警告，降级为原始 join，主流程不中断。
			slog.Warn("pipeline: summarize events failed, fallback to raw join",
				"agent_id", agentID, "event_count", len(summary), "err", err)
		} else if trimmed := strings.TrimSpace(summarized); trimmed != "" {
			// 摘要成功且非空：用摘要替换原始 body，显著降低 token 占用。
			// 保留原始事件数标注，便于 LLM 识别这是压缩后的快照。
			body = fmt.Sprintf("[已摘要 %d 条事件] %s", len(summary), trimmed)
		}
	}

	// ctxMsg 是注入到上下文的 system 消息，头部用中文标签便于大模型识别。
	// 放在 history 末尾而非开头：内容每轮随事件增长变化，放末尾不破坏前缀缓存
	// （DeepSeek 自动前缀缓存命中 system 指令 + history 前缀，events 摘要位于不可缓存尾部）。
	ctxMsg := agent.ReactMessage{
		Role:    "system",
		Content: "【近期事件】\n" + body,
	}
	// out 预先按 history 长度 +1 分配容量，减少扩容开销
	out := make([]agent.ReactMessage, 0, len(history)+1)
	// 先追加原始历史消息（构成稳定前缀，供 DeepSeek 前缀缓存命中）
	out = append(out, history...)
	// 再把上下文消息放到末尾，避免每轮变化的内容破坏前缀缓存。
	out = append(out, ctxMsg)
	return out
}

// Write 把一条事件追加到指定 agent 的事件流中；如果配置了 Store，还会异步超时持久化。
func (p *Pipeline) Write(agentID string, event agent.MemoryEvent) error {
	if agentID == "" {
		// agentID 为空无法归属事件，直接忽略，不报错也不保存
		return nil
	}
	if event.Occurred.IsZero() {
		// 事件未设置发生时间时，自动填充当前时间，保证时间线完整
		event.Occurred = time.Now()
	}

	p.mu.Lock()
	// 在内存中追加事件：直接 append 到该 agent 对应切片末尾
	p.events[agentID] = append(p.events[agentID], event)
	// 超过每 agent 容量上限时丢弃最旧事件，仅保留最新 maxEventsPerAgent 条，防止内存无限增长
	if p.maxEventsPerAgent > 0 && len(p.events[agentID]) > p.maxEventsPerAgent {
		p.events[agentID] = p.events[agentID][len(p.events[agentID])-p.maxEventsPerAgent:]
	}
	p.mu.Unlock()

	if p.store != nil {
		// 创建 5 秒超时的后台上下文，避免持久化操作无限阻塞
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel() // 函数退出时取消上下文，释放资源
		if err := p.store.SaveEvent(ctx, agentID, event); err != nil {
			// 持久化失败向上返回错误，调用方可据此重试或记录日志
			return fmt.Errorf("persist event: %w", err)
		}
	}
	return nil
}

// Events 返回指定 agent 事件的深拷贝副本，主要用于测试和诊断。
func (p *Pipeline) Events(agentID string) []agent.MemoryEvent {
	p.mu.RLock()
	// defer 在 return 后释放读锁，避免函数多返回点遗漏解锁
	defer p.mu.RUnlock()
	// 创建与内部切片等长的新切片
	out := make([]agent.MemoryEvent, len(p.events[agentID]))
	// copy 实现深拷贝（切片元素 agent.MemoryEvent 为值类型，复制后互不影响）
	copy(out, p.events[agentID])
	return out
}

// formatEvent 根据事件类型把 MemoryEvent 转换成适合上下文展示的短文本。
func formatEvent(ev agent.MemoryEvent) string {
	// 按事件类型分支处理，输出统一格式的摘要字符串
	switch ev.Type {
	case "answer":
		// 回答类型：展示 agentID 与回答内容摘要
		return fmt.Sprintf("[%s] answer: %s", ev.AgentID, truncate(ev.Content, 120))
	case "tool_call":
		// 工具调用类型：展示工具名、输入输出摘要，便于回溯调用链
		return fmt.Sprintf("[%s] tool=%s input=%s output=%s", ev.AgentID, ev.ToolName, truncate(ev.Input, 80), truncate(ev.Output, 120))
	case "call_sub_agent":
		// 调用子智能体类型：展示目标角色与触发内容摘要
		return fmt.Sprintf("[%s] called sub-agent %s (%s)", ev.AgentID, ev.Role, truncate(ev.Content, 120))
	case "sub_agent_summary":
		// 子智能体汇总类型：展示子智能体角色与返回摘要
		return fmt.Sprintf("[%s] sub-agent %s summary: %s", ev.AgentID, ev.Role, truncate(ev.Content, 120))
	default:
		// 未知或自定义类型：展示类型名与内容摘要
		return fmt.Sprintf("[%s] %s: %s", ev.AgentID, ev.Type, truncate(ev.Content, 120))
	}
}

// truncate 将字符串 s 截断到最多 n 个字符，超过时尾部补 "..." 表示省略。
func truncate(s string, n int) string {
	if len(s) <= n {
		// 长度未超限，原样返回
		return s
	}
	// 超过限制则保留前 n 个字符并附加省略号
	return s[:n] + "..."
}

// joinNonEmpty 用分隔符 sep 连接切片中非空的字符串片段。
func joinNonEmpty(sep string, parts []string) string {
	// 第一轮遍历过滤掉空字符串，避免输出中出现多余分隔符
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	// 第二轮遍历用 sep 拼接非空片段，仅在 i>0 时添加分隔符
	var result string
	for i, p := range out {
		if i > 0 {
			result += sep
		}
		result += p
	}
	return result
}
