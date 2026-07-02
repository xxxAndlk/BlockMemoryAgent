package graph

// 本文件承载 HTTP 类工具实现：HTTPGet / HTTPPost + parseStringMap。
// 从 tool_executor.go 按工具类别拆出（P0-3）。方法挂在 *ToolExecutor 上，同 package。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpGet 执行 HTTP GET 请求。
//
// 职责：发起 GET 请求，附带自定义 header 与超时，最多读 1MB 响应体。
// 参数：
//   - ctx：用于超时控制。
//   - args：含 "url" 字段，可选 "headers"/"timeout"。
//
// 返回：Output 为 "HTTP <status>\n<body>"；2xx 时 Success=true。
// 副作用：发起网络请求。
func (e *ToolExecutor) httpGet(ctx context.Context, args map[string]any) *ToolResult {
	// 取 URL
	url, _ := args["url"].(string)
	if url == "" {
		return &ToolResult{Tool: "HTTPGet", Error: "url is required"}
	}

	// 解析 headers（兼容 map[string]any / map[string]string）
	headers := parseStringMap(args["headers"])

	// 计算超时
	timeout := e.timeout
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	// 派生带超时的 ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 构造请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &ToolResult{Tool: "HTTPGet", Path: url, Error: err.Error()}
	}
	// 设置自定义 header
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// 发起请求
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &ToolResult{Tool: "HTTPGet", Path: url, Error: err.Error()}
	}
	defer resp.Body.Close()

	// 最多读 1MB 响应体，避免大响应撑爆内存
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 最多 1MB
	// 拼装输出：状态码 + 响应体
	output := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(body))
	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}

	return &ToolResult{
		Tool:    "HTTPGet",
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300, // 2xx 算成功
		Output:  output,
		Path:    url,
	}
}

// httpPost 执行 HTTP POST 请求（默认 JSON Body）。
//
// 职责：发起 POST 请求，body 默认按 JSON 序列化并自动补 Content-Type，
//
//	最多读 1MB 响应体。
//
// 参数：
//   - ctx：用于超时控制。
//   - args：含 "url" 字段，可选 "headers"/"body"/"timeout"。
//
// 返回：Output 为 "HTTP <status>\n<body>"；2xx 时 Success=true。
// 副作用：发起网络请求。
func (e *ToolExecutor) httpPost(ctx context.Context, args map[string]any) *ToolResult {
	// 取 URL
	url, _ := args["url"].(string)
	if url == "" {
		return &ToolResult{Tool: "HTTPPost", Error: "url is required"}
	}

	// 解析 headers
	headers := parseStringMap(args["headers"])

	// 序列化 body：string 原样使用，其他类型走 JSON
	var bodyBytes []byte
	if raw, ok := args["body"]; ok {
		switch v := raw.(type) {
		case string:
			bodyBytes = []byte(v) // 字符串 body 直接用
		default:
			// 其他类型（map/struct 等）序列化为 JSON
			b, err := json.Marshal(v)
			if err != nil {
				return &ToolResult{Tool: "HTTPPost", Path: url, Error: "marshal body: " + err.Error()}
			}
			bodyBytes = b
			// 自动补 Content-Type: application/json（用户未显式设置时）
			if _, ok := headers["Content-Type"]; !ok {
				if headers == nil {
					headers = make(map[string]string)
				}
				headers["Content-Type"] = "application/json"
			}
		}
	}

	// 计算超时
	timeout := e.timeout
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 构造 POST 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return &ToolResult{Tool: "HTTPPost", Path: url, Error: err.Error()}
	}
	// 设置 header
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// 发起请求
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &ToolResult{Tool: "HTTPPost", Path: url, Error: err.Error()}
	}
	defer resp.Body.Close()

	// 最多读 1MB 响应体
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	output := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(body))
	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}

	return &ToolResult{
		Tool:    "HTTPPost",
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Output:  output,
		Path:    url,
	}
}

// parseStringMap 兼容 map[string]any / map[string]string。
//
// 职责：把 LLM 传来的 headers（可能是任意 map 类型）统一转为 map[string]string。
// 参数：
//   - raw：原始值，预期为 map[string]string 或 map[string]any。
//
// 返回：归一化后的 map[string]string；nil 输入返回 nil。
// 副作用：无。
// 并发安全：纯函数。
func parseStringMap(raw any) map[string]string {
	if raw == nil {
		return nil
	}
	switch m := raw.(type) {
	case map[string]string:
		return m // 已是目标类型，直接返回
	case map[string]any:
		// 逐键转 string，非 string 值用 fmt.Sprint 兜底
		out := make(map[string]string, len(m))
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			} else {
				out[k] = fmt.Sprint(v)
			}
		}
		return out
	}
	return nil // 未知类型返回 nil
}
