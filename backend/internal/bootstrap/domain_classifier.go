package bootstrap

// domain_classifier.go 实现 project.DomainClassifier：用轻量模型读文件内容样本，
// 按职责/实体把文件分区成领域。由 bootstrap 注入到 tool.Registry（RefreshProjectDoc 工具）
// 与 ReactService session store（EnsureProjectDoc 首生成）。失败/超限/未覆盖文件由调用方
// 回退到 project 包的依赖图启发式兜底（clusterByDeps），永不阻塞、永不留空标注。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/project"
)

const (
	// maxPartitionFiles 参与 LLM 分区的文件数上限。超限返 nil 让调用方走启发式兜底，
	// 避免单 prompt 过大拖慢首 session。大仓库退依赖图 + 文件夹兜底。
	maxPartitionFiles = 200
	// maxSampleBytes 单文件内容样本上限（首部），够看职责/实体类名/导出符号即可。
	maxSampleBytes = 4096
)

// llmDomainClassifier 用轻量模型读文件样本按职责分区。
type llmDomainClassifier struct {
	factory *model.ModelFactory
}

// Partition 读各文件样本，调轻量模型按职责/实体分区。超限返 (nil,nil) 让调用方走启发式兜底。
func (c *llmDomainClassifier) Partition(ctx context.Context, root string, files []string) ([]project.DomainPartition, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) > maxPartitionFiles {
		return nil, nil
	}
	prompt, err := buildPartitionPrompt(root, files)
	if err != nil {
		return nil, fmt.Errorf("build partition prompt: %w", err)
	}
	resp, err := c.factory.CallLightweightWithRetry(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("call lightweight: %w", err)
	}
	parts, err := project.ParsePartitionJSON(resp)
	if err != nil {
		return nil, fmt.Errorf("parse partitions: %w", err)
	}
	return parts, nil
}

// fileSample 读文件首 maxSampleBytes 字节作职责判断样本。读失败返空串（仍给路径）。
func fileSample(root, rel string) string {
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return ""
	}
	if len(data) > maxSampleBytes {
		data = data[:maxSampleBytes]
	}
	return string(data)
}

// buildPartitionPrompt 构造按职责分区提示词：发 [{path, sample}]，要求返 [{name,purpose,files}]。
func buildPartitionPrompt(root string, files []string) (string, error) {
	type fileEntry struct {
		Path   string `json:"path"`
		Sample string `json:"sample"`
	}
	entries := make([]fileEntry, len(files))
	for i, f := range files {
		entries[i] = fileEntry{Path: f, Sample: fileSample(root, f)}
	}
	payload, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`把以下代码文件按职责/实体领域分区。同领域 = 同职责或同实体类，跨文件夹亦可同域；越细越好但同职责不拆；每个文件恰好归一域。

文件（path 为相对路径，sample 为文件开头内容样本）:
%s

要求:
- 领域名简短（2-6 字），体现业务/功能语义，如"游戏运行时""炮塔实体""用户认证""数据持久化"，不要用文件名或目录名
- 职责一句话，不超过 30 字
- 输出 JSON 数组，每元素 {"name":"...","purpose":"...","files":["相对路径",...]}，files 必须取自输入 path
- 每个输入文件恰好出现在一个分区的 files 中
- 不要输出任何其他文本、不要 markdown 围栏`, string(payload)), nil
}

// 编译期保证接口实现。
var _ project.DomainClassifier = (*llmDomainClassifier)(nil)
