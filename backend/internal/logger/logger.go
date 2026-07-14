// Package logger 提供结构化 JSON 日志，同时写 stderr 与 session_logs 表。
//
// 设计目标：按 session_id 串联 Agent 执行链路，支持按 agent/phase/level 过滤，
// 为 P1-2 的 Agent IO 可视化提供数据源。
package logger

import (
	"context"      // 上下文，用于数据库写入超时控制
	"io"           // io.Writer 接口
	"log/slog"     // 标准结构化日志
	"os"           // os.Stderr
	"time"         // 时间戳
	"unicode/utf8" // 按 rune 截断字符串

	"github.com/blockmemory/agent/backend/internal/store"
)

// oldLogStore 是历史接口，底层 store 使用 *store.SessionLogRecord。保留它以兼容现有调用方。
type oldLogStore interface {
	SaveSessionLog(ctx context.Context, rec *store.SessionLogRecord) error
}

// Logger 结构化日志器。
// 每条日志同时输出到 stderr（JSON）与 session_logs 表（若 store 非空）。
type Logger struct {
	slog  *slog.Logger      // 底层 slog 实例，输出 JSON 到 writer
	store LogStore          // 持久化存储接口
	batch *BatchingLogStore // 批量异步写入工作池
	attrs []slog.Attr       // 通过 With 绑定的固定字段
}

// New 创建默认 Logger，JSON 输出到 stderr；store 可选。
// store 可以是 logger.LogStore 或历史接口 *store.PostgresStore/fake store。
// 如果传入 fileWriter，则 JSON 日志输出到该 writer 而不是 stderr。
func New(store any, fileWriter ...io.Writer) *Logger {
	// 默认写入目标是标准错误；若调用方提供 writer 则使用第一个非 nil writer。
	var w io.Writer = os.Stderr
	if len(fileWriter) > 0 && fileWriter[0] != nil {
		w = fileWriter[0]
	}
	return NewWithWriter(store, w)
}

// NewWithWriter 创建 Logger 并指定 JSON 日志输出目标；store 可选。
// TUI 入口可用此函数把结构化日志重定向到文件，避免刷到终端顶乱布局。
func NewWithWriter(store any, w io.Writer) *Logger {
	// 创建 JSON handler，日志级别为 Info。
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	l := &Logger{slog: slog.New(h)}

	// 若 store 非 nil，则适配并启动批量写入工作池。
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
	// 已是目标接口，直接返回。
	if ls, ok := s.(LogStore); ok {
		return ls
	}
	// 兼容旧接口：包装为 storeAdapter。
	if ls, ok := s.(oldLogStore); ok {
		return &storeAdapter{underlying: ls}
	}
	return nil
}

// storeAdapter 将 *store.SessionLogRecord 接口适配为 logger.LogStore。
type storeAdapter struct {
	underlying oldLogStore // 底层旧接口
}

// SaveSessionLog 实现 LogStore，将 logger 的记录转换为底层 store 记录后保存。
func (a *storeAdapter) SaveSessionLog(ctx context.Context, rec *SessionLogRecord) error {
	return a.underlying.SaveSessionLog(ctx, toStoreRecord(rec))
}

// toStoreRecord 在 logger 记录与底层 store 记录之间做字段拷贝。
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
	// 合并旧字段与新字段，保证顺序：旧字段在前，新字段追加。
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
	// 若 err 非 nil，追加 error 字段到 extra。
	if err != nil {
		extra = append(extra, slog.String("error", err.Error()))
	}
	l.log(ctx, slog.LevelError, msg, nil, extra...)
}

// LLMCall 记录一次 LLM 调用（phase=llm_call）。
func (l *Logger) LLMCall(ctx context.Context, rec LLMCallRecord, extra ...slog.Attr) {
	// 构造固定属性列表。
	attrs := []slog.Attr{
		slog.String("agent", rec.Agent),
		slog.String("phase", "llm_call"),
		slog.String("model", rec.Model),
		slog.Int("input_tokens", rec.InputTokens),
		slog.Int("output_tokens", rec.OutputTokens),
		slog.Int("latency_ms", rec.LatencyMs),
	}
	attrs = append(attrs, extra...)

	// 摘要：截断 prompt/response 各 200 rune，避免日志过大。
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
	// 将 meta 中的键值对展开为 slog 属性。
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
	// 合并 With 字段与本次额外字段，输出 JSON 到 writer。
	attrs := append(l.attrs, extra...)
	l.slog.LogAttrs(ctx, level, msg, attrs...)

	// 无批量存储时直接返回，不持久化。
	if l.batch == nil {
		return
	}
	// 从固定字段中读取 session_id；无 session_id 时不持久化，避免脏数据。
	sessionID := l.attrValue("session_id")
	if sessionID == "" {
		return
	}
	// 若调用方未提供 record，创建一个空记录用于填充公共字段。
	if record == nil {
		record = &SessionLogRecord{}
	}
	record.SessionID = sessionID
	record.Level = level.String()
	// 若持久化记录缺少 Message，使用本次日志消息填充。
	if record.Message == "" {
		record.Message = msg
	}
	// 填充时间戳字段。
	now := time.Now()
	record.CreatedAt = now
	record.Timestamp = now.UnixMilli()
	// 若持久化记录缺少 Agent/Phase，从 With 字段中回退。
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
	// 遍历 attrs，找到匹配的 key 后返回其字符串值。
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
	// 长度未超过限制时原样返回。
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	// 转换为 rune 切片后截断，再加省略号。
	runes := []rune(s)
	return string(runes[:maxRunes]) + "..."
}
