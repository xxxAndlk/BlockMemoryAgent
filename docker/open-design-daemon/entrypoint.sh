#!/bin/sh
# open_design daemon 容器入口（BMA，2026-10-10 起由 docker compose 统一管理）。
#
# 职责一：启动前把环境变量里的媒体生成配置深合并进 /app/.od/media-config.json
# （等价原后端 service 插件 sync_files 的 docker exec 写入——daemon 对
# baseUrl/model 无环境变量覆盖，只能落盘）。空值跳过该键，保留容器内
# 旧值/daemon 缺省；文件不存在且无任何值可写时不创建，交给 daemon 自己生成。
# 职责二：env 改名映射（原 plugins.yaml settings.env）：.env 的媒体生成 KEY
# 映射到 daemon 识别的变量名（compose env_file 注入原始变量，这里做改名；
# compose 的 ${VAR} 插值读不到仓库根 .env，故不能放在 compose environment）。
set -e

node <<'EOF'
const fs = require("fs");
const dir = "/app/.od";
const path = dir + "/media-config.json";
let cfg = {};
let existed = true;
try {
  cfg = JSON.parse(fs.readFileSync(path, "utf8"));
} catch (e) {
  existed = false; // 不存在或损坏：从无开始，键由下方按需补齐
}
if (!cfg || typeof cfg !== "object") cfg = {};
cfg.providers = cfg.providers || {};
let touched = false;
const set = (prov, key, val) => {
  if (!val) return; // 空值跳过（对齐原 sync_files 语义）
  cfg.providers[prov] = cfg.providers[prov] || {};
  cfg.providers[prov][key] = val;
  touched = true;
};
// 生图（custom-image provider = OpenAI 兼容 /images/generations 直调）
set("custom-image", "baseUrl", process.env.IMAGE_URL);
set("custom-image", "model", process.env.IMAGE_MODEL);
// 生视频（OpenAI /videos 协议，VIDEO_PROVIDER=openai 时用）
set("aihubmix", "baseUrl", process.env.VIDEO_URL);
set("aihubmix", "model", process.env.VIDEO_MODEL);
// 生视频（方舟 tasks 协议，VIDEO_PROVIDER=volcengine 时用）
set("volcengine", "baseUrl", process.env.VIDEO_URL);
if (touched || existed) {
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path, JSON.stringify(cfg, null, 2) + "\n");
}
EOF

# 生图 KEY：同时点亮 volcengine 与 custom-image 两个 provider 槽位
#（对齐原 plugins.yaml settings.env 的强制映射：IMAGE_KEY 优先，未设时
# 才保留 .env 里已有的 ARK_API_KEY 等值）
export ARK_API_KEY="${IMAGE_KEY:-$ARK_API_KEY}"
export OD_CUSTOM_IMAGE_API_KEY="${IMAGE_KEY:-$OD_CUSTOM_IMAGE_API_KEY}"
# 生视频 KEY：OpenAI /videos 协议网关（aihubmix provider 槽位）；若切
# VIDEO_PROVIDER=volcengine，视频 key 复用 ARK_API_KEY，VIDEO_KEY 不生效
export OD_AIHUBMIX_API_KEY="${VIDEO_KEY:-$OD_AIHUBMIX_API_KEY}"

# 原镜像启动命令（ghcr.io/nexu-io/od：ENTRYPOINT ["/sbin/tini","--"] +
# CMD ["node","apps/daemon/dist/cli.js","--no-open"]，WORKDIR /app）。
exec /sbin/tini -- node apps/daemon/dist/cli.js --no-open
