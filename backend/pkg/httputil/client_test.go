package httputil

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryableClient_RetriesThenSucceeds(t *testing.T) {
	var count int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "hello" {
			t.Fatalf("unexpected body %q", body)
		}
		if atomic.AddInt32(&count, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	client := NewRetryableClient(RetryConfig{
		MaxRetries: 3,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   50 * time.Millisecond,
	})

	req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&count) != 3 {
		t.Fatalf("expected 3 attempts, got %d", count)
	}
}

func TestRetryableClient_RespectsCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewRetryableClient(RetryConfig{
		MaxRetries: 3,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   50 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	if ctx.Err() == nil {
		t.Fatalf("expected context error, got %v", err)
	}
}
