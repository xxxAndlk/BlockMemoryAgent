package embed

import (
	"context" // 上下文
	"strings" // 字符串断言
	"testing" // 测试框架

	"github.com/blockmemory/agent/backend/pkg/types" // EmbedConfig 配置类型
)

// TestNewEmbedderPseudo 验证 pseudo provider 能正确创建 PseudoEmbedder 并生成非零向量。
func TestNewEmbedderPseudo(t *testing.T) {
	// 使用 pseudo provider 创建 embedder
	emb, err := NewEmbedder(types.EmbedConfig{Provider: "pseudo"}, 768)
	// 校验构造成功
	if err != nil {
		t.Fatalf("NewEmbedder pseudo: %v", err)
	}
	// 校验维度
	if emb.Dim() != 768 {
		t.Fatalf("expected dim 768, got %d", emb.Dim())
	}
	// 嵌入文本
	vec, err := emb.Embed(context.Background(), "hello world")
	// 校验嵌入成功
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	// 校验向量长度
	if len(vec) != 768 {
		t.Fatalf("expected vector length 768, got %d", len(vec))
	}
	// 校验非空文本产生非零向量
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

// TestNewEmbedderDefaultProvider 验证空 provider 默认回退到 PseudoEmbedder。
func TestNewEmbedderDefaultProvider(t *testing.T) {
	// 空 provider 创建 embedder
	emb, err := NewEmbedder(types.EmbedConfig{}, 768)
	// 校验构造成功
	if err != nil {
		t.Fatalf("NewEmbedder empty provider: %v", err)
	}
	// 校验类型为 PseudoEmbedder
	if _, ok := emb.(*PseudoEmbedder); !ok {
		t.Fatalf("expected PseudoEmbedder for empty provider, got %T", emb)
	}
}

// TestNewEmbedderUnsupported 验证不支持的 provider 返回错误。
func TestNewEmbedderUnsupported(t *testing.T) {
	// 使用未知 provider 创建 embedder
	_, err := NewEmbedder(types.EmbedConfig{Provider: "unknown"}, 768)
	// 校验返回错误
	if err == nil {
		t.Fatalf("expected error for unsupported provider")
	}
}

// TestOpenAIEmbedderBaseURL 验证 baseURL 方法对斜杠的修剪与默认值。
func TestOpenAIEmbedderBaseURL(t *testing.T) {
	// 测试自定义带斜杠的 BaseURL
	emb := NewOpenAIEmbedder(types.EmbedConfig{BaseURL: "https://api.example.com/"}, 1536)
	if got := emb.baseURL(); got != "https://api.example.com" {
		t.Fatalf("expected trimmed base URL, got %q", got)
	}
	// 测试默认 BaseURL
	emb2 := NewOpenAIEmbedder(types.EmbedConfig{}, 1536)
	if got := emb2.baseURL(); got != "https://api.openai.com" {
		t.Fatalf("expected default openai URL, got %q", got)
	}
}

// TestOpenAIEmbedderEmptyText 验证空文本返回全零向量。
func TestOpenAIEmbedderEmptyText(t *testing.T) {
	// 创建 OpenAI embedder
	emb := NewOpenAIEmbedder(types.EmbedConfig{Model: "text-embedding-3-small"}, 768)
	// 嵌入仅空白字符的文本
	vec, err := emb.Embed(context.Background(), "   ")
	// 校验成功
	if err != nil {
		t.Fatalf("Embed empty text: %v", err)
	}
	// 校验向量长度
	if len(vec) != 768 {
		t.Fatalf("expected zero vector length 768, got %d", len(vec))
	}
}

// TestNewEmbedderONNX_Stub 默认构建（无 -tags onnx）下 provider=onnx 编译期 stub
// 返明确错误（TODO #41），指引 -tags onnx 构建。
func TestNewEmbedderONNX_Stub(t *testing.T) {
	_, err := NewEmbedder(types.EmbedConfig{Provider: "onnx"}, 768)
	if err == nil {
		t.Fatal("provider=onnx without -tags onnx must return error")
	}
	if !strings.Contains(err.Error(), "-tags onnx") {
		t.Fatalf("stub error must mention -tags onnx, got: %v", err)
	}
}
