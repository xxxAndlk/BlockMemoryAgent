package agent

// notifier_test.go 会话终态 Webhook 通知（TODO #18-5 T32）：
// httptest 服务端收报文断言 + 事件过滤 + 未接线/不关心状态零请求 + 去重防抖。
// 发送是异步 best-effort：断言用轮询等待（上界 2s），避免睡眠竞态。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestNotifier_PostsFinalEvent(t *testing.T) {
	var mu sync.Mutex
	var got NotifyWebhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("应 POST，got %s", r.Method)
		}
		var p NotifyWebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		got = p
		mu.Unlock()
	}))
	defer srv.Close()

	n := NewNotifier(srv.URL, []string{"completed", "error"})
	if n == nil {
		t.Fatal("应构造成功")
	}
	n.NotifySessionFinal("session-1", "completed", "把 TODO 做完")

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got.SessionID == "session-1"
	}, "未收到 webhook")
	mu.Lock()
	defer mu.Unlock()
	if got.Type != "session_final" || got.Status != "completed" {
		t.Fatalf("报文不符: %+v", got)
	}
	if got.Goal != "把 TODO 做完" || got.OccurredAt == "" {
		t.Fatalf("报文不符: %+v", got)
	}
}

func TestNotifier_FiltersAndNil(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
	}))
	defer srv.Close()

	// 空 URL → nil 通知器（未接线零开销）
	if NewNotifier("", []string{"completed"}) != nil {
		t.Fatal("空 URL 应返 nil")
	}
	// 空事件列表 → nil
	if NewNotifier(srv.URL, []string{}) != nil {
		t.Fatal("空事件列表应返 nil")
	}
	// nil 接收者不 panic
	var n *Notifier
	n.NotifySessionFinal("s", "completed", "g")

	// 不关心的状态零请求
	n2 := NewNotifier(srv.URL, []string{"completed"})
	n2.NotifySessionFinal("session-2", "error", "g")
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Fatalf("不关心的状态不应发请求，got %d", requests)
	}
}

func TestNotifier_DedupWindow(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
	}))
	defer srv.Close()

	n := NewNotifier(srv.URL, []string{"completed", "error"})
	n.NotifySessionFinal("session-3", "completed", "g")
	n.NotifySessionFinal("session-3", "completed", "g") // 60s 窗口内去重
	n.NotifySessionFinal("session-3", "error", "g")     // 不同状态不去重

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return requests == 2
	}, "应发 2 次（60s 窗口内重复通知去重 1 次）")
	time.Sleep(200 * time.Millisecond) // 缓冲：防第 3 个请求迟到
	mu.Lock()
	defer mu.Unlock()
	if requests != 2 {
		t.Fatalf("去重后应稳定在 2 次，got %d", requests)
	}
}

// waitFor 轮询等待 cond 成立；超时 failTest。
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}
