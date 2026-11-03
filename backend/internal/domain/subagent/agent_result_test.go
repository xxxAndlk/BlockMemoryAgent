package subagent

import "testing"

// TestBuildAgentResult B-09：终态结果信封状态映射与稳定 result_id。
func TestBuildAgentResult(t *testing.T) {
	d := &Dispatcher{}
	cases := []struct {
		name       string
		summary    string
		wantStatus string
		wantVerify string
	}{
		{"completed", "done: changed 2 files", "completed", "passed"},
		{"failed", "[failure kind=timeout retryable=true] timed out", "failed", "unknown"},
		{"unverified", "交付完成（未验证：缺 L0 证据）", "unverified", "unverified"},
	}
	for _, c := range cases {
		res := d.buildAgentResult("s1", "s1/code-1", c.summary, []string{"a.go"})
		if res.Status != c.wantStatus || res.Verification != c.wantVerify {
			t.Fatalf("%s: status=%q verify=%q, want %q/%q", c.name, res.Status, res.Verification, c.wantStatus, c.wantVerify)
		}
		if res.ResultID == "" || res.Version != AgentResultVersion {
			t.Fatalf("%s: bad envelope: %+v", c.name, res)
		}
		if len(res.FilesModified) != 1 || res.FilesModified[0] != "a.go" {
			t.Fatalf("%s: files not carried: %v", c.name, res.FilesModified)
		}
	}
	// 同摘要稳定 result_id，不同摘要不同。
	a := d.buildAgentResult("s1", "s1/code-1", "x", nil)
	b := d.buildAgentResult("s1", "s1/code-1", "x", nil)
	c := d.buildAgentResult("s1", "s1/code-1", "y", nil)
	if a.ResultID != b.ResultID || a.ResultID == c.ResultID {
		t.Fatalf("result_id stability broken: %q %q %q", a.ResultID, b.ResultID, c.ResultID)
	}
}
