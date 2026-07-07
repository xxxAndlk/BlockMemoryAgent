package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"

	"github.com/blockmemory/agent/backend/internal/model"
)

func TestExtractJSONBlock(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", `{"passed":true}`, `{"passed":true}`},
		{"markdown", "```json\n{\"passed\":false}\n```", `{"passed":false}`},
		{"with text", `ok {"passed":true} done`, `{"passed":true}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractJSONBlock(c.in)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestRunSelfTestAssistantNilFactory(t *testing.T) {
	_, err := runSelfTestAssistant(context.Background(), nil, nil, nil, nil, "task", &types.AgentResult{}, nil, "test", nil)
	if err == nil {
		t.Fatal("expected error when modelFactory is nil")
	}
}

func TestRunSelfTestAssistantPassed(t *testing.T) {
	report := SelfTestReport{Passed: true, Issues: []string{}, Report: "测试通过"}
	body, _ := json.Marshal(report)
	server := newOpenAIMockServer(t, string(body))
	defer server.Close()

	mf := newTestModelFactory(server.URL)
	result := &types.AgentResult{SummaryForUser: "实现了加法函数"}
	selfTestResult, err := runSelfTestAssistant(context.Background(), mf, nil, nil, nil, "实现加法", result, nil, "test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selfTestResult == nil {
		t.Fatal("expected non-nil self-test result")
	}
	if !strings.Contains(selfTestResult.SummaryForUser, "测试通过") {
		t.Errorf("expected report text, got %q", selfTestResult.SummaryForUser)
	}
}

func TestRunSelfTestAssistantFailed(t *testing.T) {
	report := SelfTestReport{Passed: false, Issues: []string{"缺少边界检查"}, Report: "发现错误"}
	body, _ := json.Marshal(report)
	server := newOpenAIMockServer(t, string(body))
	defer server.Close()

	mf := newTestModelFactory(server.URL)
	result := &types.AgentResult{SummaryForUser: "实现了除法函数"}
	selfTestResult, err := runSelfTestAssistant(context.Background(), mf, nil, nil, nil, "实现除法", result, nil, "test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(selfTestResult.Facts) == 0 {
		t.Error("expected issues as facts")
	}
}

// newOpenAIMockServer 返回一个模拟 OpenAI chat completions 的 httptest 服务器。
func newOpenAIMockServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-test",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": content,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 10,
				"total_tokens":      20,
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode mock response: %v", err)
		}
	}))
}

// newTestModelFactory 构造一个使用 mock 服务器作为轻量模型的 ModelFactory。
func newTestModelFactory(baseURL string) *model.ModelFactory {
	cfg := &config.RoleConfigFile{
		LightweightModel: types.AgentModelConfig{
			Provider: "openai",
			Model:    "gpt-test",
			APIKey:   "test-key",
			BaseURL:  fmt.Sprintf("%s/v1", baseURL),
		},
	}
	return model.NewModelFactory(cfg)
}
