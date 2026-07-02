// Package logger 提供结构化 JSON 日志，同时写 stderr 与 session_logs 表。
//
// 设计目标：按 session_id 串联 Agent 执行链路，支持按 agent/phase/level 过滤，
// 为 P1-2 的 Agent IO 可视化提供数据源。
package logger

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/blockmemory/agent/backend/internal/store"
)

// LogStore 是会话日志持久化接口（由 *store.PostgresStore 实现）。
type LogStore interface {
	SaveSessionLog(ctx context.Context, rec *store.SessionLogRecord) error
}

// Logger 结构化日志器。
// 每条日志同时输出到 stderr（JSON）与 session_logs 表（若 store 非空）。
type Logger struct {
	slog  *slog.Logger
	store LogStore
	attrs []slog.Attr
}

// New 创建默认 Logger，JSON 输出到 stderr；store 可选。
func New(store LogStore) *Logger {
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	return &Logger{slog: slog.New(h), store: store}
}

// With 返回附加固定字段的新 Logger。
func (l *Logger) With(attrs ...slog.Attr) *Logger {
	merged := make([]slog.Attr, 0, len(l.attrs)+len(attrs))
	merged = append(merged, l.attrs...)
	merged = append(merged, attrs...)
	return &Logger{slog: l.slog, store: l.store, attrs: merged}
}

// WithSession 返回绑定 session_id 的 Logger。
func (l *Logger) WithSession(sessionID string) *Logger {
	return l.With(slog.String("session_id", sessionID))
}

// WithAgent 返回绑定 agent 的 Logger。
func (l *Logger) WithAgent(agent string) *Logger {
	return l.With(slog.String("agent", agent))
}

// WithPhase 返回绑定 phase 的 Logger。
func (l *Logger) WithPhase(phase string) *Logger {
	return l.With(slog.String("phase", phase))
}

// Debug 写 debug 级别日志。
func (l *Logger) Debug(ctx context.Context, msg string, extra ...slog.Attr) {
	l.log(ctx, slog.LevelDebug, msg, nil, extra...)
}

// Info 写 info 级别日志。
func (l *Logger) Info(ctx context.Context, msg string, extra ...slog.Attr) {
	l.log(ctx, slog.LevelInfo, msg, nil, extra...)
}

// Warn 写 warn 级别日志。
func (l *Logger) Warn(ctx context.Context, msg string, extra ...slog.Attr) {
	l.log(ctx, slog.LevelWarn, msg, nil, extra...)
}

// Error 写 error 级别日志。
func (l *Logger) Error(ctx context.Context, msg string, err error, extra ...slog.Attr) {
	if err != nil {
		extra = append(extra, slog.String("error", err.Error()))
	}
	l.log(ctx, slog.LevelError, msg, nil, extra...)
}

// LLMCall 记录一次 LLM 调用（phase=llm_call）。
func (l *Logger) LLMCall(ctx context.Context, agent, model string, prompt, response string, inputTokens, outputTokens, latencyMs int, extra ...slog.Attr) {
	attrs := []slog.Attr{
		slog.String("agent", agent),
		slog.String("phase", "llm_call"),
		slog.String("model", model),
		slog.Int("input_tokens", inputTokens),
		slog.Int("output_tokens", outputTokens),
		slog.Int("latency_ms", latencyMs),
	}
	attrs = append(attrs, extra...)
	l.log(ctx, slog.LevelInfo, "llm_call", &store.SessionLogRecord{
		Agent:        firstNonEmpty(l.attrValue("agent"), agent),
		Phase:        "llm_call",
		Message:      truncate(prompt, 200) + " -> " + truncate(response, 200),
		Prompt:       prompt,
		Response:     response,
		Model:        model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		LatencyMs:    latencyMs,
	}, attrs...)
}

// Event 记录一次 Agent 关键事件（如 routing/task_split/memory_recall）。
func (l *Logger) Event(ctx context.Context, phase, message string, meta map[string]any, extra ...slog.Attr) {
	attrs := []slog.Attr{slog.String("phase", phase)}
	if meta != nil {
		for k, v := range meta {
			attrs = append(attrs, slog.Any(k, v))
		}
	}
	attrs = append(attrs, extra...)
	l.log(ctx, slog.LevelInfo, message, &store.SessionLogRecord{
		Phase:   phase,
		Message: message,
		Meta:    meta,
	}, attrs...)
}

// log 是底层日志入口。
func (l *Logger) log(ctx context.Context, level slog.Level, msg string, record *store.SessionLogRecord, extra ...slog.Attr) {
	attrs := append(l.attrs, extra...)
	l.slog.LogAttrs(ctx, level, msg, attrs...)

	if l.store == nil {
		return
	}
	sessionID := l.attrValue("session_id")
	if sessionID == "" {
		// 无 session_id 时不持久化，避免脏数据
		return
	}
	if record == nil {
		record = &store.SessionLogRecord{}
	}
	record.SessionID = sessionID
	record.Level = level.String()
	record.Message = msg
	record.CreatedAt = time.Now()
	if record.Agent == "" {
		record.Agent = l.attrValue("agent")
	}
	if record.Phase == "" {
		record.Phase = l.attrValue("phase")
	}
	// 异步写表避免阻塞主路径；失败仅 stderr 已由上面输出
	go func(r *store.SessionLogRecord) {
		bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = l.store.SaveSessionLog(bgCtx, r)
	}(record)
}

// attrValue 读取固定字段值。
func (l *Logger) attrValue(key string) string {
	for _, a := range l.attrs {
		if a.Key == key {
			return a.Value.String()
		}
	}
	return ""
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// truncate 截断字符串并加省略号。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
