//go:build onnx

package embed

// onnx.go 实现 TODO #41 进程内本地向量模型：ONNX Runtime 真本地 embedding。
//
// 与 openai/local（本地 HTTP 服务）的本质区别：模型权重进 Go 进程内存，
// 零 api_key、零网络、零外部服务进程。Embed 调用即内存推理（CPU 单条 ~10ms 级）。
//
// 依赖与构建：
//   - cgo 依赖 github.com/yalue/onnxruntime_go（官方 onnxruntime 共享库封装）；
//     编译期需要 onnxruntime 库（Windows: onnxruntime.dll，Linux: libonnxruntime.so，
//     macOS: libonnxruntime.dylib），放 models/lib/ 或系统 PATH，启动 dlopen；
//   - 本文件带 //go:build onnx 标签：默认构建不含（provider=onnx 走 onnx_stub.go
//     编译期拒绝并指引），`-tags onnx` 启用真实现；
//   - 模型文件不入 git（~400MB）：models/<Model>/ 下需 model.onnx + tokenizer.json +
//     config.json，缺失时启动 strict 报清晰错误，不静默回退 pseudo。
//
// 与 TODO 计划的一处偏离（变更记录）：
//   - tokenizer 未采用 HF tokenizers Rust cgo 绑定，改为纯 Go BERT WordPiece 实现
//     （bert_tokenizer.go，无 build tag 无 cgo，默认构建即可编译测试）——减少一个
//     cgo/Rust 依赖，编译面收敛到 onnxruntime 单一 cgo；BGE 系列为标准 BERT WordPiece，
//     纯 Go 实现确定性可测；若遇非 WordPiece 模型（ByteLevel 等）在加载时报
//     "unsupported tokenizer" 清晰错误而非静默错分。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/yalue/onnxruntime_go"
)

// onnxDefaultHidden BGE-base 系列隐藏维度兜底（config.json 优先）。
const onnxDefaultHidden = 768

// ONNXEmbedder 进程内 ONNX Runtime 文本嵌入器。
type ONNXEmbedder struct {
	mu       sync.Mutex // ONNX Runtime session 非线程安全，推理串行化
	session  *onnxruntime_go.AdvancedSession
	tok      *bertWordPiece
	dim      int // 输出向量维度（= hidden_size，须与 pgvector.dimensions 一致）
	maxTokens int

	// 输入/输出 tensor 构造时固定形状，每次调用原位更新数据后 Run（避免重分配）。
	inputIDs      *onnxruntime_go.Tensor[int64]
	attentionMask *onnxruntime_go.Tensor[int64]
	tokenTypeIDs  *onnxruntime_go.Tensor[int64]
	lastHidden    *onnxruntime_go.Tensor[float32]
}

// newONNXEmbedder 加载 ONNX 模型 + tokenizer，构造进程内嵌入器。
// 启动 strict：模型文件缺失/维度错配/加载失败均返回错误（上层 panic 中止启动），
// 不静默回退 pseudo。
func newONNXEmbedder(cfg types.EmbedConfig, dim int) (Embedder, error) {
	modelPath := cfg.ModelPath
	if modelPath == "" {
		modelPath = filepath.Join("models", cfg.Model)
	}
	onnxFile := filepath.Join(modelPath, "model.onnx")
	tokFile := filepath.Join(modelPath, "tokenizer.json")
	cfgFile := filepath.Join(modelPath, "config.json")

	// 必要文件存在性校验：缺失报清晰错误（含下载指引），不静默降级。
	for _, f := range []string{onnxFile, tokFile, cfgFile} {
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("onnx embedder: 缺少 %s（模型未下载，见 models/README：HuggingFace BAAI/bge-base-zh-v1.5 的 onnx/ 目录）: %w", f, err)
		}
	}

	tok, err := loadBertWordPiece(tokFile)
	if err != nil {
		return nil, fmt.Errorf("onnx embedder: 加载 tokenizer: %w", err)
	}

	// 模型结构信息：hidden_size / max_position_embeddings（config.json 权威）。
	hidden, maxTokens, err := readOnnxConfig(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("onnx embedder: 读 config.json: %w", err)
	}
	if dim > 0 && hidden != dim {
		return nil, fmt.Errorf("onnx embedder: 维度错配 hidden_size=%d != pgvector.dimensions=%d（换模型需同步改配置并重建索引）", hidden, dim)
	}

	// 初始化 ONNX Runtime 环境（幂等；进程退出由最终用户进程回收）。
	if err := onnxruntime_go.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("onnx embedder: onnxruntime init: %w（确认 onnxruntime 共享库在 PATH 或 models/lib/）", err)
	}

	// 固定形状 tensor：序列按 maxTokens 填充（动态长度经 attention_mask 表达）。
	seq := int64(maxTokens)
	inputs := []onnxruntime_go.Value{}
	inputIDs, err := onnxruntime_go.NewTensor[int64](onnxruntime_go.Shape{1, seq}, make([]int64, maxTokens))
	if err != nil {
		return nil, fmt.Errorf("onnx embedder: 创建 input_ids tensor: %w", err)
	}
	inputs = append(inputs, inputIDs)
	attentionMask, err := onnxruntime_go.NewTensor[int64](onnxruntime_go.Shape{1, seq}, make([]int64, maxTokens))
	if err != nil {
		return nil, fmt.Errorf("onnx embedder: 创建 attention_mask tensor: %w", err)
	}
	inputs = append(inputs, attentionMask)
	tokenTypeIDs, err := onnxruntime_go.NewTensor[int64](onnxruntime_go.Shape{1, seq}, make([]int64, maxTokens))
	if err != nil {
		return nil, fmt.Errorf("onnx embedder: 创建 token_type_ids tensor: %w", err)
	}
	inputs = append(inputs, tokenTypeIDs)

	lastHidden, err := onnxruntime_go.NewEmptyTensor[float32](onnxruntime_go.Shape{1, seq, int64(hidden)})
	if err != nil {
		return nil, fmt.Errorf("onnx embedder: 创建 last_hidden_state tensor: %w", err)
	}
	outputs := []onnxruntime_go.Value{lastHidden}

	session, err := onnxruntime_go.NewAdvancedSession(onnxFile,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"},
		inputs, outputs, nil)
	if err != nil {
		return nil, fmt.Errorf("onnx embedder: 加载模型会话: %w（确认模型为 BERT 类输入名 input_ids/attention_mask/token_type_ids，输出 last_hidden_state）", err)
	}

	return &ONNXEmbedder{
		session:       session,
		tok:           tok,
		dim:           hidden,
		maxTokens:     maxTokens,
		inputIDs:      inputIDs,
		attentionMask: attentionMask,
		tokenTypeIDs:  tokenTypeIDs,
		lastHidden:    lastHidden,
	}, nil
}

// Embed 单条文本编码：tokenize → 填充固定形状 tensor → 推理 → mean pooling → L2 归一化。
// 空文本返全零（与 openai.go 一致）。
func (e *ONNXEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if strings.TrimSpace(text) == "" {
		return make([]float32, e.dim), nil
	}
	ids := e.tok.Encode(text, e.maxTokens)

	e.mu.Lock()
	defer e.mu.Unlock()

	// 原位更新输入数据（padding 补 0，attention_mask 标 1 的真实位）。
	idsData := e.inputIDs.GetData()
	maskData := e.attentionMask.GetData()
	ttData := e.tokenTypeIDs.GetData()
	for i := range idsData {
		idsData[i] = 0
		maskData[i] = 0
		ttData[i] = 0
	}
	for i, id := range ids {
		idsData[i] = id
		maskData[i] = 1
	}

	if err := e.session.Run(); err != nil {
		return nil, fmt.Errorf("onnx embedder: 推理: %w", err)
	}

	// mean pooling：对 attention_mask==1 的位置求 last_hidden_state 平均（BGE 不用 CLS）。
	out := e.lastHidden.GetData()
	sum := make([]float64, e.dim)
	count := 0
	for t := 0; t < len(ids); t++ {
		if maskData[t] == 0 {
			continue
		}
		base := t * e.dim
		for d := 0; d < e.dim; d++ {
			sum[d] += float64(out[base+d])
		}
		count++
	}
	vec := make([]float32, e.dim)
	if count == 0 {
		return vec, nil
	}
	norm := 0.0
	for d := 0; d < e.dim; d++ {
		v := float32(sum[d] / float64(count))
		vec[d] = v
		norm += float64(v) * float64(v)
	}
	// L2 归一化：BGE 要求归一化后使用（余弦相似度），pgvector cosine 依赖。
	if norm > 0 {
		inv := float32(1 / math.Sqrt(norm))
		for d := range vec {
			vec[d] *= inv
		}
	}
	return vec, nil
}

// Dim 返回模型固有输出维度。
func (e *ONNXEmbedder) Dim() int { return e.dim }

// readOnnxConfig 从 config.json 读取 hidden_size 与 max_position_embeddings。
// 缺失字段用 BGE 系列默认值（768/512），不阻塞加载。
func readOnnxConfig(path string) (hidden, maxTokens int, err error) {
	hidden, maxTokens = onnxDefaultHidden, onnxDefaultMaxTokens
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var cfg struct {
		HiddenSize             int `json:"hidden_size"`
		MaxPositionEmbeddings  int `json:"max_position_embeddings"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return 0, 0, fmt.Errorf("解析 %s: %w", path, err)
	}
	if cfg.HiddenSize > 0 {
		hidden = cfg.HiddenSize
	}
	if cfg.MaxPositionEmbeddings > 0 {
		maxTokens = cfg.MaxPositionEmbeddings
	}
	return hidden, maxTokens, nil
}
