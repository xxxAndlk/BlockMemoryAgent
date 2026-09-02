package store

import (
	"encoding/json" // JSON 往返验证
	"testing"       // Go 测试框架
	"time"          // 构造时间戳
)

// TestSessionHistoryRecord_WorkDirRoundTrip 编译闸门 + JSON 往返：
// 验证 SessionHistoryRecord.WorkDir 字段存在且随 JSON 序列化保留
// (供 Web API 响应透传)。work_dir 列的 schema/列清单/Scan 对齐正确性
// 由 go build(字段缺失即编译失败)与 test/api 集成测试兜底——
// 本包单测环境无真实 PG(见 store_test.go fake 驱动约定)。
func TestSessionHistoryRecord_WorkDirRoundTrip(t *testing.T) {
	rec := SessionHistoryRecord{
		SessionID: "session-1",
		Goal:      "g",
		Summary:   "s",
		WorkDir:   `D:\ws\proj`,
		CreatedAt: time.Now(),
	}
	// 序列化后必须包含 work_dir 键
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back SessionHistoryRecord
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.WorkDir != rec.WorkDir {
		t.Fatalf("WorkDir 往返丢失: want %q, got %q", rec.WorkDir, back.WorkDir)
	}
	// 空串(回落进程默认)语义也应保留
	empty := SessionHistoryRecord{}
	data, err = json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	var backEmpty SessionHistoryRecord
	if err := json.Unmarshal(data, &backEmpty); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if backEmpty.WorkDir != "" {
		t.Fatalf("空 WorkDir 往返后应为空串, got %q", backEmpty.WorkDir)
	}
}
