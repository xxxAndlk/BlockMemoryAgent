package embed

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// Embedder 文本嵌入接口：将自然语言文本编码为 float32 向量。
// 设计意图（P3-3）：统一 pseudo / openai / local 等 embedding 实现，
// 所有调用方（store / memory / retriever）均依赖此接口，切换模型无需改业务代码。
type Embedder interface {
	// Embed 将单条文本编码为 float32 向量。
	// 返回向量长度需与 pgvector.dimensions 一致；失败时返回非 nil error。
	Embed(ctx context.Context, text string) ([]float32, error)
	// Dim 返回输出向量维度；<=0 时调用方应回退到 pgvector 配置维度。
	Dim() int
}

// NewEmbedder 根据配置创建对应 provider 的 Embedder 实现。
//
// 职责：
//   - provider=pseudo（或空） → PseudoEmbedder
//   - provider=openai/local  → OpenAIEmbedder（OpenAI 兼容协议）
//
// 参数：
//   - cfg：embed 配置段。
//   - dim：向量维度，由 pgvector.dimensions 传入；用于校验与 pseudo 实现。
//
// 返回：实现 Embedder 接口的实例；配置不合法时返回 error。
// 副作用：无网络请求（OpenAI 实现延迟到首次 Embed 调用）。
func NewEmbedder(cfg types.EmbedConfig, dim int) (Embedder, error) {
	if dim <= 0 {
		dim = 768
	}
	switch cfg.Provider {
	case "", "pseudo":
		return &PseudoEmbedder{dim: dim}, nil
	case "openai", "local":
		// openai / local 均走 OpenAI 兼容 embedding 协议
		return NewOpenAIEmbedder(cfg, dim), nil
	default:
		return nil, fmt.Errorf("unsupported embed provider: %s", cfg.Provider)
	}
}

// MustNewEmbedder 是 NewEmbedder 的 panic-on-error 版本，供启动阶段一次性调用。
func MustNewEmbedder(cfg types.EmbedConfig, dim int) Embedder {
	emb, err := NewEmbedder(cfg, dim)
	if err != nil {
		panic(fmt.Sprintf("create embedder: %v", err))
	}
	return emb
}
