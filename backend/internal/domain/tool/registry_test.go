package tool

// 导入测试所需标准库与项目包。
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

)

// TestRegistrySchema 验证内置工具注册表的 Schema 至少包含 11 个工具定义。
func TestRegistrySchema(t *testing.T) {
	// 创建内置工具注册表，使用临时目录作为工作目录。
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	// 获取所有工具定义。
	schema := r.Schema()
	// 校验工具数量是否满足预期下限。
	if len(schema) < 11 {
		t.Fatalf("expected at least 11 tools, got %d", len(schema))
	}
}

// TestIsVerificationCommand 验证验证类命令判定覆盖领域提示词规定的三种语法检查写法
// （node -c / go build / py_compile）与常见测试运行器；非验证命令不误判。
// 08-13 塔防实证：node -c 是塔防验收唯一证据来源却全不命中，致 verify_missing 误报。
func TestIsVerificationCommand(t *testing.T) {
	for _, cmd := range []string{
		"node -c js/monster.js",
		"cd D:\\x; node -c js/monster.js; node -c js/config.js",
		"node --check js/a.js",
		"go build ./pkg",
		"go vet ./...",
		"go test ./...",
		"go test",
		"python -m py_compile x.py",
		"pytest tests/",
		"npm test",
		"npm run test",
		"cargo test",
		"eslint src/",
		"npm run lint",
	} {
		if !IsVerificationCommand(cmd) {
			t.Fatalf("IsVerificationCommand(%q) 应为 true", cmd)
		}
	}
	for _, cmd := range []string{
		"echo hello",
		"node server.js",
		"python app.py",
		"cat README.md",
		"git status",
	} {
		if IsVerificationCommand(cmd) {
			t.Fatalf("IsVerificationCommand(%q) 应为 false", cmd)
		}
	}
}

// TestVerificationEvidenceTemplate 验证 TODO #63 同源模板：模板文本含证据格式要求
// （``` 代码块 + EXIT_CODE=0），且识别词表 verificationCommandPatterns 的每一项都
// 原样出现在模板中（识别器认什么，prompt 就要求什么，防漂移）。
func TestVerificationEvidenceTemplate(t *testing.T) {
	tpl := VerificationEvidenceTemplate()
	for _, want := range []string{"【验证证据格式】", "```", "EXIT_CODE=0"} {
		if !strings.Contains(tpl, want) {
			t.Fatalf("template missing %q: %q", want, tpl)
		}
	}
	for _, p := range verificationCommandPatterns {
		if !strings.Contains(tpl, p) {
			t.Fatalf("template missing pattern %q: %q", p, tpl)
		}
	}
	// 扩词覆盖实证缺口：npx tsc --noEmit / vite build / npm run build 必须命中识别口径。
	for _, cmd := range []string{"npx tsc --noEmit", "vite build", "npm run build"} {
		if !IsVerificationCommand(cmd) {
			t.Fatalf("IsVerificationCommand(%q) 应为 true（TODO #63 扩词）", cmd)
		}
	}
	// 防误匹配：vite 词表项带尾随空格，"invited"（vite 后接字母）不应命中。
	if IsVerificationCommand("invited reviewers") {
		t.Fatal("IsVerificationCommand(\"invited reviewers\") 应为 false")
	}
}

// TestReadFile 验证 ReadFile 工具可以正确读取工作目录下的文件内容。
func TestReadFile(t *testing.T) {
	// 创建临时目录并在其中写入测试文件 hello.txt。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world"), 0644); err != nil {
		// 文件写入失败则终止测试。
		t.Fatalf("write file: %v", err)
	}
	// 使用临时目录创建内置工具注册表。
	r := NewBuiltinRegistry(dir, nil, nil)
	// 构造携带 SessionID 的上下文。
	ctx := WithSessionID(context.Background(), "s1")
	// 调度 ReadFile 工具读取 hello.txt。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "hello.txt"})
	// 校验调度未返回错误。
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	// 校验工具调用成功。
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	// 校验输出内容包含 "world"。
	if !strings.Contains(res.Output, "world") {
		t.Fatalf("expected output to contain 'world', got: %s", res.Output)
	}
}

// TestReadFile_ConsecutiveSameCallLoopGuard 验证"参数完全相同的连续 ReadFile"循环检测：
// 重读不再拦截（每次直返磁盘最新内容），第 2 次连续相同调用附翻页提醒，
// 第 3 次判定死循环拦截并请求 LoopExit；ResetReadHistory 后计数清零。
// 对应日志事故：代码助手对 config.js 反复 ReadFile 10+ 次烧 token。
func TestReadFile_ConsecutiveSameCallLoopGuard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// 首次读取应成功。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res.Success {
		t.Fatalf("first read should succeed: err=%v success=%v", err, res.Success)
	}

	// 第 2 次相同参数调用：仍直返内容（不拦截），但结果末尾附翻页提醒。
	res2, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res2.Success {
		t.Fatalf("second identical read should still serve content: err=%v success=%v", err, res2.Success)
	}
	if !strings.Contains(res2.Output, "v1") {
		t.Fatalf("second read should return content, got: %s", res2.Output)
	}
	if !strings.Contains(res2.Output, "勿重复读取") {
		t.Fatalf("second identical read should carry reminder, got: %s", res2.Output)
	}

	// 参数有变化（翻页）即归零重计，不触发循环检测。
	paged := map[string]any{"path": "a.txt", "offset": float64(1), "limit": float64(1)}
	if res, err := r.Dispatch(ctx, "ReadFile", paged); err != nil || !res.Success {
		t.Fatalf("read with different params should succeed: err=%v success=%v", err, res.Success)
	}
	// 同参数第 2 次：仍成功（附提醒）。
	if res, err := r.Dispatch(ctx, "ReadFile", paged); err != nil || !res.Success {
		t.Fatalf("2nd consecutive identical read should succeed: err=%v success=%v", err, res.Success)
	}
	// 同参数第 3 次：判定死循环，拦截。
	res3, _ := r.Dispatch(ctx, "ReadFile", paged)
	if res3.Success {
		t.Fatal("3rd consecutive identical read should be blocked as loop")
	}
	if !strings.Contains(res3.Error, "死循环") {
		t.Fatalf("expected loop error, got: %s", res3.Error)
	}

	// 模拟用户新消息：ResetReadHistory 清空连读计数后可重读。
	r.ResetReadHistory("s1")
	res4, err := r.Dispatch(ctx, "ReadFile", paged)
	if err != nil || !res4.Success {
		t.Fatalf("read after reset should succeed: err=%v success=%v", err, res4.Success)
	}
}

// TestReadFile_WriteResetsConsecutiveCount 确认性复读放行（TODO #72）：
// WriteFile/EditFile 成功写某路径后清零该路径连读计数--
// Read(A)->Write(A)->Read(A)->Read(A) 的编辑后确认工作流不再第 3 次被杀。
func TestReadFile_WriteResetsConsecutiveCount(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("v1"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s2")
	args := map[string]any{"path": "a.txt", "offset": float64(1), "limit": float64(5)}

	// Read x2（连读计数 2），随后写入成功清零。
	for i := 0; i < 2; i++ {
		if res, err := r.Dispatch(ctx, "ReadFile", args); err != nil || !res.Success {
			t.Fatalf("read %d: err=%v success=%v", i+1, err, res.Success)
		}
	}
	if wres, err := r.Dispatch(ctx, "WriteFile", map[string]any{"path": "a.txt", "content": "v2"}); err != nil || !wres.Success {
		t.Fatalf("write: err=%v success=%v", err, wres.Success)
	}
	// 写后确认性复读 x2（若无写入清零，这里第 2 次已是第 4 连读、被杀）。
	for i := 0; i < 2; i++ {
		if res, err := r.Dispatch(ctx, "ReadFile", args); err != nil || !res.Success {
			t.Fatalf("confirm read %d after write should pass: err=%v success=%v", i+1, err, res.Success)
		}
	}
	// 第 3 次写后复读（计数 3）仍被杀--守卫语义保留。
	res, _ := r.Dispatch(ctx, "ReadFile", args)
	if res.Success {
		t.Fatal("3rd consecutive read after write should still be blocked")
	}
}

// TestReadFile_LongFileRelaxed 长文件连读上限放宽（TODO #72）：
// ReadFile 返回总行数 > 500 的长文件时，同区间连读上限放宽到 6。
func TestReadFile_LongFileRelaxed(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; i < longFileReadLines+50; i++ {
		lines = append(lines, "line")
	}
	target := filepath.Join(dir, "long.txt")
	if err := os.WriteFile(target, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s3")
	args := map[string]any{"path": "long.txt", "offset": float64(1), "limit": float64(5)}

	// 首读标记长文件；随后同参数 2..5 连读均放行（旧上限 3 会杀第 3 次）。
	for i := 0; i < 5; i++ {
		res, err := r.Dispatch(ctx, "ReadFile", args)
		if err != nil || !res.Success {
			t.Fatalf("long-file read %d should pass (relaxed limit): err=%v success=%v", i+1, err, res.Success)
		}
	}
	// 第 6 次达放宽上限，被杀。
	res, _ := r.Dispatch(ctx, "ReadFile", args)
	if res.Success {
		t.Fatal("6th consecutive long-file read should be blocked")
	}
}

// TestReadFile_AfterWriteFile_ReturnsFreshContent 脏数据回归测试：
// ReadFile 每次直返磁盘最新内容，WriteFile 改写后按相同参数重读必须看到新内容，
// 不依赖任何"已读记录清理"机制（WriteFile/sed/外部进程改写均被天然覆盖）。
func TestReadFile_AfterWriteFile_ReturnsFreshContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// 首次读取成功，看到 v1。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res.Success {
		t.Fatalf("first read should succeed: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, "v1") {
		t.Fatalf("expected v1 content, got: %s", res.Output)
	}

	// WriteFile 改写 a.txt 内容为 v2。
	wres, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    "a.txt",
		"content": "v2",
	})
	if err != nil || !wres.Success {
		t.Fatalf("WriteFile should succeed: err=%v success=%v", err, wres.Success)
	}

	// 相同参数重读：直返磁盘最新内容 v2，绝无脏数据。
	res3, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res3.Success {
		t.Fatalf("read after WriteFile should succeed: err=%v success=%v", err, res3.Success)
	}
	if !strings.Contains(res3.Output, "v2") {
		t.Fatalf("expected v2 content after rewrite, got: %s", res3.Output)
	}
}

// TestReadFile_Pagination 验证 offset/limit 行区间分页：
// 分页头（输出首行）给出总行数/本页区间/下一页起点，翻页返回后续区间，
// 末页标注"已到文件末尾"，offset 越界报错并告知总行数。
func TestReadFile_Pagination(t *testing.T) {
	dir := t.TempDir()
	// 构造 10 行文件。
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line-%d", i))
	}
	if err := os.WriteFile(filepath.Join(dir, "p.txt"), []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// 第一页：offset=1, limit=4，返回 1-4 行，分页头指向 offset=5。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "p.txt", "offset": float64(1), "limit": float64(4)})
	if err != nil || !res.Success {
		t.Fatalf("first page should succeed: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, "共 10 行") || !strings.Contains(res.Output, "本页 1-4 行") || !strings.Contains(res.Output, "offset=5") {
		t.Fatalf("first page header wrong: %s", res.Output)
	}
	if !strings.Contains(res.Output, "line-1") || !strings.Contains(res.Output, "line-4") || strings.Contains(res.Output, "line-5") {
		t.Fatalf("first page content wrong: %s", res.Output)
	}

	// 第二页：offset=5, limit=4，返回 5-8 行（不含上一页内容）。
	res2, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "p.txt", "offset": float64(5), "limit": float64(4)})
	if err != nil || !res2.Success {
		t.Fatalf("second page should succeed: err=%v success=%v", err, res2.Success)
	}
	if strings.Contains(res2.Output, "line-4") || !strings.Contains(res2.Output, "line-5") || !strings.Contains(res2.Output, "line-8") {
		t.Fatalf("second page content wrong: %s", res2.Output)
	}

	// 末页：limit 超出末尾时截到文件末尾并标注。
	res3, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "p.txt", "offset": float64(9), "limit": float64(4)})
	if err != nil || !res3.Success {
		t.Fatalf("last page should succeed: err=%v success=%v", err, res3.Success)
	}
	if !strings.Contains(res3.Output, "本页 9-10 行") || !strings.Contains(res3.Output, "已到文件末尾") {
		t.Fatalf("last page header wrong: %s", res3.Output)
	}

	// offset 越界：报错并告知总行数。
	res4, _ := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "p.txt", "offset": float64(11), "limit": float64(4)})
	if res4.Success {
		t.Fatal("out-of-range offset should fail")
	}
	if !strings.Contains(res4.Error, "总行数 10") {
		t.Fatalf("expected total-lines error, got: %s", res4.Error)
	}
}

// TestReadFile_LimitHardClamp 验证单次 ReadFile 行数硬上限：
// limit > maxReadFileLimit 时钳到上限返回，分页头标注截断原因与下一页起点。
// 2026-09-07 P1-2 上限 300→1000（分页截断是干扰源，字符上限仍是体积闸门）。
func TestReadFile_LimitHardClamp(t *testing.T) {
	dir := t.TempDir()
	// 构造 1100 行文件。
	var lines []string
	for i := 1; i <= 1100; i++ {
		lines = append(lines, fmt.Sprintf("line-%d", i))
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// limit=1100 超上限：应只返回 1-1000 行，分页头标注截断并指引 offset=1001。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "big.txt", "offset": float64(1), "limit": float64(1100)})
	if err != nil || !res.Success {
		t.Fatalf("clamped read should succeed: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, "本页 1-1000 行") {
		t.Fatalf("expected clamped page range 1-1000, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "已截断") {
		t.Fatalf("expected clamp note in header, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "offset=1001") {
		t.Fatalf("expected next-page hint offset=1001, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "line-1000") || strings.Contains(res.Output, "line-1001") {
		t.Fatalf("expected content up to line-1000 only, got: %s", res.Output)
	}
}

// TestWriteFileProtectedPath 验证 WriteFile 工具不能写入受保护的 .git/ 目录。
// 源码目录名（backend/ 等）已不再受保护：workDir 是用户项目，Agent 需直接编辑用户代码；
// 仅 VCS/IDE/构建产物根目录被挡。此处用 .git/ 代表受保护目录。
func TestWriteFileProtectedPath(t *testing.T) {
	// 创建临时目录作为工作目录。
	dir := t.TempDir()
	// 创建内置工具注册表。
	r := NewBuiltinRegistry(dir, nil, nil)
	// 构造携带 SessionID 的上下文。
	ctx := WithSessionID(context.Background(), "s1")
	// 尝试写入 .git/foo，该路径命中受保护目录规则（VCS 元数据）。
	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    ".git/foo",
		"content": "package foo",
	})
	// 校验调度未返回底层错误。
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	// 校验写入被拦截，Success 为 false。
	if res.Success {
		t.Fatal("expected write to .git/ to fail")
	}
	// 校验错误信息包含中文或英文的受保护路径提示。
	if !strings.Contains(res.Error, "受保护") && !strings.Contains(res.Error, "protected") {
		t.Fatalf("expected protected path error, got: %s", res.Error)
	}
}

// TestRunCommandEcho 验证 RunCommand 工具可以正确执行 echo 命令。
func TestRunCommandEcho(t *testing.T) {
	// 创建临时目录作为工作目录。
	dir := t.TempDir()
	// 创建内置工具注册表。
	r := NewBuiltinRegistry(dir, nil, nil)
	// 根据操作系统选择命令字符串，echo 在 Windows 与 Unix 下语法一致。
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "echo hello"
	} else {
		cmd = "echo hello"
	}
	// 调度 RunCommand 工具执行 echo 命令。
	res, err := r.Dispatch(context.Background(), "RunCommand", map[string]any{"command": cmd})
	// 校验调度未返回错误。
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	// 校验命令执行成功。
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	// 校验输出包含 "hello"。
	if !strings.Contains(res.Output, "hello") {
		t.Fatalf("expected output to contain 'hello', got: %s", res.Output)
	}
}

// TestUnknownTool 验证调度未知工具时会返回包含 "unknown tool" 的错误。
func TestUnknownTool(t *testing.T) {
	// 创建内置工具注册表。
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	// 尝试调度一个不存在的工具。
	_, err := r.Dispatch(context.Background(), "NotATool", map[string]any{})
	// 校验返回了错误。
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	// 校验错误信息包含 "unknown tool"。
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown tool error, got: %v", err)
	}
}

// stubCallSubAgent 是 call_sub_agent 的测试桩，模拟 subagent.Dispatcher
// 安装的工具（带 Description 可选接口）。
type stubCallSubAgent struct{}

func (s *stubCallSubAgent) Name() string      { return "call_sub_agent" }
func (s *stubCallSubAgent) Aliases() []string { return nil }
func (s *stubCallSubAgent) Description() string {
	return "stub: 派发自包含子任务给子 Agent。"
}
func (s *stubCallSubAgent) Execute(ctx context.Context, args map[string]any) *Result {
	return &Result{Tool: "call_sub_agent", Success: true, Output: "sub-1"}
}

// TestSchemaIncludesCallSubAgent 验证：call_sub_agent 注册后出现在 LLM 工具
// schema 中，且描述文本来自工具的 Description()；未注册时 schema 有 26 个工具
// （13 个文件/命令/网络/Git 工具（含 RestoreFile）+ RefreshProjectDoc/WriteSharedMemory/WriteSpec
// + ask_user（TODO #53 起实现 SchemaSource）+ remember_preference（2026-09-02
// scope 扩展起实现 SchemaSource）+ 5 个 plugin_* 插件管理工具
// + 3 个 tool_catalog/mount/unmount 挂载工具，TODO #51/52）。
func TestSchemaIncludesCallSubAgent(t *testing.T) {
	// 未安装 call_sub_agent 时，schema 恰为 30 个工具（含 RefreshProjectDoc/WriteSharedMemory/ReadSharedMemory/WriteSpec/ReadMedia/ShowArtifact/ask_user/escalate_gear/remember_preference/plugin_*/tool_*）。
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	if n := len(r.Schema()); n != 30 {
		t.Fatalf("expected 30 builtin tools without call_sub_agent, got %d", n)
	}
	// 安装后应出现在 schema 中，且描述来自 Description()。
	r.Register(&stubCallSubAgent{})
	schema := r.Schema()
	if len(schema) != 31 {
		t.Fatalf("expected 31 tools with call_sub_agent, got %d", len(schema))
	}
	// 遍历查找 call_sub_agent 并校验描述文本。
	found := false
	for _, tl := range schema {
		if tl.Name() == "call_sub_agent" {
			found = true
			if !strings.Contains(tl.Description(), "stub:") {
				t.Fatalf("expected description from Description(), got %q", tl.Description())
			}
		}
	}
	if !found {
		t.Fatal("call_sub_agent not found in schema after Register")
	}
}

// TestSchemaCallSubAgent_ModeField 验证 call_sub_agent 的 schema 含 mode 参数（TODO #29 三引擎）：
// 派发执行模式枚举暴露给 LLM，缺失则 LLM 无法感知该字段。
func TestSchemaCallSubAgent_ModeField(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.Register(&stubCallSubAgent{})
	for _, tl := range r.Schema() {
		if tl.Name() != "call_sub_agent" {
			continue
		}
		props := tl.InputSchema().Properties
		for _, want := range []string{"role_id", "task", "domain", "responsibility", "mode"} {
			if _, ok := props[want]; !ok {
				t.Fatalf("call_sub_agent schema missing parameter %q (have %v)", want, keysOf(props))
			}
		}
		return
	}
	t.Fatal("call_sub_agent not found in schema")
}

// keysOf 提取 map 键切片，用于断言信息展示。
func keysOf(m map[string]*jsonschema.Schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// fakeSharedMemoryStore 是测试用 SharedMemoryStore 实现，内存版，无持久化。
type fakeSharedMemoryStore struct {
	items   map[string]string
	workDir string
}

func newFakeSharedMemoryStore() *fakeSharedMemoryStore {
	return &fakeSharedMemoryStore{items: make(map[string]string)}
}

// WorkDir 模拟 FileSharedMemoryStore.WorkDir（可选接口）：空串表示非文件后端。
func (s *fakeSharedMemoryStore) WorkDir() string { return s.workDir }

func (s *fakeSharedMemoryStore) Set(ctx context.Context, key, value string) error {
	s.items[key] = value
	return nil
}

func (s *fakeSharedMemoryStore) Get(ctx context.Context, key string) (string, error) {
	return s.items[key], nil
}

func (s *fakeSharedMemoryStore) Delete(ctx context.Context, key string) error {
	delete(s.items, key)
	return nil
}

func (s *fakeSharedMemoryStore) Keys(ctx context.Context) []string {
	keys := make([]string, 0, len(s.items))
	for k := range s.items {
		keys = append(keys, k)
	}
	return keys
}

// TestWriteSharedMemory_StructuredAndFileTracking 验证 Layer 1：WriteSharedMemory
// 入参 files 被结构化存为 MD frontmatter（files mtime map），body 为 content 原文。
func TestWriteSharedMemory_StructuredAndFileTracking(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(target, []byte("package foo"), 0644); err != nil {
		t.Fatalf("write foo.go: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 带 files 调用：MD frontmatter 含 foo.go 的 mtime，body 为 content。
	res, err := r.Dispatch(ctx, "WriteSharedMemory", map[string]any{
		"content": "foo.go defines package foo",
		"files":   []any{target},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}

	val, _ := store.Get(ctx, "meta-1:shared")
	if val == "" {
		t.Fatal("expected KV entry after WriteSharedMemory")
	}
	fm, body, ok := DecodeSharedMD(val)
	if !ok {
		t.Fatalf("KV value not MD: %q", val)
	}
	if body != "foo.go defines package foo" {
		t.Fatalf("unexpected body: %q", body)
	}
	if len(fm.Files) != 1 {
		t.Fatalf("expected 1 tracked file, got %d", len(fm.Files))
	}
	if _, ok := fm.Files[target]; !ok {
		t.Fatalf("expected %s in Files map, got %v", target, fm.Files)
	}

	// files 缺省调用：仍写 KV，Files 为空 map。
	res2, err := r.Dispatch(ctx, "WriteSharedMemory", map[string]any{
		"content": "no files summary",
	})
	if err != nil {
		t.Fatalf("dispatch2: %v", err)
	}
	if !res2.Success {
		t.Fatalf("expected success2, got: %s", res2.Error)
	}
	val2, _ := store.Get(ctx, "meta-1:shared")
	fm2, body2, ok := DecodeSharedMD(val2)
	if !ok {
		t.Fatalf("KV value2 not MD: %q", val2)
	}
	if body2 != "no files summary" {
		t.Fatalf("unexpected body2: %q", body2)
	}
	if len(fm2.Files) != 0 {
		t.Fatalf("expected 0 tracked files, got %d", len(fm2.Files))
	}
}

// TestWriteFile_InvalidatesSharedMemory 验证 Layer 2：WriteFile 成功后，
// 引用同 path 的 KV entry 被标记 stale（invalidated_at）但不物理删除、body 保留——
// 注入侧照常注入并附行号漂移警告，防止"删记忆"导致下次派发从零重读。
func TestWriteFile_InvalidatesSharedMemory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bar.go")
	if err := os.WriteFile(target, []byte("package bar"), 0644); err != nil {
		t.Fatalf("write bar.go: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 写共享记忆，引用 bar.go。
	_, err := r.Dispatch(ctx, "WriteSharedMemory", map[string]any{
		"content": "bar.go defines package bar",
		"files":   []any{target},
	})
	if err != nil {
		t.Fatalf("WriteSharedMemory: %v", err)
	}
	if _, ok := store.items["meta-1:shared"]; !ok {
		t.Fatal("expected KV entry before WriteFile")
	}

	// WriteFile 修改 bar.go，触发失效 hook。
	wres, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    target,
		"content": "package bar // modified",
	})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !wres.Success {
		t.Fatalf("WriteFile failed: %s", wres.Error)
	}

	// KV entry 应保留并打上 stale 标记（invalidated_at），body 原样留存。
	val, ok := store.items["meta-1:shared"]
	if !ok {
		t.Fatal("expected KV entry retained (marked stale, not deleted) after WriteFile invalidated it")
	}
	fm, body, decOK := DecodeSharedMD(val)
	if !decOK {
		t.Fatalf("invalidated entry should remain decodable MD, got: %q", val)
	}
	if fm.InvalidatedAt == "" {
		t.Fatalf("expected invalidated_at marked in frontmatter, got: %q", val)
	}
	if body != "bar.go defines package bar" {
		t.Fatalf("expected body preserved after invalidation, got: %q", body)
	}
}

// TestWriteFile_DoesNotInvalidateUnrelatedEntry 验证 Layer 2 精确性：
// WriteFile 只删引用同 path 的 entry，不误删引用其他 path 的 entry。
func TestWriteFile_DoesNotInvalidateUnrelatedEntry(t *testing.T) {
	dir := t.TempDir()
	foo := filepath.Join(dir, "foo.go")
	bar := filepath.Join(dir, "bar.go")
	if err := os.WriteFile(foo, []byte("foo"), 0644); err != nil {
		t.Fatalf("write foo: %v", err)
	}
	if err := os.WriteFile(bar, []byte("bar"), 0644); err != nil {
		t.Fatalf("write bar: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 写共享记忆只引用 foo.go。
	_, _ = r.Dispatch(ctx, "WriteSharedMemory", map[string]any{
		"content": "foo summary",
		"files":   []any{foo},
	})

	// WriteFile 修改 bar.go：foo 引用的 entry 不应被删。
	wres, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    bar,
		"content": "bar modified",
	})
	if err != nil || !wres.Success {
		t.Fatalf("WriteFile bar: err=%v res=%+v", err, wres)
	}
	if _, ok := store.items["meta-1:shared"]; !ok {
		t.Fatal("KV entry should NOT be deleted: WriteFile touched bar.go, entry references foo.go only")
	}
}

// TestWriteSpec_Structured 验证 WriteSpec 入参结构化存为 MD：frontmatter 含
// goal/acceptance/constraints/files mtime，body 为人读结构化 MD。
// 存储键为 "<agentID>:spec"。
func TestWriteSpec_Structured(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "game.js")
	if err := os.WriteFile(target, []byte("var x = 1"), 0644); err != nil {
		t.Fatalf("write game.js: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":        "把 canvas 宽度从 960 改为 1280",
		"acceptance":  []any{"node -c game.js 通过", "CONFIG.canvas.width=1280"},
		"constraints": []any{"不改动其他配置项"},
		"files":       []any{target},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}

	val, _ := store.Get(ctx, "meta-1:spec")
	if val == "" {
		t.Fatal("expected KV entry at meta-1:spec")
	}
	fm, _, ok := DecodeSharedMD(val)
	if !ok {
		t.Fatalf("KV value not MD: %q", val)
	}
	if fm.Goal != "把 canvas 宽度从 960 改为 1280" {
		t.Fatalf("unexpected goal: %q", fm.Goal)
	}
	if len(fm.Acceptance) != 2 {
		t.Fatalf("expected 2 acceptance, got %d", len(fm.Acceptance))
	}
	if len(fm.Constraints) != 1 {
		t.Fatalf("expected 1 constraint, got %d", len(fm.Constraints))
	}
	if len(fm.Files) != 1 {
		t.Fatalf("expected 1 tracked file, got %d", len(fm.Files))
	}
	if _, ok := fm.Files[target]; !ok {
		t.Fatalf("expected %s in Files map", target)
	}
}

// TestWriteSpec_ContractField 验证 contract 字段（TODO #57）结构化写入：
// map 入参经 JSON 往返解析，frontmatter 含四类条目，body 渲染跨域契约段。
func TestWriteSpec_ContractField(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)
	ctx := WithAgentID(context.Background(), "meta-1")

	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "多域前端任务",
		"acceptance": []any{"跨域集成点一致"},
		"contract": map[string]any{
			"symbols": []any{map[string]any{
				"symbol": "GameEngine.init", "file": "engine.js", "refs": []any{"main.js"},
			}},
			"dom_ids": []any{map[string]any{"id": "game-canvas", "file": "index.html"}},
			"scripts": []any{map[string]any{"file": "engine.js"}, map[string]any{"file": "main.js"}},
			"signatures": []any{map[string]any{
				"symbol": "start", "signature": "function start()", "file": "engine.js",
			}},
		},
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch: err=%v res=%+v", err, res)
	}

	val, _ := store.Get(ctx, "meta-1:spec")
	fm, body, ok := DecodeSharedMD(val)
	if !ok {
		t.Fatalf("KV value not MD: %q", val)
	}
	if fm.Contract == nil || len(fm.Contract.Symbols) != 1 || len(fm.Contract.DOMIDs) != 1 ||
		len(fm.Contract.Scripts) != 2 || len(fm.Contract.Signatures) != 1 {
		t.Fatalf("contract not roundtripped: %+v", fm.Contract)
	}
	if fm.Contract.Symbols[0].Symbol != "GameEngine.init" || fm.Contract.Symbols[0].Refs[0] != "main.js" {
		t.Fatalf("symbol entry mismatch: %+v", fm.Contract.Symbols[0])
	}
	if !strings.Contains(body, "## 跨域契约") || !strings.Contains(body, "GameEngine.init") {
		t.Fatalf("body missing contract section: %q", body)
	}

	// 契约可空：不传 contract 时 frontmatter 无契约、body 无契约段。
	res2, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "g2", "acceptance": []any{"a"},
	})
	if err != nil || !res2.Success {
		t.Fatalf("dispatch without contract: err=%v res=%+v", err, res2)
	}
	val2, _ := store.Get(ctx, "meta-1:spec")
	fm2, body2, _ := DecodeSharedMD(val2)
	if fm2.Contract != nil || strings.Contains(body2, "跨域契约") {
		t.Fatalf("contract should be absent when not provided: %+v body=%q", fm2.Contract, body2)
	}
}

// TestWriteSpec_FileListPreservesMissingFiles 待创建文件（stat 失败）不进 Files mtime 索引
// 但保留在 FileList：dispatcher 冒烟检查（TODO #56）在文件创建后仍能定位目标。
func TestWriteSpec_FileListPreservesMissingFiles(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-created-yet.js")
	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)
	ctx := WithAgentID(context.Background(), "meta-1")

	if _, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "g", "acceptance": []any{"a"}, "files": []any{missing},
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	val, _ := store.Get(ctx, "meta-1:spec")
	fm, _, ok := DecodeSharedMD(val)
	if !ok {
		t.Fatalf("KV value not MD: %q", val)
	}
	if len(fm.Files) != 0 {
		t.Fatalf("missing file must not enter mtime index: %v", fm.Files)
	}
	if len(fm.FileList) != 1 || fm.FileList[0] != missing {
		t.Fatalf("file_list should preserve missing file, got %v", fm.FileList)
	}
}

// TestWriteSpec_GrowingFilesWarning files 含持续增长目录（logs/）文件时输出告警提示。
func TestWriteSpec_GrowingFilesWarning(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs", "tui")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	logFile := filepath.Join(logDir, "2026-08-10.log")
	if err := os.WriteFile(logFile, []byte("line\n"), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	srcFile := filepath.Join(dir, "main.js")
	if err := os.WriteFile(srcFile, []byte("x"), 0644); err != nil {
		t.Fatalf("write main.js: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	r.SetSharedMemory(newFakeSharedMemoryStore())
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "分析日志找失败原因",
		"acceptance": []any{"列出失败点"},
		"files":      []any{logFile, srcFile},
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Output, "豁免") || !strings.Contains(res.Output, logFile) {
		t.Fatalf("expected growing-file warning with path, got: %q", res.Output)
	}
}

// TestWriteSpec_RequiresGoalAndAcceptance 验证 goal/acceptance 必填校验。
func TestWriteSpec_RequiresGoalAndAcceptance(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetSharedMemory(newFakeSharedMemoryStore())
	ctx := WithAgentID(context.Background(), "meta-1")

	// 缺 goal：应失败。
	res, _ := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"acceptance": []any{"a"},
	})
	if res.Success {
		t.Fatal("expected fail when goal missing")
	}

	// 缺 acceptance：应失败。
	res2, _ := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "g",
	})
	if res2.Success {
		t.Fatal("expected fail when acceptance missing")
	}
}

// TestWriteSpec_InvalidatesOnWriteFile 验证 Layer 2：WriteFile 成功后引用同 path 的
// spec entry 写墓碑（TODO #74：物理删除改为 "invalidated: ..." 留痕，
// 派发侧区分"从未写"与"已失效"）。
func TestWriteSpec_InvalidatesOnWriteFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "foo.js")
	if err := os.WriteFile(target, []byte("var x = 1"), 0644); err != nil {
		t.Fatalf("write foo.js: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 写 spec，引用 foo.js。
	_, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "g",
		"acceptance": []any{"a"},
		"files":      []any{target},
	})
	if err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	if _, ok := store.items["meta-1:spec"]; !ok {
		t.Fatal("expected spec entry before WriteFile")
	}

	// WriteFile 修改 foo.js，触发失效（墓碑而非物理删除）。
	wres, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    target,
		"content": "var x = 2",
	})
	if err != nil || !wres.Success {
		t.Fatalf("WriteFile: err=%v res=%+v", err, wres)
	}
	val, ok := store.items["meta-1:spec"]
	if !ok {
		t.Fatal("expected spec tombstone retained after WriteFile invalidated it")
	}
	if !strings.HasPrefix(val, SpecTombstonePrefix) {
		t.Fatalf("expected tombstone value with prefix %q, got %q", SpecTombstonePrefix, val)
	}
}

// TestWriteFile_TemporaryOutputHasAbsPath 临时文件写入结果带落盘绝对路径（TODO #38-4 根因 D）：
// 临时文件落在会话级临时目录（.bma/tmp/<sid>/，路径不可预测），Output 必须携带绝对路径与
// $env:BMA_SESSION_TEMP_DIR 运行提示——Agent 首次运行不再盲猜工作目录路径
//（事故实证：ca-5 用 <工作目录>\verify-frost.js 找不到模块，浪费一轮修复时间）。
func TestWriteFile_TemporaryOutputHasAbsPath(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s-temp-path")

	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":      "verify-frost.js",
		"content":   "const x = 1;",
		"temporary": true,
	})
	if err != nil || !res.Success {
		t.Fatalf("temporary write should succeed: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, "bytes to ") || !strings.Contains(res.Output, "verify-frost.js") {
		t.Fatalf("output should carry abs path + size, got: %q", res.Output)
	}
	if !strings.Contains(res.Output, "$env:BMA_SESSION_TEMP_DIR") {
		t.Fatalf("output should tell how to reference the temp file, got: %q", res.Output)
	}
	if res.Path == "" || res.TempDir == "" {
		t.Fatalf("result should carry Path/TempDir for cleanup, got path=%q tempdir=%q", res.Path, res.TempDir)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("temp file should exist at %s: %v", res.Path, err)
	}
}

// TestWriteSpec_MultiKeyPerDomain 多 key 存储（TODO #65）：key=领域名时存到
// "<agentID>:spec:<key>"，与默认单键互不覆盖。
func TestWriteSpec_MultiKeyPerDomain(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)
	ctx := WithAgentID(context.Background(), "meta-1")

	if _, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "共享单份", "acceptance": []any{"a"},
	}); err != nil {
		t.Fatalf("legacy write: %v", err)
	}
	if _, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "渲染域目标", "acceptance": []any{"b"}, "key": "渲染领域",
	}); err != nil {
		t.Fatalf("domain write: %v", err)
	}
	if _, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "寻路域目标", "acceptance": []any{"c"}, "key": "寻路领域",
	}); err != nil {
		t.Fatalf("second domain write: %v", err)
	}
	if resBad, _ := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "x", "acceptance": []any{"d"}, "key": "bad:key",
	}); resBad.Success || !strings.Contains(resBad.Error, "key must not contain") {
		t.Fatalf("key with colon should be rejected, got %q", resBad.Error)
	}
	legacy, _ := store.Get(ctx, "meta-1:spec")
	fm1, _, _ := DecodeSharedMD(legacy)
	if fm1.Goal != "共享单份" {
		t.Fatalf("legacy spec overwritten: %q", fm1.Goal)
	}
	d1, _ := store.Get(ctx, "meta-1:spec:渲染领域")
	fm2, _, _ := DecodeSharedMD(d1)
	if fm2.Goal != "渲染域目标" {
		t.Fatalf("domain spec missing: %q", fm2.Goal)
	}
	d2, _ := store.Get(ctx, "meta-1:spec:寻路领域")
	fm3, _, _ := DecodeSharedMD(d2)
	if fm3.Goal != "寻路域目标" {
		t.Fatalf("second domain spec missing: %q", fm3.Goal)
	}
}

// TestWriteSpec_VerifyLevels 验收层级（TODO #59）：合法值存 frontmatter，非法值拒绝。
func TestWriteSpec_VerifyLevels(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetSharedMemory(newFakeSharedMemoryStore())
	ctx := WithAgentID(context.Background(), "meta-1")

	if _, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "g", "acceptance": []any{"a"},
		"verify_levels": []any{"visual", "integration", "visual"},
	}); err != nil {
		t.Fatalf("valid levels write: %v", err)
	}
	val, _ := r.sharedMemory.Get(ctx, "meta-1:spec")
	fm, _, ok := DecodeSharedMD(val)
	if !ok {
		t.Fatal("decode failed")
	}
	if len(fm.VerifyLevels) != 2 || fm.VerifyLevels[0] != "visual" || fm.VerifyLevels[1] != "integration" {
		t.Fatalf("verify_levels normalized wrong: %v", fm.VerifyLevels)
	}

	res, _ := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "g", "acceptance": []any{"a"},
		"verify_levels": []any{"invalid-layer"},
	})
	if res.Success {
		t.Fatal("invalid verify_levels should be rejected")
	}
	if !strings.Contains(res.Error, "verify_levels 含非法值") {
		t.Fatalf("expected validation error, got %q", res.Error)
	}
}

// TestWriteSpec_VerifyLevelsAlias 字段名误入 verify_levels（probes/scenes 等，2026-09-12
// 事故：模型把提示词并列书写的 probes 当层级填入，连续校验拒绝触发 LoopExit）：
// 自动纠正为对应层级并放行，结果回显纠正说明；真未知值仍拒绝。
func TestWriteSpec_VerifyLevelsAlias(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetSharedMemory(newFakeSharedMemoryStore())
	ctx := WithAgentID(context.Background(), "meta-1")

	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "g", "acceptance": []any{"a"},
		"verify_levels": []any{"visual", "probes", "scenes", "runtime"},
	})
	if err != nil || !res.Success {
		t.Fatalf("alias levels should be auto-corrected, got err=%v res=%+v", err, res)
	}
	val, _ := r.sharedMemory.Get(ctx, "meta-1:spec")
	fm, _, ok := DecodeSharedMD(val)
	if !ok {
		t.Fatal("decode failed")
	}
	// probes→runtime、scenes→visual，去重后应为 [visual, runtime]。
	if len(fm.VerifyLevels) != 2 || fm.VerifyLevels[0] != "visual" || fm.VerifyLevels[1] != "runtime" {
		t.Fatalf("verify_levels alias correction wrong: %v", fm.VerifyLevels)
	}
	if !strings.Contains(res.Output, "verify_levels 已自动纠正") {
		t.Fatalf("expected correction note in result, got %q", res.Output)
	}
}

// TestWriteSpec_HappyPathAutoAppend 降级/兜底关键词自动追加 happy-path 验收项（TODO #64）。
func TestWriteSpec_HappyPathAutoAppend(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetSharedMemory(newFakeSharedMemoryStore())
	ctx := WithAgentID(context.Background(), "meta-1")

	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "贴图优先，手绘兜底",
		"acceptance": []any{"文件生成到 assets/img/"},
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch: %v %v", err, res)
	}
	if !strings.Contains(res.Output, "happy-path") {
		t.Fatalf("output should announce happy-path append, got %q", res.Output)
	}
	val, _ := r.sharedMemory.Get(ctx, "meta-1:spec")
	fm, _, _ := DecodeSharedMD(val)
	found := false
	for _, a := range fm.Acceptance {
		if strings.HasPrefix(a, "【自动追加·happy-path】") {
			found = true
		}
	}
	if !found {
		t.Fatalf("happy-path acceptance not appended: %v", fm.Acceptance)
	}

	// 无兜底关键词：不追加。
	res2, _ := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "普通任务", "acceptance": []any{"a"},
	})
	if strings.Contains(res2.Output, "happy-path") {
		t.Fatalf("no fallback keywords should not append, got %q", res2.Output)
	}
}
