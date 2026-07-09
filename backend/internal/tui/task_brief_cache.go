package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/blockmemory/agent/backend/internal/model"
)

const (
	// taskTitleSummarizeThreshold triggers LLM summarization when a task title
	// exceeds this display width.
	taskTitleSummarizeThreshold = 60
	// taskTitleMaxBriefWidth is the maximum display width of the summarized title.
	taskTitleMaxBriefWidth = 40
	// taskBriefCacheMaxSize is the cache size limit; evicts an arbitrary entry
	// when exceeded.
	taskBriefCacheMaxSize = 200
)

// TaskBriefCache caches LLM-briefed task titles and limits concurrent LLM
// brief requests with a semaphore. The mutex is a pointer so that copies of
// the cache (bubbletea value semantics) still share the same lock.
type TaskBriefCache struct {
	cache map[string]string
	mu    *sync.Mutex
	sem   chan struct{}
}

// NewTaskBriefCache constructs a TaskBriefCache with a mutex and a semaphore
// that allows up to 5 concurrent LLM brief requests.
func NewTaskBriefCache() TaskBriefCache {
	return TaskBriefCache{
		cache: make(map[string]string),
		mu:    &sync.Mutex{},
		sem:   make(chan struct{}, 5),
	}
}

// Get returns a cached brief, if present.
func (c *TaskBriefCache) Get(title string) (string, bool) {
	if c.mu == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	brief, ok := c.cache[title]
	return brief, ok
}

// Set stores a brief in the cache, evicting an arbitrary entry when full.
func (c *TaskBriefCache) Set(title, brief string) {
	if c.mu == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= taskBriefCacheMaxSize {
		for k := range c.cache {
			delete(c.cache, k)
			break
		}
	}
	c.cache[title] = brief
}

// Warm requests an LLM brief for title asynchronously, respecting the cache's
// concurrency semaphore. ctx is used for cancellation; a 3-second timeout is
// applied to the LLM call.
func (c *TaskBriefCache) Warm(ctx context.Context, mf *model.ModelFactory, title string) {
	if mf == nil {
		return
	}
	select {
	case c.sem <- struct{}{}:
		go func() {
			defer func() { <-c.sem }()
			prompt := fmt.Sprintf("将以下任务描述压缩成 %d 字以内的简短任务名，保留核心动作与对象，不要解释：\n%s", taskTitleMaxBriefWidth, title)
			callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			brief, err := mf.CallLightweightWithRetry(callCtx, prompt)
			if err != nil || strings.TrimSpace(brief) == "" {
				return
			}
			brief = strings.TrimSpace(brief)
			brief = strings.Trim(brief, "\"'"+"`「」【】()")
			if runewidth.StringWidth(brief) > taskTitleMaxBriefWidth {
				brief = truncate(brief, taskTitleMaxBriefWidth)
			}
			c.Set(title, brief)
		}()
	default:
	}
}

func (c *TaskBriefCache) summarize(mf *model.ModelFactory, title string) string {
	w := runewidth.StringWidth(title)
	if w <= taskTitleSummarizeThreshold {
		return title
	}
	if brief, ok := c.Get(title); ok {
		return brief
	}
	fallback := truncate(title, taskTitleMaxBriefWidth)
	c.Set(title, fallback)
	c.Warm(context.Background(), mf, title)
	return fallback
}

// summarizeTaskTitle is the Model-facing entrypoint kept for callers/tests.
func (m Model) summarizeTaskTitle(title string) string {
	return m.taskBriefCache.summarize(m.modelFactory, title)
}
