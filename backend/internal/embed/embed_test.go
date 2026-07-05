package embed

import (
	"context"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

func TestNewEmbedderPseudo(t *testing.T) {
	emb, err := NewEmbedder(types.EmbedConfig{Provider: "pseudo"}, 768)
	if err != nil {
		t.Fatalf("NewEmbedder pseudo: %v", err)
	}
	if emb.Dim() != 768 {
		t.Fatalf("expected dim 768, got %d", emb.Dim())
	}
	vec, err := emb.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 768 {
		t.Fatalf("expected vector length 768, got %d", len(vec))
	}
	// 非空文本应产生非零向量
	allZero := true
	for _, v := range vec {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatalf("expected non-zero embedding for non-empty text")
	}
}

func TestNewEmbedderDefaultProvider(t *testing.T) {
	emb, err := NewEmbedder(types.EmbedConfig{}, 768)
	if err != nil {
		t.Fatalf("NewEmbedder empty provider: %v", err)
	}
	if _, ok := emb.(*PseudoEmbedder); !ok {
		t.Fatalf("expected PseudoEmbedder for empty provider, got %T", emb)
	}
}

func TestNewEmbedderUnsupported(t *testing.T) {
	_, err := NewEmbedder(types.EmbedConfig{Provider: "unknown"}, 768)
	if err == nil {
		t.Fatalf("expected error for unsupported provider")
	}
}

func TestOpenAIEmbedderBaseURL(t *testing.T) {
	emb := NewOpenAIEmbedder(types.EmbedConfig{BaseURL: "https://api.example.com/"}, 1536)
	if got := emb.baseURL(); got != "https://api.example.com" {
		t.Fatalf("expected trimmed base URL, got %q", got)
	}
	emb2 := NewOpenAIEmbedder(types.EmbedConfig{}, 1536)
	if got := emb2.baseURL(); got != "https://api.openai.com" {
		t.Fatalf("expected default openai URL, got %q", got)
	}
}

func TestOpenAIEmbedderEmptyText(t *testing.T) {
	emb := NewOpenAIEmbedder(types.EmbedConfig{Model: "text-embedding-3-small"}, 768)
	vec, err := emb.Embed(context.Background(), "   ")
	if err != nil {
		t.Fatalf("Embed empty text: %v", err)
	}
	if len(vec) != 768 {
		t.Fatalf("expected zero vector length 768, got %d", len(vec))
	}
}
