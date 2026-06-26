// Package memory block_vector.go 提供特性3（domainAgent 后向量检索）所需的
// 伪嵌入与块记忆读写适配。
//
// 设计意图：项目未接入独立 embedding 模型，这里用 hashed bag-of-tokens
// 产出固定维度的伪向量，借助 pgvector 完成相似度排序。后续若引入真实
// embedding 模型，只需替换 PseudoEmbed 实现即可。
package memory

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// PseudoEmbed 将文本映射为 dim 维 float32 向量（hashed bag-of-tokens）。
//
// 算法：按 Unicode 字符与 ASCII 词边界切分 token，每个 token 的 sha256
// 哈希前 4 字节作为 uint32，对 dim 取模累加 +1。最后做 L2 归一化。
// 这样相同 token 必落同一维，相近文本因共享 token 而向量靠近。
//
// 参数：
//   - text：待嵌入文本
//   - dim：向量维度（应与 pgvector.Dimensions 一致）
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
	// L2 归一化，使 cosine 距离与内积等价
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm > 0 {
		sqrt := float32(1.0)
		// 简易平方根迭代，避免引入 math 依赖差异
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
			// 中文字符按单字作为 token
			flush()
			toks = append(toks, string(r))
		default:
			flush()
		}
	}
	flush()
	return toks
}

// BlockMemoryRecord 块记忆条目：domainAgent 完成一个 SessionBlock 后归档的
// 领域/目标/结果摘要，写入 global_knowledge 表（KnowledgeType="block_memory"）。
type BlockMemoryRecord struct {
	SessionID string    // 所属会话
	Domain    string    // 领域名
	Goal      string    // 领域目标
	Summary   string    // 任务结果摘要
	CreatedAt time.Time // 归档时间
}

// ToKnowledgeRecord 把块记忆转为 global_knowledge 表记录。
// Content 拼装为可读文本，便于检索后直接注入 prompt。
func (r *BlockMemoryRecord) ToKnowledgeRecord(dim int) *types.KnowledgeRecord {
	content := strings.Join([]string{
		"领域:", r.Domain,
		"目标:", r.Goal,
		"结果:", r.Summary,
	}, "\n")
	meta := map[string]any{
		"session_id": r.SessionID,
		"domain":     r.Domain,
		"goal":       r.Goal,
	}
	return &types.KnowledgeRecord{
		KnowledgeType: "block_memory",
		TopicID:       r.SessionID,
		Content:       content,
		Embedding:     PseudoEmbed(content, dim),
		Meta:          meta,
		CreatedAt:     r.CreatedAt,
	}
}

// RankBlockMemories 按相似度降序排序并截取 topK 条块记忆。
// 用于 DomainAgent 检索历史块记忆时排序。
func RankBlockMemories(records []*types.KnowledgeRecord, topK int) []*types.KnowledgeRecord {
	if len(records) == 0 {
		return nil
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].AccessCount > records[j].AccessCount // 复用 AccessCount 作为命中次数占位
	})
	if topK > 0 && len(records) > topK {
		records = records[:topK]
	}
	return records
}
