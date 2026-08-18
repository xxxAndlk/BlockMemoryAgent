package mcpbridge

// bridge_test.go 覆盖桥接层（设计文档 §3.4）：用 go-sdk server 写 mock MCP server，
// 以 stdio 子进程方式验证连接 / 调用 / schema 透传 / 断线重连 / Stop。
//
// Mock 约定：测试二进制以环境变量 MCP_MOCK_SERVER=1 重入自身即充当 MCP server
//（TestMain 分流），stdout 只走 MCP 协议，日志一律 stderr。

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMain 分流：MCP_MOCK_SERVER=1 时作为 stdio mock server 运行。
func TestMain(m *testing.M) {
	if os.Getenv("MCP_MOCK_SERVER") == "1" {
		runMockServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runMockServer 以 stdio 提供三个工具：
//   - echo：回显文本（验证调用往返）；
//   - boom：返回 IsError 结果（验证错误回灌）；
//   - die：直接退出进程（验证断线检测 + 重连）。
func runMockServer() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mock", Version: "1.0.0"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "回显文本",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []any{"text"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct{ Text string }
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + args.Text}}}, nil
	})
	srv.AddTool(&mcp.Tool{Name: "boom", Description: "返回错误结果", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "boom happened"}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "die", Description: "退出进程（模拟崩溃）", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			os.Exit(0)
			return nil, nil
		})
	if _, err := srv.Connect(context.Background(), &mcp.StdioTransport{}, nil); err != nil {
		os.Exit(1)
	}
	select {} // 阻塞至被父进程 kill / stdin 关闭
}

// mockSettings 返回 stdio 拉起测试二进制自身的配置。
func mockSettings() Settings {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return Settings{
		Transport: "stdio",
		Command:   exe,
		Env:       map[string]string{"MCP_MOCK_SERVER": "1"},
	}
}

// mustTool 按名取出工具适配器。
func mustTool(t *testing.T, tools []tool.Tool, name string) tool.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("工具 %s 未出现在列表 %v", name, tools)
	return nil
}

// TestFromSettings 验证 settings 解析（transport/args/env/roles/destructive/超时）。
func TestFromSettings(t *testing.T) {
	s := FromSettings(map[string]any{
		"transport":         "http",
		"url":               "http://localhost:8080/mcp",
		"destructive":       true,
		"image_passthrough": true,
		"exec_timeout_sec":  30.0,
		"roles":             []any{"meta", "domain"},
	})
	if s.Transport != "http" || s.URL != "http://localhost:8080/mcp" || !s.Destructive {
		t.Fatalf("解析错误: %+v", s)
	}
	if !s.ImagePassthrough {
		t.Fatalf("image_passthrough 解析错误: %+v", s)
	}
	if s.ExecTimeout != 30*time.Second {
		t.Fatalf("exec_timeout 解析错误: %v", s.ExecTimeout)
	}
	if len(s.Roles) != 2 || s.Roles[0] != "meta" {
		t.Fatalf("roles 解析错误: %v", s.Roles)
	}
	// 缺省关闭：文本模型收到 image block 会被端点 400，必须显式开启。
	if FromSettings(map[string]any{}).ImagePassthrough {
		t.Fatalf("image_passthrough 缺省应为 false")
	}
}

// TestExtractImages 验证 image content 提取的过滤与上限：
// 空 MIME/空数据/超尺寸跳过，单次最多 maxPassthroughImages 张。
func TestExtractImages(t *testing.T) {
	mk := func(mime string, size int) *mcp.ImageContent {
		return &mcp.ImageContent{MIMEType: mime, Data: make([]byte, size)}
	}
	content := []mcp.Content{
		&mcp.TextContent{Text: "前置文本"},
		mk("image/png", 16),
		mk("", 16),                            // 无 MIME：跳过
		mk("image/jpeg", 0),                   // 空数据：跳过
		mk("image/png", maxPassthroughImageBytes+1), // 超尺寸：跳过
		mk("image/webp", 32),
	}
	images := extractImages(content)
	if len(images) != 2 {
		t.Fatalf("应提取 2 张合法图片, got %d: %+v", len(images), images)
	}
	if images[0].MIMEType != "image/png" || images[1].MIMEType != "image/webp" {
		t.Fatalf("提取结果错误: %+v", images)
	}
	// 数量上限：6 张合法图片只留前 maxPassthroughImages 张。
	var many []mcp.Content
	for i := 0; i < maxPassthroughImages+2; i++ {
		many = append(many, mk("image/png", 8))
	}
	if got := extractImages(many); len(got) != maxPassthroughImages {
		t.Fatalf("应截断到 %d 张, got %d", maxPassthroughImages, len(got))
	}
}

// TestBridgeStdioConnectCall 验证 stdio 连接、工具列表、调用往返与错误回灌。
func TestBridgeStdioConnectCall(t *testing.T) {
	b := New("mock", mockSettings(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = b.Stop(context.Background()) }()

	tools := b.Tools()
	if len(tools) != 3 {
		t.Fatalf("应暴露 3 个远端工具，got %d", len(tools))
	}
	// 调用往返。
	echo := mustTool(t, tools, "echo")
	res := echo.Execute(ctx, map[string]any{"text": "hello"})
	if !res.Success || res.Output != "echo:hello" {
		t.Fatalf("echo 调用失败: %+v", res)
	}
	// 服务端 IsError → 失败结果回灌。
	boom := mustTool(t, tools, "boom")
	res = boom.Execute(ctx, map[string]any{})
	if res.Success || !strings.Contains(res.Error, "boom happened") {
		t.Fatalf("boom 应回灌错误: %+v", res)
	}
	// Manifest 与别名。
	if b.Manifest().ID != "mock" || b.Manifest().Kind != "mcp" {
		t.Fatalf("Manifest 错误: %+v", b.Manifest())
	}
	if len(echo.Aliases()) != 0 {
		t.Fatal("MCP 工具应无别名")
	}
}

// TestBridgeSchemaPassthrough 验证描述与入参 schema 透传（LLM 可见定义）。
func TestBridgeSchemaPassthrough(t *testing.T) {
	b := New("mock", mockSettings(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = b.Stop(context.Background()) }()

	echo := mustTool(t, b.Tools(), "echo")
	if d := echo.(interface{ Description() string }).Description(); d != "回显文本" {
		t.Fatalf("描述应透传服务端值，got %q", d)
	}
	sch := echo.(interface{ InputSchema() *jsonschema.Schema }).InputSchema()
	if sch == nil {
		t.Fatal("入参 schema 不应为 nil")
	}
	if _, ok := sch.Properties["text"]; !ok {
		t.Fatalf("schema 应包含 text 属性，got %v", sch.Properties)
	}
	// 默认非破坏性。
	if d, ok := echo.(interface{ Destructive() bool }); ok && d.Destructive() {
		t.Fatal("未配置 destructive 时应为非破坏性")
	}
}

// TestBridgeReconnect 验证：子进程退出 → OnDown → 指数退避重连 → OnUp → 恢复可用。
func TestBridgeReconnect(t *testing.T) {
	b := New("mock", mockSettings(), nil)
	upCh := make(chan []tool.Tool, 4)
	downCh := make(chan error, 4)
	b.SetLifecycleHooks(LifecycleHooks{
		OnUp:   func(ts []tool.Tool) { upCh <- ts },
		OnDown: func(e error) { downCh <- e },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = b.Stop(context.Background()) }()

	// 触发子进程退出。
	die := mustTool(t, b.Tools(), "die")
	die.Execute(ctx, map[string]any{})

	select {
	case err := <-downCh:
		if err == nil {
			t.Fatal("OnDown 应携带错误")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("子进程退出后应触发 OnDown")
	}
	// 重连成功（首次退避 1s）。
	select {
	case newTools := <-upCh:
		if len(newTools) != 3 {
			t.Fatalf("重连后工具数应为 3，got %d", len(newTools))
		}
		echo := mustTool(t, newTools, "echo")
		res := echo.Execute(ctx, map[string]any{"text": "again"})
		if !res.Success || res.Output != "echo:again" {
			t.Fatalf("重连后调用失败: %+v", res)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("应自动重连成功")
	}
}

// TestBridgeStop 验证 Stop：子进程被杀、不再重连、Tools 清空。
func TestBridgeStop(t *testing.T) {
	b := New("mock", mockSettings(), nil)
	downCh := make(chan error, 4)
	b.SetLifecycleHooks(LifecycleHooks{OnDown: func(e error) { downCh <- e }})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(b.Tools()) != 0 {
		t.Fatal("Stop 后 Tools 应为空")
	}
	// Stop 不触发 OnDown（正常关闭路径）。
	select {
	case <-downCh:
		t.Fatal("Stop 不应触发 OnDown")
	case <-time.After(300 * time.Millisecond):
	}
	// Stop 后调用返回连接不可用。
	res := b.callTool(context.Background(), "echo", map[string]any{})
	if res.Success {
		t.Fatal("Stop 后调用应失败")
	}
	// 幂等。
	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("重复 Stop 应幂等: %v", err)
	}
}

// TestBridgeRestartAfterStop 验证热插拔 enable-after-disable：
// Stop 后再次 Start 应重置状态并重新建立连接（Manager 复用同一实例）。
func TestBridgeRestartAfterStop(t *testing.T) {
	b := New("mock", mockSettings(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("首次 Start: %v", err)
	}
	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Stop 后重新 Start: %v", err)
	}
	defer func() { _ = b.Stop(context.Background()) }()
	if len(b.Tools()) == 0 {
		t.Fatal("重启后应恢复工具列表")
	}
	res := b.callTool(ctx, "echo", map[string]any{"text": "hi"})
	if !res.Success || res.Output != "echo:hi" {
		t.Fatalf("重启后调用失败: %+v", res)
	}
}
