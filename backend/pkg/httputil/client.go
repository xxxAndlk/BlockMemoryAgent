// Package httputil 提供可复用的 HTTP 工具函数与客户端封装。
package httputil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"
)

// RetryConfig 控制 RetryableClient 的重试行为。
type RetryConfig struct {
	MaxRetries int           // 初始请求之后的最大重试次数
	BaseDelay  time.Duration // 初始退避延迟
	MaxDelay   time.Duration // 最大退避延迟
}

// DefaultRetryConfig 返回适合 LLM/嵌入端点的保守重试策略。
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: 3,
		BaseDelay:  500 * time.Millisecond,
		MaxDelay:   5 * time.Second,
	}
}

// RetryableClient 对 http.Client 进行包装，提供带指数退避的重试能力。
type RetryableClient struct {
	client *http.Client // 底层标准 HTTP 客户端
	cfg    RetryConfig  // 重试配置
}

// NewRetryableClient 根据 cfg 创建一个 RetryableClient。
// 若 cfg.BaseDelay <= 0，则使用默认基础延迟；若 cfg.MaxDelay <= 0，则使用默认上限。
// MaxRetries 为 0 表示仅执行初始请求，不重试。
func NewRetryableClient(cfg RetryConfig) *RetryableClient {
	// 基础延迟非法时回退到默认值，避免退避计算出现非正数。
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = DefaultRetryConfig().BaseDelay
	}
	// 最大延迟非法时回退到默认值，避免延迟无限增长。
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = DefaultRetryConfig().MaxDelay
	}
	// 返回包装后的客户端，设置 60 秒请求超时。
	return &RetryableClient{
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		cfg: cfg,
	}
}

// Do 执行 HTTP 请求，在网络错误或瞬态 HTTP 状态码（5xx、429、408）时自动重试。
// 返回最终的响应与错误；调用方负责关闭响应体。
//
// 参数: req 要执行的 HTTP 请求。
// 返回: 成功响应或重试耗尽后的错误。
func (c *RetryableClient) Do(req *http.Request) (*http.Response, error) {
	// 预读取请求体，供每次重试时复用（http.Request 的 Body 只能读取一次）。
	body, err := readBody(req)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}

	// 总尝试次数 = 初始请求 + 最大重试次数。
	attempts := c.cfg.MaxRetries + 1
	// lastErr 记录最后一次遇到的错误，用于最终包装。
	var lastErr error
	// delay 记录当前退避延迟，每次重试后指数增长。
	delay := c.cfg.BaseDelay

	// 循环执行请求，最多 attempts 次。
	for i := 0; i < attempts; i++ {
		// 非首次尝试时先进行退避等待，同时监听上下文取消。
		if i > 0 {
			select {
			// 上下文已取消，立即返回错误。
			case <-req.Context().Done():
				return nil, req.Context().Err()
			// 按 jitter(delay) 等待一段时间后继续重试。
			case <-time.After(jitter(delay)):
			}
			// 指数退避：延迟翻倍。
			delay *= 2
			// 延迟不超过配置上限。
			if delay > c.cfg.MaxDelay {
				delay = c.cfg.MaxDelay
			}
		}

		// 执行一次请求尝试。
		r, err := c.attempt(req, body)
		if err != nil {
			// 记录错误。
			lastErr = err
			// 错误不可重试时直接返回，不再浪费尝试次数。
			if !isRetryableError(err) {
				return nil, err
			}
			// 否则进入下一次重试。
			continue
		}

		// 响应状态码不可重试时直接返回响应。
		if !isRetryableStatus(r.StatusCode) {
			return r, nil
		}
		// 状态码可重试：消耗并关闭响应体，便于连接复用。
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		// 记录本次失败信息作为 lastErr。
		lastErr = fmt.Errorf("attempt %d: HTTP %d", i+1, r.StatusCode)
	}

	// 所有尝试耗尽后，返回包含 lastErr 的失败信息。
	if lastErr != nil {
		return nil, fmt.Errorf("request failed after %d attempts: %w", attempts, lastErr)
	}
	return nil, fmt.Errorf("request failed after %d attempts", attempts)
}

// attempt 执行单次请求：克隆请求并复用 body。
func (c *RetryableClient) attempt(req *http.Request, body []byte) (*http.Response, error) {
	// 基于原始请求和已读取的 body 构造新的可独立发送请求。
	nextReq, err := cloneRequest(req, body)
	if err != nil {
		return nil, err
	}
	// 使用底层客户端发送请求。
	return c.client.Do(nextReq)
}

// readBody 读取 http.Request 的请求体并关闭原始 Body。
//
// 返回: 读取到的字节切片；无 body 或 body 为空时返回 nil。
func readBody(req *http.Request) ([]byte, error) {
	// 请求体为空或显式为 NoBody 时无需读取。
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	// 读取全部请求体内容。
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	// 关闭原始 Body，避免资源泄漏。
	req.Body.Close()
	return body, nil
}

// cloneRequest 根据原始请求和已读取的 body 创建新的 http.Request。
//
// 返回: 克隆后的请求指针；错误仅在 NewRequestWithContext 失败时返回。
func cloneRequest(req *http.Request, body []byte) (*http.Request, error) {
	// bodyReader 仅在 body 非空时使用，否则为 nil 表示无请求体。
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	// 使用原始请求的上下文、方法、URL 构造新请求。
	next, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	// 深拷贝请求头，避免多次重试之间互相污染。
	next.Header = req.Header.Clone()
	return next, nil
}

// isRetryableStatus 判断 HTTP 状态码是否属于可重试的瞬态错误。
func isRetryableStatus(code int) bool {
	switch code {
	// 408 Request Timeout 与 429 Too Many Requests 属于可重试场景。
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	default:
		// 5xx 服务器错误通常可重试。
		return code >= 500
	}
}

// isRetryableError 判断错误是否属于可重试的网络错误。
func isRetryableError(err error) bool {
	// 无错误时不应触发重试。
	if err == nil {
		return false
	}
	// 上下文取消或超时属于调用方主动终止，不应重试。
	if err == context.DeadlineExceeded || err == context.Canceled {
		return false
	}
	// 其他错误（如连接失败、EOF 等）默认可重试。
	return true
}

// jitter 对给定延迟加入随机抖动，避免多个客户端在同一时刻重试造成 thundering herd。
//
// 参数: d 基础延迟。
// 返回: [d/2, d) 范围内的随机延迟。
func jitter(d time.Duration) time.Duration {
	// 非正延迟直接返回 0，避免随机数 panic。
	if d <= 0 {
		return 0
	}
	// 在 [0, d) 范围内取随机值。
	j := time.Duration(rand.Int63n(int64(d)))
	// 返回 d 的一半加上随机值，使实际等待在 d/2 到 d 之间。
	return d/2 + j
}
