package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestInMemoryKV_ReadWrite(t *testing.T) {
	kv := NewInMemoryKV(true, nil)
	ctx := context.Background()

	if err := kv.Set(ctx, "k1", "v1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := kv.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "v1" {
		t.Fatalf("expected v1, got %q", got)
	}
}

func TestInMemoryKV_ReadOnlyRejectsWrites(t *testing.T) {
	kv := NewInMemoryKV(false, nil)
	ctx := context.Background()

	if err := kv.Set(ctx, "k", "v"); !errors.Is(err, errReadOnly) {
		t.Fatalf("expected errReadOnly, got %v", err)
	}
	if err := kv.Delete(ctx, "k"); !errors.Is(err, errReadOnly) {
		t.Fatalf("expected errReadOnly, got %v", err)
	}
	if !kv.IsWritable() {
		// 正确：只读实例 IsWritable 为 false
	} else {
		t.Fatal("read-only instance should report IsWritable=false")
	}
}

func TestInMemoryKV_GetMissing(t *testing.T) {
	kv := NewInMemoryKV(true, nil)
	got, err := kv.Get(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestInMemoryKV_Delete(t *testing.T) {
	kv := NewInMemoryKV(true, nil)
	ctx := context.Background()
	_ = kv.Set(ctx, "k", "v")
	if err := kv.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, _ := kv.Get(ctx, "k")
	if got != "" {
		t.Fatalf("expected empty after delete, got %q", got)
	}
	// 幂等：再次删除不报错。
	if err := kv.Delete(ctx, "k"); err != nil {
		t.Fatalf("idempotent delete should not error, got %v", err)
	}
}

// fakeKVStore 是测试用的 KVStore 持久化后端。
type fakeKVStore struct {
	mu    sync.Mutex
	items map[string]string
	fail  bool
}

func newFakeKVStore() *fakeKVStore {
	return &fakeKVStore{items: make(map[string]string)}
}

func (s *fakeKVStore) SaveKV(ctx context.Context, key, value string, createdAt time.Time) error {
	if s.fail {
		return errors.New("store down")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[key] = value
	return nil
}

func (s *fakeKVStore) LoadKV(ctx context.Context, key string) (string, error) {
	if s.fail {
		return "", errors.New("store down")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.items[key], nil
}

func (s *fakeKVStore) DeleteKV(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, key)
	return nil
}

func TestInMemoryKV_StoreBacked(t *testing.T) {
	store := newFakeKVStore()
	// 可写实例写入，落 store。
	writer := NewInMemoryKV(true, store)
	ctx := context.Background()
	if err := writer.Set(ctx, "shared", "from-writer"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// 只读实例从 store 加载（内存为空，回填）。
	reader := NewInMemoryKV(false, store)
	got, err := reader.Get(ctx, "shared")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "from-writer" {
		t.Fatalf("expected from-writer, got %q", got)
	}
	// 只读实例不能写。
	if err := reader.Set(ctx, "x", "y"); !errors.Is(err, errReadOnly) {
		t.Fatalf("expected errReadOnly, got %v", err)
	}
}

func TestInMemoryKV_ConcurrentAccess(t *testing.T) {
	kv := NewInMemoryKV(true, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	// 并发写不同键。
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = kv.Set(ctx, "k"+itoa(n), "v")
		}(i)
	}
	// 并发读。
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = kv.Get(ctx, "k"+itoa(n))
		}(i)
	}
	wg.Wait()
	// 不 panic 即通过。
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestInMemoryKV_StoreFailure(t *testing.T) {
	store := newFakeKVStore()
	store.fail = true
	kv := NewInMemoryKV(true, store)
	// Set 时 store 失败：内存已写，但返回错误。
	err := kv.Set(context.Background(), "k", "v")
	if err == nil {
		t.Fatal("expected store failure error")
	}
	// 内存仍有值（Set 先写内存再落 store）。
	got, _ := kv.Get(context.Background(), "k")
	if got != "v" {
		t.Fatalf("memory should have value even if store fails, got %q", got)
	}
}
