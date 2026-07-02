package memory

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakePrivateStore 是 PrivateStore 的内存假实现，用于测试写入成功/失败场景。
type fakePrivateStore struct {
	episodes []*types.Episode
	mu       sync.Mutex
	failNext int64 // 原子计数：>0 时 SaveEpisodeWithStepCount 返回错误
}

func (s *fakePrivateStore) SaveEpisode(ctx context.Context, agentID, topicID string, ep *types.Episode) error {
	return s.SaveEpisodeWithStepCount(ctx, agentID, topicID, 0, ep)
}

func (s *fakePrivateStore) SaveEpisodeWithStepCount(ctx context.Context, agentID, topicID string, stepCount int, ep *types.Episode) error {
	if atomic.LoadInt64(&s.failNext) > 0 {
		atomic.AddInt64(&s.failNext, -1)
		return errors.New("fake save episode error")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.episodes = append(s.episodes, ep)
	return nil
}

func (s *fakePrivateStore) GetEpisodes(ctx context.Context, agentID, topicID string, limit int) ([]*types.Episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*types.Episode, len(s.episodes))
	copy(out, s.episodes)
	return out, nil
}

func (s *fakePrivateStore) CountEpisodes(ctx context.Context, agentID, topicID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.episodes), nil
}

// fakeSnapshotStore 是 SnapshotStore 的内存假实现。
type fakeSnapshotStore struct {
	saved []*types.AgentSnapshot
	mu    sync.Mutex
	fail  int64
}

func (s *fakeSnapshotStore) SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot) error {
	if atomic.LoadInt64(&s.fail) > 0 {
		atomic.AddInt64(&s.fail, -1)
		return errors.New("fake save snapshot error")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, snapshot)
	return nil
}

func (s *fakeSnapshotStore) GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	return nil, nil
}

// fakeRedisSnapshotStore 是 RedisSnapshotStore 的内存假实现。
type fakeRedisSnapshotStore struct{}

func (s *fakeRedisSnapshotStore) SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot, ttl time.Duration) error {
	return nil
}

func (s *fakeRedisSnapshotStore) GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	return nil, nil
}

// fakeDeadLetterStore 是 DeadLetterStore 的内存假实现。
type fakeDeadLetterStore struct {
	failures []*types.MemoryWriteFailure
	mu       sync.Mutex
}

func (s *fakeDeadLetterStore) SaveMemoryWriteFailure(ctx context.Context, agentID, topicID string, stepCount int, action, rawContent, errStr string, retryCount int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, &types.MemoryWriteFailure{
		AgentID:    agentID,
		TopicID:    topicID,
		StepCount:  stepCount,
		Action:     action,
		RawContent: rawContent,
		Error:      errStr,
		RetryCount: retryCount,
	})
	return nil
}

func (s *fakeDeadLetterStore) QueryUnresolvedMemoryWriteFailures(ctx context.Context, limit int) ([]*types.MemoryWriteFailure, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*types.MemoryWriteFailure
	for _, f := range s.failures {
		if f.ResolvedAt == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *fakeDeadLetterStore) ResolveMemoryWriteFailure(ctx context.Context, id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.failures {
		if f.ID == id {
			now := time.Now()
			f.ResolvedAt = &now
			return nil
		}
	}
	return nil
}

// fakeBroadcaster 是 Broadcaster 的内存假实现。
type fakeBroadcaster struct {
	statuses []types.AgentStatusPayload
	episodes []types.EpisodePayload
	mu       sync.Mutex
}

func (b *fakeBroadcaster) BroadcastAgentStatus(topicID string, payload types.AgentStatusPayload) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.statuses = append(b.statuses, payload)
}

func (b *fakeBroadcaster) BroadcastEpisode(topicID string, payload types.EpisodePayload) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.episodes = append(b.episodes, payload)
}

func newTestCallbackHandler(t *testing.T) (*CallbackHandler, *fakePrivateStore, *fakeSnapshotStore, *fakeDeadLetterStore, *fakeBroadcaster) {
	ps := &fakePrivateStore{}
	ss := &fakeSnapshotStore{}
	rs := &fakeRedisSnapshotStore{}
	dls := &fakeDeadLetterStore{}
	bc := &fakeBroadcaster{}
	wp := NewWriteProcessor(ps)
	sm := NewSnapshotManager(rs, ss)
	h := NewCallbackHandler(wp, sm, bc, dls)
	// 测试用短退避，避免等待数秒
	h.retryDelay = 1 * time.Millisecond
	h.writeTimeout = 100 * time.Millisecond
	return h, ps, ss, dls, bc
}

func TestCallbackHandlerQueueOrder(t *testing.T) {
	h, ps, _, _, bc := newTestCallbackHandler(t)
	defer h.Close()

	ctx := context.Background()
	h.OnEnd(ctx, "agent-1", "topic-1", "action-a", "content-a", 1)
	h.OnEnd(ctx, "agent-1", "topic-1", "action-b", "content-b", 2)
	h.OnEnd(ctx, "agent-1", "topic-1", "action-c", "content-c", 3)

	// 等待后台 worker 消费
	time.Sleep(50 * time.Millisecond)

	ps.mu.Lock()
	if len(ps.episodes) != 3 {
		t.Fatalf("expected 3 episodes, got %d", len(ps.episodes))
	}
	ps.mu.Unlock()

	bc.mu.Lock()
	if len(bc.episodes) != 3 {
		t.Fatalf("expected 3 episode broadcasts, got %d", len(bc.episodes))
	}
	bc.mu.Unlock()
}

func TestCallbackHandlerRetryExhaustionDeadLetter(t *testing.T) {
	h, ps, ss, dls, _ := newTestCallbackHandler(t)
	defer h.Close()

	// 让 Episode 和 Snapshot 都持续失败
	atomic.StoreInt64(&ps.failNext, 100)
	atomic.StoreInt64(&ss.fail, 100)

	ctx := context.Background()
	h.OnEnd(ctx, "agent-1", "topic-1", "fail-action", "content", 1)

	// 等待重试耗尽并入死信
	time.Sleep(100 * time.Millisecond)

	dls.mu.Lock()
	if len(dls.failures) != 2 {
		t.Fatalf("expected 2 dead letters (episode + snapshot), got %d", len(dls.failures))
	}
	var hasEpisode, hasSnapshot bool
	for _, f := range dls.failures {
		if f.Action == "episode_write" {
			hasEpisode = true
		}
		if f.Action == "snapshot_save" {
			hasSnapshot = true
		}
	}
	dls.mu.Unlock()
	if !hasEpisode {
		t.Fatal("missing episode_write dead letter")
	}
	if !hasSnapshot {
		t.Fatal("missing snapshot_save dead letter")
	}
}

func TestCallbackHandlerQueueFullSyncFallback(t *testing.T) {
	ps := &fakePrivateStore{}
	ss := &fakeSnapshotStore{}
	rs := &fakeRedisSnapshotStore{}
	dls := &fakeDeadLetterStore{}
	bc := &fakeBroadcaster{}
	wp := NewWriteProcessor(ps)
	sm := NewSnapshotManager(rs, ss)
	// 容量为 0 的队列，强制每次 OnEnd 都走同步降级路径
	h := NewCallbackHandler(wp, sm, bc, dls)
	h.writeQueue = make(chan writeQueueItem, 0)
	h.retryDelay = 1 * time.Millisecond
	h.writeTimeout = 100 * time.Millisecond
	defer h.Close()

	ctx := context.Background()
	h.OnEnd(ctx, "agent-1", "topic-1", "sync-action", "content", 1)

	ps.mu.Lock()
	if len(ps.episodes) != 1 {
		t.Fatalf("expected 1 episode from sync fallback, got %d", len(ps.episodes))
	}
	ps.mu.Unlock()
}

func TestCallbackHandlerReplayDeadLetters(t *testing.T) {
	h, ps, ss, dls, _ := newTestCallbackHandler(t)
	defer h.Close()

	// 预置一条未解析死信
	dls.SaveMemoryWriteFailure(context.Background(), "agent-1", "topic-1", 7, "episode_write", "raw", "err", 3)

	// 回放前让存储成功
	atomic.StoreInt64(&ps.failNext, 0)
	atomic.StoreInt64(&ss.fail, 0)

	if err := h.ReplayDeadLetters(context.Background()); err != nil {
		t.Fatalf("ReplayDeadLetters failed: %v", err)
	}

	ps.mu.Lock()
	if len(ps.episodes) != 1 {
		t.Fatalf("expected 1 replayed episode, got %d", len(ps.episodes))
	}
	ps.mu.Unlock()

	ss.mu.Lock()
	if len(ss.saved) != 1 {
		t.Fatalf("expected 1 replayed snapshot, got %d", len(ss.saved))
	}
	ss.mu.Unlock()

	dls.mu.Lock()
	unresolved := 0
	for _, f := range dls.failures {
		if f.ResolvedAt == nil {
			unresolved++
		}
	}
	dls.mu.Unlock()
	if unresolved != 0 {
		t.Fatalf("expected all dead letters resolved, got %d unresolved", unresolved)
	}
}

func TestCallbackHandlerClose(t *testing.T) {
	h, _, _, _, _ := newTestCallbackHandler(t)

	ctx := context.Background()
	// 发送若干事件后关闭
	for i := 0; i < 5; i++ {
		h.OnEnd(ctx, "agent-1", "topic-1", "action", "content", i)
	}

	done := make(chan error, 1)
	go func() {
		done <- h.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close timed out")
	}
}
