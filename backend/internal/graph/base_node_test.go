package graph

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/store"
)

// fakeLogStore 是用于测试的内存会话日志存储。
type fakeLogStore struct {
	records []*store.SessionLogRecord
}

func (f *fakeLogStore) SaveSessionLog(ctx context.Context, rec *store.SessionLogRecord) error {
	f.records = append(f.records, rec)
	return nil
}

func TestBaseAgentNode_Setters(t *testing.T) {
	b := &BaseAgentNode{}

	reg := &RoleRegistry{}
	fac := &RoleFactory{}
	mf := &model.ModelFactory{}
	pc := ProgressCallback(func(context.Context, ProgressEvent) {})
	rt := &runtime.Runtime{}
	tc := ToolCallback(func(*ToolResult) {})

	b.SetRegistry(reg)
	b.SetFactory(fac)
	b.SetModelFactory(mf)
	b.SetProgressCallback(pc)
	b.SetRuntime(rt)
	b.SetToolCallback(tc)

	if b.registry != reg {
		t.Errorf("registry not set")
	}
	if b.factory != fac {
		t.Errorf("factory not set")
	}
	if b.modelFactory != mf {
		t.Errorf("modelFactory not set")
	}
	if b.progress == nil {
		t.Errorf("progress not set")
	}
	if b.rt != rt {
		t.Errorf("runtime not set")
	}
	if b.toolCallback == nil {
		t.Errorf("toolCallback not set")
	}
}

func TestBaseAgentNode_emit(t *testing.T) {
	var got *ProgressEvent
	b := BaseAgentNode{
		agentLabel: func() string { return "MetaAgent" },
		progress: func(ctx context.Context, ev ProgressEvent) {
			got = &ev
		},
	}

	ctx := WithSessionID(context.Background(), "s1")
	b.emit(ctx, "think", "hello")

	if got == nil {
		t.Fatalf("expected progress event")
	}
	if got.SessionID != "s1" {
		t.Errorf("sessionID = %q, want s1", got.SessionID)
	}
	if got.Kind != "think" {
		t.Errorf("kind = %q, want think", got.Kind)
	}
	if got.Agent != "MetaAgent" {
		t.Errorf("agent = %q, want MetaAgent", got.Agent)
	}
	if got.Message != "hello" {
		t.Errorf("message = %q, want hello", got.Message)
	}
}

func TestBaseAgentNode_emitDetail(t *testing.T) {
	var got *ProgressEvent
	b := BaseAgentNode{
		agentLabel: func() string { return "DomainAgent[db]" },
		progress: func(ctx context.Context, ev ProgressEvent) {
			got = &ev
		},
	}

	ctx := WithSessionID(context.Background(), "s2")
	b.emitDetail(ctx, "prompt", "summary", "full prompt")

	if got == nil {
		t.Fatalf("expected progress event")
	}
	if got.Agent != "DomainAgent[db]" {
		t.Errorf("agent = %q, want DomainAgent[db]", got.Agent)
	}
	if got.Detail != "full prompt" {
		t.Errorf("detail = %q, want full prompt", got.Detail)
	}
}

func TestBaseAgentNode_emitNoCallback(t *testing.T) {
	b := BaseAgentNode{agentLabel: func() string { return "MetaAgent" }}
	// 不应 panic，也不应有副作用。
	b.emit(context.Background(), "think", "ignored")
}

func TestBaseAgentNode_sessionLoggerNoSession(t *testing.T) {
	log := logger.NewWithWriter(nil, io.Discard)
	b := BaseAgentNode{
		logger:     log,
		agentLabel: func() string { return "MetaAgent" },
	}

	got := b.sessionLogger(context.Background())
	if got == nil {
		t.Fatalf("expected non-nil logger")
	}
}

func TestBaseAgentNode_sessionLoggerWithSession(t *testing.T) {
	log := logger.NewWithWriter(nil, io.Discard)
	b := BaseAgentNode{
		logger:     log,
		agentLabel: func() string { return "DomainAgent[db]" },
	}

	ctx := WithSessionID(context.Background(), "s3")
	got := b.sessionLogger(ctx)
	if got == nil {
		t.Fatalf("expected non-nil logger")
	}
}

func TestBaseAgentNode_SetLoggerRecordCallback(t *testing.T) {
	fs := &fakeLogStore{}
	log := logger.NewWithWriter(fs, io.Discard)
	b := newBaseAgentNode()
	b.agentLabel = func() string { return "MetaAgent" }
	b.SetLogger(log)

	ctx := WithSessionID(context.Background(), "s4")
	b.llmTracker.RecordCall(ctx, 10*time.Millisecond, nil, "MetaAgent", "summary", "prompt", "response", 7, 3, false)

	var rec *store.SessionLogRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fs.records) > 0 {
			rec = fs.records[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if rec == nil {
		t.Fatalf("expected session log record")
	}
	if rec.SessionID != "s4" {
		t.Errorf("sessionID = %q, want s4", rec.SessionID)
	}
	if rec.Agent != "MetaAgent" {
		t.Errorf("agent = %q, want MetaAgent", rec.Agent)
	}
	if rec.Phase != "llm_call" {
		t.Errorf("phase = %q, want llm_call", rec.Phase)
	}
	if rec.Meta == nil {
		t.Fatalf("expected meta fields")
	}
	if rec.Meta["input_tokens"] != 7 {
		t.Errorf("input_tokens = %v, want 7", rec.Meta["input_tokens"])
	}
	if rec.Meta["output_tokens"] != 3 {
		t.Errorf("output_tokens = %v, want 3", rec.Meta["output_tokens"])
	}
}
