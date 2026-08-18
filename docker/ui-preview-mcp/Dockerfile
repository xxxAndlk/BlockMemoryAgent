# ui-preview-mcp 镜像：Microsoft Playwright MCP（Apache-2.0，
# github.com/microsoft/playwright-mcp），BMA ui_preview 插件——Agent 的"眼睛"：
# headless Chromium 打开 workspace 里的 HTML 页面，截图 / a11y 快照 / 执行 JS /
# 读 console，供 Agent 写完动态 UI 代码后自己渲染验证、看图迭代。
# 由 mcpbridge docker transport 以 `docker run -i --rm` 拉起（stdio 桥接），
# 启动参数（--headless / --output-dir / --allow-unrestricted-file-access 等）
# 由后端 settings.args 传入，本镜像不固化。
# 构建：docker build -t bma/ui-preview-mcp:local docker/ui-preview-mcp
# 注：MCR 直连慢时可用镜像加速前缀 mcr.1ms.run/playwright/mcp:latest
# （2026-08-17 实测 mcr 直连 ~2KB/s，镜像站 401 为正常 registry 挑战响应）。
FROM mcr.microsoft.com/playwright/mcp:latest

# 官方镜像 ENTRYPOINT（node cli.js）保持不变；无本地编译内容，
# 本 Dockerfile 的意义是统一 bma/*:local 命名范式 + 经 bma-plugin 一键备齐。
