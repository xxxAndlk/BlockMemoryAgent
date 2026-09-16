// image_capability.go 处理"当前模型不支持图片输入"的降级路径（2026-09-16）。
// 背景：图片（ReadMedia 读图 / ui_preview 截图透传 / 用户上传）进入对话后，
// 无视觉模型的 provider 会以 400 拒绝整轮请求（实证 ark 网关
// "Model do not support image input"），此前直接令 run 失败——日常档顶层会话
// 因此判错死掉，集群档子域死而父 Meta 收到通用失败后盲目重派。
// 现策略：识别该环境性错误 → 进程级记住该模型无视觉 → 请求视图剥图 + 注入说明
// → 继续循环（不判死）；工具侧经 ctx 门控（tool.ImageInputSupportedOf）避免
// 再次把图片塞进对话。换模型（set_agent_model）后实时恢复，不做粘性标记。
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/blockmemory/agent/backend/internal/model"
)

// imageUnsupportedModels 是进程级"无视觉模型"记忆（键=模型名，来自 provider 的
// ModelName()）。不持久化：进程重启后每模型最多重学一次（代价 1 次 400 调用）。
var imageUnsupportedModels sync.Map

// markModelNoImage 记住某模型不支持图片输入。
func markModelNoImage(name string) {
	if strings.TrimSpace(name) == "" {
		return // mock/未暴露模型名的 provider：不落缓存，避免空键误伤
	}
	imageUnsupportedModels.Store(name, struct{}{})
}

// modelNoImage 查询某模型是否已知不支持图片输入。
func modelNoImage(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	_, ok := imageUnsupportedModels.Load(name)
	return ok
}

// noImageInput 判定当前 provider 的模型是否已知不支持图片输入。
// 每轮实时查询（不落 Agent 字段）：providerFn 可能在运行中换模型，热驻 Agent
// 也必须随模型切换即时恢复读图。
func (a *ReActAgent) noImageInput() bool {
	return modelNoImage(a.llmModelName())
}

// shouldRetryLLMCall 是 agent 层 LLM 重试判定：除会话取消与单次调用超时外，
// "模型不支持图片输入"是环境性错误，重试必然复现——快速失败，交给主循环做
// 剥图降级（默认中间件会白白重试 retryCount 次）。
func shouldRetryLLMCall(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return !model.IsImageInputUnsupported(err)
}

// imageUnsupportedNotice 是视觉能力缺失的模型可见说明（注入历史），
// 也复用于工具侧拦截文案。
func imageUnsupportedNotice(modelName string) string {
	name := modelName
	if strings.TrimSpace(name) == "" {
		name = "当前"
	}
	return fmt.Sprintf("【视觉能力缺失】模型 %s 不支持图片输入，图像内容已从上下文中省略"+
		"（继续携带图像会被 provider 以 400 拒绝整个请求）。需要看图核对时的可行路径："+
		"① 换用支持视觉的模型（list_models 查看可用模型，set_agent_model / set_role_model 切换）；"+
		"② 会话顶层执行者可用 escalate_gear 请求升档，交由更高档位处理；"+
		"③ 改走文字描述或其他证据（文件清单、注释、代码审查）完成核对。", name)
}

// stripImagesForRequest 返回剥离全部图像后的请求视图（浅拷贝，不改 canonical history）。
// 工具消息的 Content 保持不动：Result.Images 是 json:"-"（Content 里本无 base64），
// 改写 JSON 反而有损坏风险；给模型的说明由 imageUnsupportedNotice 单独成条注入。
func stripImagesForRequest(messages []ReactMessage) []ReactMessage {
	out := make([]ReactMessage, len(messages))
	copy(out, messages)
	for i := range out {
		if len(out[i].Images) > 0 {
			out[i].Images = nil
		}
	}
	return out
}
