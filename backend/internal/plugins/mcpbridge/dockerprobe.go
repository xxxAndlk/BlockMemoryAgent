package mcpbridge

// dockerprobe.go Docker 引擎依赖的失败处理。
//
// 引擎不可用（没装/没启动）时，docker transport 插件的表现必须是"降级 + 说清怎么办"，
// 而不是每 30 秒刷一条重连失败日志（实测刷了 26 分钟，用户看到的只是"插件坏了"）。
//
// 拉起逻辑在 plugins 包（plugins.EnsureDockerEngine）：启用插件时最多尝试拉起
// Docker Desktop 3 次；仍不可用则插件启用失败并降级（能力缺失，不阻断启动）。
// 用户可在应用内点「启动 Docker」重新拉起（POST /api/plugins/docker/start），
// 引擎就绪后重载插件即恢复。

import (
	"context"

	"github.com/blockmemory/agent/backend/internal/plugins"
)

// dockerEngineReady 探测 Docker 引擎是否可达（薄封装，便于本包内统一调用）。
func dockerEngineReady(ctx context.Context) error {
	return plugins.DockerEngineReady(ctx)
}
