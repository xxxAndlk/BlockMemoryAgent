package team

// acceptance_runner.go 合同验收的真实证据采集（Harness 根治计划 WP3 §7.2 的 L1/L2 层，
// 2026-11-19 验收报告"证据采集未接线"的返修）。
//
// 分层职责（必须分清，否则又会退化成"纯函数单测当验收通过"）：
//   - 判定（L0）：acceptance_eval.go 的 EvaluateVerification——零 IO、可确定性测试；
//   - 采集（L1/L2/L3）：本文件——真实执行构建/单测/集成命令，真实探查 DOM，
//     把结果封装成带 Kind/ID/任务绑定/采集器/产物引用的 Evidence。
//
// 纪律：
//   - 证据必须与规则同 Kind（I-VERIFY-1），类型不匹配只能算缺口；
//   - 能力缺失（浏览器未就绪、prober 未注入）是**环境性缺口**，不是通过也不是代码失败；
//   - 命令输出只留尾部（审计够用、不进模型上下文）；
//   - 采集器抛错不得让验收"静默通过"：失败一律落成失败或缺口证据。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxEvidenceTail 单条证据保留的输出尾部长度（够定位失败，不刷屏）。
const maxEvidenceTail = 2000

// CommandResult 一条命令的执行结果。
type CommandResult struct {
	ExitCode int
	Output   string
	// EnvironmentError 环境性失败（可执行文件缺失/权限/磁盘占用/超时）：
	// 与"命令跑完但非 0"区分开（R-25）。
	EnvironmentError bool
	// Detail 环境性失败的原因。
	Detail string
}

// CommandExecutor 执行验收命令（生产实现走 os/exec；测试注入确定性桩）。
type CommandExecutor func(ctx context.Context, workDir, command string, timeout time.Duration) CommandResult

// DOMProber 采集真实 DOM 几何证据（生产可接 Playwright/浏览器桥；未注入=能力缺失）。
type DOMProber interface {
	// Probe 返回该规则对应的证据；error 表示采集失败（按环境性缺口处理）。
	Probe(ctx context.Context, workDir string, rule AcceptanceRule) (Evidence, error)
}

// VisualProber 采集截图类证据。
type VisualProber interface {
	Probe(ctx context.Context, workDir string, rule AcceptanceRule) (Evidence, error)
}

// AcceptanceRunner 收集证据并判定（判定复用 EvaluateVerification）。
type AcceptanceRunner struct {
	// Exec 命令执行器（nil = 命令类规则只能记缺口，绝不当作通过）。
	Exec CommandExecutor
	// DOM/Visual 真实视觉证据采集器（nil = 该层能力缺失，只能 unverified）。
	DOM    DOMProber
	Visual VisualProber
	// Timeout 单条规则默认超时（<=0 用 10 分钟）。
	Timeout time.Duration
}

// commandSpec 命令类规则的 spec 形状：{"cmd":"npm run build"} 或 {"cmd":["a","b"]}。
type commandSpec struct {
	Cmd any    `json:"cmd"`
	Cwd string `json:"cwd,omitempty"`
}

// Collect 采集合同全部规则的证据（按规则顺序，确定性）。
func (r *AcceptanceRunner) Collect(ctx context.Context, c *LogicalTaskContract, workDir string) []Evidence {
	if c == nil {
		return nil
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	out := make([]Evidence, 0, len(c.Acceptance))
	for _, rule := range c.Acceptance {
		out = append(out, r.collectRule(ctx, c, rule, workDir, timeout))
	}
	return out
}

// Verify 采集 + 判定一步完成（生产入口；Invs 为空表示不做不变量校验 → 只能 unverified）。
func (r *AcceptanceRunner) Verify(ctx context.Context, c *LogicalTaskContract, workDir string) VerificationVerdict {
	evidence := r.Collect(ctx, c, workDir)
	invariants := r.collectInvariants(c, workDir)
	return EvaluateVerification(c, invariants, evidence)
}

// collectInvariants 按不变量种类产出校验结果。
//
// 当前可确定性校验的种类：file_hash（真实读盘比对）。DOM 类需要 prober，
// 未注入时返回 Skipped（记缺口，绝不当作通过）。text_range/ast_node 同理。
func (r *AcceptanceRunner) collectInvariants(c *LogicalTaskContract, workDir string) []InvariantResult {
	if c == nil {
		return nil
	}
	invs := c.ActiveInvariants()
	out := make([]InvariantResult, 0, len(invs))
	for _, inv := range invs {
		switch inv.Kind {
		case InvariantKindFileHash:
			ok, observed := verifyFileHash(inv, workDir)
			out = append(out, InvariantResult{ID: inv.ID, OK: ok, Observed: observed})
		case InvariantKindDOMMetric:
			if r.DOM == nil {
				out = append(out, InvariantResult{ID: inv.ID, Skipped: true, Observed: "DOM 采集器未注入（能力缺失）"})
				continue
			}
			// DOM 不变量由 DOM 规则证据承载（避免重复实现几何计算）。
			out = append(out, InvariantResult{ID: inv.ID, Skipped: true,
				Observed: "DOM 不变量需由采集器提供几何结果（当前未接线）"})
		default:
			out = append(out, InvariantResult{ID: inv.ID, Skipped: true,
				Observed: "不变量种类 " + inv.Kind + " 尚无确定性采集器"})
		}
	}
	return out
}

// verifyFileHash 校验 file_hash 不变量：expected.* / expected.file / expected.matches。
func verifyFileHash(inv Invariant, workDir string) (bool, string) {
	var spec struct {
		File string `json:"file"`
		Hash string `json:"hash"`
	}
	if len(inv.Expected) > 0 {
		_ = json.Unmarshal(inv.Expected, &spec)
	}
	file := spec.File
	if file == "" {
		file = inv.Target.File
	}
	if file == "" {
		return false, "file_hash 不变量缺少目标文件"
	}
	if spec.Hash == "" {
		return false, "file_hash 不变量缺少期望 hash"
	}
	actual, err := hashFileHex(workDir, file)
	if err != nil {
		return false, "读取失败：" + err.Error()
	}
	if actual != spec.Hash {
		short := spec.Hash
		if len(short) > 12 {
			short = short[:12]
		}
		got := actual
		if len(got) > 12 {
			got = got[:12]
		}
		return false, fmt.Sprintf("%s 内容已变（期望 %s… 实际 %s…）", file, short, got)
	}
	return true, file + " 未变"
}

// collectRule 单条规则的证据采集。
func (r *AcceptanceRunner) collectRule(ctx context.Context, c *LogicalTaskContract,
	rule AcceptanceRule, workDir string, timeout time.Duration) Evidence {
	ev := Evidence{
		Kind:         rule.Kind,
		ID:           rule.ID,
		TaskID:       c.LogicalTaskID,
		Revision:     c.Revision,
		ContractHash: c.ContentHash,
	}
	switch rule.Kind {
	case EvidenceStatic, EvidenceUnit, EvidenceIntegration:
		return r.collectCommand(ctx, ev, rule, workDir, timeout)
	case EvidenceDOM:
		return r.collectProber(ctx, ev, rule, workDir, r.DOM, "dom-prober")
	case EvidenceVisual:
		return r.collectProber(ctx, ev, rule, workDir, r.Visual, "visual-prober")
	case EvidenceAPI:
		// API schema 校验同样走命令（curl/schema 校验脚本）；未配命令 → 缺口。
		return r.collectCommand(ctx, ev, rule, workDir, timeout)
	default:
		if rule.Kind == EvidenceManual {
			ev.Detail = "人工判定需用户确认（不是机器证据）"
			return ev
		}
		ev.Detail = "未知验收规则种类：" + rule.Kind
		return ev
	}
}

// collectCommand 执行命令类规则。
func (r *AcceptanceRunner) collectCommand(ctx context.Context, ev Evidence, rule AcceptanceRule,
	workDir string, timeout time.Duration) Evidence {
	var spec commandSpec
	if len(rule.Spec) > 0 {
		_ = json.Unmarshal(rule.Spec, &spec)
	}
	command := commandFrom(spec.Cmd)
	if command == "" {
		ev.Detail = "规则未配置可执行命令（spec.cmd 为空）"
		return ev
	}
	if r.Exec == nil {
		ev.Detail = "命令执行器未注入（能力缺失）"
		ev.EnvironmentError = true
		return ev
	}
	dir := workDir
	if spec.Cwd != "" {
		dir = workDir + "/" + strings.TrimPrefix(spec.Cwd, "/")
	}
	ev.Collector = "command"
	res := r.Exec(ctx, dir, command, timeout)
	ev.Passed = !res.EnvironmentError && res.ExitCode == 0
	ev.EnvironmentError = res.EnvironmentError
	ev.Detail = fmt.Sprintf("cmd=%s exit=%d", command, res.ExitCode)
	if res.EnvironmentError && res.Detail != "" {
		ev.Detail += " 环境问题：" + res.Detail
	}
	if tail := tailString(res.Output, maxEvidenceTail); tail != "" {
		ev.Artifact = "command-output"
		ev.Detail += "\n" + tail
	}
	return ev
}

// collectProber 用注入的 prober 采集证据；未注入时记为环境性缺口。
func (r *AcceptanceRunner) collectProber(ctx context.Context, ev Evidence, rule AcceptanceRule,
	workDir string, prober any, name string) Evidence {
	if prober == nil {
		ev.EnvironmentError = true
		ev.Detail = name + " 未注入（当前部署无真实视觉/DOM 采集能力）"
		return ev
	}
	var (
		got Evidence
		err error
	)
	switch p := prober.(type) {
	case DOMProber:
		got, err = p.Probe(ctx, workDir, rule)
	case VisualProber:
		got, err = p.Probe(ctx, workDir, rule)
	default:
		err = fmt.Errorf("unsupported prober type %T", prober)
	}
	if err != nil {
		ev.EnvironmentError = true
		ev.Detail = name + " 采集失败：" + err.Error()
		return ev
	}
	// 采集器返回值必须补上绑定字段（防止伪造跨任务/跨 revision 的证据）。
	got.Kind = rule.Kind
	got.ID = rule.ID
	got.TaskID = ev.TaskID
	got.Revision = ev.Revision
	got.ContractHash = ev.ContractHash
	if got.Collector == "" {
		got.Collector = name
	}
	return got
}

// CommandExecutorFunc 把普通函数适配成 CommandExecutor（测试/嵌入用）。
func CommandExecutorFunc(fn func(ctx context.Context, workDir, command string, timeout time.Duration) CommandResult) CommandExecutor {
	if fn == nil {
		return nil
	}
	return CommandExecutor(fn)
}

// ShellCommandExecutor 用 os/exec 执行命令（生产默认实现）。
//
// 说明：验收命令来自**合同**（用户批准的计划/规则），不是模型临时拼的字符串；
// 这里仍按 argv 拆分以减少 shell 注入面，含 shell 元字符时退回 shell 执行并记录。
func ShellCommandExecutor(ctx context.Context, workDir, command string, timeout time.Duration) CommandResult {
	if strings.TrimSpace(command) == "" {
		return CommandResult{EnvironmentError: true, Detail: "empty command"}
	}
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var cmd *exec.Cmd
	if needsShell(command) {
		cmd = shellCommand(runCtx, command)
	} else {
		parts := strings.Fields(command)
		cmd = exec.CommandContext(runCtx, parts[0], parts[1:]...)
	}
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()
	res := CommandResult{Output: string(output)}
	if err == nil {
		return res
	}
	if runCtx.Err() == context.DeadlineExceeded {
		res.ExitCode = -1
		res.EnvironmentError = true
		res.Detail = "timeout"
		return res
	}
	var exitErr *exec.ExitError
	if asExitError(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res
	}
	// 起不来（可执行文件缺失/权限不足）：环境性失败，不是行为失败。
	res.ExitCode = -1
	res.EnvironmentError = true
	res.Detail = err.Error()
	return res
}

// commandFrom 把 spec.cmd（string 或 []any/[]string）还原成命令行。
func commandFrom(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	case []string:
		return strings.Join(t, " ")
	}
	return ""
}

func tailString(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

// needsShell 判定命令是否必须经 shell 解释（管道/重定向/串联等元字符）。
func needsShell(command string) bool {
	return strings.ContainsAny(command, "|&;<>()$`\\\"'\n*?")
}

// shellCommand 按平台构造 shell 调用（Windows 优先 cmd.exe，类 Unix 用 sh -c）。
//
// 不沿用 tool 包的 cachedShell（跨包依赖会破坏层级）：验收命令来自合同规则，
// 且这里只需一个确定的解释器，便于复现。
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd.exe", "/C", command)
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}

// asExitError 包装 errors.As（避免在调用处 import errors）。
func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}

// hashFileHex 计算工作区文件的 sha256（file_hash 不变量校验用）。
func hashFileHex(workDir, rel string) (string, error) {
	path := filepath.Join(workDir, filepath.FromSlash(rel))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
