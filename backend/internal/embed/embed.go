// Package embed 提供文本嵌入抽象与实现。
//
// 设计意图（P3-3）：定义统一的 Embedder 接口，支持 pseudo（字符哈希，零依赖）、
// openai（官方/兼容端点）、local（本地 ollama/xinference 等 OpenAI 兼容服务）。
// 所有调用方（store / memory / retriever）均依赖 Embedder 接口，切换模型只需改配置。
// PseudoEmbed 作为默认实现保留，保证无外部模型时也能跑通全链路。
//
// 抽到独立包是为避免 store ↔ memory 之间的循环依赖。
package embed

import (
	"crypto/sha256"   // SHA-256 哈希
	"encoding/binary" // 大端序转换
	"strings"         // 字符串处理
)

// PseudoEmbed 将文本映射为 dim 维 float32 向量（hashed bag-of-tokens）。
//
// 参数：
//   - text：待嵌入文本
//   - dim：向量维度（应与 pgvector.Dimensions 一致；<=0 时默认 768）
//
// 返回：归一化后的 float32 切片；空文本返回全零向量。
func PseudoEmbed(text string, dim int) []float32 {
	// 非法维度时回退到 768
	if dim <= 0 {
		dim = 768
	}
	// 初始化全零向量
	vec := make([]float32, dim)
	// 空文本或仅空白文本直接返回全零向量
	if strings.TrimSpace(text) == "" {
		return vec
	}
	// 遍历分词结果，累加哈希桶
	for _, tok := range tokenize(text) {
		// 跳过空 token
		if tok == "" {
			continue
		}
		// 计算 token 的 SHA-256
		sum := sha256.Sum256([]byte(tok))
		// 取前 4 字节作为大端序 uint32，再对 dim 取模得到桶索引
		idx := int(binary.BigEndian.Uint32(sum[:4])) % dim
		// 保证 idx 非负（Go 取模对负数行为特殊）
		if idx < 0 {
			idx += dim
		}
		// 对应桶计数加一
		vec[idx] += 1.0
	}
	// L2 归一化：先计算模长平方
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	// 若向量非零，则进行归一化
	if norm > 0 {
		// 使用牛顿迭代法近似计算 1/sqrt(norm)，迭代 8 次
		sqrt := float32(1.0)
		x := float32(norm)
		for i := 0; i < 8; i++ {
			sqrt = 0.5 * (sqrt + x/sqrt)
		}
		// 用近似倒数对每个分量做归一化
		if sqrt > 0 {
			for i := range vec {
				vec[i] /= sqrt
			}
		}
	}
	return vec
}

// tokenize 切分中文按字、英文按词，统一转小写。
//
// 参数：
//   - text: 待分词文本
//
// 返回：token 字符串切片。
func tokenize(text string) []string {
	// 统一转小写，降低大小写差异
	text = strings.ToLower(text)
	// 初始化 token 切片
	var toks []string
	// 用 strings.Builder 累积连续 ASCII 字符
	var b strings.Builder
	// flush 将当前累积的字符写入 toks 并重置 Builder
	flush := func() {
		if b.Len() > 0 {
			toks = append(toks, b.String())
			b.Reset()
		}
	}
	// 逐 rune 处理
	for _, r := range text {
		switch {
		// 英文字母、数字、下划线作为连续 token 的一部分
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		// 中文字符（CJK 统一表意文字范围）每个字单独成 token
		case r >= 0x4e00 && r <= 0x9fff:
			flush()
			toks = append(toks, string(r))
		// 其他字符作为分隔符
		default:
			flush()
		}
	}
	// 刷新末尾可能残留的 ASCII 字符
	flush()
	return toks
}
