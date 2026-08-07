package tool

// refresh_project_doc.go 提供 RefreshProjectDoc 工具：MetaAgent/DomainAgent 大改动后
// 显式调用，重写 .bma/PROJECT.md 的 managed 区以同步项目概览。
// 标记区外的人手补充保留。无参数。实际扫描逻辑在 internal/project 包。

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/backend/internal/project"
)

// refreshProjectDocTool 是 RefreshProjectDoc 工具的封装。
type refreshProjectDocTool struct{ exec *Executor }

// Name 返回工具标准名称 RefreshProjectDoc。
func (t *refreshProjectDocTool) Name() string { return "RefreshProjectDoc" }

// Aliases 返回 RefreshProjectDoc 的别名列表。
func (t *refreshProjectDocTool) Aliases() []string {
	return []string{"refresh_project_doc", "refreshProjectDoc"}
}

// Description 返回工具的人类可读描述，供 schema 与 UI 展示。
func (t *refreshProjectDocTool) Description() string {
	return "重写 .bma/PROJECT.md 的 managed 区（启发式扫描当前工作目录：模块/语言/命令/" +
		"推荐领域拆分/文档地图）。大改动后调用以同步项目概览。标记区外的人手补充保留。" +
		"无参数。仅 MetaAgent/DomainAgent。"
}

// Execute 调用 project.RefreshProjectDoc 重写 managed 区，返回新正文摘要。
// 注入的 DomainClassifier 非 nil 时调 LLM 语义命名簇；nil 走启发式（永不留空标注）。
func (t *refreshProjectDocTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.exec == nil {
		return &Result{Tool: "RefreshProjectDoc", Error: "executor not configured"}
	}
	wd := t.exec.WorkDir()
	if wd == "" {
		return &Result{Tool: "RefreshProjectDoc", Error: "workDir is empty"}
	}
	// DomainClassifier() 为 nil 时 RefreshProjectDoc 走启发式命名（boot 兼容）。
	if err := project.RefreshProjectDoc(ctx, wd, t.exec.DomainClassifier()); err != nil {
		return &Result{Tool: "RefreshProjectDoc", Error: fmt.Sprintf("refresh: %v", err)}
	}
	body := project.LoadProjectDoc(wd)
	out := fmt.Sprintf("PROJECT.md refreshed at %s\n\n%s", project.ProjectDocPath(wd), body)
	return &Result{Tool: "RefreshProjectDoc", Success: true, Output: out, Path: project.ProjectDocPath(wd)}
}
