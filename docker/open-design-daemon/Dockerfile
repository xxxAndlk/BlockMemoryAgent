# open_design 守护进程本地补丁镜像（BMA，2026-08-18 新增）。
#
# 为什么需要补丁：上游 ghcr.io/nexu-io/od 的 OpenAI 兼容生图路径 size 硬编码
# 1024x1024（openaiSizeFor 兜底），而火山方舟 Agent Plan 网关（/api/plan/v3）
# 图像模型对 size 有尺寸下限，1024x1024 必被 InvalidParameter 拒绝。补丁替换
# renderVolcengineImage 与 renderCustomOpenAIImage 两处的 size：2026-10-14 起
# （seedream-5.0-pro）按 aspect 映射显式尺寸（16:9→2048x1152 等），脚本内
# RESTORE_ASPECT=false 可一行回退恒 2048x2048，其余与上游一致。
#
# 构建：docker compose -f docker/docker-compose.yml up -d open_design
# （compose 缺镜像自动构建；或手动 docker build -t bma/open-design-daemon:local
# docker/open-design-daemon）。上游镜像更新后若补丁锚点失效，build 会
# 直接失败（锚点校验），届时人工复核 patch-image-size.js。
FROM ghcr.io/nexu-io/od:latest
COPY patch-image-size.js /tmp/patch-image-size.js
# 注：不 rm 补丁脚本——镜像以非 root 运行，/tmp sticky 位下删 root 属主文件会失败。
RUN node /tmp/patch-image-size.js
# 入口脚本：启动前深合并 media-config.json + env 改名映射，再 exec 原命令。
# 基础镜像 USER 为 open-design（非 root），COPY --chmod 避免 RUN chmod 权限失败。
COPY --chmod=0755 entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
