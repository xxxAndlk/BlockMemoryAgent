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
// 图像侧 nanobanana 的 URL 归一：填了 generateContent 全路径会拼出双路径，剥到网关根。
const toNanoRoot = (u) => {
  if (!u) return u;
  const stripped = u.replace(/\/v1beta\/models\/[^/]+:(stream)?[Gg]enerateContent\/?$/, "");
  if (stripped !== u) console.error(`[bma-entrypoint] IMAGE_URL 含 generateContent 全路径，已归一为网关根: ${stripped}`);
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
// ---- 媒体配置单点化（2026-10-14）：仓库根 .env 媒体段 = 唯一手改点 ----
// 按 IMAGE_PROVIDER / VIDEO_PROVIDER 排他写入槽位——桥（ui_design MCP）以
// "providers.<槽>.model 是否非空"探测用户选定通道，因此**未选中的槽必须清掉 model**。
// 声明闸门：媒体段一个变量都没有时不动 media-config（保护存量容器里 Web UI 手配的值）。
const imageProvider = (pick("IMAGE_PROVIDER") || "custom-image").toLowerCase();
const videoProvider = (pick("VIDEO_PROVIDER") || "volcengine").toLowerCase();
const imageDeclared = Boolean(pick("IMAGE_PROVIDER", "IMAGE_URL", "IMAGE_MODEL"));
const videoDeclared = Boolean(pick("VIDEO_PROVIDER", "VIDEO_URL", "VIDEOS_URL", "VIDEO_MODEL", "VIDEOS_MODEL"));
const del = (prov, key) => {
  const slot = cfg.providers[prov];
  if (slot && Object.prototype.hasOwnProperty.call(slot, key)) {
    delete slot[key];
    touched = true;
  }
};
// ---- 能力声明（2026-10-24 媒体能力路由）----
// IMAGE_CAPABILITIES / VIDEO_CAPABILITIES 是非敏感能力清单，例如：
//   IMAGE_CAPABILITIES=t2i,subject_reference,aspect
//   VIDEO_CAPABILITIES=t2v,i2v,aspect,duration
// 写入选中槽位的 capabilities 数组，桥（ui_design MCP）探测后据此做本地校验：
// 未声明的能力（尤其 image_edit/inpaint）一律拒绝，绝不静默丢弃参数。
// 未声明 → 删除该键（桥回落保守默认集，不默认宣称局部编辑可用）。
const capsList = (raw) => String(raw || '').split(/[,\s]+/).map((s) => s.trim().toLowerCase()).filter(Boolean);
const setCaps = (prov, raw) => {
  const list = capsList(raw);
  if (list.length) {
    cfg.providers[prov] = cfg.providers[prov] || {};
    cfg.providers[prov].capabilities = list;
    touched = true;
  } else {
    del(prov, "capabilities");
  }
};
if (imageDeclared) {
  if (imageProvider === "custom-image") {
    // OpenAI 兼容 /images/generations 直调（聚合网关/Gemini 兼容网关等）
    set("custom-image", "baseUrl", process.env.IMAGE_URL);
    set("custom-image", "model", process.env.IMAGE_MODEL);
    setCaps("custom-image", process.env.IMAGE_CAPABILITIES);
    del("nanobanana", "model");
    del("nanobanana", "capabilities");
  } else if (imageProvider === "nanobanana") {
    // Google nano-banana；baseUrl 空 = daemon 默认官方端点
    set("nanobanana", "baseUrl", toNanoRoot(process.env.IMAGE_URL));
    set("nanobanana", "model", process.env.IMAGE_MODEL);
    setCaps("nanobanana", process.env.IMAGE_CAPABILITIES);
    del("custom-image", "model"); // baseUrl 残留无害：判定键是 model 非空
    del("custom-image", "capabilities");
  } else {
    console.error(`[bma-entrypoint] 未知 IMAGE_PROVIDER=${imageProvider}（可选 custom-image|nanobanana），本次不改图像槽位`);
  }
}
if (videoDeclared) {
  if (videoProvider === "openai") {
    // OpenAI /videos 协议聚合网关
    set("aihubmix", "baseUrl", toApiRoot(videoUrl));
    set("aihubmix", "model", videoModel);
    setCaps("aihubmix", process.env.VIDEO_CAPABILITIES);
    del("volcengine", "model");
    del("volcengine", "capabilities");
    del("grok", "model");
    del("grok", "capabilities");
  } else if (videoProvider === "volcengine") {
    // 方舟 tasks 协议（模型须目录 id / OD_MEDIA_MODEL_ALIASES 可解析）
    set("volcengine", "baseUrl", toApiRoot(videoUrl));
    set("volcengine", "model", videoModel);
    setCaps("volcengine", process.env.VIDEO_CAPABILITIES);
    del("aihubmix", "model");
    del("aihubmix", "capabilities");
    del("grok", "model");
    del("grok", "capabilities");
  } else if (videoProvider === "grok") {
    // grok 线 = OpenAI 同步形状 POST {base}/videos/generations（2026-10-16：BMA 本机
    // 8090 Gemini 桥走这条；配套 patch-image-size.js ③ 让 renderGrokVideo 接受其
    // video_url 字段。模型仍写目录 id——daemon 按目录定义分发到 grok 渲染器）。
    // 2026-10-24：MiniMax 适配桥（docker/minimax-media）也挂在这条线上，
    // agent 无需知道协议名——VIDEO_PROVIDER=grok + auto 路由即可。
    set("grok", "baseUrl", toApiRoot(videoUrl));
    set("grok", "model", videoModel);
    setCaps("grok", process.env.VIDEO_CAPABILITIES);
    del("aihubmix", "model");
    del("aihubmix", "capabilities");
    del("volcengine", "model");
    del("volcengine", "capabilities");
  } else {
    console.error(`[bma-entrypoint] 未知 VIDEO_PROVIDER=${videoProvider}（可选 openai|volcengine|grok），本次不改视频槽位`);
  }
}
if (touched || existed) {
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path, JSON.stringify(cfg, null, 2) + "\n");
}
// 生效摘要（仅配置非密文；secrets 不落）。
const slotOf = (p) => (cfg.providers[p] && cfg.providers[p].model) || "无";
console.error(
  `[bma-entrypoint] 媒体单点生效（源=仓库根 .env 媒体段）：IMAGE_PROVIDER=${imageProvider} ｜ custom-image.model=${slotOf("custom-image")} ｜ nanobanana.model=${slotOf("nanobanana")} ｜ VIDEO_PROVIDER=${videoProvider} ｜ aihubmix.model=${slotOf("aihubmix")} ｜ volcengine.model=${slotOf("volcengine")} ｜ grok.model=${slotOf("grok")}`
);
// 能力声明摘要（非敏感）：未声明时桥用保守默认集。
const capsOf = (p) => ((cfg.providers[p] && cfg.providers[p].capabilities) || []).join(",") || "未声明(保守默认)";
console.error(
  `[bma-entrypoint] 能力声明：image=${capsOf(imageProvider === "nanobanana" ? "nanobanana" : "custom-image")} ｜ video=${capsOf(videoProvider === "openai" ? "aihubmix" : videoProvider)}`
);
EOF

# 生图 KEY：按 provider 点亮对应链首（依据 daemon media/config.js 的 ENV_KEYS：
# nanobanana=[OD_NANOBANANA_API_KEY, GOOGLE_API_KEY, GEMINI_API_KEY]、
# custom-image=[OD_CUSTOM_IMAGE_API_KEY,...]）——导出链首最稳，不依赖通用名杂散值。
if [ "${IMAGE_PROVIDER:-custom-image}" = "nanobanana" ]; then
  export OD_NANOBANANA_API_KEY="${IMAGE_KEY:-$OD_NANOBANANA_API_KEY}"
fi
export OD_CUSTOM_IMAGE_API_KEY="${IMAGE_KEY:-$OD_CUSTOM_IMAGE_API_KEY}"
export ARK_API_KEY="${IMAGE_KEY:-$ARK_API_KEY}"
# 生视频 KEY：openai → aihubmix 槽位；grok → OD_GROK_API_KEY（8090 桥不鉴权，但
# renderGrokVideo 要求 apiKey 非空才进场，占位即可）；volcengine → 方舟 key
#（VIDEO_KEY 优先，2026-10-14 起在 volcengine 侧生效——旧文档"VIDEO_KEY 不生效"作废）。
if [ "${VIDEO_PROVIDER:-volcengine}" = "openai" ]; then
  export OD_AIHUBMIX_API_KEY="${VIDEO_KEY:-$OD_AIHUBMIX_API_KEY}"
elif [ "${VIDEO_PROVIDER:-volcengine}" = "grok" ]; then
  export OD_GROK_API_KEY="${VIDEO_KEY:-$OD_GROK_API_KEY}"
elif [ -n "${VIDEO_KEY:-}" ]; then
  export ARK_API_KEY="$VIDEO_KEY"
fi

# 原镜像启动命令（ghcr.io/nexu-io/od：ENTRYPOINT ["/sbin/tini","--"] +
# CMD ["node","apps/daemon/dist/cli.js","--no-open"]，WORKDIR /app）。
exec /sbin/tini -- node apps/daemon/dist/cli.js --no-open
