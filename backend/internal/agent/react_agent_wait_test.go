package agent

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakeWaitPendingChecker 第一次 PendingChildren 返回 1（进入等待），第二次归 0 退出循环。
// 与 react_agent_test.go 的 fakePendingChecker 区分：本桩不需要 mailbox 即可退出等待，
// 专测"进入等待时是否上报 child_wait"这一点。
type fakeWaitPendingChecker struct{ calls int }

func (f *fakeWaitPendingChecker) PendingChildren(string) int {
	if f.calls == 0 {
		f.calls++
		return 1
	}
	return 0
}
func (f *fakeWaitPendingChecker) WaitForAnyChild(string, time.Duration) bool { return true }

// TestWaitForChildrenReportsChildWait 验证 waitForChildren 进入等待时上报 child_wait
// 展示态（编排页"等待下级返回"标识的数据源）。
func TestWaitForChildrenReportsChildWait(t *testing.T) {
	var kinds []string
	a := NewReActAgent("session-1/domain-1", types.RoleDefinition{ID: "domain"}, nil, nil).
		WithPendingChildrenChecker(&fakeWaitPendingChecker{}).
		WithActivityReporter(func(k string) { kinds = append(kinds, k) })
	_, paused := a.waitForChildren(context.Background(), nil)
	if paused {
		t.Fatal("无 Paused 子节点时不应返回 paused=true")
	}
	found := false
	for _, k := range kinds {
		if k == "child_wait" {
			found = true
		}
	}
	if !found {
		t.Fatalf("waitForChildren 未上报 child_wait: %v", kinds)
	}
}
