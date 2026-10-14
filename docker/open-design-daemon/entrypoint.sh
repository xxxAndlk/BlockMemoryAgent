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
// 变量名容错：主名优先，兼容旧文档/笔误的 VIDEOS_*（2026-10-14 线上曾用
// VIDEOS_MODEL/VIDEOS_URL 导致配置被静默丢弃——别再静默：检测到即 stderr 告警）。
const pick = (...names) => {
  for (const n of names) {
    const v = process.env[n];
    if (v && v.trim()) return v.trim();
  }
  return undefined;
};
// URL 归一：daemon 渲染器自行拼接协议后缀（volcengine 拼 ${baseUrl}/contents/
// generations/tasks、aihubmix 拼 ${baseUrl}/videos），.env 里带后缀会拼出双路径 404
// ——统一剥掉常见后缀取 API 根，并提示实际生效值。
const toApiRoot = (u) => {
  if (!u) return u;
  const stripped = u.replace(/\/(contents\/generations\/tasks|videos\/generations|videos)\/?$/, "");
  if (stripped !== u) console.error(`[bma-entrypoint] VIDEO_URL 含协议后缀，已归一为 API 根: ${stripped}`);
  return stripped;
};
const videoUrl = pick("VIDEO_URL", "VIDEOS_URL");
const videoModel = pick("VIDEO_MODEL", "VIDEOS_MODEL");
if (!process.env.VIDEO_URL && process.env.VIDEOS_URL) {
  console.error("[bma-entrypoint] 警告: 只找到 VIDEOS_URL（应为 VIDEO_URL）——已按兼容名读入，建议改正 .env 键名");
}
if (!process.env.VIDEO_MODEL && process.env.VIDEOS_MODEL) {
  console.error("[bma-entrypoint] 警告: 只找到 VIDEOS_MODEL（应为 VIDEO_MODEL）——已按兼容名读入，建议改正 .env 键名");
}
// 生图（custom-image provider = OpenAI 兼容 /images/generations 直调）
set("custom-image", "baseUrl", process.env.IMAGE_URL);
set("custom-image", "model", process.env.IMAGE_MODEL);
// 生视频（OpenAI /videos 协议，VIDEO_PROVIDER=openai 时用）
set("aihubmix", "baseUrl", toApiRoot(videoUrl));
set("aihubmix", "model", videoModel);
// 生视频（方舟 tasks 协议，VIDEO_PROVIDER=volcengine 时用）
set("volcengine", "baseUrl", toApiRoot(videoUrl));
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
