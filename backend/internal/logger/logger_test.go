package logger

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/store"
)

// fakeLogStore 用于测试 Logger 是否正确调用 SaveSessionLog。
type fakeLogStore struct {
	mu      sync.Mutex
	records []*store.SessionLogRecord
}

func (f *fakeLogStore) SaveSessionLog(ctx context.Context, rec *store.SessionLogRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
	return nil
}

func (f *fakeLogStore) Records() []*store.SessionLogRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*store.SessionLogRecord, len(f.records))
	copy(out, f.records)
	return out
}

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
