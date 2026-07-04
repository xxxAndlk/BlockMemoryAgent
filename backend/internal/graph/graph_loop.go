package graph

// 本文件承载三层图的状态机循环：Invoke + fingerprintState（死循环检测）。
// 从 three_layer_graph.go 拆出（P0-3）。路由见 graph_routes.go，节点解析见 graph_resolve.go。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// Invoke 执行三层图。
// 状态机循环：每轮从 nodes 取出当前节点 → Invoke → 由 determineNext 决定下一节点。
// 终止条件：NextAction == Finish、next 为空、达到 200 步上限或节点报错。
// 参数：
//   - ctx：上下文，会被 WithSessionID 注入会话 ID。
//   - state：图状态，跨节点共享。
//
// 返回：终态 state 或错误。
// 副作用：每轮通过 progress 回调推送 graph_step 调试事件。
// 并发安全：同一会话不应并发 Invoke；nodes 表访问持读锁。
func (g *ThreeLayerGraph) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	ctx = WithSessionID(ctx, state.SessionID) // 把 sessionID 写入 ctx，供下游日志/存储使用
	current := "MetaAgent"                    // 入口固定从 MetaAgent 开始
	stepCount := 0                            // 已执行步数

	// —— 死循环防护配置（替代硬步数上限）——
	// 三层防御：进展检测 + 状态指纹去重 + wall-clock 超时
	// 配置缺失视为系统级故障，不做兜底；默认值在 config.Load 阶段填充，启动时已校验
	if g.rt == nil || g.rt.AgentCfg == nil {
		return nil, fmt.Errorf("运行时配置未注入 (Runtime/AgentCfg 为空)，状态机无法启动")
	}
	// 进展检测：连续 N 步状态指纹无变化 → 判死循环
	stallSteps := g.rt.AgentCfg.StallSteps
	// 状态指纹去重：同一指纹连续出现 N 次 → 判死循环
	maxRepeatFP := g.rt.AgentCfg.MaxRepeatFingerprint
	// wall-clock 超时：单次 Invoke 超过 sessionTimeout → 判死循环
	sessionTimeout := time.Duration(g.rt.AgentCfg.SessionTimeoutMin) * time.Minute

	// wall-clock 超时：若 ctx 已有 deadline 不早于 sessionTimeout 则保留，否则叠加
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sessionTimeout)
		defer cancel()
	}

	// 进展检测：记录最近一次有进展的步号。进展 = state 关键字段变化（指纹变化）。
	// 指纹只取影响路由的字段，避免无关字段抖动误判进展。
	lastProgressStep := 0
	// 状态指纹去重：记录上一指纹与连续重复次数
	prevFP := ""
	repeatCount := 0

	for {
		// 1. wall-clock 超时 / 取消检查
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("会话 wall-clock 超时 (%v)，状态机终止 (已执行 %d 步)", sessionTimeout, stepCount)
			}
			return nil, fmt.Errorf("会话被取消或上下文终止 (原因=%v，已执行 %d 步)", err, stepCount)
		}
		stepCount++

		// 2. 查节点：先查静态节点表，未命中则尝试按实例 ID 动态解析
		g.mu.RLock()
		node, ok := g.nodes[current]
		g.mu.RUnlock()
		if !ok {
			// 静态表里没有 → 可能是某 Domain/Assistant 的实例 ID
			node = g.resolveInstanceNode(current)
			if node == nil {
				return nil, fmt.Errorf("节点 %s 未找到", current)
			}
		}

		// 3. 执行节点：传入当前 state，拿到更新后的 state
		newState, err := node.Invoke(ctx, state)
		if err != nil {
			return nil, fmt.Errorf("节点 %s 执行失败: %w", current, err)
		}
		state = newState

		// 4. 路由：根据当前节点名 + state.NextAction 决定下一节点
		next := g.determineNext(current, state)

		// 推送图步骤调试事件（每个 tick 都推，便于 TUI / Web 调试）
		// message 含步号/节点跳转/action；detail 含 state 关键字段变化，便于定位循环卡点
		if g.progress != nil {
			g.progress(ctx, ProgressEvent{
				SessionID: state.SessionID,
				Kind:      "graph_step",
				Agent:     "Graph",
				Message: fmt.Sprintf("Step %d: %s → %s (action=%s, domain=%s, block=%s, stack=%d)",
					stepCount, current, next, state.NextAction,
					state.CurrentDomain, state.CurrentBlockID, len(state.CallStack)),
				Detail: fmt.Sprintf("completed=%d active=%d roles=%d calling=%v clarify=%v",
					len(state.CompletedBlocks), len(state.ActiveBlocks), len(state.RoleInstances),
					state.IsCalling(), state.PendingClarify != nil),
			})
		}

		// 5. 进展检测 + 指纹去重（在终止判定之前，避免挂起态误判）
		fp := fingerprintState(state)
		if fp != prevFP {
			// 指纹变化 → 有进展，重置计数
			lastProgressStep = stepCount
			repeatCount = 0
			prevFP = fp
		} else {
			// 指纹未变
			repeatCount++
			// 渐进 warning：达 50% / 80% 阈值时推送，便于观察死循环形成过程
			if g.progress != nil {
				if repeatCount == maxRepeatFP/2 && maxRepeatFP >= 2 {
					g.progress(ctx, ProgressEvent{
						SessionID: state.SessionID, Kind: "wait", Agent: "Graph",
						Message: fmt.Sprintf("⚠ 状态指纹连续 %d/%d 步重复 (节点=%s)，疑似死循环", repeatCount, maxRepeatFP, current),
					})
				} else if repeatCount == maxRepeatFP-1 && maxRepeatFP >= 2 {
					g.progress(ctx, ProgressEvent{
						SessionID: state.SessionID, Kind: "wait", Agent: "Graph",
						Message: fmt.Sprintf("⚠⚠ 状态指纹连续 %d/%d 步重复，即将触发死循环终止", repeatCount, maxRepeatFP),
					})
				}
			}
			if repeatCount >= maxRepeatFP {
				return nil, fmt.Errorf("检测到死循环：状态连续 %d 步无变化 (节点=%s)，状态机终止", repeatCount, current)
			}
		}
		// 无进展步数渐进 warning：达 50% / 80% 阈值时推送
		stallGap := stepCount - lastProgressStep
		if g.progress != nil && stallGap > 0 {
			if stallGap == stallSteps/2 && stallSteps >= 2 {
				g.progress(ctx, ProgressEvent{
					SessionID: state.SessionID, Kind: "wait", Agent: "Graph",
					Message: fmt.Sprintf("⚠ 连续 %d/%d 步无进展 (节点=%s)，疑似卡死", stallGap, stallSteps, current),
				})
			} else if stallGap == stallSteps-1 && stallSteps >= 2 {
				g.progress(ctx, ProgressEvent{
					SessionID: state.SessionID, Kind: "wait", Agent: "Graph",
					Message: fmt.Sprintf("⚠⚠ 连续 %d/%d 步无进展，即将触发死循环终止", stallGap, stallSteps),
				})
			}
		}
		// 无进展步数超阈值 → 判死循环
		if stallGap >= stallSteps {
			return nil, fmt.Errorf("检测到死循环：连续 %d 步无进展 (节点=%s)，状态机终止", stallSteps, current)
		}

		// 6. 终止判定：Finish 或 next 为空都结束循环
		if state.NextAction == enums.ActionFinish {
			return state, nil
		}

		// ActionWait：挂起图循环，等待人机对话答复后由 server 侧恢复（特性5）。
		// 直接 return 而非 continue，避免下一 tick 继续推进；server 在收到 /clarify 答复后会
		// 通过 resumeSession 重新调用 Invoke，从当前 state 继续执行。
		// 注意：Wait 态指纹稳定会重复，但此处已 return，不会触发死循环判定。
		if state.NextAction == enums.ActionWait {
			return state, nil
		}

		if next == "" {
			return state, nil // 无下一跳，安全退出
		}
		current = next // 推进到下一节点
	}
}

// fingerprintState 计算状态指纹：取影响路由与语义的关键字段做 SHA256。
// 只取关键字段而非全 state，避免无关字段（如时间戳）抖动误判进展。
// 用于死循环检测：连续 N 步指纹不变 → 判死循环。
// 注意：map 迭代顺序在 Go 中随机，必须先对 key 排序再拼接，
// 否则相同状态会产生不同指纹，导致死锁检测漏报/误报（C3）。
func fingerprintState(s *types.ThreeLayerState) string {
	if s == nil {
		return ""
	}
	h := sha256.New()
	// 当前节点路由相关
	fmt.Fprintf(h, "block=%s|domain=%s|goal=%s|action=%s|target=%s|",
		s.CurrentBlockID, s.CurrentDomain, s.DomainGoal, s.NextAction, s.TargetRoleID)
	// 调用栈深度 + 顶层 callee（反映 Assistant 调用进展）
	fmt.Fprintf(h, "stack=%d|top=%s|", len(s.CallStack), s.CurrentAssistantID)
	// 已完成块（反映领域推进）——切片有序，可直接拼接
	fmt.Fprintf(h, "done=%v|", s.CompletedBlocks)
	// 活跃块 ID 集合（反映领域创建进展）——map 无序，必须排序后再拼接
	activeIDs := make([]string, 0, len(s.ActiveBlocks))
	for id := range s.ActiveBlocks {
		activeIDs = append(activeIDs, id)
	}
	sort.Strings(activeIDs) // 排序保证相同 map 内容产生相同指纹
	fmt.Fprintf(h, "active=%v|", activeIDs)
	// 角色实例数（反映 Agent 创建进展）
	fmt.Fprintf(h, "roles=%d|", len(s.RoleInstances))
	// 待处理澄清请求 ID（反映人机对话态）
	if s.PendingClarify != nil {
		fmt.Fprintf(h, "clarify=%s|", s.PendingClarify.ID)
	}
	return hex.EncodeToString(h.Sum(nil))[:16] // 16 字符够去重，节省内存
}
