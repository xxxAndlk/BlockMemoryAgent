// minimax-media：MiniMax（M Plan 订阅 Key / 开放平台 Key）→ open_design daemon 形状适配桥。
//
// 为什么需要它（2026-10-23）：
//   open_design daemon 的图像/视频渲染器只认几种固定协议（custom-image 的 OpenAI
//   /images/generations、grok 的同步 /videos/generations、方舟 tasks …），MiniMax 的
//   /v1/image_generation（data.image_base64[]）与 /v1/video_generation（task_id 轮询 +
//   files/retrieve）都不在其中。与其补丁上游 dist（每次升级都要重锚），不如把 MiniMax
//   翻译成 daemon 已经说得很好的两种形状：
//     - 图像：OpenAI 形状 POST /v1/images/generations（回 data[0].b64_json）
//     - 视频：grok 线形状 POST /v1/videos/generations → {id,status:'pending'}，
//              GET /v1/videos/{id} → {status:'done',video:{url}} / {status:'failed'}。
//              daemon 的 renderGrokVideo 每 4s 轮询（上限 OD_GROK_VIDEO_MAX_POLL_MS，
//              默认 8min），提交侧无硬超时——长任务不会掐断。
//   因此 daemon / ui_design 桥均零改动：.env 媒体段把 IMAGE_PROVIDER=custom-image 指向
//   本服务、VIDEO_PROVIDER=grok 指向本服务即可（见仓库根 .env「通通 D」）。
//
// 配置（全部来自环境变量；容器经 compose env_file 读仓库根 .env）：
//   MINIMAX_API_KEY          订阅 Key（sk-cp-…）或开放平台 Key。请求头里的 Bearer 优先，
//                            本变量兜底（daemon 会把 IMAGE_KEY/VIDEO_KEY 作为 Bearer 发来）。
//   MINIMAX_BASE_URL         默认 https://api.minimaxi.com（国内站）；国际站 = https://api.minimax.io
//   MINIMAX_IMAGE_MODEL      默认 image-01（可选 image-01-live）
//   MINIMAX_VIDEO_MODEL      默认 MiniMax-Hailuo-2.3（v1 端点）。写 MiniMax-H3 / MiniMax-H3-Max
//                            自动走 v2 端点（已用 M Plan 订阅 Key 实测通过）。官方时长下限：
//                            H3 = 4s、H3-Max = 5s（**不支持 1~3s**）；H3 分辨率 768P/2K、
//                            H3-Max 480P/768P（不支持 2K）。
//   MINIMAX_VIDEO_RESOLUTION 默认 768P（v1 端点：2.3 可选 768P/1080P；10s 仅 768P）
//   MINIMAX_VIDEO_V2_RESOLUTION 默认 768P（v2 端点：H3 可 2K；H3-Max 可 480P）
//   MINIMAX_VIDEO_DURATION   显式钉死时长（秒）；留空 = 按请求时长就近取端点允许值。
//                            想省钱/省时可用它钉死下限（H3 = 4）。
//   MINIMAX_VIDEO_API        auto|v1|v2，默认 auto（按模型名判断）
//
// 期限与配额：M Plan 订阅 Key 在「已有按量计费价格」的开放平台端点上按目录价扣减套餐额度
// （官方 FAQ），因此 /v1/image_generation、/v1/video_generation 可用；上游 1008（额度/余额
// 不足）、1026（内容审核）等错误原文透传，Agent/用户能看到真实原因，不吞错。
//
// 日志走 stderr（容器日志），stdout 不占用。

'use strict';

const http = require('node:http');

const CONFIG = {
  port: Number(process.env.MINIMAX_MEDIA_PORT || process.env.PORT || 8787),
  apiKey: String(process.env.MINIMAX_API_KEY || '').trim(),
  baseUrl: String(process.env.MINIMAX_BASE_URL || 'https://api.minimaxi.com').replace(/\/+$/, ''),
  imageModel: String(process.env.MINIMAX_IMAGE_MODEL || 'image-01').trim(),
  videoModel: String(process.env.MINIMAX_VIDEO_MODEL || 'MiniMax-Hailuo-2.3').trim(),
  videoResolution: String(process.env.MINIMAX_VIDEO_RESOLUTION || '768P').trim().toUpperCase(),
  videoDuration: Number(process.env.MINIMAX_VIDEO_DURATION || 0) || 0,
  videoApi: String(process.env.MINIMAX_VIDEO_API || 'auto').trim().toLowerCase(),
  video2Resolution: String(process.env.MINIMAX_VIDEO_V2_RESOLUTION || '768P').trim().toUpperCase(),
  imageTimeoutMs: Number(process.env.MINIMAX_IMAGE_TIMEOUT_MS || 300000),
  videoSubmitTimeoutMs: Number(process.env.MINIMAX_VIDEO_SUBMIT_TIMEOUT_MS || 60000),
  videoQueryTimeoutMs: Number(process.env.MINIMAX_VIDEO_QUERY_TIMEOUT_MS || 30000),
};

// MiniMax 图像端点支持的画幅（image-01）；超出集合的请求画幅省略不传，让上游取默认。
const IMAGE_ASPECTS = ['1:1', '16:9', '4:3', '3:2', '2:3', '3:4', '9:16', '21:9'];
// 供 size（"2048x1152"）→ 画幅就近映射用。
const ASPECT_RATIO_OF = {
  '1:1': 1,
  '16:9': 16 / 9,
  '4:3': 4 / 3,
  '3:2': 1.5,
  '2:3': 2 / 3,
  '3:4': 3 / 4,
  '9:16': 9 / 16,
  '21:9': 21 / 9,
};
// base_resp.status_code 常见值 → 人话（原文一并保留，排障要看上游原句）。
const API_ERROR_HINT = {
  1002: 'MiniMax 限流（RPM/TPM），稍后重试',
  1004: 'MiniMax 鉴权失败（Key 无效，或 Key 与 MINIMAX_BASE_URL 区域不匹配：国内站 key 走 api.minimaxi.com，国际站 key 走 api.minimax.io）',
  1008: 'MiniMax 额度/余额不足（M Plan 额度窗口耗尽，或积分包用尽）',
  1026: 'MiniMax 内容审核拦截（prompt 触发敏感内容）',
  1027: 'MiniMax 内容审核拦截（生成结果触发敏感内容）',
  2013: 'MiniMax 参数不合法（模型/时长/分辨率组合不被接受）',
  2049: 'MiniMax API Key 无效',
};

function log(...args) {
  console.error('[minimax-media]', ...args);
}

// 任务 id → 提交时用的端点版本（v1/v2）。daemon 轮询只带 id，不带端点信息，
// 所以提交时登记、查询时回查；进程重启后丢失也没关系——默认回落 v1（H3 需显式
// MINIMAX_VIDEO_API=v2，或 model 名本身就是 MiniMax-H3*）。
const TASKS = new Map();
const TASK_TTL_MS = 2 * 60 * 60 * 1000;

function rememberTask(id, api, model) {
  const now = Date.now();
  for (const [k, v] of TASKS) {
    if (now - v.at > TASK_TTL_MS) TASKS.delete(k);
  }
  TASKS.set(id, { api, model, at: now });
}

// ---------------------------------------------------------------------------
// 通用 HTTP 工具
// ---------------------------------------------------------------------------
function sendJson(res, status, body) {
  const payload = JSON.stringify(body);
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(payload),
  });
  res.end(payload);
}

function sendError(res, httpStatus, message, extra) {
  sendJson(res, httpStatus, { error: { message, type: 'minimax_media_error', ...(extra || {}) } });
}

async function readJsonBody(req, limitBytes = 32 * 1024 * 1024) {
  const chunks = [];
  let size = 0;
  for await (const chunk of req) {
    size += chunk.length;
    if (size > limitBytes) throw new Error(`请求体超过上限 ${limitBytes} 字节`);
    chunks.push(chunk);
  }
  if (!chunks.length) return {};
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

// 取用哪个 key：请求头 Bearer（daemon 侧 IMAGE_KEY/VIDEO_KEY）优先，环境变量兜底。
function keyFrom(req) {
  const auth = String(req.headers['authorization'] || '');
  const m = auth.match(/^Bearer\s+(.+)$/i);
  if (m && m[1].trim()) return m[1].trim();
  return CONFIG.apiKey;
}

// 调 MiniMax 开放平台；返回 { status, json }，不做错误解释（由调用方按 base_resp 判别）。
async function minimaxFetch(req, path, { method = 'GET', body, timeoutMs = CONFIG.videoQueryTimeoutMs } = {}) {
  const key = keyFrom(req);
  if (!key) {
    throw Object.assign(new Error('缺少 MiniMax Key：请在仓库根 .env 媒体段填 IMAGE_KEY / VIDEO_KEY（或本服务 MINIMAX_API_KEY）'), { httpStatus: 401 });
  }
  const url = `${CONFIG.baseUrl}${path}`;
  const init = {
    method,
    headers: { authorization: `Bearer ${key}` },
    signal: AbortSignal.timeout(timeoutMs),
  };
  if (body !== undefined) {
    init.headers['content-type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const resp = await fetch(url, init);
  const text = await resp.text();
  let json = null;
  try {
    json = text ? JSON.parse(text) : null;
  } catch {
    throw new Error(`MiniMax ${path} 返回非 JSON（HTTP ${resp.status}）：${text.slice(0, 200)}`);
  }
  return { status: resp.status, json };
}

// base_resp 逻辑错误 → 抛错（HTTP 200 也可能是业务失败，MiniMax 惯例）。
function assertBaseResp(json, what) {
  const br = json && json.base_resp;
  if (br && Number(br.status_code) !== 0) {
    const code = Number(br.status_code);
    const hint = API_ERROR_HINT[code] ? `（${API_ERROR_HINT[code]}）` : '';
    const err = new Error(`${what}失败 ${code}${hint}: ${br.status_msg || 'unknown'}`);
    err.httpStatus = code === 1004 || code === 2049 ? 401 : 502;
    err.minimaxCode = code;
    throw err;
  }
}

// size "2048x1152" → 最近的 MiniMax 画幅；显式 aspect_ratio 合法则直用。
function aspectFrom(size, aspectRatio) {
  const explicit = String(aspectRatio || '').trim();
  if (IMAGE_ASPECTS.includes(explicit)) return explicit;
  const m = String(size || '').match(/^(\d+)\s*[x×]\s*(\d+)$/i);
  if (!m) return '1:1';
  const w = Number(m[1]);
  const h = Number(m[2]);
  if (!w || !h) return '1:1';
  const target = w / h;
  let best = '1:1';
  let bestDiff = Infinity;
  for (const a of IMAGE_ASPECTS) {
    const diff = Math.abs(ASPECT_RATIO_OF[a] - target);
    if (diff < bestDiff) {
      bestDiff = diff;
      best = a;
    }
  }
  return best;
}

// ---------------------------------------------------------------------------
// 图像：POST /v1/images/generations（OpenAI 形状）
//   daemon（custom-image 渲染器 + 本地补丁）发来 { prompt, model, n, size, image? }，
//   其中 image 是 i2i 参考图 data URL（补丁把 /images/edits 改成 generations + image）。
//   MiniMax 侧：POST /v1/image_generation，i2i 用 subject_reference（人物参考）。
// ---------------------------------------------------------------------------
async function handleImageGeneration(req, res) {
  const body = await readJsonBody(req, 64 * 1024 * 1024);
  const prompt = String(body.prompt || '').trim();
  if (!prompt) return sendError(res, 400, 'prompt 不能为空');
  // 请求里的 model 只有在长得像 MiniMax 图像模型时才采用（daemon 可能传占位名）。
  const requested = String(body.model || '').trim();
  const model = /^image-/i.test(requested) ? requested : CONFIG.imageModel;
  const aspect = aspectFrom(body.size, body.aspect_ratio);
  const n = Math.min(Math.max(Number(body.n) || 1, 1), 9);

  const payload = {
    model,
    prompt,
    response_format: 'base64', // URL 形式 24h 过期；base64 落盘即持久
    n,
    aspect_ratio: aspect,
  };
  // i2i：MiniMax 的 subject_reference 是"人物/主体参考"（type 仅支持 character）。
  if (typeof body.image === 'string' && body.image.trim()) {
    payload.subject_reference = [{ type: 'character', image_file: body.image.trim() }];
  }

  const t0 = Date.now();
  const { status, json } = await minimaxFetch(req, '/v1/image_generation', {
    method: 'POST',
    body: payload,
    timeoutMs: CONFIG.imageTimeoutMs,
  });
  if (status < 200 || status >= 300) {
    return sendError(res, 502, `MiniMax 图像 HTTP ${status}: ${JSON.stringify(json).slice(0, 240)}`);
  }
  assertBaseResp(json, 'MiniMax 图像生成');

  const list = json && json.data && Array.isArray(json.data.image_base64) ? json.data.image_base64 : null;
  if (!list || !list.length) {
    return sendError(res, 502, `MiniMax 图像响应缺少 data.image_base64: ${JSON.stringify(json).slice(0, 240)}`);
  }
  log(`图像 ok model=${model} aspect=${aspect} n=${list.length} ${Date.now() - t0}ms`);
  return sendJson(res, 200, {
    created: Math.floor(Date.now() / 1000),
    model,
    data: list.map((b64) => ({ b64_json: b64 })),
  });
}

// ---------------------------------------------------------------------------
// 视频：POST /v1/videos/generations + GET /v1/videos/{id}（grok 线形状）
//   daemon（renderGrokVideo）发来 { model, prompt, duration, aspect_ratio, resolution, image? }。
//   - v1 端点（MiniMax-Hailuo-* / T2V-* / I2V-*）：submit 回 task_id，poll 查状态，
//     Success 后经 /v1/files/retrieve 换 download_url（有效期 1h，daemon 随即下载）。
//   - v2 端点（MiniMax-H3*，需按量付费 API）：content 数组 + /v2/query/video_generation/{id}，
//     成功响应自带 content.url，无需 retrieve。
// ---------------------------------------------------------------------------
function resolveVideoTarget(requestedModel) {
  const requested = String(requestedModel || '').trim();
  // daemon 传的是目录 id（grok-imagine-video）——不像 MiniMax 模型名就用配置默认值。
  const model = /^(MiniMax-|T2V-|I2V-)/i.test(requested) ? requested : CONFIG.videoModel;
  const api = CONFIG.videoApi === 'v1' || CONFIG.videoApi === 'v2'
    ? CONFIG.videoApi
    : (/^MiniMax-H3/i.test(model) ? 'v2' : 'v1');
  return { model, api };
}

// v1 时长只能是 6 或 10（768P）；1080P 仅 6s。就近取。
function v1DurationFor(requested, resolution) {
  if (CONFIG.videoDuration) return CONFIG.videoDuration;
  const want = Number(requested) || 6;
  if (resolution === '1080P') return 6;
  return want <= 8 ? 6 : 10;
}

function v1ResolutionFor() {
  return ['768P', '1080P', '720P'].includes(CONFIG.videoResolution) ? CONFIG.videoResolution : '768P';
}

// v2（H3 / H3-Max）时长：H3 支持 4~15s，H3-Max 支持 5~15s（官方文档：不支持 1~3s）。
// MINIMAX_VIDEO_DURATION 显式钉死时优先（省钱/省时场景常用下限 4）。
function v2DurationFor(requested, model) {
  const min = /H3-Max/i.test(model) ? 5 : 4;
  const want = CONFIG.videoDuration || Number(requested) || 5;
  return Math.min(Math.max(want, min), 15);
}

// v2 分辨率：H3 只有 768P / 2K；H3-Max 只有 480P / 768P（不支持 2K）——按模型纠正而非盲信 env。
function v2ResolutionFor(model) {
  const isMax = /H3-Max/i.test(model);
  const want = CONFIG.video2Resolution;
  if (isMax) return want === '480P' ? '480P' : '768P';
  return want === '2K' ? '2K' : '768P';
}

async function handleVideoSubmit(req, res) {
  const body = await readJsonBody(req, 64 * 1024 * 1024);
  const prompt = String(body.prompt || '').trim();
  if (!prompt) return sendError(res, 400, 'prompt 不能为空');
  const { model, api } = resolveVideoTarget(body.model);
  const image = typeof body.image === 'string' && body.image.trim() ? body.image.trim() : null;

  if (api === 'v2') {
    // 文档硬约束：文生视频（content 仅 text）时 ratio 必填且不能是 adaptive；
    // 图生视频（首帧）时宽高比由首帧决定、恒 adaptive（传别的值也会被忽略）。
    const requestedRatio = String(body.aspect_ratio || '');
    const explicitRatios = ['21:9', '16:9', '4:3', '1:1', '3:4', '9:16'];
    const ratio = image
      ? 'adaptive'
      : (explicitRatios.includes(requestedRatio) ? requestedRatio : '16:9');
    const content = [{ type: 'text', text: prompt }];
    if (image) content.push({ type: 'image_url', image_url: { url: image }, role: 'first_frame' });
    const payload = {
      model,
      content,
      duration: v2DurationFor(body.duration, model),
      resolution: v2ResolutionFor(model),
      ratio,
    };
    const { status, json } = await minimaxFetch(req, '/v2/video_generation', {
      method: 'POST',
      body: payload,
      timeoutMs: CONFIG.videoSubmitTimeoutMs,
    });
    if (status < 200 || status >= 300) {
      // v2 用 OpenAI 风格错误体
      const msg = (json && json.error && json.error.message) || JSON.stringify(json).slice(0, 240);
      return sendError(res, status === 401 ? 401 : 502, `MiniMax v2 视频提交失败 HTTP ${status}: ${msg}`);
    }
    const taskId = json && (json.task_id || (json.task && json.task.id) || json.id);
    if (!taskId) return sendError(res, 502, `MiniMax v2 未返回 task_id: ${JSON.stringify(json).slice(0, 200)}`);
    log(`视频 v2 已提交 model=${model} task=${taskId}`);
    rememberTask(String(taskId), 'v2', model);
    return sendJson(res, 200, { id: String(taskId), status: 'pending', api: 'v2', model });
  }

  const resolution = v1ResolutionFor();
  const payload = {
    model,
    prompt,
    duration: v1DurationFor(body.duration, resolution),
    resolution,
  };
  if (image) payload.first_frame_image = image; // i2v 首帧（data URL）
  const { status, json } = await minimaxFetch(req, '/v1/video_generation', {
    method: 'POST',
    body: payload,
    timeoutMs: CONFIG.videoSubmitTimeoutMs,
  });
  if (status < 200 || status >= 300) {
    return sendError(res, 502, `MiniMax 视频 HTTP ${status}: ${JSON.stringify(json).slice(0, 240)}`);
  }
  assertBaseResp(json, 'MiniMax 视频提交');
  if (!json || !json.task_id) {
    return sendError(res, 502, `MiniMax 视频未返回 task_id: ${JSON.stringify(json).slice(0, 200)}`);
  }
  log(`视频 v1 已提交 model=${model} ${payload.duration}s ${resolution}${image ? ' i2v' : ''} task=${json.task_id}`);
  rememberTask(String(json.task_id), 'v1', model);
  return sendJson(res, 200, { id: String(json.task_id), status: 'pending', api: 'v1', model });
}

const V1_DONE = new Set(['Success']);
const V1_FAIL = new Set(['Fail']);

async function handleVideoQuery(req, res, taskId, api) {
  if (api === 'v2') {
    const { status, json } = await minimaxFetch(req, `/v2/query/video_generation/${encodeURIComponent(taskId)}`);
    if (status < 200 || status >= 300) {
      return sendError(res, 502, `MiniMax v2 查询失败 HTTP ${status}: ${JSON.stringify(json).slice(0, 240)}`);
    }
    const task = (json && json.task) || json || {};
    const st = String(task.status || '').toLowerCase();
    if (st === 'succeeded') {
      const url = task.content && task.content.url;
      if (!url) return sendError(res, 502, `MiniMax v2 成功但缺 content.url: ${JSON.stringify(json).slice(0, 200)}`);
      return sendJson(res, 200, { status: 'done', video: { url }, width: null, height: null });
    }
    if (st === 'failed' || st === 'cancelled') {
      const e = task.error || {};
      return sendJson(res, 200, { status: 'failed', error: { message: e.message || st, code: e.code || st } });
    }
    return sendJson(res, 200, { status: 'processing' });
  }

  const { status, json } = await minimaxFetch(req, `/v1/query/video_generation?task_id=${encodeURIComponent(taskId)}`);
  if (status < 200 || status >= 300) {
    return sendError(res, 502, `MiniMax 查询失败 HTTP ${status}: ${JSON.stringify(json).slice(0, 240)}`);
  }
  assertBaseResp(json, 'MiniMax 视频状态查询');
  const st = String((json && json.status) || '');
  if (V1_DONE.has(st)) {
    const fileId = json && json.file_id;
    if (!fileId) return sendError(res, 502, `MiniMax 视频成功但缺 file_id: ${JSON.stringify(json).slice(0, 200)}`);
    const r = await minimaxFetch(req, `/v1/files/retrieve?file_id=${encodeURIComponent(fileId)}`);
    if (r.status < 200 || r.status >= 300) {
      return sendError(res, 502, `MiniMax 取文件失败 HTTP ${r.status}: ${JSON.stringify(r.json).slice(0, 240)}`);
    }
    assertBaseResp(r.json, 'MiniMax 文件下载地址');
    const url = r.json && r.json.file && r.json.file.download_url;
    if (!url) return sendError(res, 502, `MiniMax 文件无 download_url: ${JSON.stringify(r.json).slice(0, 200)}`);
    return sendJson(res, 200, {
      status: 'done',
      video: { url },
      width: json.video_width || null,
      height: json.video_height || null,
    });
  }
  if (V1_FAIL.has(st)) {
    return sendJson(res, 200, { status: 'failed', error: { message: 'MiniMax 视频任务失败（Fail）', code: 'Fail' } });
  }
  // Preparing / Queueing / Processing / 未知 → 继续轮询
  return sendJson(res, 200, { status: 'processing', upstream: st || 'unknown' });
}

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------
const server = http.createServer(async (req, res) => {
  const startedAt = Date.now();
  const url = new URL(req.url || '/', 'http://localhost');
  const path = url.pathname.replace(/\/+$/, '') || '/';
  try {
    if (req.method === 'GET' && (path === '/health' || path === '/')) {
      return sendJson(res, 200, {
        ok: true,
        baseUrl: CONFIG.baseUrl,
        imageModel: CONFIG.imageModel,
        videoModel: CONFIG.videoModel,
        videoApi: CONFIG.videoApi,
        keyConfigured: Boolean(CONFIG.apiKey),
      });
    }
    if (req.method === 'POST' && (path === '/v1/images/generations' || path === '/v1/images/edits')) {
      return await handleImageGeneration(req, res);
    }
    if (req.method === 'POST' && path === '/v1/videos/generations') {
      return await handleVideoSubmit(req, res);
    }
    const videoMatch = path.match(/^\/v1\/videos\/([^/]+)$/);
    if (req.method === 'GET' && videoMatch) {
      const id = decodeURIComponent(videoMatch[1]);
      // 端点版本优先级：查询参数 > 提交登记 > 配置。
      const known = TASKS.get(id);
      const api = String(url.searchParams.get('api') || (known && known.api) || (CONFIG.videoApi === 'v2' ? 'v2' : 'v1')).toLowerCase() === 'v2' ? 'v2' : 'v1';
      return await handleVideoQuery(req, res, id, api);
    }
    return sendError(res, 404, `未知路由 ${req.method} ${path}`);
  } catch (err) {
    const status = err && err.httpStatus ? err.httpStatus : 502;
    log(`${req.method} ${path} 失败 ${Date.now() - startedAt}ms:`, err && err.message);
    return sendError(res, status, String((err && err.message) || err));
  }
});

server.listen(CONFIG.port, '0.0.0.0', () => {
  log(`listening :${CONFIG.port} base=${CONFIG.baseUrl} image=${CONFIG.imageModel} video=${CONFIG.videoModel}(${CONFIG.videoApi}) key=${CONFIG.apiKey ? 'env' : 'header-only'}`);
});
