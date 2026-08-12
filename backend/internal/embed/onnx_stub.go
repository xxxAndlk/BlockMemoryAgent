//go:build !onnx

package embed

// onnx_stub.go 是无 onnx build tag 时的编译期 stub（TODO #41）：
// provider=onnx 需要 `-tags onnx` 构建（cgo 依赖 onnxruntime + 模型文件），
// 默认构建编译期即拒绝，错误信息明确指引，避免运行时才发现未编译。
//
// 注意：stub 与 onnx.go（//go:build onnx）定义同名 newONNXEmbedder，
// 两文件互斥编译，保证 embedder.go 分发点无感知构建差异。

import (
	"fmt"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// newONNXEmbedder 返回明确错误：需 -tags onnx 构建。
func newONNXEmbedder(cfg types.EmbedConfig, dim int) (Embedder, error) {
	return nil, fmt.Errorf("embed provider=onnx 需要 -tags onnx 构建（进程内 ONNX Runtime 推理，见 doc/TODO.md #41；模型文件见 models/README）")
}
