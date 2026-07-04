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

func newTestCallbackHandler(t *testing.T) (*CallbackHandler, *fakePrivateStore, *fakeSnapshotStore, *fakeBroadcaster) {
	ps := &fakePrivateStore{}
	ss := &fakeSnapshotStore{}
	rs := &fakeRedisSnapshotStore{}
	bc := &fakeBroadcaster{}
	wp := NewWriteProcessor(ps)
	sm := NewSnapshotManager(rs, ss, nil)
	h := NewCallbackHandler(wp, sm, bc)
	return h, ps, ss, bc
}

func TestCallbackHandlerOnEndSyncWrite(t *testing.T) {
	h, ps, ss, bc := newTestCallbackHandler(t)
	defer h.Close()

	ctx := context.Background()
	h.OnEnd(ctx, "agent-1", "topic-1", "action-a", "content-a", 1)

	ps.mu.Lock()
	if len(ps.episodes) != 1 {
		t.Fatalf("expected 1 episode, got %d", len(ps.episodes))
	}
	ps.mu.Unlock()

	ss.mu.Lock()
	if len(ss.saved) != 1 {
		t.Fatalf("expected 1 saved snapshot, got %d", len(ss.saved))
	}
	ss.mu.Unlock()

	bc.mu.Lock()
	if len(bc.episodes) != 1 {
		t.Fatalf("expected 1 episode broadcast, got %d", len(bc.episodes))
	}
	if len(bc.statuses) != 1 {
		t.Fatalf("expected 1 status broadcast, got %d", len(bc.statuses))
	}
	bc.mu.Unlock()
}

func TestCallbackHandlerOnEndFailureNotBlocking(t *testing.T) {
	h, ps, ss, _ := newTestCallbackHandler(t)
	defer h.Close()

	// 让 Episode 和 Snapshot 都失败
	atomic.StoreInt64(&ps.failNext, 100)
	atomic.StoreInt64(&ss.fail, 100)

	ctx := context.Background()
	// 同步写入失败不应 panic 或阻塞
	h.OnEnd(ctx, "agent-1", "topic-1", "fail-action", "content", 1)

	ps.mu.Lock()
	if len(ps.episodes) != 0 {
		t.Fatalf("expected 0 episodes due to failure, got %d", len(ps.episodes))
	}
	ps.mu.Unlock()
}

func TestCallbackHandlerOnStartOnError(t *testing.T) {
	h, _, _, bc := newTestCallbackHandler(t)
	defer h.Close()

	ctx := context.Background()
	h.OnStart(ctx, "agent-1", "topic-1")
	h.OnError(ctx, "agent-1", "topic-1", errors.New("boom"))

	bc.mu.Lock()
	if len(bc.statuses) != 2 {
		t.Fatalf("expected 2 status broadcasts, got %d", len(bc.statuses))
	}
	bc.mu.Unlock()
}

func TestCallbackHandlerClose(t *testing.T) {
	h, _, _, _ := newTestCallbackHandler(t)
	if err := h.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}
