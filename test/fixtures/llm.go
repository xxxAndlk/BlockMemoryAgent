//go:build integration

package fixtures

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// MockResponse describes one possible response from the mock LLM server.
type MockResponse struct {
	// Content is returned as the assistant message content when no ToolCalls are
	// present.
	Content string
	// ToolCalls is returned when the model should invoke tools.
	ToolCalls []MockToolCall
	// FinishReason is the finish_reason for the choice. Defaults to "stop" or
	// "tool_calls" depending on ToolCalls.
	FinishReason string
}

// MockToolCall is a simplified OpenAI tool call representation.
type MockToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// MockLLMServer is an httptest server that mimics an OpenAI-compatible
// /chat/completions endpoint. Tests register responses by prompt substring or
// set a default response. It never requires a real OPENAI_API_KEY.
type MockLLMServer struct {
	server     *httptest.Server
	mu         sync.RWMutex
	responses  map[string]MockResponse // keyed by prompt substring
	defaultRes *MockResponse
	sequence   []MockResponse // FIFO 队列：非空时优先按序返回，驱动确定性多轮工具调用
	requests   []mockRequest
}

type mockRequest struct {
	Model    string          `json:"model"`
	Messages []mockMessage  `json:"messages"`
	Tools    json.RawMessage `json:"tools"`
}

// mockMessage 兼容 OpenAI 两种 content 格式：
//   - 字符串："content": "hello"
//   - 数组（多模态/文本块）："content": [{"type":"text","text":"hello"}]
//
// blades contrib/openai provider 默认发送数组格式，旧 mock 只接受字符串导致 400。
type mockMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// textContent 把 mockMessage.Content（字符串或数组）归一为纯文本。
// 数组格式取所有 type=text 块的 text 字段拼接；其他类型块忽略。
func (m mockMessage) textContent() string {
	if len(m.Content) == 0 {
		return ""
	}
	// 尝试作为字符串解析。
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	// 尝试作为 content 块数组解析。
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" || b.Type == "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// NewMockLLMServer creates and starts a new mock LLM server.
func NewMockLLMServer(t testing.TB) *MockLLMServer {
	m := &MockLLMServer{
		responses: make(map[string]MockResponse),
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

// BaseURL returns the mock server's base URL. Pass this to roles.yaml as the
// OpenAI base URL.
func (m *MockLLMServer) BaseURL() string {
	return m.server.URL + "/v1"
}

// SetDefaultResponse sets the response used when no substring matches.
func (m *MockLLMServer) SetDefaultResponse(r MockResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultRes = &r
}

// RegisterResponseBySubstring registers a response returned when the combined
// prompt contains the given substring. Later registrations override earlier
// ones.
func (m *MockLLMServer) RegisterResponseBySubstring(substring string, r MockResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[substring] = r
}

// RegisterSequence 注册一组按序返回的响应（FIFO 队列）。
// 队列非空时优先于 substring/default，每次请求弹出队首；耗尽后回退到 substring/default。
// 用途：驱动确定性的多轮 ReAct 序列（如先 WriteFile 工具调用，再最终文本）。
func (m *MockLLMServer) RegisterSequence(responses ...MockResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sequence = append(m.sequence, responses...)
}

// Requests returns a copy of all requests received so far.
func (m *MockLLMServer) Requests() []mockRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]mockRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

// RequestPrompts returns the joined prompt text (all message contents) for each
// received request. Lets external-package tests inspect what reached the LLM
// without accessing unexported mockRequest fields.
func (m *MockLLMServer) RequestPrompts() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.requests))
	for i, r := range m.requests {
		out[i] = m.promptText(r)
	}
	return out
}

func (m *MockLLMServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req mockRequest
	if err := json.Unmarshal(body, &req); err != nil {
		preview := string(body)
		if len(preview) > 1024 {
			preview = preview[:1024] + "..."
		}
		fmt.Fprintf(os.Stderr, "[mock-llm] unmarshal error: %v\nbody: %s\n", err, preview)
		http.Error(w, "unmarshal: "+err.Error(), http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	m.requests = append(m.requests, req)
	res := m.pickResponseLocked(req)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(openAICompletionResponse(req.Model, res))
}

// pickResponseLocked 选取响应，调用方必须已持有 m.mu。
// 优先消费 FIFO 序列队列（确定性多轮驱动）；其次按 substring 匹配；最后回退 default。
func (m *MockLLMServer) pickResponseLocked(req mockRequest) MockResponse {
	if len(m.sequence) > 0 {
		res := m.sequence[0]
		m.sequence = m.sequence[1:]
		return res
	}
	prompt := m.promptText(req)
	for sub, res := range m.responses {
		if strings.Contains(prompt, sub) {
			return res
		}
	}
	if m.defaultRes != nil {
		return *m.defaultRes
	}
	return MockResponse{Content: "ok"}
}

func (m *MockLLMServer) promptText(req mockRequest) string {
	var parts []string
	for _, msg := range req.Messages {
		parts = append(parts, msg.textContent())
	}
	return strings.Join(parts, "\n")
}

type openAIResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int             `json:"index"`
		Message      responseMessage `json:"message"`
		FinishReason string          `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type responseMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []MockToolCall `json:"tool_calls,omitempty"`
}

func openAICompletionResponse(model string, res MockResponse) openAIResponse {
	if model == "" {
		model = "mock-model"
	}
	finish := res.FinishReason
	if finish == "" {
		if len(res.ToolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	msg := responseMessage{
		Role:    "assistant",
		Content: res.Content,
	}
	if len(res.ToolCalls) > 0 {
		msg.ToolCalls = res.ToolCalls
	}
	return openAIResponse{
		ID:      "mock-id",
		Object:  "chat.completion",
		Created: 1,
		Model:   model,
		Choices: []struct {
			Index        int             `json:"index"`
			Message      responseMessage `json:"message"`
			FinishReason string          `json:"finish_reason"`
		}{
			{Index: 0, Message: msg, FinishReason: finish},
		},
		Usage: struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		}{
			PromptTokens:     len(msg.Content),
			CompletionTokens: len(msg.Content),
			TotalTokens:      len(msg.Content) * 2,
		},
	}
}
