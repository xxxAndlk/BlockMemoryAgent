# open_design 守护进程本地补丁镜像（BMA，2026-08-18 新增）。
#
# 为什么需要补丁：上游 ghcr.io/nexu-io/od 的 OpenAI 兼容生图路径 size 硬编码
# 1024x1024（openaiSizeFor 兜底），而火山方舟 Agent Plan 网关（/api/plan/v3）
# 唯一可用的图像模型 doubao-seedream-5.0-lite 要求 ≥3686400px，1024x1024 必被
# InvalidParameter 拒绝。补丁把 renderVolcengineImage 与 renderCustomOpenAIImage
# 两处的 size 钉为 2048x2048，其余与上游一致。
#
# 构建：docker build -t bma/open-design-daemon:local docker/open-design-daemon
# （或一把梭 docker/bma-plugin.sh）。上游镜像更新后若补丁锚点失效，build 会
# 直接失败（锚点校验），届时人工复核 patch-image-size.js。
FROM ghcr.io/nexu-io/od:latest
COPY patch-image-size.js /tmp/patch-image-size.js
# 注：不 rm 补丁脚本——镜像以非 root 运行，/tmp sticky 位下删 root 属主文件会失败。
RUN node /tmp/patch-image-size.js
