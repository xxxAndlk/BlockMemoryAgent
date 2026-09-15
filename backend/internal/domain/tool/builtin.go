// Package tool 定义并实现了内置工具（ReadFile / WriteFile / ListDir / SearchInFiles / RunCommand / HTTP / Git 等）的执行逻辑。
// 这些工具通过 Executor 提供统一入口，每个方法接收 map[string]any 参数并返回 *Result 结果对象。
package tool

import (
	// bytes 用于在 RunCommand 中缓存命令的标准输出与标准错误。
	"bytes"
	// context 用于控制工具执行的生命周期（超时、取消）。
	"context"
	// encoding/json 用于将 HTTP POST 请求体序列化为 JSON。
	"encoding/json"
	// fmt 用于格式化输出字符串、错误信息以及日志字段。
	"fmt"
	// io 用于读取 HTTP 响应体的限流读取器。
	"io"
	// log/slog 用于记录进程树清理失败等调试信息。
	"log/slog"
	// net/http 用于执行 HTTPGet 与 HTTPPost 网络请求。
	"net/http"
	// os 用于文件读写、环境变量读取以及目录创建。
	"os"
	// os/exec 用于执行外部命令（RunCommand 与 Git 工具）。
	"os/exec"
	// path/filepath 用于路径解析、清洗以及相对路径计算。
	"path/filepath"
	// runtime 用于根据操作系统选择命令解释器与进程管理策略。
	"runtime"
	// slices 用于判断文件扩展名是否命中搜索白名单。
	"slices"
	// strconv 用于将进程 PID 转换为字符串以执行 taskkill/kill。
	"strconv"
	// strings 用于字符串切分、大小写转换、前缀判断与拼接。
	"strings"
	// sync 用于进程内缓存命令解释器探测结果。
	"sync"
	// time 用于超时时间计算与 context.WithTimeout。
	"time"
	// unicode 用于 Git 子命令首字母大写转换。
	"unicode"
)

// 以下输入结构体用于 JSON Schema 生成与运行时参数反序列化。
type (
	// readFileInput 表示 ReadFile 工具的输入参数。
	readFileInput struct {
		// Path 为待读取文件的相对或绝对路径。
		Path string `json:"path"`
		// Offset 为起始行号（1-based，默认 1）。
		Offset float64 `json:"offset,omitempty"`
		// Limit 为本次读取的最大行数（默认 defaultReadFileLimit 行）。
		Limit float64 `json:"limit,omitempty"`
	}
	// writeFileInput 表示 WriteFile 工具的输入参数。
	writeFileInput struct {
		// Path 为待写入文件的目标路径。
		Path string `json:"path"`
		// Content 为要写入文件的文本内容。必须是文件的**完整内容**--WriteFile 是整文件覆盖，
		// 不是局部替换/追加。修改已有文件一律走 EditFile 局部替换；WriteFile 限新建与整写。
		// 禁止只发修改片段（只发片段会把原文件整文件覆盖为片段，造成数据丢失），
		// 缩小守卫会拒收并引导改用 EditFile。
		Content string `json:"content"`
		// Temporary 为 true 时表示写入到当前会话的临时目录，受会话上下文约束。
		Temporary bool `json:"temporary"`
		// ConfirmShrink 为 true 时显式确认"新内容远小于原文件"是有意精简，绕过极端缩小硬拒绝。
		// 仅在确实要大幅删减文件时设置；常规修改不要设。
		ConfirmShrink bool `json:"confirm_shrink"`
		// ExpectedMtime 为可选乐观锁：把 ReadFile 分页头里的 mtime=... 原样传回，
		// 写入前比对（2s 容差），不符拒收并提示重读——拦截"读取后被其他 Agent/进程
		// 修改"的并发覆盖。不传则不校验（现状 advisory 行为不变）。
		ExpectedMtime string `json:"expected_mtime,omitempty"`
	}
	// editFileInput 表示 EditFile 工具的输入参数。
	// EditFile 是精确局部替换：old_string/new_string 语义对齐主流 Agent 工具的编辑能力，
	// 小改（<20% 文件）优先 EditFile，避免 WriteFile 整文件重写导致的输出 token 黑洞（TODO #49）。
	editFileInput struct {
		// Path 为待编辑文件的目标路径。文件必须已存在；新建文件用 WriteFile。
		Path string `json:"path"`
		// OldString 为要替换的原文片段，必须与文件内容逐字符一致（含缩进/空白；
		// 行尾 \r\n 与 \n 视为等价）。默认必须唯一匹配；多处匹配被拒绝并提示。
		OldString string `json:"old_string"`
		// NewString 为替换后的新文本。
		NewString string `json:"new_string"`
		// ReplaceAll 为 true 时替换所有匹配处（old_string 在文件中出现多次时使用）；
		// 默认 false 只替换第一处且要求唯一匹配。
		ReplaceAll bool `json:"replace_all"`
		// ExpectedMtime 为可选乐观锁：把 ReadFile 分页头里的 mtime=... 原样传回，
		// 写入前比对（2s 容差），不符拒收并提示重读——拦截"读取后被其他 Agent/进程
		// 修改"的并发覆盖（old_string 基于过期内容时 fuzzy 匹配也可能误换到别处）。
		// 不传则不校验（现状 advisory 行为不变）。
		ExpectedMtime string `json:"expected_mtime,omitempty"`
	}
	// restoreFileInput 表示 RestoreFile 工具的输入参数。
	restoreFileInput struct {
		// Path 为待恢复文件的目标路径。
		Path string `json:"path"`
		// Timestamp 为可选的快照时间戳（格式 20060102-150530），缺省时恢复到最新一份快照。
		Timestamp string `json:"timestamp,omitempty"`
	}
	// listDirInput 表示 ListDir 工具的输入参数。
	listDirInput struct {
		// Path 为待列出目录的路径，空字符串时默认使用当前工作目录。
		Path string `json:"path"`
	}
	// runCommandInput 表示 RunCommand 工具的输入参数。
	runCommandInput struct {
		// Command 为要执行的命令字符串。
		Command string `json:"command"`
		// Timeout 为命令最大执行时间（秒），0 或缺失时使用默认超时。
		Timeout float64 `json:"timeout,omitempty"`
	}
	// searchInFilesInput 表示 SearchInFiles 工具的输入参数。
	searchInFilesInput struct {
		// Pattern 为在文件中搜索的文本模式（大小写不敏感）。
		Pattern string `json:"pattern"`
		// Dir 为搜索起始目录，空字符串时默认使用当前工作目录。
		Dir string `json:"dir,omitempty"`
	}
	// httpGetInput 表示 HTTPGet 工具的输入参数。
	httpGetInput struct {
		// URL 为请求的完整目标地址。
		URL string `json:"url"`
		// Headers 为可选的请求头映射。
		Headers map[string]string `json:"headers,omitempty"`
		// Timeout 为可选的请求超时时间（秒）。
		Timeout float64 `json:"timeout,omitempty"`
	}
	// httpPostInput 表示 HTTPPost 工具的输入参数。
	httpPostInput struct {
		// URL 为请求的完整目标地址。
		URL string `json:"url"`
		// Headers 为可选的请求头映射。
		Headers map[string]string `json:"headers,omitempty"`
		// Body 为请求体，支持任意 JSON 可序列化结构或字符串。
		Body map[string]any `json:"body,omitempty"`
		// Timeout 为可选的请求超时时间（秒）。
		Timeout float64 `json:"timeout,omitempty"`
	}
	// gitDiffInput 表示 GitDiff 工具的输入参数。
	gitDiffInput struct {
		// Target 为可选的 diff 目标引用（分支、提交等）。
		Target string `json:"target,omitempty"`
		// Path 为可选的限制 diff 范围的文件或目录路径。
		Path string `json:"path,omitempty"`
	}
	// gitStatusInput 表示 GitStatus 工具的输入参数（当前无字段）。
	gitStatusInput struct{}
	// gitLogInput 表示 GitLog 工具的输入参数。
	gitLogInput struct {
		// Limit 为返回的最大提交条数，默认 20。
		Limit float64 `json:"limit,omitempty"`
		// Path 为可选的限制日志范围的文件或目录路径。
		Path string `json:"path,omitempty"`
	}
	// gitBlameInput 表示 GitBlame 工具的输入参数。
	gitBlameInput struct {
		// Path 为待执行 blame 的文件路径。
		Path string `json:"path"`
	}
	// refreshProjectDocInput 表示 RefreshProjectDoc 工具的输入参数（当前无字段）。
	refreshProjectDocInput struct{}
	// callSubAgentInput 表示 call_sub_agent 工具的输入参数。
	// 与 subagent 包内部入参结构保持一致（按字段名 JSON 解码）。
	callSubAgentInput struct {
		// RoleID 为被调用子 Agent 的角色标识。
		RoleID string `json:"role_id" description:"被调用子 Agent 的角色标识，从工具描述的角色清单中选。默认走 domain（其收到后默认自执行），仅单函数级、单文件、领域明确的任务直派固定助手。"`
		// Task 为交给子 Agent 执行的自包含任务描述。
		Task string `json:"task" description:"自包含任务描述（<=500 字）：背景、目标、相关文件路径、前置结论与验收标准。子 Agent 看不到当前对话历史，规格原文走 WriteSharedMemory。"`
		// Domain 为领域分类简称（如 金融/认证/UI/数据库），仅 role_id="domain" 时有效，
		// 用于 DomainAgent 展示名（如"金融领域Agent"）。空时回退到 task 首行兜底。
		Domain string `json:"domain" description:"领域分类简称（如 金融/认证/UI/数据库/配置），仅 role_id=domain 时填，用于子 Agent 展示名。固定助手忽略。"`
		// Responsibility 为职责边界描述，仅 role_id="domain" 时必填，
		// 注入 DomainAgent 系统提示词头部，防止长 ReAct 循环中越界实现他域文件。
		Responsibility string `json:"responsibility" description:"职责边界（<=200 字）：写明该领域 Agent 负责哪些文件/模块、不碰哪些。role_id=domain 时必填，会注入子 Agent 系统提示词。"`
		// Mode 为派发执行模式（TODO #29）：react（默认）/ reflection / plan_execute。
		// 空串按 react 处理，零行为变化。
		Mode string `json:"mode" description:"派发执行模式（可选）：react（默认，直接 ReAct 循环）/ reflection（产出后对照验收标准自检，不达标带反馈重试）/ plan_execute（先出步骤计划再逐步执行）。琐碎单步任务省略；正确性敏感任务用 reflection；多步骤长任务用 plan_execute。"`
		// VerifyKind 为校验分层（TODO #43）：auto（默认，代码/测试助手→可执行证据，reflection→rubric）
		// / executable（L0：须有验证命令成功执行的客观证据，无则反馈重试 1 轮）/ rubric（L2：交叉模型
		// judge 按验收标准逐条判）/ none（不校验）。
		VerifyKind string `json:"verify_kind" description:"校验分层（可选）：auto（默认，按角色/模式自动选）/ executable（必须有测试/lint/--check 成功运行的客观证据）/ rubric（交叉模型按验收标准逐条评审）/ none（跳过校验）。"`
		// Skills 为下放给子 Agent 的技能名列表（技能渐进披露）：校验 ⊆ 父 Agent 持有集
		// 后并入子持有集，越界项忽略并随派发结果回告。子 Agent 系统提示注入其【可用技能】
		// 元数据块，正文经 load_skill 按需获取。
		Skills []string `json:"skills" description:"下放给子 Agent 的技能名列表（可选，来自你的【可用技能】块）：子 Agent 将获得对应技能的元数据与 load_skill 加载权限。省略=不下放技能（子 Agent 仍有其角色固定技能）。"`
	}
)

// ---- ReadFile（读取文件） ----

// defaultReadFileLimit 是 ReadFile 未显式指定 limit 时的默认读取行数。
// 与 ReadFileMaxChars 字符上限共同生效（先到先截），保证单页输出可控。
// 取值 200：实证（2026-08-10 塔防日志）120 行/页被 4000 字符上限截停到单页 60-110 行，
// 领域 Agent 读不完职责文件即耗尽探索预算被判死；升档后多数文件单页即可覆盖。
const defaultReadFileLimit = 200

// maxReadFileLimit 是单次 ReadFile 的硬性行数上限：limit 超过它一律钳到该值。
// 原"单次 ReadFile 不超过 300 行"仅靠提示词纪律约束，实证被违反
// （2026-08-14 塔防 9 叶子并行重绘：单次 401/410/450 行各出现）；改为工具层硬截断，
// 并在分页头标注，让模型知道被钳制、可分页续读。
// 2026-09-07 TODO 第八项 P1-2：300→1000——分页截断本身是冲突干扰源（一次读不完
// 中等文件需 3-5 页拼接），字符上限 ReadFileMaxChars 仍是体积闸门，行数放宽不失控。
const maxReadFileLimit = 1000

// readFile 按行区间读取指定路径的文本内容（1-based offset + limit 分页）。
// 输出带行号（cat -n 风格），顶部首行放置分页头（总行数/本页区间/下一页 offset），
// 放在顶部是因为写历史的 tool_output_history_max_runes 截断保头不保尾，
// 页脚式分页信息会在历史里丢失，导致模型忘记如何翻页。
// 超出 ReadFileMaxChars 时按行截停，分页头中给出实际返回区间，保证"继续读"指引始终准确。
func (e *Executor) readFile(ctx context.Context, args map[string]any) *Result {
	// 从参数中取出 path，要求必须是非空字符串。
	path, ok := args["path"].(string)
	if !ok || path == "" {
		// 参数缺失或为空时直接返回错误结果。
		return &Result{Tool: "ReadFile", Error: "path is required"}
	}
	// 解析路径并校验沙箱约束。
	absPath, err := e.resolvePathWithSandbox(ctx, path)
	if err != nil {
		// 路径越界或解析失败时返回错误。
		return &Result{Tool: "ReadFile", Path: absPath, Error: err.Error()}
	}
	// 读取文件全部字节。
	data, err := os.ReadFile(absPath)
	if err != nil {
		// 文件不存在或无权限时返回错误。
		return &Result{Tool: "ReadFile", Path: absPath, Error: err.Error()}
	}

	// 解析分页参数：offset 为 1-based 起始行，limit 为本页最大行数。
	offset := 1
	if v, ok := args["offset"].(float64); ok && v > 0 {
		offset = int(v)
	}
	limit := defaultReadFileLimit
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	// 硬截断：limit 超 maxReadFileLimit 一律钳到上限（提示词纪律实证会被违反，工具层兜底）。
	clamped := false
	if limit > maxReadFileLimit {
		limit = maxReadFileLimit
		clamped = true
	}

	// 按行切分文件内容。
	lines := strings.Split(string(data), "\n")
	total := len(lines)
	// offset 越界时直接报错并告知总行数，引导模型给出合法区间。
	if offset > total {
		return &Result{Tool: "ReadFile", Path: absPath,
			Error: fmt.Sprintf("offset %d 超出文件总行数 %d，请用 1-%d 之间的 offset", offset, total, total)}
	}
	// 计算本页结束行（含），不超出文件末尾。
	end := offset - 1 + limit
	if end > total {
		end = total
	}

	// 逐行拼接待行号内容；超出字符上限时提前截停，actualEnd 记录实际返回到哪一行。
	maxChars := e.agentConfig().ReadFileMaxChars
	var b strings.Builder
	actualEnd := offset - 1
	for i := offset - 1; i < end; i++ {
		// CRLF 文件每行尾部残留 \r：进入 TUI/日志后被终端当作回车导致同行覆盖乱码，先剥离。
		line := fmt.Sprintf("%6d\t%s\n", i+1, strings.TrimSuffix(lines[i], "\r"))
		// 至少保留第一行，避免 maxChars 极小时返回空内容。
		if b.Len()+len(line) > maxChars && actualEnd >= offset {
			break
		}
		b.WriteString(line)
		actualEnd = i + 1
	}

	// 分页头放在输出顶部：历史截断保留头部，模型始终知道文件规模与下一页起点。
	// mtime 段（TODO #16 T16）：作为 expected_mtime 乐观锁的取值来源——模型编辑前
	// 把它原样传回 EditFile/WriteFile，写入前比对拦截并发覆盖。stat 失败省略该段。
	var mtimeSeg string
	if fi, err := os.Stat(absPath); err == nil {
		mtimeSeg = fmt.Sprintf(" | mtime=%s", fi.ModTime().Format(time.RFC3339))
	}
	header := fmt.Sprintf("[共 %d 行 | 本页 %d-%d 行", total, offset, actualEnd)
	header += mtimeSeg
	if clamped {
		header += fmt.Sprintf(" | limit 超单次上限 %d 行，已截断", maxReadFileLimit)
	}
	if actualEnd < total {
		header += fmt.Sprintf(" | 继续读请 ReadFile(path, offset=%d)]\n", actualEnd+1)
	} else {
		header += " | 已到文件末尾]\n"
	}
	// 返回成功结果，包含文件路径与分页头 + （可能截停后的）本页内容。
	return &Result{Tool: "ReadFile", Success: true, Output: header + b.String(), Path: absPath}
}

// ---- WriteFile（写入文件） ----

// maxWriteFileContentRunes 是单次 WriteFile 内容上限：超限拒收要求拆分。
// 提示词已有"单文件目标 <= 300 行"软约束，但大文件整写仍触发 LLM max_tokens
// 截断（实证 monster.js 730 行被截断写入 64 行后 node --check 仍通过）。
// 工具层硬上限兜底：单响应生成超长内容必截断，先拒绝并指导拆分。
const maxWriteFileContentRunes = 100000

// writeFileSizeWarnRatio 重写已有文件时，新内容小于原大小该比例（且原文件
// 超过 minWriteFileSizeWarnBytes）则输出附警告，辅助模型发现截断写入。
const (
	writeFileSizeWarnRatio    = 0.3
	minWriteFileSizeWarnBytes = 1000
)

// 极端缩小硬拒绝阈值：原文件 >= minWriteFileShrinkRefuseBytes 且新内容 < 原文件
// writeFileShrinkRefuseRatio 比例时，WriteFile 直接拒收（除非显式 confirm_shrink=true）。
// 实证：模型把 WriteFile 当"局部替换"用，只发修改片段（509B/22495B = 2%、755B/59888B = 1%），
// os.WriteFile 整文件覆盖把原文件截成 ~20 行数据丢失。warn 文案"若为有意的精简请忽略"
// 给了模型逃避口（直接判"符合预期"），需硬拒绝强制模型 ReadFile 全文重写。
// 阈值取 10% + 5KB：常规大幅精简（删半/重写更紧凑）不会触发；只触发"明显是片段"场景。
const (
	writeFileShrinkRefuseRatio    = 0.10
	minWriteFileShrinkRefuseBytes = 5000
)

// expectedMtimeTolerance 是 expected_mtime 乐观锁的比对容差（TODO #16 T16）：
// mtime 精度跨文件系统（FAT 2s）与 RFC3339 秒级序列化（小数截断）都会漂移，2s 内视为
// 一致。护栏目标是"读取→其他 Agent 修改→写入"这类秒-分钟级竞态，2s 内不构成现实冲突。
const expectedMtimeTolerance = 2 * time.Second

// checkExpectedMtime 校验可选乐观锁参数 expected_mtime（TODO #16 T16 并发写护栏）。
// 模型把 ReadFile 分页头里的 mtime=... 原样传回；写入前 stat 比对，不符拒收本次写入
//（工具结果级错误，不中止 ReAct 循环），列出当前 mtime 引导重读。expected 为空时
// 不校验（现状 advisory 行为不变）。要求校验但文件已不存在同样拒收（可能被并发删除）。
func checkExpectedMtime(absPath, expected string) error {
	if expected == "" {
		return nil
	}
	fi, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("expected_mtime 校验失败: 文件当前不存在（可能已被并发删除）--请 ListDir 确认现状后再操作")
	}
	got, ok := parseFlexibleTime(expected)
	if !ok {
		return fmt.Errorf("expected_mtime 无法解析: %q（应为 ReadFile 分页头里 mtime=... 的原值，RFC3339 格式）", expected)
	}
	cur := fi.ModTime()
	if d := cur.Sub(got); d > expectedMtimeTolerance || d < -expectedMtimeTolerance {
		return fmt.Errorf("mtime 冲突: 你基于 mtime=%s 的内容写入，但文件当前 mtime=%s（已被其他 Agent/进程修改）。"+
			"为避免覆盖他人改动已拒收本次写入；请重新 ReadFile 获取最新内容与新 mtime 后重试",
			got.Format(time.RFC3339), cur.Format(time.RFC3339))
	}
	return nil
}

// parseFlexibleTime 解析模型可能传回的 mtime 字符串：RFC3339/Nano（ReadFile 分页头格式）、
// Go time.String() 默认布局、无时区的日期时间（按本地时区解释）。全部失败 ok=false。
func parseFlexibleTime(s string) (time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// writeFile 将内容写入指定路径，支持普通文件与会话临时文件两种模式。
func (e *Executor) writeFile(ctx context.Context, args map[string]any) *Result {
	// 从参数中取出各字段，缺失时使用零值。
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	temporary, _ := args["temporary"].(bool)
	allowSpaces, _ := args["allow_spaces"].(bool)
	confirmShrink, _ := args["confirm_shrink"].(bool)
	expectedMtime, _ := args["expected_mtime"].(string)

	// path 为空时不允许写入，直接返回错误。
	if path == "" {
		return &Result{Tool: "WriteFile", Error: "path is required"}
	}
	// 内容硬上限：超长单次写入会被模型输出截断写残（provider 层丢弃截断
	// tool_call 只是让模型重试，这里从源头拒绝并给拆分指引）。
	if r := []rune(content); len(r) > maxWriteFileContentRunes {
		return &Result{Tool: "WriteFile", Path: path, Error: fmt.Sprintf(
			"content too large: %d runes (max %d)。单次 WriteFile 超长内容易被模型输出截断写残文件；"+
				"请拆分写入（单文件目标 <= 300 行）或分多次 WriteFile", len(r), maxWriteFileContentRunes)}
	}

	// 调用写保护守卫进行策略校验（例如禁止写入某些目录、检查路径特征）。
	if err := e.guards.CheckWrite(path, content, allowSpaces); err != nil {
		return &Result{Tool: "WriteFile", Path: path, Error: err.Error()}
	}

	// 解析目标路径。
	absPath := e.resolvePath(ctx, path)
	// tempDir 用于保存当前会话的临时目录路径，仅在 temporary 模式下使用。
	tempDir := ""
	if temporary {
		// 临时文件需要会话上下文，否则无法确定保存位置。
		sessionID := SessionIDFromContext(ctx)
		if sessionID == "" {
			return &Result{Tool: "WriteFile", Error: "temporary file requires a session context"}
		}
		// 获取当前会话专属的临时目录。
		tempDir = e.sessionTempDir(ctx, sessionID)
		// 清洗传入路径，防止 .. 等导致越界。
		cleanPath := filepath.Clean(path)
		if filepath.IsAbs(cleanPath) {
			// 临时文件不接受绝对路径，仅使用文件名部分。
			cleanPath = filepath.Base(cleanPath)
		}
		// 组合为临时目录下的最终绝对路径。
		absPath = filepath.Join(tempDir, cleanPath)
	} else {
		// 非临时文件需要额外校验写入路径（禁止写到系统目录等）。
		if err := e.sanitizeWritePath(ctx, absPath); err != nil {
			return &Result{Tool: "WriteFile", Path: absPath, Error: err.Error()}
		}
		// 角色级写沙箱（Layer 4）：角色配了 allowed_write_paths 时进一步限制写入范围。
		// 未配置的角色（含 MetaAgent/DomainAgent/默认叶子助手）roleWritePaths 返空，跳过。
		if err := e.enforceRoleWritePath(ctx, absPath); err != nil {
			return &Result{Tool: "WriteFile", Path: absPath, Error: err.Error()}
		}
	}

	// 乐观锁（TODO #16 T16）：expected_mtime 传入时写入前比对，拦截并发覆盖。
	if err := checkExpectedMtime(absPath, expectedMtime); err != nil {
		return &Result{Tool: "WriteFile", Path: absPath, Error: err.Error()}
	}

	// 确保目标文件所在目录存在，不存在时递归创建。
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return &Result{Tool: "WriteFile", Path: absPath, Error: "mkdir: " + err.Error()}
	}
	// 写入前记录原文件大小：供截断对比警告与快照决策（须在覆盖前取）。
	var prevSize int64 = -1
	if fi, err := os.Stat(absPath); err == nil && fi.Mode().IsRegular() {
		prevSize = fi.Size()
	}
	// 极端缩小硬拒绝：原文件较大且新内容远小于原文件时，几乎肯定是模型把 WriteFile
	// 当局部替换用了（只发修改片段）。拒收并引导模型改用 EditFile 局部替换；若确为有意精简，
	// 模型可显式传 confirm_shrink=true 绕过（合法大幅删减仍可放行）。
	if prevSize >= minWriteFileShrinkRefuseBytes &&
		int64(len(content)) < int64(float64(prevSize)*writeFileShrinkRefuseRatio) &&
		!confirmShrink {
		return &Result{Tool: "WriteFile", Path: absPath, Error: fmt.Sprintf(
			"refused: 新内容 %d 字节，仅为原文件 %d 字节的 %d%%。"+
				"WriteFile 是整文件覆盖，不是局部替换--只发修改片段会把原文件整文件覆盖为片段，造成数据丢失。"+
				"修改已有文件请改用 EditFile 局部替换（old_string=原文片段，new_string=新片段）；"+
				"若确为有意的极端精简，请加参数 confirm_shrink=true。",
			len(content), prevSize, int64(len(content))*100/prevSize)}
	}
	// 写入前快照原文件：os.WriteFile 是整文件覆盖，截断写入会把原文件写残（实证：
	// 22495B->509B、59888B->755B），无备份则只能靠 LLM 从片段记忆重建。快照存到
	// .bma/snapshots/<sessionID>/<relPath>.<timestamp>.bak，截断发现后可直接恢复。
	// 仅对非 temporary 的已存在文件做快照；新文件与临时文件跳过。
	var snapPath string
	if !temporary && prevSize >= 0 {
		snapPath = e.snapshotBeforeWrite(ctx, absPath)
	}
	// 执行文件写入。
	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return &Result{Tool: "WriteFile", Path: absPath, Error: err.Error()}
	}

	// 构造成功结果对象。
	result := &Result{
		Tool:    "WriteFile",
		Success: true,
		Output:  fmt.Sprintf("wrote %d bytes", len(content)),
		Path:    absPath,
	}
	// 截断对比警告：新内容远小于原文件时提醒。截断写入（LLM 输出被 max_tokens
	// 切断、端点补齐 JSON）的新内容仅为原文件一小段，且 node --check 在截断
	// 边界仍可能通过（实证 monster.js 730 行被截断写入 64 行后验收"通过"），
	// 模型靠此警告自查重写完整文件。仅警告不拒绝（合法的大幅删减需放行，
	// 极端缩小由上面的硬拒绝兜底）。
	if prevSize >= minWriteFileSizeWarnBytes && int64(len(content)) < int64(float64(prevSize)*writeFileSizeWarnRatio) {
		result.Output += fmt.Sprintf("；写入警告: 新内容 %d 字节，仅为原文件 %d 字节的 %d%%。"+
			"若为意外截断（内容不完整）请 ReadFile 完整文件后重写；若为有意的精简可忽略（极端缩小需 confirm_shrink=true 才放行）",
			len(content), prevSize, int64(len(content))*100/prevSize)
	}
	// 附加快照路径：截断发现后可从快照恢复，避免靠 LLM 重建。
	if snapPath != "" {
		result.Output += fmt.Sprintf("；原文件已备份到 %s", snapPath)
	}
	// 若为临时文件，标记结果并记录临时目录，便于会话结束后清理。
	if temporary {
		result.IsTemporary = true
		result.TempDir = tempDir
		// 输出落盘绝对路径（TODO #38-4 根因 D）：临时文件落在会话级 .bma/tmp/<sid>/ 下，
		// 路径不可预测，只写 "wrote N bytes" 会让 Agent 首次运行必猜工作目录路径然后失败
		//（事故实证：ca-5 用 <工作目录>\verify-frost.js 找不到模块，白耗 1 条连杀额度）。
		result.Output = fmt.Sprintf("wrote %d bytes to %s（会话临时目录，运行用 $env:BMA_SESSION_TEMP_DIR\\%s）",
			len(content), absPath, filepath.Base(absPath))
	}
	// 返回结果。
	return result
}

// snapshotRetention 是 WriteFile 快照的保留时长：超过即自动删除。
// 取 24h：截断通常在写入后短时间内发现（同会话/次日），1 天足够恢复窗口；
// 有 git 的项目历史版本更全，快照只是近期兜底。无 git 项目也按此时长，避免盘积压。
const snapshotRetention = 24 * time.Hour

// snapshotBeforeWrite 在覆盖前把原文件复制到 .bma/snapshots/<sessionID>/<relPath>.<timestamp>.bak。
// 调用方须保证 absPath 指向已存在的常规文件。返回快照绝对路径；失败时返回空串（不阻塞写入）。
// 设计：os.WriteFile 整文件覆盖不可逆，截断写入会把原文件写残（22495B->509B 事故），
// 无备份只能靠 LLM 从片段记忆重建。快照给用户/Agent 一条恢复路径。
// 清理：每次写新快照后扫 .bma/snapshots/ 全目录，删 mtime 超过 snapshotRetention 的 .bak 文件。
// 扫描全量（跨 sessionID）保证废弃会话的快照也能被回收，成本可接受（每次 WriteFile 一次 Walk）。
func (e *Executor) snapshotBeforeWrite(ctx context.Context, absPath string) string {
	sessionID := SessionIDFromContext(ctx)
	workDir := e.workDirOf(ctx)
	if sessionID == "" || workDir == "" {
		return ""
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(workDir, absPath)
	if err != nil || rel == "" {
		rel = filepath.Base(absPath)
	}
	// rel 用 OS 原生分隔符，快照路径在 Windows 下形如 css\style.css.20260812-150530.bak。
	// 时间戳用秒级精度：同一文件秒内多次写入只保留最后一次快照（够用，避免噪声）。
	stamp := time.Now().Format("20060102-150405")
	snapPath := filepath.Join(workDir, ".bma", "snapshots", sessionID, rel+"."+stamp+".bak")
	if err := os.MkdirAll(filepath.Dir(snapPath), 0755); err != nil {
		return ""
	}
	if err := os.WriteFile(snapPath, data, 0644); err != nil {
		return ""
	}
	// 写完后异步清理过期快照：不阻塞当前写入，失败静默（best-effort）。
	go e.cleanExpiredSnapshots(workDir)
	return snapPath
}

// cleanExpiredSnapshots 扫描 <workDir>/.bma/snapshots/ 全目录，删除 mtime 超过 snapshotRetention 的 .bak 文件。
// best-effort：Walk/Stat/Remove 任一错误均跳过，不影响主流程。空目录保留（无伤害）。
func (e *Executor) cleanExpiredSnapshots(workDir string) {
	root := filepath.Join(workDir, ".bma", "snapshots")
	cutoff := time.Now().Add(-snapshotRetention)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 父目录不可达等，跳过
		}
		if info.IsDir() {
			return nil
		}
		// 仅清 .bak 文件，避免误删他物。
		if !strings.HasSuffix(info.Name(), ".bak") {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
		return nil
	})
}

// ---- RestoreFile（从快照恢复文件） ----

// restoreFile 把文件恢复到 .bma/snapshots 中最近（或指定时间戳）的快照版本。
// 与 WriteFile/EditFile 同一套写沙箱校验；恢复本身是覆盖操作，
// 覆盖前会给当前内容再留一份快照（恢复可回退）。
func (e *Executor) restoreFile(ctx context.Context, args map[string]any) *Result {
	// 从参数中取出各字段，缺失时使用零值。
	path, _ := args["path"].(string)
	stamp, _ := args["timestamp"].(string)
	if path == "" {
		return &Result{Tool: "RestoreFile", Error: "path is required"}
	}
	// 解析目标路径并做与 WriteFile 相同的写路径校验（沙箱边界 + 角色级写沙箱）。
	absPath := e.resolvePath(ctx, path)
	if err := e.sanitizeWritePath(ctx, absPath); err != nil {
		return &Result{Tool: "RestoreFile", Path: absPath, Error: err.Error()}
	}
	if err := e.enforceRoleWritePath(ctx, absPath); err != nil {
		return &Result{Tool: "RestoreFile", Path: absPath, Error: err.Error()}
	}
	workDir := e.workDirOf(ctx)
	if workDir == "" {
		return &Result{Tool: "RestoreFile", Path: absPath, Error: "no work directory in context"}
	}
	// 查找快照：优先本会话目录，找不到时回退扫描全部会话目录（快照跨会话保留 24h，
	// 会话重启后新 sessionID 下也能找回旧快照）。
	snapPath, available := findSnapshot(workDir, SessionIDFromContext(ctx), absPath, stamp)
	if snapPath == "" {
		msg := fmt.Sprintf("未找到 %s 的快照。快照仅在 WriteFile/EditFile/RestoreFile 覆盖已存在文件前自动生成，保留 %v。",
			path, snapshotRetention)
		if len(available) > 0 {
			msg += fmt.Sprintf("该文件可用快照时间戳：%s", strings.Join(available, ", "))
		}
		return &Result{Tool: "RestoreFile", Path: absPath, Error: msg}
	}
	data, err := os.ReadFile(snapPath)
	if err != nil {
		return &Result{Tool: "RestoreFile", Path: absPath, Error: "read snapshot: " + err.Error()}
	}
	// 确保目标文件所在目录存在（误删场景可能连目录一并被删）。
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return &Result{Tool: "RestoreFile", Path: absPath, Error: "mkdir: " + err.Error()}
	}
	// 覆盖前给当前内容留快照：恢复操作本身可回退（与 WriteFile/EditFile 同一机制）。
	backupPath := ""
	if fi, serr := os.Stat(absPath); serr == nil && fi.Mode().IsRegular() {
		backupPath = e.snapshotBeforeWrite(ctx, absPath)
	}
	if err := os.WriteFile(absPath, data, 0644); err != nil {
		return &Result{Tool: "RestoreFile", Path: absPath, Error: err.Error()}
	}
	out := fmt.Sprintf("restored %d bytes from snapshot %s", len(data), snapPath)
	if backupPath != "" {
		out += fmt.Sprintf("；覆盖前内容已备份到 %s", backupPath)
	}
	return &Result{Tool: "RestoreFile", Success: true, Output: out, Path: absPath}
}

// findSnapshot 在 <workDir>/.bma/snapshots/ 下查找 absPath 对应的快照文件。
// 快照命名与 snapshotBeforeWrite 一致：<sessionID>/<relPath>.<stamp>.bak（stamp 格式 20060102-150405，
// 字典序即时间序）。stamp 非空时精确匹配该时间戳，否则返回最新一份。
// 返回快照绝对路径与可用时间戳列表（去重升序，供错误提示）；未找到时快照路径为空串。
func findSnapshot(workDir, sessionID, absPath, stamp string) (snapPath string, available []string) {
	rel, err := filepath.Rel(workDir, absPath)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
		rel = filepath.Base(absPath)
	}
	root := filepath.Join(workDir, ".bma", "snapshots")
	// 候选会话目录：本会话优先，其余会话目录随后（跨会话回退）。
	var dirs []string
	if sessionID != "" {
		dirs = append(dirs, filepath.Join(root, sessionID))
	}
	if entries, rerr := os.ReadDir(root); rerr == nil {
		for _, ent := range entries {
			if ent.IsDir() && ent.Name() != sessionID {
				dirs = append(dirs, filepath.Join(root, ent.Name()))
			}
		}
	}
	base := filepath.Base(rel)
	relDir := filepath.Dir(rel)
	stampSet := map[string]bool{}
	best, bestStamp := "", ""
	for _, d := range dirs {
		dir := filepath.Join(d, relDir)
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			continue
		}
		for _, ent := range entries {
			name := ent.Name()
			if ent.IsDir() || !strings.HasPrefix(name, base+".") || !strings.HasSuffix(name, ".bak") {
				continue
			}
			s := strings.TrimSuffix(strings.TrimPrefix(name, base+"."), ".bak")
			if !stampSet[s] {
				stampSet[s] = true
				available = append(available, s)
			}
			if stamp != "" {
				// 指定时间戳：精确匹配即返回。
				if s == stamp {
					return filepath.Join(dir, name), available
				}
				continue
			}
			if s > bestStamp {
				bestStamp, best = s, filepath.Join(dir, name)
			}
		}
	}
	slices.Sort(available)
	return best, available
}

// ---- EditFile（精确局部替换） ----

// maxEditStringRunes 是 EditFile 单次 old_string/new_string 的长度上限。
// EditFile 定位是"小改局部替换"（TODO #49）：替换片段超大说明改动面接近整文件，
// 应退回 WriteFile 整写（含截断防护/缩小警告全套保护）。
const maxEditStringRunes = 50000

// fuzzyEditRange 是一次空白宽容匹配命中的区域：start 为 0-based 起始行号，lines 为行数。
type fuzzyEditRange struct {
	start int
	lines int
}

// fuzzyEditRegions 在 content 中做空白宽容匹配：content 与 old 按行拆分后逐行
// TrimSpace 做连续子序列比对（old 首尾空行忽略），返回全部命中区域。
// 覆盖场景：old_string 基于过期读取，文件缩进/行尾空白已被并发修改，逐字符
// 匹配落空但空白归一后仍能对上。内容本身有差异的行不命中。
func fuzzyEditRegions(content, old string) []fuzzyEditRange {
	trimLines := func(s string) []string {
		lines := strings.Split(s, "\n")
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = strings.TrimSpace(l)
		}
		return out
	}
	trimmedContent := trimLines(content)
	trimmedOld := trimLines(old)
	for len(trimmedOld) > 0 && trimmedOld[0] == "" {
		trimmedOld = trimmedOld[1:]
	}
	for len(trimmedOld) > 0 && trimmedOld[len(trimmedOld)-1] == "" {
		trimmedOld = trimmedOld[:len(trimmedOld)-1]
	}
	if len(trimmedOld) == 0 {
		return nil
	}
	var out []fuzzyEditRange
	for i := 0; i+len(trimmedOld) <= len(trimmedContent); i++ {
		match := true
		for j := range trimmedOld {
			if trimmedContent[i+j] != trimmedOld[j] {
				match = false
				break
			}
		}
		if match {
			out = append(out, fuzzyEditRange{start: i, lines: len(trimmedOld)})
			i += len(trimmedOld) - 1
		}
	}
	return out
}

// replaceFuzzyRegions 把命中区域整段替换为 newStr（按行插入），从后往前替换防行号漂移。
func replaceFuzzyRegions(content string, regions []fuzzyEditRange, newStr string) string {
	lines := strings.Split(content, "\n")
	newLines := strings.Split(newStr, "\n")
	for i := len(regions) - 1; i >= 0; i-- {
		r := regions[i]
		updated := make([]string, 0, len(lines)-r.lines+len(newLines))
		updated = append(updated, lines[:r.start]...)
		updated = append(updated, newLines...)
		updated = append(updated, lines[r.start+r.lines:]...)
		lines = updated
	}
	return strings.Join(lines, "\n")
}

// editFile 在已存在文件中做精确局部替换：old_string 唯一匹配（或 replace_all）替换为
// new_string。与 WriteFile 对齐的机制：写守卫/沙箱/角色写路径校验、.bma/snapshots 快照、
// 成功后共享记忆失效与 PROJECT.md 去抖刷新（后者由 Registry.Dispatch 统一触发）。
// 匹配健壮性：行尾 \r\n 与 \n 视为等价（按文件主行尾风格写回，保持全文件风格一致）。
func (e *Executor) editFile(ctx context.Context, args map[string]any) *Result {
	// 从参数中取出各字段，缺失时使用零值。
	path, _ := args["path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)
	replaceAll, _ := args["replace_all"].(bool)
	allowSpaces, _ := args["allow_spaces"].(bool)
	expectedMtime, _ := args["expected_mtime"].(string)

	if path == "" {
		return &Result{Tool: "EditFile", Error: "path is required"}
	}
	if oldStr == "" {
		return &Result{Tool: "EditFile", Path: path, Error: "old_string is required（要替换的原文片段不能为空）"}
	}
	if oldStr == newStr {
		return &Result{Tool: "EditFile", Path: path, Error: "old_string 与 new_string 相同，无需编辑"}
	}
	if len([]rune(newStr)) > maxEditStringRunes {
		return &Result{Tool: "EditFile", Path: path, Error: fmt.Sprintf(
			"new_string too large: %d runes (max %d)。EditFile 是局部替换，替换片段不应超大；"+
				"改动面接近整文件时请用 WriteFile 整文件重写", len([]rune(newStr)), maxEditStringRunes)}
	}

	// 调用写保护守卫进行策略校验（与 WriteFile 同口径：保护目录/邮箱文件等）。
	if err := e.guards.CheckWrite(path, newStr, allowSpaces); err != nil {
		return &Result{Tool: "EditFile", Path: path, Error: err.Error()}
	}

	// 解析目标路径并做沙箱/角色级写路径校验（与 WriteFile 一致）。
	absPath := e.resolvePath(ctx, path)
	if err := e.sanitizeWritePath(ctx, absPath); err != nil {
		return &Result{Tool: "EditFile", Path: absPath, Error: err.Error()}
	}
	if err := e.enforceRoleWritePath(ctx, absPath); err != nil {
		return &Result{Tool: "EditFile", Path: absPath, Error: err.Error()}
	}

	// 乐观锁（TODO #16 T16）：expected_mtime 传入时写入前比对，拦截并发覆盖
	//（old_string 基于过期内容时，空白宽容匹配还可能误换到别的区域）。
	if err := checkExpectedMtime(absPath, expectedMtime); err != nil {
		return &Result{Tool: "EditFile", Path: absPath, Error: err.Error()}
	}

	// 读取现有文件：EditFile 只编辑已存在文件。
	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &Result{Tool: "EditFile", Path: absPath, Error: "文件不存在: " + path +
				"。EditFile 只能编辑已存在文件；新建文件请用 WriteFile。"}
		}
		return &Result{Tool: "EditFile", Path: absPath, Error: "read: " + err.Error()}
	}

	// 匹配：两侧行尾统一为 \n 再计数（\r\n 与 \n 等价），缩进/空格必须逐字符一致。
	normContent := normalizeCRLF(string(data))
	normOld := normalizeCRLF(oldStr)
	normNew := normalizeCRLF(newStr)
	count := strings.Count(normContent, normOld)
	var newContent string
	var fuzzyWarn string
	line := 1
	switch {
	case count == 0:
		// 任务137 空白宽容回退：old_string 基于过期读取（文件被并发修改/缩进漂移）时
		// 逐字符匹配落空。逐行 TrimSpace 序列比对作为"自动重读"替代——命中即按实际
		// 原文区域替换并附警告，省一轮"报错→重读→重试"往返（实证 wuhaotui 两处
		// EditFile FAIL 后用户杀任务）。
		regions := fuzzyEditRegions(normContent, normOld)
		if len(regions) == 0 {
			// 无匹配：返回就近上下文提示（文件开头片段）帮助模型自查，而非静默。
			hint := truncateRunes(normContent, 200)
			if hint == "" {
				hint = "（文件为空）"
			}
			return &Result{Tool: "EditFile", Path: absPath, Error: fmt.Sprintf(
				"old_string 未在文件中找到（含空白宽容匹配）。old_string 必须与文件内容逐字符一致（含缩进/空格；"+
					"行尾 \\r\\n 与 \\n 视为等价）。请先用 SearchInFiles/ReadFile 定位当前实际文本再重试。\n文件开头片段：\n%s", hint)}
		}
		if len(regions) > 1 && !replaceAll {
			return &Result{Tool: "EditFile", Path: absPath, Error: fmt.Sprintf(
				"old_string 空白宽容匹配在文件中出现 %d 处，不唯一。请扩大 old_string 的上下文（包含周围独特行）"+
					"使其唯一，或显式传 replace_all=true 替换全部 %d 处。", len(regions), len(regions))}
		}
		line = regions[0].start + 1
		newContent = replaceFuzzyRegions(normContent, regions, normNew)
		count = len(regions)
		fuzzyWarn = "；old_string 与当前文件存在空白差异（文件可能已被修改），已按空白宽容匹配替换，建议核对周边内容"
	case count > 1 && !replaceAll:
		return &Result{Tool: "EditFile", Path: absPath, Error: fmt.Sprintf(
			"old_string 在文件中出现 %d 次，不唯一。请扩大 old_string 的上下文（包含周围独特行）"+
				"使其唯一，或显式传 replace_all=true 替换全部 %d 处。", count, count)}
	default:
		// 应用替换：replace_all 全替换，否则仅第一处。
		if replaceAll {
			newContent = strings.ReplaceAll(normContent, normOld, normNew)
		} else {
			newContent = strings.Replace(normContent, normOld, normNew, 1)
		}
		// 首个匹配位置的行号（1-based），供结果报告。
		firstIdx := strings.Index(normContent, normOld)
		line = 1 + strings.Count(normContent[:firstIdx], "\n")
	}

	// 按文件主行尾风格写回：原文 CRLF 为主时整体转回 CRLF，保持全文件风格一致。
	crlf := bytes.Count(data, []byte("\r\n"))
	lfOnly := bytes.Count(data, []byte("\n")) - crlf
	if crlf > 0 && crlf >= lfOnly {
		newContent = strings.ReplaceAll(newContent, "\n", "\r\n")
	}

	// 写入前快照原文件（与 WriteFile 同一 .bma/snapshots 机制，覆盖前留恢复点）。
	snapPath := e.snapshotBeforeWrite(ctx, absPath)
	if err := os.WriteFile(absPath, []byte(newContent), 0644); err != nil {
		return &Result{Tool: "EditFile", Path: absPath, Error: err.Error()}
	}

	result := &Result{
		Tool:    "EditFile",
		Success: true,
		Output:  fmt.Sprintf("replaced %d occurrence(s) at line %d (%d -> %d bytes)%s", count, line, len(data), len(newContent), fuzzyWarn),
		Path:    absPath,
	}
	if snapPath != "" {
		result.Output += fmt.Sprintf("；原文件已备份到 %s", snapPath)
	}
	return result
}

// normalizeCRLF 把 \r\n 统一为 \n，用于匹配时行尾等价。
func normalizeCRLF(s string) string {
	if !strings.Contains(s, "\r\n") {
		return s
	}
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// truncateRunes 按 rune 数截断字符串并追加省略号（省略号占 1 个 rune）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---- ListDir（列出目录） ----

// listDir 列出指定目录下的条目，并标注每个条目的类型与大小。
func (e *Executor) listDir(ctx context.Context, args map[string]any) *Result {
	// 从参数中读取目录路径，空字符串时默认当前目录。
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	// 解析路径并校验沙箱约束。
	absPath, err := e.resolvePathWithSandbox(ctx, path)
	if err != nil {
		return &Result{Tool: "ListDir", Path: absPath, Error: err.Error()}
	}
	// 读取目录下所有条目。
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return &Result{Tool: "ListDir", Path: absPath, Error: err.Error()}
	}
	// lines 用于拼接最终的目录列表文本。
	var lines []string
	// 遍历每个目录条目。
	for _, entry := range entries {
		// 默认前缀表示普通文件。
		prefix := "  "
		if entry.IsDir() {
			// 目录条目使用 D 前缀标识。
			prefix = "D "
		}
		// 获取条目详细信息，失败时不影响列出名称。
		info, _ := entry.Info()
		// size 默认空，若成功获取信息则格式化为 8 位宽数字字符串。
		size := ""
		if info != nil {
			size = fmt.Sprintf("%8d", info.Size())
		}
		// 将类型前缀、大小、名称组合成一行。
		lines = append(lines, fmt.Sprintf("%s %s %s", prefix, size, entry.Name()))
	}
	// 返回拼接后的目录列表。
	return &Result{Tool: "ListDir", Success: true, Output: strings.Join(lines, "\n"), Path: absPath}
}

// ---- SearchInFiles（文件内搜索） ----

// searchInFiles 在指定目录下按扩展名白名单递归搜索包含指定模式的文本行。
func (e *Executor) searchInFiles(ctx context.Context, args map[string]any) *Result {
	// 从参数中读取搜索模式与目录，目录缺失时使用当前目录。
	pattern, _ := args["pattern"].(string)
	dir, _ := args["dir"].(string)
	if dir == "" {
		dir = "."
	}
	// 解析并校验搜索目录的沙箱约束。
	absDir, err := e.resolvePathWithSandbox(ctx, dir)
	if err != nil {
		return &Result{Tool: "SearchInFiles", Path: absDir, Error: err.Error()}
	}
	// 将搜索模式转为小写，实现大小写不敏感匹配。
	// pattern 含 "|" 时按关键词拆分、任意命中即记一行（贴近模型 grep 习惯，
	// 消除"a|b 交替模式零命中"的最大踩坑点；代价是字面含 | 的文本无法精确匹配，可接受）。
	patternLower := strings.ToLower(pattern)
	patterns := []string{patternLower}
	if strings.Contains(patternLower, "|") {
		patterns = nil
		for _, p := range strings.Split(patternLower, "|") {
			if p = strings.TrimSpace(p); p != "" {
				patterns = append(patterns, p)
			}
		}
	}
	// exts 定义允许搜索的文件扩展名白名单。
	exts := []string{".go", ".py", ".js", ".ts", ".java", ".yaml", ".yml", ".md", ".txt", ".json", ".toml", ".css", ".html"}
	// lines 保存所有命中的搜索结果行。
	var lines []string
	// 递归遍历目录下的文件与目录。
	filepath.WalkDir(absDir, func(path string, d os.DirEntry, err error) error {
		// 遇到遍历错误或结果已满 500 条时跳过当前路径。
		if err != nil || len(lines) > 500 {
			return nil
		}
		// 对目录进行特殊处理：跳过常见的依赖/版本控制目录。
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		// 获取文件扩展名并转为小写。
		ext := strings.ToLower(filepath.Ext(d.Name()))
		// 如果扩展名不在白名单中则跳过该文件。
		if !slices.Contains(exts, ext) {
			return nil
		}
		// 读取文件全部内容。
		data, err := os.ReadFile(path)
		if err != nil {
			// 无权限或读取失败时忽略该文件。
			return nil
		}
		// 将字节转换为字符串以便逐行匹配。
		content := string(data)
		// 按行切分并逐行检查是否包含任一关键词。
		for i, line := range strings.Split(content, "\n") {
			lineLower := strings.ToLower(line)
			for _, p := range patterns {
				if strings.Contains(lineLower, p) {
					// 计算相对路径以提升结果可读性。
					relPath, _ := filepath.Rel(absDir, path)
					// 记录相对路径、行号、去空白后的内容。
					lines = append(lines, fmt.Sprintf("%s:%d: %s", relPath, i+1, strings.TrimSpace(line)))
					break
				}
			}
		}
		return nil
	})
	// 构造结果对象。
	result := &Result{
		Tool:   "SearchInFiles",
		Path:   absDir,
		Output: strings.Join(lines, "\n"),
		// 查无此物是有效信息而非失败：零命中标记成功，
		// 输出提示文案供模型区分"确认不存在"与"工具出错"。
		Success: true,
	}
	// 零命中时给出明确提示，避免模型误判为工具失败而无效重试。
	if len(lines) == 0 {
		result.Output = fmt.Sprintf("（无匹配：%d 个关键词未在任何扩展名白名单文件中命中。如需精确定位请检查关键词拼写或改用 ReadFile 逐文件查看）", len(patterns))
	}
	// 若输出过长，按配置的最大字符数截断。
	if maxChars := e.agentConfig().ReadFileMaxChars; len(result.Output) > maxChars {
		result.Output = result.Output[:maxChars] + "\n... (truncated)"
	}
	// 返回搜索结果。
	return result
}

// ---- RunCommand（执行命令） ----

// psForceUTF8Prefix 是 Windows 下 PowerShell 命令的前导语句：
// 把控制台输出编码与管道编码都切到 UTF-8，避免中文系统 GBK/936 输出被 Go 端按 UTF-8 读成乱码。
const psForceUTF8Prefix = "[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new();$OutputEncoding=[System.Text.UTF8Encoding]::new();"

// shellCommand 描述一次 RunCommand 的解释器调用方式。
type shellCommand struct {
	Exe       string   // 解释器可执行文件
	Args      []string // 前置参数（-c / -Command），命令串追加其后
	ForceUTF8 bool     // PowerShell：命令串前置 UTF-8 编码语句
	ExtraEnv  []string // 解释器专属附加环境变量
}

func unixShell() shellCommand {
	return shellCommand{Exe: "sh", Args: []string{"-c"}}
}

func bashShell(exe string) shellCommand {
	// MSYS_NO_PATHCONV=1 关闭 Git Bash 对以 / 开头参数的自动路径改写（防命令串被误转换）。
	return shellCommand{Exe: exe, Args: []string{"-c"}, ExtraEnv: []string{"MSYS_NO_PATHCONV=1"}}
}

// windowsShell 探测可用解释器：优先 Git Bash（bash -c）。模型生成 bash 命令的
// 准确率显著高于 PowerShell，且 PowerShell 的三类实证坑（npm.ps1 执行策略拦截、
// GBK 重定向损坏中文、node -e 引号转义）在 bash 下全部不存在；Git for Windows
// 是 worktree 派发的硬依赖，bash.exe 基本必在。探测顺序：PATH 中的 bash →
// git.exe 同级推断 → 常见安装路径；排除 WSL 的 System32\bash.exe stub（Linux
// 文件系统/工具链语义与宿主会话不通用）；全落空回落 powershell -Command。
// lookPath/fileExists 参数便于单测注入。
func windowsShell(lookPath func(string) (string, error), fileExists func(string) bool) shellCommand {
	if p, err := lookPath("bash"); err == nil && !isWSLBashStub(p) {
		return bashShell(p)
	}
	if p, err := lookPath("git"); err == nil {
		dir := filepath.Dir(p)
		for _, cand := range []string{
			filepath.Join(dir, "..", "bin", "bash.exe"),
			filepath.Join(dir, "..", "usr", "bin", "bash.exe"),
			filepath.Join(dir, "bash.exe"),
		} {
			if fileExists(cand) {
				return bashShell(cand)
			}
		}
	}
	for _, cand := range []string{
		`C:\Program Files\Git\bin\bash.exe`,
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Git", "bin", "bash.exe"),
	} {
		if fileExists(cand) {
			return bashShell(cand)
		}
	}
	return shellCommand{Exe: "powershell", Args: []string{"-NoLogo", "-NoProfile", "-Command"}, ForceUTF8: true}
}

// isWSLBashStub 排除 Windows 自带的 WSL bash 启动器（System32 下），
// 其 cwd/路径语义与宿主会话不通用，误用会错乱工作目录。
func isWSLBashStub(p string) bool {
	return strings.Contains(strings.ToLower(filepath.Clean(p)), `\system32\`)
}

// cachedShell 进程内缓存解释器探测结果（探测含磁盘访问，进程生命周期内不变）。
var cachedShell = sync.OnceValue(func() shellCommand {
	if runtime.GOOS != "windows" {
		return unixShell()
	}
	return windowsShell(exec.LookPath, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
})

// parseMkdirDir 解析形如 "mkdir -p /some/dir" 的命令字符串，尝试提取目标目录。
func parseMkdirDir(cmd string) string {
	// 去除首尾空白。
	cmd = strings.TrimSpace(cmd)
	// 如果命令不是以 mkdir 开头，则返回空表示无法解析。
	if !strings.HasPrefix(cmd, "mkdir") {
		return ""
	}
	// 去掉 "mkdir" 前缀。
	rest := strings.TrimSpace(strings.TrimPrefix(cmd, "mkdir"))
	// 去掉可选的 "-p" 参数。
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "-p"))
	// 剩余部分即目标目录。
	return rest
}

// runCommand 执行外部命令或处理 mkdir 快捷命令，并返回输出结果。
func (e *Executor) runCommand(ctx context.Context, args map[string]any) *Result {
	// 从参数中读取命令字符串，空字符串表示未提供命令。
	cmdStr, _ := args["command"].(string)
	if cmdStr == "" {
		return &Result{Tool: "RunCommand", Error: "command is required"}
	}

	// 调用命令守卫检查命令是否被策略禁止。
	if blocked, reason := e.guards.CheckCommand(cmdStr); blocked {
		return &Result{Tool: "RunCommand", Error: reason}
	}

	// 如果命令是 mkdir，则直接本地处理而不调用外部 shell。
	if dir := parseMkdirDir(cmdStr); dir != "" {
		// 解析目标目录路径。
		absDir := e.resolvePath(ctx, dir)
		// 校验目录是否允许写入。
		if err := e.sanitizeWritePath(ctx, absDir); err != nil {
			return &Result{Tool: "RunCommand", Error: err.Error()}
		}
		// 递归创建目录。
		if err := os.MkdirAll(absDir, 0755); err != nil {
			return &Result{Tool: "RunCommand", Error: "mkdir: " + err.Error()}
		}
		// 返回目录创建成功的结果。
		return &Result{Tool: "RunCommand", Success: true, Output: "created: " + absDir}
	}

	// 默认使用 Executor 自身的超时时间。
	timeout := e.timeout
	// 若参数中显式提供了超时秒数且为正数，则覆盖默认超时。
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	// 限制超时不超过 Agent 配置中允许的最大命令执行时间。
	if maxTimeout := time.Duration(e.agentConfig().RunCommandTimeoutSec) * time.Second; timeout > maxTimeout {
		timeout = maxTimeout
	}

	// 基于当前上下文创建带超时的子上下文。
	ctx, cancel := context.WithTimeout(ctx, timeout)
	// 函数返回时取消上下文，释放相关资源。
	defer cancel()

	// 选择命令解释器（Windows 优先 Git Bash，回落 PowerShell；类 Unix 用 sh -c）。
	sc := cachedShell()
	// 组装 argv：PowerShell 命令串前置 UTF-8 编码语句（原生命令直写 stdout 的
	// GBK 字节由 DecodeCommandOutput 回退解码兜底）。
	argv := append([]string{}, sc.Args...)
	if sc.ForceUTF8 {
		argv = append(argv, psForceUTF8Prefix+cmdStr)
	} else {
		argv = append(argv, cmdStr)
	}
	cmd := exec.CommandContext(ctx, sc.Exe, argv...)
	// 设置命令的工作目录为当前会话的工作目录。
	cmd.Dir = e.workDirOf(ctx)
	// 解释器专属环境变量 + 会话临时目录通过环境变量暴露给子进程。
	cmd.Env = append(os.Environ(), sc.ExtraEnv...)
	if sessionID := SessionIDFromContext(ctx); sessionID != "" {
		cmd.Env = append(cmd.Env, "BMA_SESSION_TEMP_DIR="+e.sessionTempDir(ctx, sessionID))
	}

	// stdout 与 stderr 用于缓存命令输出。
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// 执行命令，Windows 下使用树形进程kill以处理超时。
	err := runCommandWithTreeKill(ctx, cmd)

	// 清理并合并标准输出与标准错误（GBK 控制台输出回退解码，见 DecodeCommandOutput）。
	output := DecodeCommandOutput(stdout.Bytes())
	if stderr.Len() > 0 {
		output += "\n[stderr]\n" + DecodeCommandOutput(stderr.Bytes())
	}
	// 检测输出是否暗示端口占用，并追加友好提示。
	if hint := detectPortConflictHint(cmdStr, output); hint != "" {
		output += "\n[port-conflict]\n" + hint
	}
	// 若输出超过最大限制，则截断并追加提示。
	if maxOut := e.agentConfig().RunCommandMaxOutput; len(output) > maxOut {
		output = output[:maxOut] + "\n... (truncated)"
	}

	// 构造结果对象，默认 Success 由命令是否出错决定。
	result := &Result{
		Tool:    "RunCommand",
		Path:    e.workDirOf(ctx),
		Output:  output,
		Success: err == nil,
	}
	// 若命令执行出错，将错误信息写入结果。
	if err != nil {
		result.Error = err.Error()
	}
	// 返回命令执行结果。
	return result
}

// runCommandWithTreeKill 运行命令并在超时或取消时尝试结束整个进程树。
func runCommandWithTreeKill(ctx context.Context, cmd *exec.Cmd) error {
	// 非 Windows 平台直接调用 cmd.Run，由 context 驱动取消（保持原有行为：
	// killProcessTree 的进程组 kill 在未设置 Setpgid 时会误杀自身进程组）。
	if runtime.GOOS != "windows" {
		return cmd.Run()
	}
	// Windows 平台需要手动启动并等待，以便在超时后杀掉进程树。
	if err := cmd.Start(); err != nil {
		return err
	}
	// done 通道用于接收命令结束信号，缓冲 1 防止 goroutine 泄漏。
	done := make(chan error, 1)
	// 启动 goroutine 等待命令结束，并将错误发送到 done。
	go func() {
		done <- cmd.Wait()
	}()
	// 同时监听上下文取消与命令结束事件。
	select {
	case <-ctx.Done():
		// 上下文已取消（通常因超时），若进程存在则杀掉整个进程树。
		if cmd.Process != nil {
			killProcessTree(cmd.Process.Pid)
		}
		// 等待 Wait 返回，避免 goroutine 泄漏与僵尸进程；但孙进程继承 stdout/stderr
		// 句柄时 Wait 会因管道不到 EOF 而永久阻塞（实证 td-game domain 挂死 74 分钟），
		// 故设宽限期，到期放弃等待直接返回超时错误——进程树已杀，泄漏的 Wait goroutine
		// 会在管道最终关闭后自行退出。
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			slog.Warn("cmd.Wait did not return after process tree kill, abandoning wait", slog.String("error", ctx.Err().Error()))
		}
		// 返回上下文错误。
		return ctx.Err()
	case err := <-done:
		// 命令自行结束，返回其执行错误（如有）。
		return err
	}
}

// killProcessTree 根据操作系统杀掉指定 PID 及其子进程。
func killProcessTree(pid int) {
	// Windows 使用 taskkill /T /F /PID 强制结束进程树。
	if runtime.GOOS == "windows" {
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run(); err != nil {
			slog.Debug("kill process tree failed", slog.Int("pid", pid), slog.String("error", err.Error()))
		}
	} else {
		// 类 Unix 使用 kill -9 -PGID 结束整个进程组。
		if err := exec.Command("kill", "-9", "-"+strconv.Itoa(pid)).Run(); err != nil {
			slog.Debug("kill process tree failed", slog.Int("pid", pid), slog.String("error", err.Error()))
		}
	}
}

// detectPortConflictHint 检查命令输出中是否包含端口占用相关关键词，并返回中文提示。
func detectPortConflictHint(cmdStr, output string) string {
	// 空输出无需检测。
	if output == "" {
		return ""
	}
	// 将输出转为小写以进行不敏感匹配。
	lowerOut := strings.ToLower(output)
	// conflictPatterns 列出常见的端口占用错误特征字符串。
	conflictPatterns := []string{
		"address already in use",
		"errno -98",
		"eaddrinuse",
		"bind: an attempt was made",
		"通常每个套接字地址",
		"端口已被占用",
		"端口被占用",
		"only one usage of each socket",
		"no permission to use port",
	}
	// matched 保存第一个命中的模式，用于后续提示。
	matched := ""
	// 遍历所有模式并检查输出中是否包含。
	for _, p := range conflictPatterns {
		if strings.Contains(lowerOut, p) {
			matched = p
			// 命中后立即停止，只取第一个。
			break
		}
	}
	// 没有任何模式命中则返回空。
	if matched == "" {
		return ""
	}
	// 尝试从命令字符串中提取端口号。
	port := extractPortFromCommand(cmdStr)
	// 构造中文提示，说明检测到端口占用。
	hint := fmt.Sprintf("检测到端口占用错误（模式: %q）", matched)
	if port != "" {
		// 若成功提取端口，提示用户换用其他端口或杀掉占用进程。
		hint += fmt.Sprintf("。端口 %s 已被占用，请换用 8001/8002/.../8009 重试，", port)
	} else {
		hint += "。请换用其他端口（如 8001-8009）重试，"
	}
	// 追加查找与释放端口的操作指引。
	hint += "或先执行 `netstat -ano | findstr :<port>` 找到占用进程 PID，再 `taskkill /F /PID <pid>` 释放。" +
		"注意：长运行服务器（http.server/flask/node dev server）会被系统拦截，建议改用 `node --check` / `python -m py_compile` 做语法验证。"
	// 返回完整的端口占用提示。
	return hint
}

// extractPortFromCommand 从命令字符串中尝试提取可能绑定的端口号。
func extractPortFromCommand(cmdStr string) string {
	// 空命令直接返回空。
	if cmdStr == "" {
		return ""
	}
	// 优先匹配 "--port" 参数形式。
	if idx := strings.Index(strings.ToLower(cmdStr), "--port"); idx >= 0 {
		// 取 "--port" 之后的子串。
		rest := cmdStr[idx+6:]
		// 去掉可能存在的空格或等号分隔符。
		rest = strings.TrimLeft(rest, " =")
		// num 用于累积连续数字。
		num := ""
		// 遍历后续字符提取数字。
		for _, r := range rest {
			if r >= '0' && r <= '9' {
				num += string(r)
				// 端口号最多 5 位，达到上限即可停止。
				if len(num) >= 5 {
					break
				}
			} else {
				// 一旦遇到非数字且已累积到数字，即可停止。
				if num != "" {
					break
				}
			}
		}
		// 至少 2 位数字才认为是端口号。
		if len(num) >= 2 {
			return num
		}
	}
	// 其次匹配 "-p " 参数形式。
	if idx := strings.Index(cmdStr, "-p "); idx >= 0 {
		// 取 "-p " 之后的子串。
		rest := cmdStr[idx+3:]
		num := ""
		for _, r := range rest {
			if r >= '0' && r <= '9' {
				num += string(r)
				if len(num) >= 5 {
					break
				}
			} else {
				if num != "" {
					break
				}
			}
		}
		if len(num) >= 2 {
			return num
		}
	}
	// 兜底匹配冒号形式（如 :8000）。
	idx := strings.Index(cmdStr, ":")
	for idx >= 0 {
		// 取冒号之后的子串。
		rest := cmdStr[idx+1:]
		num := ""
		for _, r := range rest {
			if r >= '0' && r <= '9' {
				num += string(r)
				if len(num) >= 5 {
					break
				}
			} else {
				if num != "" {
					break
				}
			}
		}
		// 端口号长度在 2 到 5 位之间才接受。
		if len(num) >= 2 && len(num) <= 5 {
			return num
		}
		// 查找下一个冒号继续尝试。
		next := strings.Index(cmdStr[idx+1:], ":")
		if next < 0 {
			break
		}
		idx = idx + 1 + next
	}
	// 最后尝试提取命令中最后一个长度合理的连续数字作为端口候选。
	var lastNum string
	var inNum bool
	curNum := ""
	for _, r := range cmdStr {
		if r >= '0' && r <= '9' {
			curNum += string(r)
			inNum = true
			// 超过 5 位则放弃当前数字段。
			if len(curNum) > 5 {
				curNum = ""
				inNum = false
			}
		} else {
			// 遇到非数字且当前数字段长度合法，则记录为 lastNum。
			if inNum && len(curNum) >= 2 {
				lastNum = curNum
			}
			curNum = ""
			inNum = false
		}
	}
	// 若遍历结束正处于数字段且长度合法，也作为候选。
	if inNum && len(curNum) >= 2 {
		lastNum = curNum
	}
	// 返回最后一个候选数字，可能为空。
	return lastNum
}

// ---- HTTP 工具 ----

// parseStringMap 将任意类型的输入转换为 map[string]string，用于解析 headers 等字段。
func parseStringMap(raw any) map[string]string {
	// 空输入直接返回 nil。
	if raw == nil {
		return nil
	}
	// 根据实际类型分支处理。
	switch m := raw.(type) {
	case map[string]string:
		// 已经是目标类型，直接返回。
		return m
	case map[string]any:
		// 将 map[string]any 的每个值转为字符串。
		out := make(map[string]string, len(m))
		for k, v := range m {
			if s, ok := v.(string); ok {
				// 值为字符串时直接使用。
				out[k] = s
			} else {
				// 非字符串值使用 fmt.Sprint 进行默认格式化。
				out[k] = fmt.Sprint(v)
			}
		}
		return out
	}
	// 其他不支持的类型返回 nil。
	return nil
}

// httpGet 执行 HTTP GET 请求并返回响应状态码与体内容。
func (e *Executor) httpGet(ctx context.Context, args map[string]any) *Result {
	// 读取目标 URL，空 URL 直接返回错误。
	url, _ := args["url"].(string)
	if url == "" {
		return &Result{Tool: "HTTPGet", Error: "url is required"}
	}
	// 解析可选的请求头。
	headers := parseStringMap(args["headers"])
	// 默认使用 Executor 的超时时间。
	timeout := e.timeout
	// 若参数中显式设置超时则覆盖。
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	// 创建带超时的子上下文。
	ctx, cancel := context.WithTimeout(ctx, timeout)
	// 函数返回时取消上下文。
	defer cancel()
	// 构造 GET 请求。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &Result{Tool: "HTTPGet", Path: url, Error: err.Error()}
	}
	// 设置请求头。
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 发送请求。
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &Result{Tool: "HTTPGet", Path: url, Error: err.Error()}
	}
	// 函数返回时关闭响应体，防止连接泄漏。
	defer resp.Body.Close()
	// 读取响应体，限制最大 1MB。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// 响应体是不可信外部源（TODO #18-4 T31）：包 untrusted 围栏（转义防逃逸），
	// 状态码行是我们自己的可信输出，留在围栏外。
	output := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, WrapUntrusted(url, string(body)))
	// 若输出超过 10000 字符则截断。
	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}
	// 返回结果，2xx 视为成功。
	return &Result{
		Tool:    "HTTPGet",
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Output:  output,
		Path:    url,
	}
}

// httpPost 执行 HTTP POST 请求，支持字符串或 JSON 对象作为请求体。
func (e *Executor) httpPost(ctx context.Context, args map[string]any) *Result {
	// 读取目标 URL，空 URL 直接返回错误。
	url, _ := args["url"].(string)
	if url == "" {
		return &Result{Tool: "HTTPPost", Error: "url is required"}
	}
	// 解析可选的请求头。
	headers := parseStringMap(args["headers"])
	// bodyBytes 用于保存最终序列化后的请求体字节。
	var bodyBytes []byte
	// 若参数中包含 body，则根据类型处理。
	if raw, ok := args["body"]; ok {
		switch v := raw.(type) {
		case string:
			// body 为字符串时直接转字节。
			bodyBytes = []byte(v)
		default:
			// 其他类型序列化为 JSON。
			b, err := json.Marshal(v)
			if err != nil {
				return &Result{Tool: "HTTPPost", Path: url, Error: "marshal body: " + err.Error()}
			}
			bodyBytes = b
			// 若请求头中未设置 Content-Type，则默认设置为 application/json。
			if _, ok := headers["Content-Type"]; !ok {
				if headers == nil {
					headers = make(map[string]string)
				}
				headers["Content-Type"] = "application/json"
			}
		}
	}
	// 默认使用 Executor 的超时时间。
	timeout := e.timeout
	// 若参数中显式设置超时则覆盖。
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	// 创建带超时的子上下文。
	ctx, cancel := context.WithTimeout(ctx, timeout)
	// 函数返回时取消上下文。
	defer cancel()
	// 构造 POST 请求，将 bodyBytes 作为请求体。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return &Result{Tool: "HTTPPost", Path: url, Error: err.Error()}
	}
	// 设置请求头。
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 发送请求。
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &Result{Tool: "HTTPPost", Path: url, Error: err.Error()}
	}
	// 函数返回时关闭响应体。
	defer resp.Body.Close()
	// 读取响应体，限制最大 1MB。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// 响应体是不可信外部源（TODO #18-4 T31）：包 untrusted 围栏（转义防逃逸），
	// 状态码行是我们自己的可信输出，留在围栏外。
	output := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, WrapUntrusted(url, string(body)))
	// 若输出超过 10000 字符则截断。
	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}
	// 返回结果，2xx 视为成功。
	return &Result{
		Tool:    "HTTPPost",
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Output:  output,
		Path:    url,
	}
}

// ---- Git 工具 ----

// gitMaxOutput 限制 Git 工具返回文本的最大长度。
const gitMaxOutput = 10000

// runGit 执行 git 子命令并封装为 *Result 返回。
func (e *Executor) runGit(ctx context.Context, args []string, path string) *Result {
	// 构造 git 命令，参数已包含子命令与选项。
	cmd := exec.Command("git", args...)
	// 设置命令的工作目录为当前会话的工作目录。
	cmd.Dir = e.workDirOf(ctx)
	// 执行命令并捕获合并后的标准输出与错误。
	out, err := cmd.CombinedOutput()
	// 根据子命令名称构造工具名，如 GitDiff / GitStatus。
	toolName := "Git" + capitalizeFirst(args[0])
	// 初始化结果对象，记录工具名与路径。
	result := &Result{Tool: toolName, Path: path}
	// 命令失败且没有任何输出时，将错误信息放入 Error 字段。
	if err != nil && len(out) == 0 {
		result.Error = err.Error()
		return result
	}
	// 只要存在输出（即使 err 非空也视为 git 返回了错误信息），将结果标记为成功。
	result.Success = true
	// 对输出进行长度截断。
	result.Output = truncateGitOutput(string(out))
	return result
}

// capitalizeFirst 将字符串首字母转为大写，用于构造 Git 工具名。
func capitalizeFirst(s string) string {
	// 空字符串无需处理。
	if s == "" {
		return ""
	}
	// 转为 rune 切片以正确处理 Unicode。
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	// 返回首字母大写后的字符串。
	return string(r)
}

// truncateGitOutput 对过长的 Git 输出进行截断并追加提示。
func truncateGitOutput(s string) string {
	// 如果输出超过最大长度限制，截取前段并追加截断提示。
	if len(s) > gitMaxOutput {
		return s[:gitMaxOutput] + "\n... (truncated)"
	}
	// 未超过限制时原样返回。
	return s
}

// gitDiff 执行 git diff，支持指定目标引用与路径范围。
func (e *Executor) gitDiff(ctx context.Context, args map[string]any) *Result {
	// 读取可选参数 target 与 path。
	target, _ := args["target"].(string)
	path, _ := args["path"].(string)
	// 构造 git diff 参数列表。
	gitArgs := []string{"diff"}
	// 若指定目标引用，则加入参数。
	if target != "" {
		gitArgs = append(gitArgs, target)
	}
	// 若指定路径，则解析为绝对路径后加入参数。
	if path != "" {
		absPath, err := e.resolvePathWithSandbox(ctx, path)
		if err != nil {
			return &Result{Tool: "GitDiff", Path: absPath, Error: err.Error()}
		}
		gitArgs = append(gitArgs, absPath)
	}
	// 调用统一的 git 执行入口。
	return e.runGit(ctx, gitArgs, path)
}

// gitStatus 执行 git status -sb 返回仓库精简状态。
func (e *Executor) gitStatus(ctx context.Context, args map[string]any) *Result {
	// 直接调用 git status 短格式命令。
	return e.runGit(ctx, []string{"status", "-sb"}, "")
}

// gitLog 执行 git log，支持限制条数与路径范围。
func (e *Executor) gitLog(ctx context.Context, args map[string]any) *Result {
	// 默认返回最近 20 条提交。
	limit := 20
	// 若参数中显式设置 limit 且为正数，则覆盖默认值。
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	// 读取可选路径参数。
	path, _ := args["path"].(string)
	// 构造 git log 参数：单行格式并限制数量。
	gitArgs := []string{"log", "--oneline", "-n", fmt.Sprintf("%d", limit)}
	// 若指定路径，则解析为绝对路径后加入。
	if path != "" {
		absPath, err := e.resolvePathWithSandbox(ctx, path)
		if err != nil {
			return &Result{Tool: "GitLog", Path: absPath, Error: err.Error()}
		}
		gitArgs = append(gitArgs, "--", absPath)
	}
	// 调用统一的 git 执行入口。
	return e.runGit(ctx, gitArgs, path)
}

// gitBlame 执行 git blame --line-porcelain 并返回指定文件的逐行作者信息。
func (e *Executor) gitBlame(ctx context.Context, args map[string]any) *Result {
	// 读取文件路径，空字符串直接返回错误。
	path, _ := args["path"].(string)
	if path == "" {
		return &Result{Tool: "GitBlame", Error: "path is required"}
	}
	// 解析路径并校验沙箱约束。
	absPath, err := e.resolvePathWithSandbox(ctx, path)
	if err != nil {
		return &Result{Tool: "GitBlame", Path: absPath, Error: err.Error()}
	}
	// 构造 git blame 参数，使用 line-porcelain 格式。
	gitArgs := []string{"blame", "--line-porcelain", absPath}
	// 调用统一的 git 执行入口。
	return e.runGit(ctx, gitArgs, absPath)
}
