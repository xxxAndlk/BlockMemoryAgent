package tui

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/blockmemory/agent/backend/internal/agent"
)

const (
	// taskTitleSummarizeThreshold 是触发 LLM 语义精简的显示宽度阈值：
	// 当任务标题显示宽度超过该值时，尝试使用 LLM 生成简短摘要。
	taskTitleSummarizeThreshold = 60
	// taskTitleMaxBriefWidth 是摘要标题的最大显示宽度，超出部分会被截断。
	taskTitleMaxBriefWidth = 40
	// taskBriefCacheMaxSize 是缓存条目上限，超过时淘汰任意一条旧记录。
	taskBriefCacheMaxSize = 200
)

// TaskBriefCache 缓存 LLM 生成的任务标题摘要，并通过信号量限制并发 LLM 请求数。
// 使用指针互斥锁，保证在 bubbletea 值语义下 Model 拷贝时仍共享同一把锁。
type TaskBriefCache struct {
	// cache 是标题到摘要的映射。
	cache map[string]string
	// mu 保护 cache 的并发读写。
	mu *sync.Mutex
	// sem 是并发信号量，最多允许 5 个并发 LLM 摘要请求。
	sem chan struct{}
}

// NewTaskBriefCache 构造一个 TaskBriefCache，初始化互斥锁与容量为 5 的信号量。
func NewTaskBriefCache() TaskBriefCache {
	return TaskBriefCache{
		cache: make(map[string]string),
		mu:    &sync.Mutex{},
		sem:   make(chan struct{}, 5),
	}
}

// Get 从缓存中获取指定标题的摘要，若缓存未命中则返回 false。
func (c *TaskBriefCache) Get(title string) (string, bool) {
	// 防御性检查：未初始化（mu 为 nil）时直接返回未命中。
	if c.mu == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	brief, ok := c.cache[title]
	return brief, ok
}

// Set 将标题与摘要存入缓存；若缓存已满则淘汰一条旧记录。
func (c *TaskBriefCache) Set(title, brief string) {
	// 防御性检查：未初始化时不执行写入。
	if c.mu == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 达到容量上限时，迭代 map 并删除第一条记录（map 迭代顺序随机，语义为“任意淘汰”）。
	if len(c.cache) >= taskBriefCacheMaxSize {
		for k := range c.cache {
			delete(c.cache, k)
			break
		}
	}
	c.cache[title] = brief
}

// Warm 异步请求 LLM 为指定标题生成摘要，受缓存信号量限制，并为 LLM 调用设置 3 秒超时。
func (c *TaskBriefCache) Warm(ctx context.Context, a agent.Agent, title string) {
	// agent 为空时无需请求。
	if a == nil {
		return
	}
	select {
	// 尝试获取信号量，成功则启动 goroutine 异步请求。
	case c.sem <- struct{}{}:
		go func() {
			// goroutine 退出时释放信号量。
			defer func() { <-c.sem }()
			// 为 LLM 调用创建带 3 秒超时的子上下文。
			callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			brief := a.SummarizeTaskTitle(callCtx, title)
			// 空摘要直接丢弃。
			if strings.TrimSpace(brief) == "" {
				return
			}
			brief = strings.TrimSpace(brief)
			// 去除常见引号与括号，避免 LLM 返回被包裹的内容。
			brief = strings.Trim(brief, "\"'"+"`「」【】()")
			// 若摘要仍超长，则按显示宽度截断。
			if runewidth.StringWidth(brief) > taskTitleMaxBriefWidth {
				brief = truncate(brief, taskTitleMaxBriefWidth)
			}
			c.Set(title, brief)
		}()
	default:
		// 信号量已满时直接放弃，避免阻塞渲染主循环。
	}
}

// summarize 是缓存内部入口：短标题直接返回；已缓存则返回缓存；否则先按宽度截断作为兜底，
// 并异步触发 LLM 生成更优摘要。
func (c *TaskBriefCache) summarize(a agent.Agent, title string) string {
	// 按显示宽度判断是否需要摘要。
	w := runewidth.StringWidth(title)
	if w <= taskTitleSummarizeThreshold {
		return title
	}
	// 命中缓存直接返回。
	if brief, ok := c.Get(title); ok {
		return brief
	}
	// 未命中时先用截断结果作为兜底展示，避免标题区域长时间空白或错位。
	fallback := truncate(title, taskTitleMaxBriefWidth)
	c.Set(title, fallback)
	// 异步请求更优摘要，下次渲染时可能命中缓存。
	c.Warm(context.Background(), a, title)
	return fallback
}

// summarizeTaskTitle 是 Model 侧的调用入口，供渲染逻辑与测试使用。
func (m Model) summarizeTaskTitle(title string) string {
	return m.taskBriefCache.summarize(m.agent, title)
}
