package embed

// openai_breaker_test.go 验证 TODO #46 防挂起保护：
//   - Embed 单次调用带独立短超时（不随父 ctx 长超时挂死）；
//   - 连续失败达阈值后熔断开路，期内快速失败不再打端点；
//   - 冷却到期成功探测后闭路恢复正常。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/httputil"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func breakerTestEmbedder(t *testing.T, handler http.Handler) (*OpenAIEmbedder, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	e := NewOpenAIEmbedder(types.EmbedConfig{Provider: "local", Model: "test", BaseURL: srv.URL}, 3)
	// 单次尝试、短超时：测试内不做重试退避，行为由熔断器语义保证。
	e.client = httputil.NewRetryableClient(httputil.RetryConfig{MaxRetries: 0, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	e.WithCallTimeout(200 * time.Millisecond)
	t.Cleanup(srv.Close)
	return e, srv
}

func TestEmbed_CallTimeout(t *testing.T) {
	e, _ := breakerTestEmbedder(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	start := time.Now()
	if _, err := e.Embed(context.Background(), "hello"); err == nil {
		t.Fatal("expected timeout error from hung endpoint")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("embed call took %s, want fast-fail near callTimeout=200ms", elapsed)
	}
}

func TestEmbed_BreakerOpensAndCloses(t *testing.T) {
	var healthy atomic.Bool
	reqs := 0
	e, _ := breakerTestEmbedder(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs++
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"data":[{"embedding":[1,2,3]}]}`)
	}))

	ctx := context.Background()
	// 前 3 次连续失败 -> 第 4 次开路快速失败，且未打到端点。
	for i := 0; i < failThreshold; i++ {
		if _, err := e.Embed(ctx, "hello"); err == nil {
			t.Fatalf("call %d: expected failure while endpoint down", i+1)
		}
	}
	hungReqs := reqs
	if _, err := e.Embed(ctx, "hello"); err == nil || !e.breakerOpen() {
		t.Fatalf("breaker should be open after %d consecutive failures, err=%v", failThreshold, err)
	}
	if reqs != hungReqs {
		t.Fatalf("open-breaker call hit endpoint %d extra times", reqs-hungReqs)
	}

	// 端点恢复 + 冷却到期：探测成功后闭路，后续正常返回向量。
	healthy.Store(true)
	e.mu.Lock()
	e.openUntil = time.Now().Add(-time.Second)
	e.mu.Unlock()
	vec, err := e.Embed(ctx, "hello")
	if err != nil {
		t.Fatalf("probe after cooldown failed: %v", err)
	}
	if len(vec) != 3 {
		t.Fatalf("unexpected vector dim %d", len(vec))
	}
	if e.breakerOpen() {
		t.Fatal("breaker should be closed after successful probe")
	}
}
