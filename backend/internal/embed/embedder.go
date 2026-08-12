package embed

import (
	"context" // 上下文传递
	"fmt"     // 错误格式化

	"github.com/blockmemory/agent/backend/pkg/types" // EmbedConfig 配置类型
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
//   - provider=onnx          → ONNXEmbedder（进程内 ONNX Runtime，TODO #41；
//     需 `-tags onnx` 构建，默认构建编译期拒绝并指引）
//
// 参数：
//   - cfg：embed 配置段。
//   - dim：向量维度，由 pgvector.dimensions 传入；用于校验与 pseudo 实现。
//
// 返回：实现 Embedder 接口的实例；配置不合法时返回 error。
// 副作用：无网络请求（OpenAI 实现延迟到首次 Embed 调用；onnx 在构造期加载模型权重）。
func NewEmbedder(cfg types.EmbedConfig, dim int) (Embedder, error) {
	// 非法维度时回退到 768
	if dim <= 0 {
		dim = 768
	}
	// 根据 provider 名称分发
	switch cfg.Provider {
	case "", "pseudo":
		// 空字符串与 pseudo 均使用零依赖的伪嵌入实现
		return &PseudoEmbedder{dim: dim}, nil
	case "openai", "local":
		// openai / local 均走 OpenAI 兼容 embedding 协议
		return NewOpenAIEmbedder(cfg, dim), nil
	case "onnx":
		// 进程内 ONNX Runtime 推理（TODO #41）：模型权重进进程内存，零 api_key/零外部服务。
		return newONNXEmbedder(cfg, dim)
	default:
		// 不支持的 provider 返回错误
		return nil, fmt.Errorf("unsupported embed provider: %s", cfg.Provider)
	}
}

// MustNewEmbedder 是 NewEmbedder 的 panic-on-error 版本，供启动阶段一次性调用。
//
// 参数：
//   - cfg：embed 配置段
//   - dim：向量维度
//
// 返回：Embedder 实例。
func MustNewEmbedder(cfg types.EmbedConfig, dim int) Embedder {
	// 调用普通构造函数
	emb, err := NewEmbedder(cfg, dim)
	if err != nil {
		// 构造失败直接 panic，启动阶段不应继续
		panic(fmt.Sprintf("create embedder: %v", err))
	}
	// 返回成功构造的实例
	return emb
}
