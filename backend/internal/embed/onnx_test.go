//go:build onnx

package embed

// onnx_test.go 验证 TODO #41 进程内 ONNX embedding：
//   - tokenizer（纯 Go WordPiece）无需模型文件，合成 tokenizer.json 即可测；
//   - Embed 侧测试需真实模型文件（models/bge-base-zh-v1.5/），缺失时 t.Skip，
//     不把模型下载变成 CI 前置。
//
// 运行：go test -tags onnx ./internal/embed/（需 onnxruntime 共享库 + 模型文件）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// bgeModelDir 返回模型目录；缺失时跳过 Embed 侧测试（不把模型下载变成测试前置）。
func bgeModelDir(t *testing.T) string {
	t.Helper()
	dir := "models/bge-base-zh-v1.5"
	for _, f := range []string{"model.onnx", "tokenizer.json", "config.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Skipf("模型文件缺失 %s（见 models/README），跳过 Embed 集成测试", filepath.Join(dir, f))
		}
	}
	return dir
}

// TestONNXEmbedder_DimAndNorm 真实模型：Dim()=768、向量 L2 范数≈1（BGE 归一化）。
func TestONNXEmbedder_DimAndNorm(t *testing.T) {
	dir := bgeModelDir(t)
	emb, err := newONNXEmbedder(types.EmbedConfig{Provider: "onnx", ModelPath: dir}, 768)
	if err != nil {
		t.Fatalf("newONNXEmbedder: %v", err)
	}
	if emb.Dim() != 768 {
		t.Fatalf("Dim() = %d, want 768", emb.Dim())
	}
	vec, err := emb.Embed(context.Background(), "塔防游戏怪物路径")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 768 {
		t.Fatalf("vec len = %d, want 768", len(vec))
	}
	norm := 0.0
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm < 0.99 || norm > 1.01 {
		t.Fatalf("L2 norm = %f, want ≈1（BGE 归一化）", norm)
	}
}

// TestONNXEmbedder_SemanticSimilarity 语义召回：相近文本余弦 > 不相关对（pseudo 不过此测）。
func TestONNXEmbedder_SemanticSimilarity(t *testing.T) {
	dir := bgeModelDir(t)
	emb, err := newONNXEmbedder(types.EmbedConfig{Provider: "onnx", ModelPath: dir}, 768)
	if err != nil {
		t.Fatalf("newONNXEmbedder: %v", err)
	}
	cos := func(a, b []float32) float64 {
		s := 0.0
		for i := range a {
			s += float64(a[i]) * float64(b[i])
		}
		return s
	}
	near1, _ := emb.Embed(context.Background(), "塔防游戏怪物路径")
	near2, _ := emb.Embed(context.Background(), "tower defense enemy path")
	far, _ := emb.Embed(context.Background(), "数据库连接池")
	if c := cos(near1, near2); c < 0.4 {
		t.Fatalf("相似文本余弦 = %f, want >= 0.4", c)
	}
	if c := cos(near1, far); c > cos(near1, near2)-0.1 {
		t.Fatalf("不相关对余弦 %f 应显著低于相似对 %f", c, cos(near1, near2))
	}
}

// TestONNXEmbedder_EmptyText 空文本返全零不 panic（与 openai.go 语义一致）。
func TestONNXEmbedder_EmptyText(t *testing.T) {
	dir := bgeModelDir(t)
	emb, err := newONNXEmbedder(types.EmbedConfig{Provider: "onnx", ModelPath: dir}, 768)
	if err != nil {
		t.Fatalf("newONNXEmbedder: %v", err)
	}
	vec, err := emb.Embed(context.Background(), "  ")
	if err != nil {
		t.Fatalf("empty text Embed: %v", err)
	}
	for _, v := range vec {
		if v != 0 {
			t.Fatal("empty text must return zero vector")
		}
	}
}

// TestONNXEmbedder_DimMismatch 维度错配启动报错（hidden_size != pgvector dims）。
func TestONNXEmbedder_DimMismatch(t *testing.T) {
	dir := bgeModelDir(t)
	if _, err := newONNXEmbedder(types.EmbedConfig{Provider: "onnx", ModelPath: dir}, 512); err == nil {
		t.Fatal("dim mismatch must error at construction")
	}
}

// TestONNXEmbedder_MissingModelFiles 模型文件缺失报清晰错误（不静默回退）。
func TestONNXEmbedder_MissingModelFiles(t *testing.T) {
	dir := t.TempDir()
	_, err := newONNXEmbedder(types.EmbedConfig{Provider: "onnx", ModelPath: dir}, 768)
	if err == nil {
		t.Fatal("missing model files must error")
	}
}
