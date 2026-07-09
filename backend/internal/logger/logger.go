// Package logger 提供结构化 JSON 日志，同时写 stderr 与 session_logs 表。
//
// 设计目标：按 session_id 串联 Agent 执行链路，支持按 agent/phase/level 过滤，
// 为 P1-2 的 Agent IO 可视化提供数据源。
package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/store"
)

// oldLogStore 是历史接口，底层 store 使用 *store.SessionLogRecord。保留它以兼容现有调用方。
type oldLogStore interface {
	SaveSessionLog(ctx context.Context, rec *store.SessionLogRecord) error
}

// Logger 结构化日志器。
// 每条日志同时输出到 stderr（JSON）与 session_logs 表（若 store 非空）。
type Logger struct {
	slog  *slog.Logger
	store LogStore
	batch *BatchingLogStore
	attrs []slog.Attr
}

// New 创建默认 Logger，JSON 输出到 stderr；store 可选。
// store 可以是 logger.LogStore 或历史接口 *store.PostgresStore/fake store。
// 如果传入 fileWriter，则 JSON 日志输出到该 writer 而不是 stderr。
func New(store any, fileWriter ...io.Writer) *Logger {
	var w io.Writer = os.Stderr
	if len(fileWriter) > 0 && fileWriter[0] != nil {
		w = fileWriter[0]
	}
	return NewWithWriter(store, w)
}

// NewWithWriter 创建 Logger 并指定 JSON 日志输出目标；store 可选。
// TUI 入口可用此函数把结构化日志重定向到文件，避免刷到终端顶乱布局。
func NewWithWriter(store any, w io.Writer) *Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	l := &Logger{slog: slog.New(h)}

	if store != nil {
		l.store = adaptStore(store)
		if l.store != nil {
			l.batch = NewBatchingLogStore(l.store, 4, 1024)
		}
	}
	return l
}

// adaptStore 将传入的 store 适配为 logger.LogStore。
func adaptStore(s any) LogStore {
	if ls, ok := s.(LogStore); ok {
		return ls
	}
	if ls, ok := s.(oldLogStore); ok {
		return &storeAdapter{underlying: ls}
	}
	return nil
}

// storeAdapter 将 *store.SessionLogRecord 接口适配为 logger.LogStore。
type storeAdapter struct {
	underlying oldLogStore
}

func (a *storeAdapter) SaveSessionLog(ctx context.Context, rec *SessionLogRecord) error {
	return a.underlying.SaveSessionLog(ctx, toStoreRecord(rec))
}

func toStoreRecord(rec *SessionLogRecord) *store.SessionLogRecord {
	if rec == nil {
		return nil
	}
	return &store.SessionLogRecord{
		SessionID:    rec.SessionID,
		Agent:        rec.Agent,
		Level:        rec.Level,
		Phase:        rec.Phase,
		Message:      rec.Message,
		Prompt:       rec.Prompt,
		Response:     rec.Response,
		InputTokens:  rec.InputTokens,
		OutputTokens: rec.OutputTokens,
		Model:        rec.Model,
		LatencyMs:    rec.LatencyMs,
		CreatedAt:    rec.CreatedAt,
		Meta:         rec.Meta,
	}
}

// With 返回附加固定字段的新 Logger。
func (l *Logger) With(attrs ...slog.Attr) *Logger {
	merged := make([]slog.Attr, 0, len(l.attrs)+len(attrs))
	merged = append(merged, l.attrs...)
	merged = append(merged, attrs...)
	return &Logger{slog: l.slog, store: l.store, batch: l.batch, attrs: merged}
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
func (l *Logger) LLMCall(ctx context.Context, rec LLMCallRecord, extra ...slog.Attr) {
	attrs := []slog.Attr{
		slog.String("agent", rec.Agent),
		slog.String("phase", "llm_call"),
		slog.String("model", rec.Model),
		slog.Int("input_tokens", rec.InputTokens),
		slog.Int("output_tokens", rec.OutputTokens),
		slog.Int("latency_ms", rec.LatencyMs),
	}
	attrs = append(attrs, extra...)

	summary := truncate(rec.Prompt, 200) + " -> " + truncate(rec.Response, 200)
	l.log(ctx, slog.LevelInfo, summary, &SessionLogRecord{
		Agent:        firstNonEmpty(l.attrValue("agent"), rec.Agent),
		Phase:        "llm_call",
		Message:      summary,
		Prompt:       rec.Prompt,
		Response:     rec.Response,
		Model:        rec.Model,
		InputTokens:  rec.InputTokens,
		OutputTokens: rec.OutputTokens,
		LatencyMs:    rec.LatencyMs,
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
	l.log(ctx, slog.LevelInfo, message, &SessionLogRecord{
		Phase:   phase,
		Message: message,
		Meta:    meta,
	}, attrs...)
}

// log 是底层日志入口。
func (l *Logger) log(ctx context.Context, level slog.Level, msg string, record *SessionLogRecord, extra ...slog.Attr) {
	attrs := append(l.attrs, extra...)
	l.slog.LogAttrs(ctx, level, msg, attrs...)

	if l.batch == nil {
		return
	}
	sessionID := l.attrValue("session_id")
	if sessionID == "" {
		// 无 session_id 时不持久化，避免脏数据
		return
	}
	if record == nil {
		record = &SessionLogRecord{}
	}
	record.SessionID = sessionID
	record.Level = level.String()
	if record.Message == "" {
		record.Message = msg
	}
	now := time.Now()
	record.CreatedAt = now
	record.Timestamp = now.UnixMilli()
	if record.Agent == "" {
		record.Agent = l.attrValue("agent")
	}
	if record.Phase == "" {
		record.Phase = l.attrValue("phase")
	}

	// 使用有界工作池异步写入，避免每条日志都启动 goroutine。
	if err := l.batch.Enqueue(ctx, record); err != nil {
		l.slog.Error("enqueue session log failed",
			slog.String("error", err.Error()),
			slog.String("session_id", record.SessionID),
			slog.String("phase", record.Phase))
	}
}

// Close 停止日志器的工作池，等待所有待处理日志落盘。
func (l *Logger) Close() error {
	if l.batch != nil {
		l.batch.Stop()
	}
	return nil
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

// truncate 按 rune 截断字符串并加省略号，避免破坏多字节 UTF-8 字符。
func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "..."
}
