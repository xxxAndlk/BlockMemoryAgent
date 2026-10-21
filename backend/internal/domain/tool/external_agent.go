package tool

// external_agent.go 实现 CallExternalAgent 工具（doc/计划_外部Agent调用.md §4.3）：
// 把任务派发给本机已安装的外部 Agent CLI（claude code/codex 等），一次调用 = 一次短命
// 子进程。任务文本经 {{task}} 占位符渲染为独立 argv 元素（exec.CommandContext 数组形式，
// 不经 shell 解析，防注入）；超时整树终止（复用 runCommandWithTreeKill）；
// 错误分类按契约：未注册/未启用/未安装 → validation_rejected，非零退出/超时 → execution_failed。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/config"
)

// ExternalAgentMaxTimeoutSec 调用级超时封顶（秒）。
const ExternalAgentMaxTimeoutSec = 7200

// maxExternalTaskRunes 任务文本软限（超出截断并附警告，防单次调用塞爆上下文）。
const maxExternalTaskRunes = 20000

// ExternalAgentDef 是工具侧的外部 Agent 注册条目（bootstrap 从 config.ExternalAgentEntry 转换注入）。
type ExternalAgentDef struct {
	ID          string   `json:"id"`
	Enabled     bool     `json:"enabled"`
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	TimeoutSec  int      `json:"timeout_sec"`
	Description string   `json:"description"`
}

// externalAgentRegistry 是 CallExternalAgent 的注册表数据源：SetExternalAgentDefs
// 整表热替换（reload），执行与 Schema 描述经快照读取（锁内拷贝，避免撕裂）。
type externalAgentRegistry struct {
	mu   sync.RWMutex
	defs []ExternalAgentDef
}

func newExternalAgentRegistry() *externalAgentRegistry { return &externalAgentRegistry{} }

// Replace 整表替换注册条目（按 ID 排序保证 Schema 描述清单稳定；timeout 归一化兜底）。
func (r *externalAgentRegistry) Replace(defs []ExternalAgentDef) {
	out := append([]ExternalAgentDef(nil), defs...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	for i := range out {
		if out[i].TimeoutSec <= 0 {
			out[i].TimeoutSec = config.ExternalAgentDefaultTimeoutSec
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs = out
}

// Lookup 按 ID 查条目（含 disabled 条目——未启用按契约与未注册同口径拒绝）。
func (r *externalAgentRegistry) Lookup(id string) (ExternalAgentDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.defs {
		if d.ID == id {
			return d, true
		}
	}
	return ExternalAgentDef{}, false
}

// Enabled 返回启用条目快照（Schema 描述清单来源）。
func (r *externalAgentRegistry) Enabled() []ExternalAgentDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ExternalAgentDef, 0, len(r.defs))
	for _, d := range r.defs {
		if d.Enabled {
			out = append(out, d)
		}
	}
	return out
}

// Snapshot 返回全部条目快照（管理 API 列表来源，含 disabled）。
func (r *externalAgentRegistry) Snapshot() []ExternalAgentDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]ExternalAgentDef(nil), r.defs...)
}

// externalAgentTool 把任务派发给外部 Agent CLI 执行。
type externalAgentTool struct {
	exec *Executor
	reg  *externalAgentRegistry
}

func (t *externalAgentTool) Name() string      { return "CallExternalAgent" }
func (t *externalAgentTool) Aliases() []string { return []string{"call_external_agent"} }

// Destructive 返回 true：外部 CLI 副作用不可预期，接入审批链（本机全放行，生产环境恢复审批）。
func (t *externalAgentTool) Destructive() bool { return true }

// callExternalAgentInput 表示 CallExternalAgent 工具的输入参数。
type callExternalAgentInput struct {
	// Agent 为外部 Agent 注册 ID（external_agents.yaml agents 键）。
	Agent string `json:"agent" description:"外部 Agent 注册 ID（见启用清单）"`
	// Task 为派给外部 Agent 的任务文本。
	Task string `json:"task" description:"派给外部 Agent 的任务文本"`
	// TimeoutSec 可选超时（秒），覆盖条目默认，封顶 7200。
	TimeoutSec float64 `json:"timeout_sec,omitempty" description:"可选超时秒数，覆盖条目默认，封顶 7200"`
}

// Execute 执行外部 Agent 调用：查注册表 → LookPath 预检 → 占位符渲染 → 子进程树杀执行。
func (t *externalAgentTool) Execute(ctx context.Context, args map[string]any) *Result {
	res := &Result{Tool: "CallExternalAgent", Path: t.exec.workDirOf(ctx)}
	agentID, _ := args["agent"].(string)
	task, _ := args["task"].(string)
	var timeoutParam float64
	switch v := args["timeout_sec"].(type) {
	case float64:
		timeoutParam = v
	case int:
		timeoutParam = float64(v)
	}
	agentID = strings.TrimSpace(agentID)
	if agentID == "" || strings.TrimSpace(task) == "" {
		res.Success = false
		res.Category = "validation_rejected"
		res.Error = "agent 与 task 均为必填"
		return res
	}
	def, ok := t.reg.Lookup(agentID)
	if !ok || !def.Enabled {
		res.Success = false
		res.Category = "validation_rejected"
		res.Error = fmt.Sprintf("未注册的外部 Agent: %s（可用: %s）", agentID, strings.Join(t.enabledIDs(), ","))
		return res
	}
	if _, err := exec.LookPath(def.Command); err != nil {
		res.Success = false
		res.Category = "validation_rejected"
		res.Error = fmt.Sprintf("未安装: %s（请安装或检查 external_agents.yaml）", def.Command)
		return res
	}
	if runes := utf8.RuneCountInString(task); runes > maxExternalTaskRunes {
		task = string([]rune(task)[:maxExternalTaskRunes])
		res.Output = fmt.Sprintf("[task-truncated] 任务文本超 %d 字软限，已截断\n", maxExternalTaskRunes)
	}
	timeout := clampExternalTimeout(int(timeoutParam), def.TimeoutSec)
	// 渲染 args：{{task}} 占位符替换为任务文本（独立 argv 元素，不经 shell）；
	// 无占位符时任务文本追加为最后一个 argv（config 归一化之外的执行侧兜底）。
	argv := make([]string, 0, len(def.Args)+1)
	replaced := false
	for _, a := range def.Args {
		if a == config.ExternalAgentTaskPlaceholder {
			argv = append(argv, task)
			replaced = true
			continue
		}
		argv = append(argv, a)
	}
	if !replaced {
		argv = append(argv, task)
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, def.Command, argv...)
	cmd.Dir = t.exec.workDirOf(ctx)
	cmd.Env = os.Environ()
	if sessionID := SessionIDFromContext(ctx); sessionID != "" {
		cmd.Env = append(cmd.Env, "BMA_SESSION_TEMP_DIR="+t.exec.sessionTempDir(ctx, sessionID))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := runCommandWithTreeKill(runCtx, cmd)
	output := DecodeCommandOutput(stdout.Bytes())
	if stderr.Len() > 0 {
		output += "\n[stderr]\n" + DecodeCommandOutput(stderr.Bytes())
	}
	res.Output += output
	if err != nil {
		res.Success = false
		res.Category = "execution_failed"
		switch {
		case runCtx.Err() != nil:
			res.Error = fmt.Sprintf("外部 Agent 执行超时（%d 秒），进程树已终止: %s", timeout, agentID)
		case exitErrErr(err):
			res.Error = fmt.Sprintf("外部 Agent 非零退出: %s", agentID)
		default:
			res.Error = err.Error()
		}
		return res
	}
	res.Success = true
	return res
}

// exitErrErr 判定是否子进程非零退出（exit code 走 Output 保留，错误消息保持单行）。
func exitErrErr(err error) bool {
	_, ok := err.(*exec.ExitError)
	return ok
}

// enabledIDs 返回当前启用条目 ID 清单（错误消息的"可用"列表来源）。
func (t *externalAgentTool) enabledIDs() []string {
	defs := t.reg.Enabled()
	ids := make([]string, 0, len(defs))
	for _, d := range defs {
		ids = append(ids, d.ID)
	}
	return ids
}

// clampExternalTimeout 归一化生效超时：调用级 <=0 回落条目默认（再兜底 1800），上限封顶 7200。
func clampExternalTimeout(param, defSec int) int {
	if param <= 0 {
		param = defSec
	}
	if param <= 0 {
		param = config.ExternalAgentDefaultTimeoutSec
	}
	if param > ExternalAgentMaxTimeoutSec {
		return ExternalAgentMaxTimeoutSec
	}
	return param
}

// SetExternalAgentDefs 注入/热更新外部 Agent 注册表（CallExternalAgent 工具数据源）。
// 空/nil = 清空（工具返回未注册错误，Schema 描述显示空清单）。
func (r *Registry) SetExternalAgentDefs(defs []ExternalAgentDef) {
	if r == nil || r.externalAgents == nil {
		return
	}
	r.externalAgents.Replace(defs)
}

// ExternalAgentDefs 返回当前生效的外部 Agent 条目快照（含 disabled；管理 API 用）。
func (r *Registry) ExternalAgentDefs() []ExternalAgentDef {
	if r == nil || r.externalAgents == nil {
		return nil
	}
	return r.externalAgents.Snapshot()
}

// externalAgentSchemaDesc 生成 Schema 描述：动态内嵌当前启用清单（热重载后下轮 Schema 生效）。
func (r *Registry) externalAgentSchemaDesc() string {
	base := "把任务派发给本机已安装的外部 Agent CLI 执行并取回输出（一次调用 = 一次短命子进程，任务文本经独立 argv 元素传入、不经 shell）。agent 为注册 ID（必填），task 为任务文本（必填），timeout_sec 可选（秒，覆盖条目默认，封顶 7200）。当前启用清单："
	if r.externalAgents == nil {
		return base + "（未配置）"
	}
	defs := r.externalAgents.Enabled()
	if len(defs) == 0 {
		return base + "（空——external_agents.yaml 缺失或无启用条目）"
	}
	parts := make([]string, 0, len(defs))
	for _, d := range defs {
		if d.Description != "" {
			parts = append(parts, d.ID+"（"+d.Command+"，"+d.Description+"）")
		} else {
			parts = append(parts, d.ID+"（"+d.Command+"）")
		}
	}
	return base + strings.Join(parts, "; ")
}
