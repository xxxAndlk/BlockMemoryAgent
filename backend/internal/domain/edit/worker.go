package edit

// worker.go 语义编辑 worker 的 Go 侧客户端（Harness 根治计划 WP4 §8.3）。
//
// 职责边界：
//   - 只做「启动 worker + 收发 JSON-RPC + 应用 TextEdit」，不做任何源码解析
//     （解析权威在 worker 的语言编译器：@vue/compiler-sfc / postcss / TypeScript）；
//   - 懒启动、健康检查、崩溃重启、并发序列化（单进程单管道）；
//   - **不把 worker 的失败静默降级为正则盲写**：调用方按 semantic_edit_mode 决定
//     fail closed（enforce）或记录后走旧的显式编辑路径（shadow/off）。
//
// 协议见 tools/semantic-edit/worker.mjs 头部注释（行分隔 JSON，一条请求一条响应）。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// WorkerError 一组结构化的 worker 失败（供上层映射 fail closed / 记录）。
type WorkerError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *WorkerError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("semantic-edit: %s (code=%d)", e.Message, e.Code)
}

// IsWorkerUnavailable 判定是否为"worker 不可用"（依赖缺失/启动失败/超时/崩溃）。
func IsWorkerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var we *WorkerError
	if errors.As(err, &we) {
		switch we.Code {
		case -32001, -32002: // dependency_missing / read_failed
			return false
		}
	}
	return true
}

// WorkerConfig worker 启动参数。
type WorkerConfig struct {
	// Script 入口脚本路径（tools/semantic-edit/worker.mjs）；空=未配置。
	Script string
	// NodeBin node 可执行文件（空="node"，桌面版注入包内 node）。
	NodeBin string
	// Timeout 单次请求超时（<=0 默认 30s）。
	Timeout time.Duration
	// StartTimeout 启动+ping 超时（<=0 默认 15s）。
	StartTimeout time.Duration
}

// Worker 语义编辑 worker 客户端（单实例串行）。
type Worker struct {
	cfg WorkerConfig

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   *bufio.Writer
	stdout  *bufio.Reader
	started bool
	seq     int64
	// version 由 ping 返回，用于版本不匹配时安全失败。
	version string
	// stderrTail 保留 worker stderr 尾（进程早退诊断）。
	stderrTail *tailBuffer
}

// NewWorker 构造客户端（不启动进程）。
func NewWorker(cfg WorkerConfig) *Worker {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 15 * time.Second
	}
	if cfg.NodeBin == "" {
		cfg.NodeBin = "node"
	}
	return &Worker{cfg: cfg}
}

// Configured 判定 worker 是否已配置路径（未配置时调用方不应尝试调用）。
func (w *Worker) Configured() bool {
	return w != nil && strings.TrimSpace(w.cfg.Script) != ""
}

// Version 返回最近一次成功的 ping 版本（空=未启动）。
func (w *Worker) Version() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.version
}

// Start 启动进程并做一次 ping 健康检查。
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.spawnLocked(ctx); err != nil {
		return err
	}
	if w.version != "" {
		return nil // 已启动且已通过健康检查
	}
	return w.pingLocked(ctx)
}

// spawnLocked 启动进程（调用方持锁）。
//
// 2026-11-19 验收修复：不再在这里做 ping。旧实现在**持锁**状态下调 callLocked，
// 而 callLocked 自己又拿 w.mu（非重入）→ 必然超时/死锁，实测表现就是
// `semantic-edit: read: EOF (code=-32099)`。现在拆成两步：起进程释放锁后再寻 ping。
func (w *Worker) spawnLocked(_ context.Context) error {
	if w.started {
		return nil
	}
	if !w.Configured() {
		return &WorkerError{Code: -32099, Message: "worker_script_not_configured"}
	}
	if _, err := os.Stat(w.cfg.Script); err != nil {
		return &WorkerError{Code: -32099, Message: "worker_script_missing: " + w.cfg.Script}
	}
	cmd := exec.Command(w.cfg.NodeBin, w.cfg.Script)
	// 工作目录固定为脚本所在目录：worker 的 node_modules 解析依赖它，
	// 且绝不允许用户输入影响 cwd（防目录逃逸到用户项目）。
	cmd.Dir = filepath.Dir(w.cfg.Script)
	// 剔除代码注入类环境变量（详见 minimalNodeEnv 注释）。
	cmd.Env = minimalNodeEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return &WorkerError{Code: -32099, Message: "stdin_pipe: " + err.Error()}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return &WorkerError{Code: -32099, Message: "stdout_pipe: " + err.Error()}
	}
	// stderr 必须被消费：管道写满会让 worker 阻塞在写日志上（表现为"无响应"）。
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return &WorkerError{Code: -32099, Message: "stderr_pipe: " + err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return &WorkerError{Code: -32099, Message: "start: " + err.Error()}
	}
	w.cmd = cmd
	w.stdin = bufio.NewWriter(stdin)
	w.stdout = bufio.NewReaderSize(stdout, 1<<20)
	w.started = true
	// 保留 stderr 尾部：进程早退时是唯一能解释 EOF 的证据（不得丢弃）。
	w.stderrTail = newTailBuffer(4096)
	go func(tail *tailBuffer) {
		_, _ = io.Copy(tail, stderr)
	}(w.stderrTail)
	return nil
}

// pingLocked 健康检查（调用方持锁），失败则关闭进程供下次重试。
func (w *Worker) pingLocked(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, w.cfg.StartTimeout)
	defer cancel()
	res, err := w.callLocked(pingCtx, "ping", map[string]any{})
	if err != nil {
		err = w.withStderr(err)
		w.stopLocked()
		return err
	}
	var info struct {
		Version string `json:"version"`
		Node    string `json:"node"`
	}
	if err := json.Unmarshal(res, &info); err != nil {
		w.stopLocked()
		return &WorkerError{Code: -32099, Message: "ping_decode: " + err.Error()}
	}
	w.version = info.Version
	return nil
}

// withStderr 把 worker 的 stderr 尾部附到错误上（诊断 EOF/启动失败必需）。
func (w *Worker) withStderr(err error) error {
	if err == nil || w.stderrTail == nil {
		return err
	}
	if tail := strings.TrimSpace(w.stderrTail.String()); tail != "" {
		return fmt.Errorf("%w（worker stderr: %s）", err, tail)
	}
	return err
}

// minimalNodeEnv 返回 worker 进程环境变量。
//
// 策略（2026-11-19 验收修复）：**黑名单剔除**而不是白名单保留。
// 原先的白名单做法会连带剔掉版本管理器（volta/fnm/nvm-windows shim）用来定位真实
// node 的环境变量（VOLTA_HOME/FNM_DIR/NVM_HOME/…），导致 shim 启动即退出、Go 侧
// 只看到一句墓碑式的 `read: EOF`（验收报告实测：shell 里 ping 正常，Go 端稳定 EOF）。
//
// 真正需要防的是"向 Node 注入代码"的变量；用户内容从不进环境，worker 脚本是仓库
// 可信代码，因此保留其余环境（含代理/区域设置/临时目录）是安全的。
func minimalNodeEnv() []string {
	// 代码注入 / 解释器行为接管类变量：一律剔除。
	deny := map[string]bool{
		"NODE_OPTIONS":                 true, // --require/--import 任意模块
		"NODE_REPL_EXTERNAL_MODULE":    true,
		"NODE_REPL_HISTORY":            true,
		"NODE_EXTRA_CA_CERTS":          true, // 换根证书等于可拦截一切
		"NODE_TLS_REJECT_UNAUTHORIZED": true, // 0 = 关证书校验
		// ELECTRON_RUN_AS_NODE 不是代码注入参数：Windows 的 Electron 系 Node shim
		// 必须保留它才能以 Node 模式启动；真实 node.exe 会忽略该变量。
		"BMA_SEMANTIC_EDIT_INJECT": true, // 预留给测试的显式注入点，生产禁用
	}
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i > 0 {
			key = kv[:i]
		}
		if deny[strings.ToUpper(key)] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Stop 关闭进程（可重复调用）。
func (w *Worker) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopLocked()
}

func (w *Worker) stopLocked() {
	if w.stdin != nil {
		_ = w.stdin.Flush()
	}
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
		_ = w.cmd.Wait()
	}
	w.started = false
	w.cmd = nil
	w.stdin = nil
	w.stdout = nil
	w.version = ""
}

// tailBuffer 保留最后 N 字节（环形覆盖，内存上限固定）。
type tailBuffer struct {
	mu   sync.Mutex
	max  int
	data []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	if t == nil {
		return len(p), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > t.max {
		t.data = t.data[len(t.data)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}

// Call 发起一次请求（自动启动 + 健康检查；崩溃重启一次）。
func (w *Worker) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if w == nil {
		return nil, &WorkerError{Code: -32099, Message: "worker_not_initialized"}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		if err := w.spawnLocked(ctx); err != nil {
			return nil, err
		}
		if err := w.pingLocked(ctx); err != nil {
			return nil, err
		}
	}
	res, err := w.callLocked(ctx, method, params)
	if err == nil {
		return res, nil
	}
	// 管道类错误（进程崩溃）重启一次再试；业务错误（参数/解析失败）不重试。
	if !IsWorkerUnavailable(err) {
		return nil, err
	}
	err = w.withStderr(err)
	w.stopLocked()
	if startErr := w.spawnLocked(ctx); startErr != nil {
		return nil, startErr
	}
	if pingErr := w.pingLocked(ctx); pingErr != nil {
		return nil, pingErr
	}
	res, retryErr := w.callLocked(ctx, method, params)
	if retryErr != nil {
		retryErr = w.withStderr(retryErr)
	}
	return res, retryErr
}

// callLocked 单次往返（调用方持锁）。
//
// 传输层纪律（2026-11-19 再次验收返修）：
//   - 超时后必须**杀掉进程**再返回。bufio.Reader 不支持并发读，而超时路径残留的
//     读取 goroutine 会与下一次调用新起的 goroutine 同时读同一条管道——两个读者
//     互相偷行，表现就是随机 `decode response: unexpected end of JSON input` /
//     `read: EOF`。杀掉进程让孤儿 goroutine 立即收到错误退出，重启后是全新管道。
//   - 空行跳过而不是当响应解析（某些 node/shim 会向 stdout 抖一行空行）。
//   - 解码失败时附上原始行片段 + worker stderr 尾，避免只有一句无解的 JSON 错误。
func (w *Worker) callLocked(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	w.seq++
	reqID := fmt.Sprintf("r%d", w.seq)
	req := map[string]any{"request_id": reqID, "method": method, "params": params}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, &WorkerError{Code: -32602, Message: "encode: " + err.Error()}
	}
	deadline := time.Now().Add(w.cfg.Timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	// 写请求 + 刷新（worker 按行读取）。
	if _, err := w.stdin.WriteString(string(payload) + "\n"); err != nil {
		return nil, &WorkerError{Code: -32099, Message: "write: " + err.Error()}
	}
	if err := w.stdin.Flush(); err != nil {
		return nil, &WorkerError{Code: -32099, Message: "flush: " + err.Error()}
	}
	// 读一行响应：用 goroutine + 超时控制（bufio 读取不可取消）。
	// 每次重试都重新绑定**当前** reader 到局部变量，避免 goroutine 读 w.stdout
	// 字段（stopLocked 会把它置 nil，字段级并发读写是数据竞争）。
	type readResult struct {
		line string
		err  error
	}
	readLine := func() <-chan readResult {
		ch := make(chan readResult, 1)
		reader := w.stdout
		if reader == nil {
			ch <- readResult{err: io.EOF}
			return ch
		}
		go func() {
			line, err := reader.ReadString('\n')
			ch <- readResult{line: line, err: err}
		}()
		return ch
	}
	ch := readLine()
	var rr readResult
	for {
		select {
		case rr = <-ch:
		case <-time.After(time.Until(deadline)):
			// 关键：孤儿读 goroutine 必须被进程退出解阻塞，否则下一次调用会出现两个读者。
			w.stopLocked()
			return nil, &WorkerError{Code: -32098, Message: "timeout after " + w.cfg.Timeout.String()}
		}
		if rr.err != nil {
			return nil, &WorkerError{Code: -32099, Message: "read: " + rr.err.Error()}
		}
		if strings.TrimSpace(rr.line) != "" {
			break
		}
		if !time.Now().Before(deadline) {
			w.stopLocked()
			return nil, &WorkerError{Code: -32098, Message: "timeout after " + w.cfg.Timeout.String() + " (only blank lines received)"}
		}
		ch = readLine()
	}
	var resp struct {
		RequestID string          `json:"request_id"`
		OK        bool            `json:"ok"`
		Result    json.RawMessage `json:"result"`
		Error     *WorkerError    `json:"error"`
	}
	if err := json.Unmarshal([]byte(rr.line), &resp); err != nil {
		return nil, w.withStderr(&WorkerError{Code: -32099,
			Message: "decode response: " + err.Error() + "（原文片段：" + previewLine(rr.line) + "）"})
	}
	if resp.RequestID != "" && resp.RequestID != reqID {
		return nil, w.withStderr(&WorkerError{Code: -32099, Message: "response id mismatch: " + resp.RequestID})
	}
	if !resp.OK {
		if resp.Error != nil {
			return nil, resp.Error
		}
		return nil, &WorkerError{Code: -32000, Message: "worker returned failure without error detail"}
	}
	return resp.Result, nil
}

// previewLine 截取一行用于错误消息（避免把 4MB 响应打进日志）。
func previewLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 240 {
		return s
	}
	return s[:240] + "…"
}

// --- 协议类型 ---------------------------------------------------------------

// TextEdit 最小源码范围编辑（仅此形状被允许应用到用户文件）。
// Unit 声明 offset 的单位：worker 返回 utf16（JS 字符串索引），应用前必须换算成字节。
type TextEdit struct {
	Start      int64  `json:"start"`
	End        int64  `json:"end"`
	NewText    string `json:"new_text"`
	BeforeHash string `json:"before_hash,omitempty"`
	TargetKind string `json:"target_kind,omitempty"`
	Selector   string `json:"selector,omitempty"`
	Property   string `json:"property,omitempty"`
	FromValue  string `json:"from_value,omitempty"`
	ToValue    string `json:"to_value,omitempty"`
	Tag        string `json:"tag,omitempty"`
}

// EditPlan worker 返回的一次编辑计划。
type EditPlan struct {
	Unit        string     `json:"unit"`
	ContentHash string     `json:"content_hash"`
	Edits       []TextEdit `json:"edits"`
	Diagnostics []struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"diagnostics"`
}

// CSSDeclarationParams plan_css_edit 参数。
type CSSDeclarationParams struct {
	File          string `json:"file"`
	Source        string `json:"source,omitempty"`
	Selector      string `json:"selector"`
	Property      string `json:"property"`
	ExpectedValue string `json:"expected_value,omitempty"`
	NewValue      string `json:"new_value"`
}

// PlanCSSEdit 定位唯一 CSS 声明并生成最小 TextEdit（ExpectedMatches 固定为 1）。
func (w *Worker) PlanCSSEdit(ctx context.Context, p CSSDeclarationParams) (*EditPlan, error) {
	raw, err := w.Call(ctx, "plan_css_edit", map[string]any{
		"file": p.File, "source": p.Source, "selector": p.Selector, "property": p.Property,
		"expected_value": p.ExpectedValue, "new_value": p.NewValue, "expected_matches": 1,
	})
	if err != nil {
		return nil, err
	}
	var plan EditPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, &WorkerError{Code: -32099, Message: "decode plan: " + err.Error()}
	}
	if len(plan.Edits) != 1 {
		return nil, &WorkerError{Code: -32050,
			Message: fmt.Sprintf("expected exactly 1 edit, got %d (bulk edits are forbidden)", len(plan.Edits))}
	}
	return &plan, nil
}

// ParseSFCParams parse_sfc 参数。
type ParseSFCParams struct {
	File   string `json:"file"`
	Source string `json:"source,omitempty"`
}

// ParseSFC 解析 Vue SFC，返回块范围与诊断。
func (w *Worker) ParseSFC(ctx context.Context, p ParseSFCParams) (map[string]any, error) {
	raw, err := w.Call(ctx, "parse_sfc", map[string]any{"file": p.File, "source": p.Source})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &WorkerError{Code: -32099, Message: "decode sfc: " + err.Error()}
	}
	return out, nil
}

// ByteRangeOfEdit 把 plan 的唯一 TextEdit 换算成字节范围 [start,end)。
//
// 单独导出：复验要用它证明"除该范围外逐字节未变"（I-EDIT-2）。
func ByteRangeOfEdit(src []byte, plan *EditPlan) (int64, int64, error) {
	if plan == nil {
		return 0, 0, &WorkerError{Code: -32050, Message: "plan required"}
	}
	if len(plan.Edits) != 1 {
		return 0, 0, &WorkerError{Code: -32050,
			Message: fmt.Sprintf("expected exactly 1 edit, got %d", len(plan.Edits))}
	}
	e := plan.Edits[0]
	start, err := utf16OffsetToByteOffset(src, e.Start)
	if err != nil {
		return 0, 0, err
	}
	end, err := utf16OffsetToByteOffset(src, e.End)
	if err != nil {
		return 0, 0, err
	}
	if start > end || end > int64(len(src)) {
		return 0, 0, &WorkerError{Code: -32051,
			Message: fmt.Sprintf("invalid byte range [%d,%d) for %d-byte source", start, end, len(src))}
	}
	return start, end, nil
}

// ApplyUTF16Edits 把 worker 的 utf16 offset 换算成字节偏移并应用（只允许 1 条 edit）。
//
// 纪律（I-EDIT-1）：调用前必须校验目标文件内容 hash 与 plan.ContentHash 一致，
// 否则拒绝（防"读到的不是要改的那版"）。返回应用后的新内容。
func ApplyUTF16Edits(src []byte, plan *EditPlan) ([]byte, error) {
	start, end, err := ByteRangeOfEdit(src, plan)
	if err != nil {
		return nil, err
	}
	e := plan.Edits[0]
	out := make([]byte, 0, len(src)-int(end-start)+len(e.NewText))
	out = append(out, src[:start]...)
	out = append(out, e.NewText...)
	out = append(out, src[end:]...)
	return out, nil
}

// utf16OffsetToByteOffset 把 utf16 code unit 偏移换算成 UTF-8 字节偏移（越界报错）。
func utf16OffsetToByteOffset(src []byte, offset int64) (int64, error) {
	if offset == 0 {
		return 0, nil
	}
	if offset < 0 {
		return 0, &WorkerError{Code: -32051, Message: "negative offset"}
	}
	var units int64
	for i := 0; i < len(src); {
		if units == offset {
			return int64(i), nil
		}
		r, size := utf8.DecodeRune(src[i:])
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
		if units > offset {
			return 0, &WorkerError{Code: -32051,
				Message: fmt.Sprintf("offset %d falls inside a surrogate pair at byte %d", offset, i)}
		}
		i += size
	}
	if units == offset {
		return int64(len(src)), nil
	}
	return 0, &WorkerError{Code: -32051,
		Message: fmt.Sprintf("offset %d beyond source (only %d utf16 units)", offset, units)}
}
