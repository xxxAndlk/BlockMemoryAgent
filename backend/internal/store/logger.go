package store

import (
	"context"  // 日志调用上下文
	"log"      // 未注入 Logger 时的回退输出
	"log/slog" // 结构化日志 Attr 类型，与 logger.Logger 方法签名保持一致
)

// Logger 是 store 包内部使用的最小日志接口，由装配层注入 *logger.Logger。
//
// 设计说明：logger 包依赖 store（SessionLogRecord 等类型），store 不能反向
// import logger，否则构成循环导入；因此这里用窄接口做依赖倒置，
// *logger.Logger 天然满足该接口。
type Logger interface {
	// Error 按 error 级别记录日志，语义与 logger.Logger.Error 一致。
	Error(ctx context.Context, msg string, err error, extra ...slog.Attr)
}

// logError 记录错误类日志：已注入 Logger 时走结构化 [ERRO] 输出，
// 未注入（如单元测试直接构造存储）时回退标准库 log，保持旧行为不丢日志。
//
// 参数:
//   - l:   注入的日志器，可为 nil。
//   - ctx: 请求上下文。
//   - msg: 日志消息（错误详情由 err 追加，无需内嵌 %v）。
//   - err: 原始错误。
func logError(l Logger, ctx context.Context, msg string, err error) {
	if l != nil {
		l.Error(ctx, msg, err)
		return
	}
	log.Printf("%s: %v", msg, err)
}
