package tool

import "errors"

// ErrLoopExit 是循环守卫（连读死循环 / 探索预算耗尽 / 单工具连续失败）命中时返回的哨兵错误。
// 命中守卫的 Dispatch 调用不再被吞成普通工具结果，而是返回包装哨兵的非 nil error；
// ReAct 主循环检测 errors.Is(err, ErrLoopExit) 后终止循环（子 Agent 走 Failed 语义，
// 失败打捞回灌父 Agent）。替换旧的 blades tools.ActionLoopExit 上下文信号——该信号
// 依赖 tools.NewContext 注入，全项目无调用方，FromContext 永远失败，守卫信号被静默丢弃。
var ErrLoopExit = errors.New("loop guard: force exit")
