// Package embed 提供 hashed bag-of-tokens 伪嵌入实现。
//
// 设计意图：项目未接入独立 embedding 模型，这里用 sha256 哈希把 token
// 映射到固定维度向量，再 L2 归一化。相同 token 必落同一维，相近文本因
// 共享 token 而向量靠近。后续若引入真实 embedding 模型，只需替换本文件
// 中的 PseudoEmbed 实现即可，所有调用方（store / memory / graph）自动复用。
//
// 抽到独立包是为避免 store ↔ memory 之间的循环依赖。
package embed

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
)

// PseudoEmbed 将文本映射为 dim 维 float32 向量（hashed bag-of-tokens）。
//
// 参数：
//   - text：待嵌入文本
//   - dim：向量维度（应与 pgvector.Dimensions 一致；<=0 时默认 768）
//
// 返回：归一化后的 float32 切片；空文本返回全零向量。
func PseudoEmbed(text string, dim int) []float32 {
	if dim <= 0 {
		dim = 768
	}
	vec := make([]float32, dim)
	if strings.TrimSpace(text) == "" {
		return vec
	}
	for _, tok := range tokenize(text) {
		if tok == "" {
			continue
		}
		sum := sha256.Sum256([]byte(tok))
		idx := int(binary.BigEndian.Uint32(sum[:4])) % dim
		if idx < 0 {
			idx += dim
		}
		vec[idx] += 1.0
	}
	// L2 归一化
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm > 0 {
		sqrt := float32(1.0)
		x := float32(norm)
		for i := 0; i < 8; i++ {
			sqrt = 0.5 * (sqrt + x/sqrt)
		}
		if sqrt > 0 {
			for i := range vec {
				vec[i] /= sqrt
			}
		}
	}
	return vec
}

// tokenize 切分中文按字、英文按词，统一转小写。
func tokenize(text string) []string {
	text = strings.ToLower(text)
	var toks []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			toks = append(toks, b.String())
			b.Reset()
		}
	}
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 0x4e00 && r <= 0x9fff:
			flush()
			toks = append(toks, string(r))
		default:
			flush()
		}
	}
	flush()
	return toks
}
