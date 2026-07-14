package logger

import (
	"context"     // 上下文，用于超时控制
	"errors"      // 构造 stopped 错误
	"sync"        // WaitGroup / Once
	"sync/atomic" // stopped 原子标志
	"time"        // 写入超时
)

// LogStore 是会话日志持久化接口。与底层 store 解耦，logger 包内部统一使用该接口。
type LogStore interface {
	// SaveSessionLog 持久化一条会话日志。
	//
	// 参数：
	//   - ctx：上下文，用于超时/取消控制
	//   - rec：待持久化的日志记录
	//
	// 返回：持久化过程中发生的错误。
	SaveSessionLog(ctx context.Context, rec *SessionLogRecord) error
}

// SessionLogRecord 是会话日志记录的数据传输对象。
// 前六个字段是通用摘要字段；后续扩展字段用于在写入底层 store 时保持数据库格式不变。
type SessionLogRecord struct {
	SessionID string // 会话标识
	Agent     string // Agent 标识
	Level     string // 日志级别
	Phase     string // 执行阶段
	Message   string // 日志消息
	Timestamp int64  // 毫秒时间戳

	Prompt       string         // LLM prompt（仅 llm_call 阶段使用）
	Response     string         // LLM response（仅 llm_call 阶段使用）
	InputTokens  int            // 输入 token 数
	OutputTokens int            // 输出 token 数
	Model        string         // 模型名称
	LatencyMs    int            // 调用耗时（毫秒）
	Meta         map[string]any // 扩展元数据
	CreatedAt    time.Time      // 创建时间
}

// BatchingLogStore 使用有界工作池批量异步持久化日志。
// 它替代了原本每条日志单独启动 goroutine + 重试的实现，避免高并发时 goroutine 爆炸。
type BatchingLogStore struct {
	store    LogStore               // 底层持久化接口
	workers  int                    // 工作 goroutine 数量
	queue    chan *SessionLogRecord // 有界任务队列
	wg       sync.WaitGroup         // 等待工作 goroutine 退出
	stopOnce sync.Once              // 保证 Stop 只执行一次
	cancel   context.CancelFunc     // 用于通知 worker 退出
	stopped  atomic.Bool            // 是否已停止
}

// NewBatchingLogStore 创建一个批量日志存储器。
// workers 控制并发工作 goroutine 数量；queueSize 控制有界队列大小，队列满时 Enqueue 会阻塞。
func NewBatchingLogStore(s LogStore, workers int, queueSize int) *BatchingLogStore {
	// 非法参数兜底，保证至少有一个 worker 和一个队列槽位。
	if workers <= 0 {
		workers = 1
	}
	if queueSize <= 0 {
		queueSize = 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	b := &BatchingLogStore{
		store:   s,
		workers: workers,
		queue:   make(chan *SessionLogRecord, queueSize),
		cancel:  cancel,
	}

	// 启动指定数量的 worker goroutine。
	for i := 0; i < workers; i++ {
		b.wg.Add(1)
		go b.worker(ctx)
	}
	return b
}

// Enqueue 将日志记录加入异步处理队列。队列满时会阻塞，直到工作池取走记录或上下文取消。
func (b *BatchingLogStore) Enqueue(ctx context.Context, rec *SessionLogRecord) error {
	// 已停止时不接受新记录，避免关闭 channel 后写入 panic。
	if b.stopped.Load() {
		return errors.New("batching log store stopped")
	}

	// select 在入队成功与上下文取消之间等待。
	select {
	case b.queue <- rec:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop 停止工作池并等待队列中的记录全部处理完成。
func (b *BatchingLogStore) Stop() {
	// stopOnce 保证仅执行一次：设置停止标志、取消上下文、关闭队列。
	b.stopOnce.Do(func() {
		b.stopped.Store(true)
		b.cancel()
		close(b.queue)
	})
	// 等待所有 worker 处理完队列后退出。
	b.wg.Wait()
}

// worker 是工作池中的 goroutine，从队列取记录并持久化。
func (b *BatchingLogStore) worker(ctx context.Context) {
	defer b.wg.Done() // 函数退出时通知 WaitGroup
	for rec := range b.queue {
		// 忽略 nil 记录，防御脏数据。
		if rec == nil {
			continue
		}
		// 每次写入设置 3 秒超时，避免单个卡住的存储调用无限阻塞 worker。
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := b.store.SaveSessionLog(callCtx, rec)
		cancel()
		if err != nil {
			// 持久化失败时仅丢弃；更复杂的重试/降级策略可在此扩展。
			_ = err
		}
	}
}
