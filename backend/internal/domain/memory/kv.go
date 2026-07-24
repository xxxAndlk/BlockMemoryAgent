package memory

// kv.go 实现键值对记忆（KVMemory）：按任意键存取上下文记忆，与按 agentID 分组的事件流
// Pipeline 互补。用途：多 Agent 协作验证闭环中，主线程 Agent（meta/domain）写入共享记忆，
// 协程 Agent（被询问方）按同键读取，实现"协程 Agent 共享主 Agent 记忆"语义。
//
// 权限模型：创建时通过 writable 参数决定可写与否。
//   - 主线程 Agent 持有 writable=true 实例，独占写入；
//   - 协程 Agent 持有 writable=false 实例，只读，Set/Delete 返回 errReadOnly。
//
// 并发安全：sync.RWMutex 保护，所有公开方法自行加锁。
// 锁使用注意：不持锁调用外部代码（如 Store 持久化），避免死锁与 panic；
// defer unlock 保证异常路径释放锁。

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errReadOnly 是只读 KVMemory 实例调用 Set/Delete 时返回的错误。
var errReadOnly = errors.New("kv memory is read-only")

// KVMemory 是键值对记忆的接口抽象，支持 Get/Set/Delete 三项基本操作。
// 实现方决定持久化方式（内存 / Postgres / Redis）；默认 InMemoryKV 仅内存。
// 读写权限由实现方在构造时决定（见 InMemoryKV.writable）。
type KVMemory interface {
	// Get 按键读取记忆值，不存在返回空串与 nil error。
	Get(ctx context.Context, key string) (string, error)
	// Set 写入键值对。只读实例返回 errReadOnly。
	Set(ctx context.Context, key, value string) error
	// Delete 删除键。只读实例返回 errReadOnly；键不存在幂等返回 nil。
	Delete(ctx context.Context, key string) error
}

// KVStore 是 KVMemory 的可选持久化后端，由实现方按需注入。
// 为 nil 时 InMemoryKV 仅内存保存，不持久化。
type KVStore interface {
	// SaveKV 持久化一个键值对。
	SaveKV(ctx context.Context, key, value string, createdAt time.Time) error
	// LoadKV 按键加载持久化的值，不存在返回空串与 nil error。
	LoadKV(ctx context.Context, key string) (string, error)
	// DeleteKV 删除持久化的键值对，键不存在幂等返回 nil。
	DeleteKV(ctx context.Context, key string) error
}

// InMemoryKV 是 KVMemory 的内存实现，可选挂载 KVStore 做持久化。
// writable=false 时 Set/Delete 返回 errReadOnly，Get 仍可读（含从 Store 加载）。
type InMemoryKV struct {
	mu       sync.RWMutex
	items    map[string]string
	store    KVStore
	writable bool
}

// NewInMemoryKV 创建一个内存 KV 记忆实例。
// writable=true 时可写，false 时只读。store 可为 nil（仅内存）。
func NewInMemoryKV(writable bool, store KVStore) *InMemoryKV {
	return &InMemoryKV{
		items:    make(map[string]string),
		store:    store,
		writable: writable,
	}
}

// Get 按键读取记忆值。
// 先查内存，未命中且配置了 store 时查 store 并回填内存。
func (m *InMemoryKV) Get(ctx context.Context, key string) (string, error) {
	// 快速路径：读锁查内存。
	m.mu.RLock()
	v, ok := m.items[key]
	m.mu.RUnlock()
	if ok {
		return v, nil
	}
	// 内存未命中且无 store：返回空。
	if m.store == nil {
		return "", nil
	}
	// 查 store。不持任何锁调用外部代码，避免死锁。
	sv, err := m.store.LoadKV(ctx, key)
	if err != nil {
		return "", err
	}
	if sv == "" {
		return "", nil
	}
	// 回填内存：升级为写锁。
	m.mu.Lock()
	// 双重检查：可能并发期间已被其他 goroutine 写入。
	if _, exists := m.items[key]; !exists {
		m.items[key] = sv
	}
	m.mu.Unlock()
	return sv, nil
}

// Set 写入键值对。只读实例返回 errReadOnly。
// 写内存成功后异步落 store（失败仅记日志，不影响读路径）。
func (m *InMemoryKV) Set(ctx context.Context, key, value string) error {
	if !m.writable {
		return errReadOnly
	}
	// 写内存。
	m.mu.Lock()
	m.items[key] = value
	m.mu.Unlock()
	// 持久化：不持锁调用外部代码。
	if m.store != nil {
		if err := m.store.SaveKV(ctx, key, value, time.Now()); err != nil {
			// 持久化失败不影响内存读，返回错误供调用方决定是否重试。
			return err
		}
	}
	return nil
}

// Delete 删除键。只读实例返回 errReadOnly；键不存在幂等返回 nil。
func (m *InMemoryKV) Delete(ctx context.Context, key string) error {
	if !m.writable {
		return errReadOnly
	}
	m.mu.Lock()
	delete(m.items, key)
	m.mu.Unlock()
	if m.store != nil {
		if err := m.store.DeleteKV(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

// IsWritable 报告该实例是否可写。供调用方在写入前判断（避免依赖错误返回）。
func (m *InMemoryKV) IsWritable() bool {
	return m.writable
}
