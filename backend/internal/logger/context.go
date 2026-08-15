package logger

// 本文件提供 Logger 的 context 传递机制。
// 动机：轻量 LLM 调用（事实提取/事件摘要/意图仲裁等经 ModelFactory.CallLightweightWithRetry）
// 链路只持有 ctx，无法显式接收 session 级 Logger；挂到 ctx 后由调用点取出写 session_logs，
// 补齐轻量调用的耗时/token 记账缺口。

import "context"

// ctxKey 是 Logger 在 context 中的私有键类型，避免与其他包的 ctx 值冲突。
type ctxKey struct{}

// NewContext 返回携带 l 的 ctx；l 为 nil 时原样返回（调用方无需判空）。
func NewContext(ctx context.Context, l *Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext 取出 ctx 中携带的 Logger；未携带时返回 nil，调用方应跳过日志写入。
func FromContext(ctx context.Context) *Logger {
	if l, ok := ctx.Value(ctxKey{}).(*Logger); ok {
		return l
	}
	return nil
}
