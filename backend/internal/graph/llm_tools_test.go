package graph

import (
	"context"
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
	if resp != "hello world" {
		t.Fatalf("响应应为 hello world，got %s", resp)
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
	if resp != "ok" {
		t.Fatalf("响应应为 ok，got %s", resp)
	}
}
