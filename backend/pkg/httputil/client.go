// Package httputil provides reusable HTTP helpers.
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

// RetryConfig controls the retry behavior of RetryableClient.
type RetryConfig struct {
	MaxRetries int           // maximum number of retries after the initial request
	BaseDelay  time.Duration // initial backoff delay
	MaxDelay   time.Duration // maximum backoff delay
}

// DefaultRetryConfig returns a conservative retry policy suitable for LLM/embedding endpoints.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: 3,
		BaseDelay:  500 * time.Millisecond,
		MaxDelay:   5 * time.Second,
	}
}

// RetryableClient wraps an http.Client with exponential-backoff retries.
type RetryableClient struct {
	client *http.Client
	cfg    RetryConfig
}

// NewRetryableClient creates a RetryableClient from cfg.
// If cfg.BaseDelay <= 0 a default base delay is used. If cfg.MaxDelay <= 0 a
// default cap is used. A zero MaxRetries means only the initial request is made.
func NewRetryableClient(cfg RetryConfig) *RetryableClient {
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = DefaultRetryConfig().BaseDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = DefaultRetryConfig().MaxDelay
	}
	return &RetryableClient{
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		cfg: cfg,
	}
}

// Do executes req, retrying on network errors or transient HTTP status codes
// (5xx, 429, 408). It returns the final response and error. The caller is
// responsible for closing the response body.
func (c *RetryableClient) Do(req *http.Request) (*http.Response, error) {
	body, err := readBody(req)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}

	attempts := c.cfg.MaxRetries + 1
	var lastErr error
	delay := c.cfg.BaseDelay

	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(jitter(delay)):
			}
			delay *= 2
			if delay > c.cfg.MaxDelay {
				delay = c.cfg.MaxDelay
			}
		}

		r, err := c.attempt(req, body)
		if err != nil {
			lastErr = err
			if !isRetryableError(err) {
				return nil, err
			}
			continue
		}

		if !isRetryableStatus(r.StatusCode) {
			return r, nil
		}
		// Consume and close the body before retrying so the connection can be reused.
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		lastErr = fmt.Errorf("attempt %d: HTTP %d", i+1, r.StatusCode)
	}

	if lastErr != nil {
		return nil, fmt.Errorf("request failed after %d attempts: %w", attempts, lastErr)
	}
	return nil, fmt.Errorf("request failed after %d attempts", attempts)
}

func (c *RetryableClient) attempt(req *http.Request, body []byte) (*http.Response, error) {
	nextReq, err := cloneRequest(req, body)
	if err != nil {
		return nil, err
	}
	return c.client.Do(nextReq)
}

func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body.Close()
	return body, nil
}

func cloneRequest(req *http.Request, body []byte) (*http.Request, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	next, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	next.Header = req.Header.Clone()
	return next, nil
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	default:
		return code >= 500
	}
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if err == context.DeadlineExceeded || err == context.Canceled {
		return false
	}
	return true
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	j := time.Duration(rand.Int63n(int64(d)))
	return d/2 + j
}
