package embed

import "context"

// PseudoEmbedder 基于字符哈希的伪嵌入实现。
//
// 设计意图：项目未接入独立 embedding 模型时，复用原有 PseudoEmbed 算法，
// 保证零外部依赖、零网络调用即可跑通全链路。后续切到真实模型只需改配置。
type PseudoEmbedder struct {
	dim int // 输出向量维度
}

// Embed 实现 Embedder 接口，将文本编码为 dim 维 float32 向量。
//
// 参数：
//   - _: 上下文（当前实现无需使用，但保留以满足接口签名）
//   - text: 待嵌入文本
//
// 返回：
//   - []float32: 编码后的向量
//   - error: 当前实现永不返回错误
func (e *PseudoEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	// 直接复用包级 PseudoEmbed 函数
	return PseudoEmbed(text, e.dim), nil
}

// Dim 返回向量维度。
//
// 返回：int，输出向量维度。
func (e *PseudoEmbedder) Dim() int {
	// 返回构造时指定的维度
	return e.dim
}
