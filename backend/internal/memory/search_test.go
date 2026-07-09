package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// failingEmbedder 是一个总是返回固定错误的 Embedder 实现，用于测试错误传播。
type failingEmbedder struct {
	err error
}

func (f failingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, f.err
}

func TestSearchAndScore_EmbedderError(t *testing.T) {
	wantErr := errors.New("embedding unavailable")
	scorer := NewSearchScorer(failingEmbedder{err: wantErr}, nil)

	_, err := scorer.SearchAndScore(
		context.Background(),
		"agent-1",
		"topic-1",
		"test query",
		[]*types.Episode{
			{
				ObservationSummary: "observation",
				Timestamp:          time.Now(),
			},
		},
	)

	if err == nil {
		t.Fatal("expected error when embedder fails, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected error wrapping %v, got %v", wantErr, err)
	}
}

func TestCosineSimilarityClamped(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
	}{
		{
			name: "identical vectors",
			a:    []float32{1, 0},
			b:    []float32{1, 0},
			want: 1,
		},
		{
			name: "opposite vectors clamped to 0",
			a:    []float32{1, 0},
			b:    []float32{-1, 0},
			want: 0,
		},
		{
			name: "orthogonal vectors",
			a:    []float32{1, 0},
			b:    []float32{0, 1},
			want: 0,
		},
		{
			name: "empty vectors",
			a:    []float32{},
			b:    []float32{},
			want: 0,
		},
		{
			name: "different lengths",
			a:    []float32{1},
			b:    []float32{1, 0},
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cosineSimilarity(tc.a, tc.b)
			if got < 0 || got > 1 {
				t.Fatalf("cosineSimilarity returned %v, want value in [0,1]", got)
			}
			if got != tc.want {
				t.Fatalf("cosineSimilarity returned %v, want %v", got, tc.want)
			}
		})
	}
}
