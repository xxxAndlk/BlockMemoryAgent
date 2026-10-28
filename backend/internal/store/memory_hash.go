package store

// memory_hash.go 长期记忆内容归一化 hash（2026-10-24 长期记忆撤回与污染修复）。
//
// 用途：撤回墓碑（tombstone）与"同内容换排版"候选的判定。同一句话换了空格/标点/
// 大小写，hash 必须一致——否则用户撤回的条目会被自动沉淀"换一种排版"写回来。
//
// 归一化只做保守处理（空白、常见中英标点、引号括号、大小写），不做同义词改写：
// 语义近似由 agent 侧 resurrection_suspect 检查承载，不在这里猜。

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// MemoryContentHash 规范化文本 hash。
func MemoryContentHash(content string) string {
	return hashNormalizedText(content)
}

// normalizeMemoryText 归一化文本：去空白/标点/符号并小写。
func normalizeMemoryText(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if isMemoryIgnorableRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isMemoryIgnorableRune 判断该字符是否属于"排版噪声"（不作为内容差异）。
func isMemoryIgnorableRune(r rune) bool {
	switch r {
	// 空白
	case ' ', '\t', '\r', '\n', '\u3000':
		return true
	// 中英文标点
	case '\u3001', '\u3002', '\uff0c', '\uff1b', '\uff1a', '\uff01', '\uff1f',
		',', '.', ';', ':', '!', '?':
		return true
	// 引号与括号
	case '"', '\'', '`', '\u2018', '\u2019', '\u201c', '\u201d',
		'(', ')', '[', ']', '{', '}', '\uff08', '\uff09',
		'\u300a', '\u300b', '\u3010', '\u3011':
		return true
	// 符号
	case '-', '_', '*', '#', '>', '~', '/', '\\', '|', '+', '=', '@', '$', '%', '^', '&':
		return true
	}
	return false
}

// hashNormalizedText 归一化文本后取 sha256（十六进制）。
func hashNormalizedText(s string) string {
	sum := sha256.Sum256([]byte(normalizeMemoryText(s)))
	return hex.EncodeToString(sum[:])
}

// MemoryTextSimilarity 粗粒度相似度（0~1）：按归一化后的字符二元组 Jaccard 系数。
// 仅用于 agent 侧 resurrection_suspect 的保守判定（阈值由调用方定），
// 不作为"是否同一条记忆"的唯一依据。
func MemoryTextSimilarity(a, b string) float64 {
	x, y := normalizeMemoryText(a), normalizeMemoryText(b)
	if x == "" || y == "" {
		return 0
	}
	if x == y {
		return 1
	}
	bi := func(s string) map[string]bool {
		rs := []rune(s)
		out := make(map[string]bool, len(rs))
		if len(rs) == 1 {
			out[string(rs)] = true
			return out
		}
		for i := 0; i+1 < len(rs); i++ {
			out[string(rs[i:i+2])] = true
		}
		return out
	}
	ga, gb := bi(x), bi(y)
	if len(ga) == 0 || len(gb) == 0 {
		return 0
	}
	inter := 0
	for g := range ga {
		if gb[g] {
			inter++
		}
	}
	union := len(ga) + len(gb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
