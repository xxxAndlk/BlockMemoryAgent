package logger

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// LogStore 是会话日志持久化接口。与底层 store 解耦，logger 包内部统一使用该接口。
type LogStore interface {
	SaveSessionLog(ctx context.Context, rec *SessionLogRecord) error
}

// SessionLogRecord 是会话日志记录的数据传输对象。
// 前六个字段是通用摘要字段；后续扩展字段用于在写入底层 store 时保持数据库格式不变。
type SessionLogRecord struct {
	SessionID string
	Agent     string
	Level     string
	Phase     string
	Message   string
	Timestamp int64

	Prompt       string
	Response     string
	InputTokens  int
	OutputTokens int
	Model        string
	LatencyMs    int
	Meta         map[string]any
	CreatedAt    time.Time
}

// BatchingLogStore 使用有界工作池批量异步持久化日志。
// 它替代了原本每条日志单独启动 goroutine + 重试的实现，避免高并发时 goroutine 爆炸。
type BatchingLogStore struct {
	store    LogStore
	workers  int
	queue    chan *SessionLogRecord
	wg       sync.WaitGroup
	stopOnce sync.Once
	cancel   context.CancelFunc
	stopped  atomic.Bool
}

// NewBatchingLogStore 创建一个批量日志存储器。
// workers 控制并发工作 goroutine 数量；queueSize 控制有界队列大小，队列满时 Enqueue 会阻塞。
func NewBatchingLogStore(s LogStore, workers int, queueSize int) *BatchingLogStore {
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

	for i := 0; i < workers; i++ {
		b.wg.Add(1)
		go b.worker(ctx)
	}
	return b
}

// Enqueue 将日志记录加入异步处理队列。队列满时会阻塞，直到工作池取走记录或上下文取消。
func (b *BatchingLogStore) Enqueue(ctx context.Context, rec *SessionLogRecord) error {
	if b.stopped.Load() {
		return errors.New("batching log store stopped")
	}

	select {
	case b.queue <- rec:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop 停止工作池并等待队列中的记录全部处理完成。
func (b *BatchingLogStore) Stop() {
	b.stopOnce.Do(func() {
		b.stopped.Store(true)
		b.cancel()
		close(b.queue)
	})
	b.wg.Wait()
}

func (b *BatchingLogStore) worker(ctx context.Context) {
	defer b.wg.Done()
	for rec := range b.queue {
		if rec == nil {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := b.store.SaveSessionLog(callCtx, rec)
		cancel()
		if err != nil {
			// 持久化失败时仅丢弃；更复杂的重试/降级策略可在此扩展。
			_ = err
		}
	}
}
