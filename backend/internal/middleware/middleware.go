// Package middleware 提供链式中间件核心抽象（TODO #19 统一拦截链）。
//
// 设计：洋葱模型 + 泛型 + 纯标准库。Handler 是链上可执行的下一步；
// Middleware 拿到 next 返回包装后的 Handler；Chain 按 Use 顺序构建洋葱。
// 中间件可读写 *C（改写请求消息/工具参数/结果）；返回 error 立即短路。
// 中间件实例无状态，跨调用状态（预算计数/连读 map/token 用量）由持有引用的
// 中间件构造时闭包捕获，保持现有 per-agent scope 隔离不变。
//
// 两条实例链见 llm.go（LLM 链）与 toolchain.go（Tool 链）。
package middleware

import "context"

// Handler 是链上可执行的下一步（终端 handler 或下一个中间件的包装结果）。
type Handler[C any] func(ctx context.Context, c *C) error

// Middleware 是标准洋葱签名：拿到 next，返回包装后的 Handler。
type Middleware[C any] func(next Handler[C]) Handler[C]

// Chain 构建器；Use 顺序 = 入向执行顺序，出向逆序。
type Chain[C any] struct {
	mws []Middleware[C]
}

// New 创建空链。
func New[C any]() *Chain[C] {
	return &Chain[C]{}
}

// Use 追加中间件（唯一扩展点）。返回自身支持链式调用。
func (c *Chain[C]) Use(mw ...Middleware[C]) *Chain[C] {
	c.mws = append(c.mws, mw...)
	return c
}

// Then 把终端 handler 反向包裹成洋葱并返回可调用入口。
// 空链时直接返回 final（零开销直调）。
func (c *Chain[C]) Then(final Handler[C]) Handler[C] {
	h := final
	for i := len(c.mws) - 1; i >= 0; i-- {
		h = c.mws[i](h)
	}
	return h
}
