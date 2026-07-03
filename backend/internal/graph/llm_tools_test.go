package graph

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakeLLMClient 实现 model.LLMClient，用于 executeWithTools mock 路径测试。
type fakeLLMClient struct {
	resp string
	err  error
}

func (f *fakeLLMClient) Generate(ctx context.Context, prompt string) (string, error) {
	return f.resp, f.err
}

// TestExecuteWithToolsMockPathRecordsTokens 验证 P0-4：mock 路径（provider==nil）
// 执行后通过 llmTracker.RecordCall 记录 token 用量。
func TestExecuteWithToolsMockPathRecordsTokens(t *testing.T) {
	tracker := model.NewLLMCallTracker()
	var mu sync.Mutex
	var records []model.CallRecord
	tracker.SetRecordCallback(func(ctx context.Context, r model.CallRecord) {
		mu.Lock()
		records = append(records, r)
		mu.Unlock()
	})

	llm := &fakeLLMClient{resp: "hello world"}
	roleDef := &types.RoleDefinition{ID: "x", Name: "t", SystemPrompt: "sp"}
	state := types.NewThreeLayerState("s1")

	// provider 传 nil → 走 mock 路径
	resp, results := executeWithTools(
		context.Background(), nil, llm, nil, roleDef, "做某事", state, "", nil, "助手[t]", 0, tracker,
	)
	if resp == nil || resp.SummaryForUser != "hello world" {
		t.Fatalf("响应应为 hello world，got %v", resp)
	}
	if len(results) != 0 {
		t.Fatalf("mock 路径不应产生工具结果，got %d", len(results))
	}

	// 等待异步回调（RecordCallback 通过 goroutine 触发）
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		mu.Lock()
		n := len(records)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(records) != 1 {
		t.Fatalf("应记录 1 次 LLM 调用，got %d", len(records))
	}
	if records[0].Response != "hello world" {
		t.Fatalf("记录的 response 应为 hello world，got %s", records[0].Response)
	}
	if records[0].InputTokens <= 0 {
		t.Fatal("应记录非零输入 token（估算）")
	}
	if records[0].OutputTokens <= 0 {
		t.Fatal("应记录非零输出 token（估算）")
	}
	if records[0].Caller != "助手[t]" {
		t.Fatalf("caller 应为 助手[t]，got %s", records[0].Caller)
	}
}

// TestExecuteWithToolsMockPathNilTracker 验证 llmTracker 为 nil 时不 panic。
func TestExecuteWithToolsMockPathNilTracker(t *testing.T) {
	llm := &fakeLLMClient{resp: "ok"}
	roleDef := &types.RoleDefinition{ID: "x", Name: "t", SystemPrompt: "sp"}
	state := types.NewThreeLayerState("s1")
	resp, _ := executeWithTools(
		context.Background(), nil, llm, nil, roleDef, "做某事", state, "", nil, "助手[t]", 0, nil,
	)
	if resp == nil || resp.SummaryForUser != "ok" {
		t.Fatalf("响应应为 ok，got %v", resp)
	}
}

// TestExecuteWithToolsMockPathEmitsTokenUsage 验证 P0-4：mock 路径会推送带 in/out 数值的
// token_usage 事件，且 message 格式可被后端 parseTokenUsage 解析出非零 token。
func TestExecuteWithToolsMockPathEmitsTokenUsage(t *testing.T) {
	llm := &fakeLLMClient{resp: "hello world"}
	roleDef := &types.RoleDefinition{ID: "x", Name: "t", SystemPrompt: "sp"}
	state := types.NewThreeLayerState("s1")

	var mu sync.Mutex
	var events []ProgressEvent
	progress := ProgressCallback(func(ctx context.Context, ev ProgressEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})

	executeWithTools(
		context.Background(), nil, llm, nil, roleDef, "做某事", state, "", progress, "助手[t]", 0, nil,
	)

	mu.Lock()
	defer mu.Unlock()

	var tokenUsage *ProgressEvent
	for i := range events {
		if events[i].Kind == "token_usage" {
			tokenUsage = &events[i]
			break
		}
	}
	if tokenUsage == nil {
		t.Fatalf("未找到 token_usage 事件，got events: %+v", events)
	}
	if !strings.Contains(tokenUsage.Message, "in=") || !strings.Contains(tokenUsage.Message, "out=") {
		t.Fatalf("token_usage 事件 message 应包含 in=/out=，got: %s", tokenUsage.Message)
	}
	inStr := extractIntFromMessage(tokenUsage.Message, "in=")
	outStr := extractIntFromMessage(tokenUsage.Message, "out=")
	if inStr <= 0 {
		t.Fatalf("输入 token 应大于 0，got %d (message: %s)", inStr, tokenUsage.Message)
	}
	if outStr <= 0 {
		t.Fatalf("输出 token 应大于 0，got %d (message: %s)", outStr, tokenUsage.Message)
	}
}

// TestExecuteWithToolsMockPathEmptyResponseStillNonZeroTokens 验证 P0-4：mock 路径返回空
// response 时，token_usage 事件的 input/output token 仍大于 0（用占位符兜底）。
func TestExecuteWithToolsMockPathEmptyResponseStillNonZeroTokens(t *testing.T) {
	llm := &fakeLLMClient{resp: ""}
	roleDef := &types.RoleDefinition{ID: "x", Name: "t", SystemPrompt: "sp"}
	state := types.NewThreeLayerState("s1")

	var mu sync.Mutex
	var events []ProgressEvent
	progress := ProgressCallback(func(ctx context.Context, ev ProgressEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})

	executeWithTools(
		context.Background(), nil, llm, nil, roleDef, "做某事", state, "", progress, "助手[t]", 0, nil,
	)

	mu.Lock()
	defer mu.Unlock()

	var tokenUsage *ProgressEvent
	for i := range events {
		if events[i].Kind == "token_usage" {
			tokenUsage = &events[i]
			break
		}
	}
	if tokenUsage == nil {
		t.Fatalf("未找到 token_usage 事件")
	}
	inTok := extractIntFromMessage(tokenUsage.Message, "in=")
	outTok := extractIntFromMessage(tokenUsage.Message, "out=")
	if inTok <= 0 {
		t.Fatalf("空响应时输入 token 应大于 0，got %d", inTok)
	}
	if outTok <= 0 {
		t.Fatalf("空响应时输出 token 应大于 0（兜底占位符），got %d", outTok)
	}
}

// TestExecuteWithToolsMockPathErrorStillNonZeroTokens 验证 P0-4：mock 路径 LLM 调用失败时，
// token_usage 事件 input/output token 仍大于 0（用错误信息兜底 output）。
func TestExecuteWithToolsMockPathErrorStillNonZeroTokens(t *testing.T) {
	llm := &fakeLLMClient{err: fmt.Errorf("connection refused")}
	roleDef := &types.RoleDefinition{ID: "x", Name: "t", SystemPrompt: "sp"}
	state := types.NewThreeLayerState("s1")

	var mu sync.Mutex
	var events []ProgressEvent
	progress := ProgressCallback(func(ctx context.Context, ev ProgressEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})

	executeWithTools(
		context.Background(), nil, llm, nil, roleDef, "做某事", state, "", progress, "助手[t]", 0, nil,
	)

	mu.Lock()
	defer mu.Unlock()

	var tokenUsage *ProgressEvent
	for i := range events {
		if events[i].Kind == "token_usage" {
			tokenUsage = &events[i]
			break
		}
	}
	if tokenUsage == nil {
		t.Fatalf("未找到 token_usage 事件")
	}
	inTok := extractIntFromMessage(tokenUsage.Message, "in=")
	outTok := extractIntFromMessage(tokenUsage.Message, "out=")
	if inTok <= 0 {
		t.Fatalf("失败时输入 token 应大于 0，got %d", inTok)
	}
	if outTok <= 0 {
		t.Fatalf("失败时输出 token 应大于 0（错误信息兜底），got %d", outTok)
	}
}

// extractIntFromMessage 从 message 中解析 marker 后的整数，与 server.parseTokenUsage 逻辑保持一致。
func extractIntFromMessage(msg, marker string) int {
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return 0
	}
	start := idx + len(marker)
	for start < len(msg) && (msg[start] == ' ' || msg[start] == '\t') {
		start++
	}
	end := start
	for end < len(msg) && msg[end] >= '0' && msg[end] <= '9' {
		end++
	}
	if start == end {
		return 0
	}
	n, _ := strconv.Atoi(msg[start:end])
	return n
}
