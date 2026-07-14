package logger

import (
	"context" // 测试上下文
	"sync"    // 互斥锁保护测试 store
	"testing" // Go 测试框架
	"time"    // 等待异步写入

	"github.com/blockmemory/agent/backend/internal/store"
)

// fakeLogStore 用于测试 Logger 是否正确调用 SaveSessionLog。
type fakeLogStore struct {
	mu      sync.Mutex                // 保护 records 切片的并发访问
	records []*store.SessionLogRecord // 已保存的记录
}

// SaveSessionLog 实现 oldLogStore 接口，追加记录到内部切片。
func (f *fakeLogStore) SaveSessionLog(ctx context.Context, rec *store.SessionLogRecord) error {
	f.mu.Lock()         // 加锁：修改 records
	defer f.mu.Unlock() // 函数退出时释放锁
	f.records = append(f.records, rec)
	return nil
}

// Records 返回当前已保存记录的副本。
func (f *fakeLogStore) Records() []*store.SessionLogRecord {
	f.mu.Lock()         // 加锁：读取 records
	defer f.mu.Unlock() // 函数退出时释放锁
	out := make([]*store.SessionLogRecord, len(f.records))
	copy(out, f.records)
	return out
}

// waitForRecords 轮询等待 fake store 中至少存在 min 条记录，最多等待 500ms。
func waitForRecords(fs *fakeLogStore, min int) []*store.SessionLogRecord {
	for i := 0; i < 50; i++ {
		recs := fs.Records()
		if len(recs) >= min {
			return recs
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fs.Records()
}

// TestLoggerWithSession 验证带 session/agent/phase 的日志能正确持久化。
func TestLoggerWithSession(t *testing.T) {
	fs := &fakeLogStore{}
	l := New(fs).WithSession("s1").WithAgent("MetaAgent").WithPhase("routing")

	ctx := context.Background()
	l.Info(ctx, "路由判定")

	recs := waitForRecords(fs, 1)
	if len(recs) != 1 {
		t.Fatalf("应写入 1 条日志，got %d", len(recs))
	}
	if recs[0].SessionID != "s1" {
		t.Fatalf("session_id 应为 s1，got %s", recs[0].SessionID)
	}
	if recs[0].Agent != "MetaAgent" {
		t.Fatalf("agent 应为 MetaAgent，got %s", recs[0].Agent)
	}
	if recs[0].Phase != "routing" {
		t.Fatalf("phase 应为 routing，got %s", recs[0].Phase)
	}
}

// TestLoggerWithoutSessionDoesNotPersist 验证无 session_id 时不应写入数据库。
func TestLoggerWithoutSessionDoesNotPersist(t *testing.T) {
	fs := &fakeLogStore{}
	l := New(fs)
	ctx := context.Background()
	l.Info(ctx, "no session")

	recs := fs.Records()
	if len(recs) != 0 {
		t.Fatalf("无 session_id 时不应持久化，got %d", len(recs))
	}
}

// TestLoggerLLMCall 验证 LLMCall 能正确记录 token 数、模型与耗时。
func TestLoggerLLMCall(t *testing.T) {
	fs := &fakeLogStore{}
	l := New(fs).WithSession("s2")
	ctx := context.Background()
	l.LLMCall(ctx, LLMCallRecord{
		Agent:        "DomainAgent[frontend]",
		Model:        "deepseek-v4",
		Prompt:       "prompt",
		Response:     "response",
		InputTokens:  100,
		OutputTokens: 50,
		LatencyMs:    800,
	})

	recs := waitForRecords(fs, 1)
	if len(recs) != 1 {
		t.Fatalf("应写入 1 条 LLM 日志，got %d", len(recs))
	}
	if recs[0].InputTokens != 100 || recs[0].OutputTokens != 50 {
		t.Fatalf("token 数不匹配，got in=%d out=%d", recs[0].InputTokens, recs[0].OutputTokens)
	}
	if recs[0].LatencyMs != 800 {
		t.Fatalf("latency 应为 800，got %d", recs[0].LatencyMs)
	}
}
