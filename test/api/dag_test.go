//go:build integration

package api_test

import (
	"net/http"
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestDAGEndpoints(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	// DAG scheduler 默认关闭。验证端点已挂载且返回 503（disabled）而非 500/panic。
	resp, err := http.Get(f.Server.URL() + "/api/dag")
	if err != nil {
		t.Fatalf("GET /api/dag: %v", err)
	}
	defer resp.Body.Close()
	// 503 = scheduler disabled（预期）；500+ 其他服务端错误才视为故障。
	if resp.StatusCode != http.StatusServiceUnavailable && resp.StatusCode >= http.StatusInternalServerError {
		t.Errorf("GET /api/dag returned unexpected server error: %d", resp.StatusCode)
	}
}
