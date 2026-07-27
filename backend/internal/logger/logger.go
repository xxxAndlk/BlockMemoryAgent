// Package logger 提供结构化日志，同时写 stderr/文件与 session_logs 表。
//
// 设计目标：按 session_id 串联 Agent 执行链路，支持按 agent/phase/level 过滤，
// 为 P1-2 的 Agent IO 可视化提供数据源。
package logger

import (
	"context"      // 上下文，用于数据库写入超时控制
	"fmt"          // 格式化错误与栈信息
	"io"           // io.Writer 接口
	"log/slog"     // 标准结构化日志 Attr 类型，保留以兼容现有调用方
	"os"           // os.Stderr
	"runtime"      // 捕获调用栈
	"strings"      // 字符串处理
	"time"         // 时间戳
	"unicode/utf8" // 按 rune 截断字符串

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/store"
)

func init() {
	// 让 zerolog 在错误对象携带 stack trace 时能够正确格式化。
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack
}

// oldLogStore 是历史接口，底层 store 使用 *store.SessionLogRecord。保留它以兼容现有调用方。
type oldLogStore interface {
	SaveSessionLog(ctx context.Context, rec *store.SessionLogRecord) error
}

// Logger 结构化日志器。
// 每条日志同时输出到 writer（console 纯文本或 JSON）与 session_logs 表（若 store 非空）。
type Logger struct {
	zl           zerolog.Logger // 底层 zerolog 实例
	store        LogStore       // 持久化存储接口
	batch        *BatchingLogStore
	fields       map[string]string // 通过 With 绑定的固定字段
	stackEnabled bool              // error 级别是否附加调用栈
}

// New 使用默认配置创建 Logger；store 可选，fileWriter 可选（默认 os.Stderr）。
func New(store any, fileWriter ...io.Writer) *Logger {
	return NewWithConfig(defaultLoggingConfig(), store, fileWriter...)
}

// NewWithWriter 使用默认配置创建 Logger 并指定 writer；store 可选。
// TUI 入口可用此函数把日志重定向到文件，避免刷到终端顶乱布局。
func NewWithWriter(store any, w io.Writer) *Logger {
	return newLogger(defaultLoggingConfig(), store, w)
}

// NewWithConfig 使用指定配置创建 Logger；store 可选，fileWriter 可选（默认 os.Stderr）。
func NewWithConfig(cfg config.LoggingConfig, store any, fileWriter ...io.Writer) *Logger {
	var w io.Writer = os.Stderr
	if len(fileWriter) > 0 && fileWriter[0] != nil {
		w = fileWriter[0]
	}
	return newLogger(cfg, store, w)
}

// defaultLoggingConfig 返回与旧行为兼容的默认日志配置。
func defaultLoggingConfig() config.LoggingConfig {
	t := true
	return config.LoggingConfig{
		Format:       "console",
		Level:        "info",
		Timezone:     "Local",
		StackEnabled: &t,
		NoColor:      &t,
	}
}

// newLogger 根据配置创建 zerolog 实例并装配存储。
func newLogger(cfg config.LoggingConfig, store any, w io.Writer) *Logger {
	lvl := parseLevel(cfg.Level)
	writer := buildWriter(cfg, w)
	zl := zerolog.New(writer).Level(lvl).With().Timestamp().Logger()

	l := &Logger{
		zl:           zl,
		stackEnabled: cfg.StackEnabled == nil || *cfg.StackEnabled,
		fields:       make(map[string]string),
	}

	if store != nil {
		l.store = adaptStore(store)
		if l.store != nil {
			l.batch = NewBatchingLogStore(l.store, 4, 1024)
		}
	}
	return l
}

// buildWriter 根据 format 构造 zerolog 输出 writer。
func buildWriter(cfg config.LoggingConfig, w io.Writer) io.Writer {
	if cfg.Format == "json" {
		return w
	}

	loc := parseTimezone(cfg.Timezone)
	noColor := cfg.NoColor == nil || *cfg.NoColor
	return zerolog.ConsoleWriter{
		Out:        w,
		NoColor:    noColor,
		TimeFormat: "2006-01-02T15:04:05.000-07:00",
		FormatTimestamp: func(i interface{}) string {
			switch t := i.(type) {
			case time.Time:
				return t.In(loc).Format("2006-01-02T15:04:05.000-07:00")
			case string:
				tt, err := time.Parse(time.RFC3339Nano, t)
				if err != nil {
					return t
				}
				return tt.In(loc).Format("2006-01-02T15:04:05.000-07:00")
			default:
				return fmt.Sprintf("%s", i)
			}
		},
		FormatLevel: func(i interface{}) string {
			level := strings.ToUpper(fmt.Sprintf("%s", i))
			switch level {
			case "TRACE":
				return "[TRAC]"
			case "DEBUG":
				return "[DEBU]"
			case "INFO":
				return "[INFO]"
			case "WARN", "WARNING":
				return "[WARN]"
			case "ERROR":
				return "[ERRO]"
			case "FATAL":
				return "[FATA]"
			case "PANIC":
				return "[PANIC]"
			default:
				return "[" + level + "]"
			}
		},
	}
}

// parseLevel 把配置字符串解析为 zerolog 级别。
func parseLevel(level string) zerolog.Level {
	switch strings.ToLower(level) {
	case "trace":
		return zerolog.TraceLevel
	case "debug":
		return zerolog.DebugLevel
	case "warn", "warning":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	case "fatal":
		return zerolog.FatalLevel
	case "panic":
		return zerolog.PanicLevel
	default:
		return zerolog.InfoLevel
	}
}

// parseTimezone 解析时区配置，无效时回退到 Local。
func parseTimezone(tz string) *time.Location {
	switch strings.ToLower(tz) {
	case "", "local":
		return time.Local
	case "utc":
		return time.UTC
	default:
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return time.Local
		}
		return loc
	}
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
	newFields := make(map[string]string, len(l.fields)+len(attrs))
	for k, v := range l.fields {
		newFields[k] = v
	}
	zctx := l.zl.With()
	for _, attr := range attrs {
		val := attr.Value.String()
		newFields[attr.Key] = val
		zctx = zctx.Str(attr.Key, val)
	}
	return &Logger{
		zl:           zctx.Logger(),
		store:        l.store,
		batch:        l.batch,
		fields:       newFields,
		stackEnabled: l.stackEnabled,
	}
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
	l.log(ctx, zerolog.DebugLevel, msg, nil, extra...)
}

// Info 写 info 级别日志。
func (l *Logger) Info(ctx context.Context, msg string, extra ...slog.Attr) {
	l.log(ctx, zerolog.InfoLevel, msg, nil, extra...)
}

// Warn 写 warn 级别日志。
func (l *Logger) Warn(ctx context.Context, msg string, extra ...slog.Attr) {
	l.log(ctx, zerolog.WarnLevel, msg, nil, extra...)
}

// Error 写 error 级别日志，可选附加调用栈。
func (l *Logger) Error(ctx context.Context, msg string, err error, extra ...slog.Attr) {
	fullMsg := msg
	if err != nil {
		fullMsg = fmt.Sprintf("%s: %v", msg, err)
	}
	if l.stackEnabled {
		fullMsg += "\n" + captureStack(2)
	}
	l.log(ctx, zerolog.ErrorLevel, fullMsg, &SessionLogRecord{Message: msg}, extra...)
}

// LLMCall 记录一次 LLM 调用（phase=llm_call）。
// 控制台与 session_logs 均保留完整 prompt/response，不截断，便于排查 LLM I/O 问题。
func (l *Logger) LLMCall(ctx context.Context, rec LLMCallRecord, extra ...slog.Attr) {
	summary := rec.Prompt + " -> " + rec.Response

	event := l.zl.Info()
	if event == nil {
		return
	}
	event = event.Str("agent", rec.Agent).
		Str("phase", "llm_call").
		Str("model", rec.Model).
		Int("input_tokens", rec.InputTokens).
		Int("output_tokens", rec.OutputTokens).
		Int("latency_ms", rec.LatencyMs)
	for _, attr := range extra {
		event = attrToEvent(event, attr)
	}
	event.Msg(summary)

	if l.batch == nil {
		return
	}
	sessionID := l.fields["session_id"]
	if sessionID == "" {
		return
	}
	now := time.Now()
	l.enqueue(ctx, &SessionLogRecord{
		SessionID:    sessionID,
		Agent:        firstNonEmpty(l.fields["agent"], rec.Agent),
		Phase:        "llm_call",
		Message:      summary,
		Prompt:       rec.Prompt,
		Response:     rec.Response,
		Model:        rec.Model,
		InputTokens:  rec.InputTokens,
		OutputTokens: rec.OutputTokens,
		LatencyMs:    rec.LatencyMs,
		CreatedAt:    now,
		Timestamp:    now.UnixMilli(),
	})
}

// Event 记录一次 Agent 关键事件（如 routing/task_split/memory_recall）。
func (l *Logger) Event(ctx context.Context, phase, message string, meta map[string]any, extra ...slog.Attr) {
	event := l.zl.Info()
	if event == nil {
		return
	}
	event = event.Str("phase", phase)
	if meta != nil {
		event = event.Fields(meta)
	}
	for _, attr := range extra {
		event = attrToEvent(event, attr)
	}
	event.Msg(message)

	if l.batch == nil {
		return
	}
	sessionID := l.fields["session_id"]
	if sessionID == "" {
		return
	}
	now := time.Now()
	l.enqueue(ctx, &SessionLogRecord{
		SessionID: sessionID,
		Agent:     l.fields["agent"],
		Phase:     phase,
		Message:   message,
		Meta:      meta,
		CreatedAt: now,
		Timestamp: now.UnixMilli(),
	})
}

// log 是底层日志入口。
func (l *Logger) log(ctx context.Context, level zerolog.Level, msg string, record *SessionLogRecord, extra ...slog.Attr) {
	event := l.zl.WithLevel(level)
	if event == nil {
		return
	}
	for _, attr := range extra {
		event = attrToEvent(event, attr)
	}
	event.Msg(msg)

	if l.batch == nil {
		return
	}
	sessionID := l.fields["session_id"]
	if sessionID == "" {
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
		record.Agent = l.fields["agent"]
	}
	if record.Phase == "" {
		record.Phase = l.fields["phase"]
	}
	l.enqueue(ctx, record)
}

// enqueue 把记录推入批量写入工作池。
func (l *Logger) enqueue(ctx context.Context, record *SessionLogRecord) {
	if err := l.batch.Enqueue(ctx, record); err != nil {
		l.zl.Error().Err(err).Str("session_id", record.SessionID).Str("phase", record.Phase).Msg("enqueue session log failed")
	}
}

// Close 停止日志器的工作池，等待所有待处理日志落盘。
func (l *Logger) Close() error {
	if l.batch != nil {
		l.batch.Stop()
	}
	return nil
}

// StdLogWriter 返回一个 io.Writer，可用于 log.SetOutput，
// 把标准库 log 的输出按 INFO 级别转发到本 logger，保证全系统日志格式统一。
func (l *Logger) StdLogWriter() io.Writer {
	return &stdLogForwarder{zl: l.zl}
}

// stdLogForwarder 把标准库 log 的文本行转发给 zerolog。
type stdLogForwarder struct {
	zl zerolog.Logger
}

// Write 实现 io.Writer。
func (f *stdLogForwarder) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	f.zl.Info().Msg(msg)
	return len(p), nil
}

// attrValue 读取固定字段值。
func (l *Logger) attrValue(key string) string {
	return l.fields[key]
}

// attrToEvent 把 slog.Attr 转换为 zerolog 事件字段。
func attrToEvent(event *zerolog.Event, attr slog.Attr) *zerolog.Event {
	if event == nil {
		return nil
	}
	switch attr.Value.Kind() {
	case slog.KindBool:
		return event.Bool(attr.Key, attr.Value.Bool())
	case slog.KindDuration:
		return event.Dur(attr.Key, attr.Value.Duration())
	case slog.KindFloat64:
		return event.Float64(attr.Key, attr.Value.Float64())
	case slog.KindInt64:
		return event.Int64(attr.Key, attr.Value.Int64())
	case slog.KindString:
		return event.Str(attr.Key, attr.Value.String())
	case slog.KindTime:
		return event.Time(attr.Key, attr.Value.Time())
	case slog.KindUint64:
		return event.Uint64(attr.Key, attr.Value.Uint64())
	default:
		return event.Interface(attr.Key, attr.Value.Any())
	}
}

// captureStack 捕获当前 goroutine 的调用栈，skip 指定跳过的帧数。
func captureStack(skip int) string {
	const depth = 32
	pcs := make([]uintptr, depth)
	n := runtime.Callers(skip+1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	b.WriteString("Stack:")
	idx := 1
	for {
		frame, more := frames.Next()
		b.WriteString(fmt.Sprintf("\n%d.  %s\n    %s:%d", idx, frame.Function, frame.File, frame.Line))
		idx++
		if !more {
			break
		}
	}
	return b.String()
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
