// fallback.go 模型备胎链（TODO 第15项 T17）：主模型调用出错（非 ctx 取消）时依序
// 降级到 models.json role_bindings[roleID].fallback 列出的备胎条目；备胎构造失败或
// 调用失败继续下一档，全败返回最后一次错误。每次调用从主模型重试起步（不粘住上一
// 成功档）——自愈语义，主模型恢复即自动回主。
package model

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/go-kratos/blades"
)

// FallbackEvent 一次模型降级切换的观测数据。
type FallbackEvent struct {
	RoleID  string // 触发降级的角色 ID
	From    string // 失效一档的展示名（条目 ID 或底层模型名）
	To      string // 切换到的备胎条目 ID
	Attempt int    // 切换到的档位序号（0=主模型，1=第 1 备胎…）
	Err     string // 被降级那一档的错误文本
}

// FallbackObserver 降级事件回调（agent 层注入会话作用域观察者：写 session 事件 +
// slog；model 包不反向依赖 agent）。nil = 只落本地日志。
type FallbackObserver func(FallbackEvent)

// fallbackNode 备胎链一档：展示名 + provider（构造失败时 prov=nil、buildErr 非 nil）。
type fallbackNode struct {
	name     string
	prov     blades.ModelProvider
	buildErr error
}

// fallbackProvider 主模型 + 备胎链包装 provider（实现 blades.ModelProvider）。
// 备胎 provider 惰性构造且每档只构造一次（构造失败永久标记本档不可用）。
type fallbackProvider struct {
	roleID   string
	primary  blades.ModelProvider
	ids      []string // 备胎条目 ID（依序）
	build    func(roleID, entryID string) (blades.ModelProvider, error)
	mu       sync.Mutex
	cands    []fallbackNode // 与 ids 对齐的缓存槽
	observe  FallbackObserver
}

// newFallbackProvider 构造备胎链包装。build 由调用方注入（工厂传真实构造，测试传
// fake），保证本包不依赖具体条目解析细节。
func newFallbackProvider(
	roleID string,
	primary blades.ModelProvider,
	ids []string,
	build func(roleID, entryID string) (blades.ModelProvider, error),
	observe FallbackObserver,
) *fallbackProvider {
	return &fallbackProvider{
		roleID:  roleID,
		primary: primary,
		ids:     ids,
		build:   build,
		cands:   make([]fallbackNode, len(ids)),
		observe: observe,
	}
}

// Name 返回主模型名（包装器对外呈现为角色主模型）。
func (p *fallbackProvider) Name() string { return p.primary.Name() }

// candidate 取第 i 个备胎（惰性构造一次；构造失败永久标记本档不可用）。
func (p *fallbackProvider) candidate(i int) fallbackNode {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := &p.cands[i]
	if c.prov == nil && c.buildErr == nil {
		prov, err := p.build(p.roleID, p.ids[i])
		if err != nil {
			c.buildErr = fmt.Errorf("备胎 %q 不可用: %w", p.ids[i], err)
		} else {
			c.prov = prov
		}
	}
	return *c
}

// node 返回第 i 档（0=主模型）：备胎档惰性构造——只在循环真正轮到它时才构建，
// ctx 取消短路路径不触碰注册表构造。
func (p *fallbackProvider) node(i int) fallbackNode {
	if i == 0 {
		return fallbackNode{name: p.primary.Name(), prov: p.primary}
	}
	c := p.candidate(i - 1)
	c.name = p.ids[i-1]
	return c
}

// nodeName 第 i 档展示名（0=主模型名，其余=备胎条目 ID）。
func (p *fallbackProvider) nodeName(i int) string {
	if i == 0 {
		return p.primary.Name()
	}
	return p.ids[i-1]
}

// notify 降级切换留痕：本地日志 + 观察者回调（可空）。
func (p *fallbackProvider) notify(fromIdx, toIdx int, err error) {
	from, to := p.nodeName(fromIdx), p.nodeName(toIdx)
	log.Printf("[model] fallback: role=%s %s → %s (err=%v)", p.roleID, from, to, err)
	if p.observe != nil {
		p.observe(FallbackEvent{RoleID: p.roleID, From: from, To: to, Attempt: toIdx, Err: err.Error()})
	}
}

// Generate 一次性调用：主模型出错（非 ctx 取消）依序试备胎，全败返回最后一次错误。
func (p *fallbackProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	total := len(p.ids) + 1
	var lastErr error
	for i := 0; i < total; i++ {
		node := p.node(i)
		if node.prov == nil {
			lastErr = node.buildErr
		} else {
			var resp *blades.ModelResponse
			resp, lastErr = node.prov.Generate(ctx, req)
			if lastErr == nil {
				return resp, nil
			}
		}
		// ctx 取消/超时：换模型救不了已放弃的调用，原样返回。
		if ctx.Err() != nil {
			return nil, lastErr
		}
		if i+1 < total {
			p.notify(i, i+1, lastErr)
		}
	}
	return nil, lastErr
}

// NewStreaming 流式调用：仅在产出任何 chunk 之前出错才切下一档（已产出后中断重启
// 会向调用方重复输出，不做流中降级）；下游停止消费即终止。
func (p *fallbackProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		total := len(p.ids) + 1
		var lastErr error
		for i := 0; i < total; i++ {
			node := p.node(i)
			if node.prov == nil {
				lastErr = node.buildErr
			} else {
				failed, err := streamOnce(ctx, node.prov, req, yield)
				if !failed {
					return
				}
				lastErr = err
			}
			if ctx.Err() != nil {
				yield(nil, lastErr)
				return
			}
			if i+1 < total {
				p.notify(i, i+1, lastErr)
			}
		}
		yield(nil, lastErr)
	}
}

// streamOnce 消费单档流：返回 (true, err)=首个 chunk 之前失败（可安全降级）；
// (false, nil)=正常完成或下游停止消费。产出 chunk 后的错误不在本层降级，直接透传下游。
func streamOnce(
	ctx context.Context,
	prov blades.ModelProvider,
	req *blades.ModelRequest,
	yield func(*blades.ModelResponse, error) bool,
) (bool, error) {
	first := true
	for resp, err := range prov.NewStreaming(ctx, req) {
		if err != nil {
			if first {
				return true, err
			}
			yield(nil, err)
			return false, nil
		}
		first = false
		if !yield(resp, nil) {
			return false, nil
		}
	}
	return false, nil
}
