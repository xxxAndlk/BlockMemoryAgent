package agent

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCreateRequestRoundTrip(t *testing.T) {
	req := CreateRequest{
		Goal: "test goal",
		Meta: map[string]any{"key": "value"},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal CreateRequest: %v", err)
	}
	var got CreateRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal CreateRequest: %v", err)
	}
	if got.Goal != req.Goal {
		t.Errorf("Goal = %q, want %q", got.Goal, req.Goal)
	}
	if got.Meta["key"] != "value" {
		t.Errorf("Meta = %v, want %v", got.Meta, req.Meta)
	}
}

func TestResumeRequestRoundTrip(t *testing.T) {
	req := ResumeRequest{CarryOver: "summary", UserInput: "go on"}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal ResumeRequest: %v", err)
	}
	var got ResumeRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal ResumeRequest: %v", err)
	}
	if got.CarryOver != req.CarryOver {
		t.Errorf("CarryOver = %q, want %q", got.CarryOver, req.CarryOver)
	}
	if got.UserInput != req.UserInput {
		t.Errorf("UserInput = %q, want %q", got.UserInput, req.UserInput)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	msg := Message{Role: "user", Content: "hello"}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal Message: %v", err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal Message: %v", err)
	}
	if got.Role != msg.Role {
		t.Errorf("Role = %q, want %q", got.Role, msg.Role)
	}
	if got.Content != msg.Content {
		t.Errorf("Content = %q, want %q", got.Content, msg.Content)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	s := &Session{
		ID:        "session-1",
		Goal:      "goal",
		Status:    "running",
		Result:    "result",
		State:     "state",
		StartedAt: now,
		EndedAt:   now,
		Events: []Event{
			{Type: "system", Agent: "MetaAgent", Message: "start"},
		},
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
		TempDir: "/tmp/session-1",
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal Session: %v", err)
	}
	var got Session
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal Session: %v", err)
	}
	if got.ID != s.ID {
		t.Errorf("ID = %q, want %q", got.ID, s.ID)
	}
	if got.Goal != s.Goal {
		t.Errorf("Goal = %q, want %q", got.Goal, s.Goal)
	}
	if got.Status != s.Status {
		t.Errorf("Status = %q, want %q", got.Status, s.Status)
	}
	if got.Result != s.Result {
		t.Errorf("Result = %q, want %q", got.Result, s.Result)
	}
	if got.State != s.State {
		t.Errorf("State = %q, want %q", got.State, s.State)
	}
	if !got.StartedAt.Equal(s.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, s.StartedAt)
	}
	if !got.EndedAt.Equal(s.EndedAt) {
		t.Errorf("EndedAt = %v, want %v", got.EndedAt, s.EndedAt)
	}
	if len(got.Events) != 1 || got.Events[0].Message != "start" {
		t.Errorf("Events = %v, want %v", got.Events, s.Events)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "hello" {
		t.Errorf("Messages = %v, want %v", got.Messages, s.Messages)
	}
	if got.TempDir != s.TempDir {
		t.Errorf("TempDir = %q, want %q", got.TempDir, s.TempDir)
	}
}

func TestEventRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	e := &Event{
		Type:         "progress",
		Agent:        "agent",
		Message:      "msg",
		Kind:         "think",
		Tool:         "tool",
		ToolPath:     "/path",
		ToolOutput:   "output",
		ToolError:    "error",
		Success:      true,
		Timestamp:    now,
		Prompt:       "prompt",
		InputTokens:  10,
		OutputTokens: 20,
		DetailJSON:   `{"key":"value"}`,
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal Event: %v", err)
	}
	var got Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal Event: %v", err)
	}
	if got.Type != e.Type {
		t.Errorf("Type = %q, want %q", got.Type, e.Type)
	}
	if got.Agent != e.Agent {
		t.Errorf("Agent = %q, want %q", got.Agent, e.Agent)
	}
	if got.Message != e.Message {
		t.Errorf("Message = %q, want %q", got.Message, e.Message)
	}
	if got.Kind != e.Kind {
		t.Errorf("Kind = %q, want %q", got.Kind, e.Kind)
	}
	if got.Tool != e.Tool {
		t.Errorf("Tool = %q, want %q", got.Tool, e.Tool)
	}
	if got.ToolPath != e.ToolPath {
		t.Errorf("ToolPath = %q, want %q", got.ToolPath, e.ToolPath)
	}
	if got.ToolOutput != e.ToolOutput {
		t.Errorf("ToolOutput = %q, want %q", got.ToolOutput, e.ToolOutput)
	}
	if got.ToolError != e.ToolError {
		t.Errorf("ToolError = %q, want %q", got.ToolError, e.ToolError)
	}
	if got.Success != e.Success {
		t.Errorf("Success = %v, want %v", got.Success, e.Success)
	}
	if !got.Timestamp.Equal(e.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, e.Timestamp)
	}
	if got.Prompt != e.Prompt {
		t.Errorf("Prompt = %q, want %q", got.Prompt, e.Prompt)
	}
	if got.InputTokens != e.InputTokens {
		t.Errorf("InputTokens = %d, want %d", got.InputTokens, e.InputTokens)
	}
	if got.OutputTokens != e.OutputTokens {
		t.Errorf("OutputTokens = %d, want %d", got.OutputTokens, e.OutputTokens)
	}
	if got.DetailJSON != e.DetailJSON {
		t.Errorf("DetailJSON = %q, want %q", got.DetailJSON, e.DetailJSON)
	}
}

func TestAgentInstanceRoundTrip(t *testing.T) {
	inst := &AgentInstance{
		Name:     "name",
		Role:     "role",
		ModuleID: "module-1",
		Status:   "running",
	}
	data, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("marshal AgentInstance: %v", err)
	}
	var got AgentInstance
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal AgentInstance: %v", err)
	}
	if got.Name != inst.Name {
		t.Errorf("Name = %q, want %q", got.Name, inst.Name)
	}
	if got.Role != inst.Role {
		t.Errorf("Role = %q, want %q", got.Role, inst.Role)
	}
	if got.ModuleID != inst.ModuleID {
		t.Errorf("ModuleID = %q, want %q", got.ModuleID, inst.ModuleID)
	}
	if got.Status != inst.Status {
		t.Errorf("Status = %q, want %q", got.Status, inst.Status)
	}
}

func TestQueryAndResultRoundTrip(t *testing.T) {
	q := Query{Kind: "state", Args: map[string]any{"k": "v"}}
	data, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal Query: %v", err)
	}
	var gotQ Query
	if err := json.Unmarshal(data, &gotQ); err != nil {
		t.Fatalf("unmarshal Query: %v", err)
	}
	if gotQ.Kind != q.Kind {
		t.Errorf("Query.Kind = %q, want %q", gotQ.Kind, q.Kind)
	}
	if gotQ.Args["k"] != "v" {
		t.Errorf("Query.Args = %v, want %v", gotQ.Args, q.Args)
	}

	r := Result{Data: map[string]any{"x": 1}}
	data, err = json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal Result: %v", err)
	}
	var gotR Result
	if err := json.Unmarshal(data, &gotR); err != nil {
		t.Fatalf("unmarshal Result: %v", err)
	}
	m, ok := gotR.Data.(map[string]any)
	if !ok || m["x"] != float64(1) {
		t.Errorf("Result.Data = %v, want map[x:1]", gotR.Data)
	}
}

func TestControlCommandAndFilterRoundTrip(t *testing.T) {
	cmd := ControlCommand{Op: "cancel", Args: map[string]any{"reason": "timeout"}}
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal ControlCommand: %v", err)
	}
	var gotCmd ControlCommand
	if err := json.Unmarshal(data, &gotCmd); err != nil {
		t.Fatalf("unmarshal ControlCommand: %v", err)
	}
	if gotCmd.Op != cmd.Op {
		t.Errorf("ControlCommand.Op = %q, want %q", gotCmd.Op, cmd.Op)
	}

	f := Filter{Status: "running", Limit: 10}
	data, err = json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal Filter: %v", err)
	}
	var gotF Filter
	if err := json.Unmarshal(data, &gotF); err != nil {
		t.Fatalf("unmarshal Filter: %v", err)
	}
	if gotF.Status != f.Status {
		t.Errorf("Filter.Status = %q, want %q", gotF.Status, f.Status)
	}
	if gotF.Limit != f.Limit {
		t.Errorf("Filter.Limit = %d, want %d", gotF.Limit, f.Limit)
	}
}

func TestErrorsAreDistinctAndNonNil(t *testing.T) {
	if ErrSessionNotFound == nil {
		t.Error("ErrSessionNotFound is nil")
	}
	if ErrSessionFinished == nil {
		t.Error("ErrSessionFinished is nil")
	}
	if ErrQueueFull == nil {
		t.Error("ErrQueueFull is nil")
	}
	if ErrSessionNotFound == ErrSessionFinished {
		t.Error("ErrSessionNotFound and ErrSessionFinished are equal")
	}
	if ErrSessionNotFound == ErrQueueFull {
		t.Error("ErrSessionNotFound and ErrQueueFull are equal")
	}
	if ErrSessionFinished == ErrQueueFull {
		t.Error("ErrSessionFinished and ErrQueueFull are equal")
	}
}
