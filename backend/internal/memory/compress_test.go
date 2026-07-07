package memory

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

func TestCompressorKeepsHighImportanceRecent(t *testing.T) {
	store := &fakePrivateStore{}
	store.episodes = []*types.Episode{
		{StepID: "e1", Importance: 0.9, Timestamp: time.Now(), FullObservation: "important recent observation"},
	}
	c := NewCompressor(store, nil)
	stats, err := c.Compress(context.Background(), "agent-1", "topic-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.RawKept != 1 {
		t.Errorf("expected RawKept=1, got %d", stats.RawKept)
	}
	if stats.Compressed != 0 {
		t.Errorf("expected Compressed=0, got %d", stats.Compressed)
	}
	// 保留 Raw：FullObservation 不应被清空
	store.mu.Lock()
	obs := store.episodes[0].FullObservation
	store.mu.Unlock()
	if obs == "" {
		t.Error("high-importance recent episode should keep FullObservation")
	}
}

func TestCompressorCompressesOldOrLowImportance(t *testing.T) {
	store := &fakePrivateStore{}
	store.episodes = []*types.Episode{
		{StepID: "e1", Importance: 0.9, Timestamp: time.Now().Add(-48 * time.Hour), FullObservation: "old but important"},
		{StepID: "e2", Importance: 0.3, Timestamp: time.Now(), FullObservation: "low importance"},
	}
	c := NewCompressor(store, nil)
	stats, err := c.Compress(context.Background(), "agent-1", "topic-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.RawKept != 0 {
		t.Errorf("expected RawKept=0, got %d", stats.RawKept)
	}
	if stats.Compressed != 2 {
		t.Errorf("expected Compressed=2, got %d", stats.Compressed)
	}
	if stats.BytesBefore <= stats.BytesAfter {
		t.Errorf("expected BytesBefore > BytesAfter, got %d <= %d", stats.BytesBefore, stats.BytesAfter)
	}
	// 被压缩的 Episode FullObservation 应被清空并回写
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, ep := range store.episodes {
		if ep.FullObservation != "" {
			t.Errorf("episode %s should have empty FullObservation after compression", ep.StepID)
		}
	}
}

func TestCompressorStatsNonZero(t *testing.T) {
	store := &fakePrivateStore{}
	longObs := string(make([]byte, 1000))
	store.episodes = []*types.Episode{
		{StepID: "e1", Importance: 0.3, Timestamp: time.Now(), FullObservation: longObs},
	}
	c := NewCompressor(store, nil)
	stats, err := c.Compress(context.Background(), "agent-1", "topic-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.BytesBefore == 0 {
		t.Error("expected non-zero BytesBefore")
	}
	if stats.BytesAfter != 0 {
		t.Errorf("expected BytesAfter=0 after compression, got %d", stats.BytesAfter)
	}
}
