package embed

// bert_tokenizer.go 纯 Go BERT WordPiece tokenizer（TODO #41）：
// 无 build tag、无 cgo 依赖——默认构建即可编译测试（与 onnx.go 的偏离记录见 onnx.go 文件头：
// 未采用 HF tokenizers Rust cgo 绑定，减少一个 cgo/Rust 依赖，编译面收敛到 onnxruntime 单一 cgo）。
// 从 HF tokenizer.json 读取 vocab + 配置驱动（lowercase/strip_accents/pre_tokenizer 类型），
// BGE 系列为标准 BERT WordPiece，纯 Go 实现确定性可测；非 WordPiece 模型加载时报清晰错误。

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// onnxDefaultMaxTokens BGE 系列模型最大序列长度（含 CLS/SEP）；
// onnx.go（tagged）也引用此值作 config.json 缺失时的兜底。
const onnxDefaultMaxTokens = 512

// bertWordPiece 从 tokenizer.json 加载的 BERT WordPiece tokenizer。
type bertWordPiece struct {
	vocab        map[string]int32
	unkID        int32
	clsID        int32
	sepID        int32
	padID        int32
	prefix       string // continuing_subword_prefix，通常 "##"
	lowercase    bool
	stripAccents bool
}

// loadBertWordPiece 解析 HF tokenizer.json 的 WordPiece 模型。
// 非 WordPiece（ByteLevel/Unigram 等）报清晰错误，不静默错分。
func loadBertWordPiece(path string) (*bertWordPiece, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Model struct {
			Type                    string         `json:"type"`
			Vocab                   map[string]int `json:"vocab"`
			UnkToken                string         `json:"unk_token"`
			ContinuingSubwordPrefix string         `json:"continuing_subword_prefix"`
		} `json:"model"`
		Normalizer *struct {
			Type            string `json:"type"`
			Lowercase       *bool  `json:"lowercase"`
			StripAccents    *bool  `json:"strip_accents"`
			HandleChinese   *bool  `json:"handle_chinese_chars"`
		} `json:"normalizer"`
		PreTokenizer *struct {
			Type string `json:"type"`
		} `json:"pre_tokenizer"`
		AddedTokens []struct {
			ID      int    `json:"id"`
			Content string `json:"content"`
		} `json:"added_tokens"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析 tokenizer.json: %w", err)
	}
	if doc.Model.Type != "WordPiece" {
		return nil, fmt.Errorf("不支持的 tokenizer 类型 %q（TODO #41 仅支持 WordPiece/BERT 系）", doc.Model.Type)
	}
	if doc.PreTokenizer != nil && doc.PreTokenizer.Type != "BertPreTokenizer" {
		return nil, fmt.Errorf("不支持的 pre_tokenizer 类型 %q（仅 BertPreTokenizer）", doc.PreTokenizer.Type)
	}

	b := &bertWordPiece{
		vocab:  make(map[string]int32, len(doc.Model.Vocab)),
		prefix: "##",
	}
	for w, id := range doc.Model.Vocab {
		b.vocab[w] = int32(id)
	}
	// 特殊 token：vocab 优先，回退 added_tokens。
	idOf := func(s string) int32 {
		if id, ok := b.vocab[s]; ok {
			return id
		}
		for _, t := range doc.AddedTokens {
			if t.Content == s {
				return int32(t.ID)
			}
		}
		return -1
	}
	if b.unkID = idOf("[UNK]"); b.unkID < 0 {
		return nil, fmt.Errorf("tokenizer.json 缺 [UNK]")
	}
	if b.clsID = idOf("[CLS]"); b.clsID < 0 {
		return nil, fmt.Errorf("tokenizer.json 缺 [CLS]")
	}
	if b.sepID = idOf("[SEP]"); b.sepID < 0 {
		return nil, fmt.Errorf("tokenizer.json 缺 [SEP]")
	}
	if b.padID = idOf("[PAD]"); b.padID < 0 {
		b.padID = b.unkID
	}
	if doc.Model.ContinuingSubwordPrefix != "" {
		b.prefix = doc.Model.ContinuingSubwordPrefix
	}
	if doc.Normalizer != nil {
		if doc.Normalizer.Lowercase != nil {
			b.lowercase = *doc.Normalizer.Lowercase
		}
		if doc.Normalizer.StripAccents != nil {
			b.stripAccents = *doc.Normalizer.StripAccents
		}
	}
	return b, nil
}

// Encode 把文本编码为 token id 序列：[CLS] + tokens + [SEP]，截断到 maxLen。
func (b *bertWordPiece) Encode(text string, maxLen int) []int32 {
	if maxLen <= 2 {
		maxLen = onnxDefaultMaxTokens
	}
	// 归一化（小写/去变音符号，按 tokenizer.json 配置）。
	t := text
	if b.lowercase {
		t = strings.Map(unicode.ToLower, t)
	}
	if b.stripAccents {
		t = stripAccents(t)
	}
	// 预切分 → 各词 WordPiece → 组装；超长截断（[SEP] 留位，截断后仍以 [SEP] 收尾）。
	ids := []int32{b.clsID}
	for _, word := range basicTokenize(t) {
		for _, id := range b.wordPiece(word) {
			if len(ids) >= maxLen-1 { // 给 [SEP] 留位
				return append(ids, b.sepID)
			}
			ids = append(ids, id)
		}
	}
	return append(ids, b.sepID)
}

// wordPiece 对单个词做贪心最长前缀匹配；任一字无法匹配时整词回退 [UNK]（BERT 语义）。
func (b *bertWordPiece) wordPiece(word string) []int32 {
	runes := []rune(word)
	var pieces []int32
	start := 0
	for start < len(runes) {
		end := len(runes)
		matched := false
		for {
			sub := string(runes[start:end])
			key := sub
			if start > 0 {
				key = b.prefix + sub
			}
			if id, ok := b.vocab[key]; ok {
				pieces = append(pieces, id)
				matched = true
				break
			}
			end--
			if end == start {
				break
			}
		}
		if !matched {
			// 整词 UNK（BERT 规则：任一字不可分即整词回退）。
			return []int32{b.unkID}
		}
		start = end
	}
	return pieces
}

// basicTokenize BERT BasicTokenizer 语义的预切分：
// 空白切分；标点与 CJK 字符各自独立成 token（中文无空格，须逐字）。
func basicTokenize(text string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			flush()
		case isPunct(r) || isCJK(r):
			flush()
			out = append(out, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// isCJK 判断是否 CJK 表意字符（BERT BasicTokenizer 逐字切分的范围）。
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
		(r >= 0x3400 && r <= 0x4DBF) || // Extension A
		(r >= 0xF900 && r <= 0xFAFF) || // Compatibility Ideographs
		(r >= 0x2E80 && r <= 0x2EFF) || // CJK Radicals
		(r >= 0x3040 && r <= 0x30FF) || // Hiragana + Katakana
		(r >= 0x31F0 && r <= 0x31FF) // Katakana Phonetic Extensions
}

// isPunct BERT BasicTokenizer 的标点判定：ASCII 标点区间 + CJK 标点。
func isPunct(r rune) bool {
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	switch {
	case r >= 0x3001 && r <= 0x3011: // 、。〃《》「」『』【】等
		return true
	case r >= 0x3014 && r <= 0x301F:
		return true
	case r >= 0xFF01 && r <= 0xFF0F, r >= 0xFF1A && r <= 0xFF1F, r >= 0xFF3B && r <= 0xFF3D, r >= 0xFF5B && r <= 0xFF65:
		return true // 全角标点
	}
	return false
}

// stripAccents 去除组合变音符号（NFD 归一化后删 Mn 类字符）。
func stripAccents(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

