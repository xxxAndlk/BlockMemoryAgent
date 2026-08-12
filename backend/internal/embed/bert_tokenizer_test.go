package embed

// bert_tokenizer_test.go 纯 Go WordPiece tokenizer 测试（默认构建可跑，无需 onnx 模型）：
// 合成 tokenizer.json 验证加载/编码/子词/未知词/截断/非 WordPiece 拒绝。

import (
	"os"
	"path/filepath"
	"testing"
)

// miniTokenizerJSON 构造一个最小的 BERT WordPiece tokenizer.json。
// 词表覆盖：[UNK] [CLS] [SEP] [PAD]、单字（塔/防/游/戏）、子词（##play）、整词（##er）。
func miniTokenizerJSON() string {
	return `{
  "model": {
    "type": "WordPiece",
    "vocab": {
      "[UNK]": 0, "[CLS]": 1, "[SEP]": 2, "[PAD]": 3,
      "塔": 4, "防": 5, "游": 6, "戏": 7, "the": 8, "##play": 9, "##er": 10, "play": 11
    },
    "unk_token": "[UNK]",
    "continuing_subword_prefix": "##"
  },
  "normalizer": {"type": "BertNormalizer", "lowercase": true, "strip_accents": null},
  "pre_tokenizer": {"type": "BertPreTokenizer"}
}`
}

func writeMiniTokenizer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "tokenizer.json")
	if err := os.WriteFile(p, []byte(miniTokenizerJSON()), 0o644); err != nil {
		t.Fatalf("write tokenizer.json: %v", err)
	}
	return p
}

// TestBertWordPiece_LoadAndEncode 合成词表下的加载与编码语义：
// [CLS] 塔 防 游 戏 [SEP]；子词匹配（##play + ##er）；未知词整词 [UNK]。
func TestBertWordPiece_LoadAndEncode(t *testing.T) {
	tok, err := loadBertWordPiece(writeMiniTokenizer(t))
	if err != nil {
		t.Fatalf("loadBertWordPiece: %v", err)
	}

	// 中文逐字切分（CJK 无空格，basicTokenize 逐字隔离）。
	ids := tok.Encode("塔防游戏", 512)
	want := []int32{1, 4, 5, 6, 7, 2} // [CLS] 塔 防 游 戏 [SEP]
	if len(ids) != len(want) {
		t.Fatalf("Encode(塔防游戏) = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("Encode(塔防游戏) = %v, want %v", ids, want)
		}
	}

	// 子词合并：play + ##er → player（lowercase 后）。
	ids = tok.Encode("Player", 512)
	if len(ids) != 4 || ids[1] != 11 || ids[2] != 10 {
		t.Fatalf("Encode(Player) = %v, want [CLS] play ##er [SEP]", ids)
	}

	// 未知词整词 [UNK]（BERT 语义）。
	ids = tok.Encode("xyzzy", 512)
	if len(ids) != 3 || ids[1] != 0 {
		t.Fatalf("Encode(xyzzy) = %v, want [CLS] [UNK] [SEP]", ids)
	}
}

// TestBertWordPiece_Truncation 截断到 maxLen（[SEP] 留位）。
func TestBertWordPiece_Truncation(t *testing.T) {
	tok, err := loadBertWordPiece(writeMiniTokenizer(t))
	if err != nil {
		t.Fatalf("loadBertWordPiece: %v", err)
	}
	ids := tok.Encode("塔防游戏塔防游戏塔防游戏", 6)
	if len(ids) != 6 {
		t.Fatalf("Encode truncation = %v (len %d), want 6", ids, len(ids))
	}
	if ids[0] != 1 || ids[len(ids)-1] != 2 {
		t.Fatalf("truncated sequence must keep [CLS]/[SEP], got %v", ids)
	}
}

// TestBertWordPiece_RejectNonWordPiece 非 WordPiece tokenizer 报清晰错误。
func TestBertWordPiece_RejectNonWordPiece(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tokenizer.json")
	bad := `{"model": {"type": "ByteLevel", "vocab": {}}, "pre_tokenizer": {"type": "ByteLevel"}}`
	if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadBertWordPiece(p); err == nil {
		t.Fatal("non-WordPiece tokenizer must be rejected")
	}
}

