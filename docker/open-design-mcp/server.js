// open-design-mcp：open_design 守护进程 REST API 的 stdio MCP 桥（BMA ui_design 插件）。
//
// 调研实测结论（2026-08-17，doc/设计文档_插件范式.md §9）：
//   - 守护进程无 MCP 端点；媒体生成走 /api/projects/:id/media/generate（异步任务）。
//   - 媒体路由有 isLocalSameOrigin 校验：Host 头端口必须等于守护进程自身端口（7456），
//     与宿主映射端口无关——跨容器调用必须改写 Host 头（OD_HOST_HEADER）。
//   - /api/media/tasks/:id/wait 需 odtt_ 工具令牌（仅 OD 内部 chat run 可铸），外部不可用；
//     改用 GET /api/projects/:id/media/tasks?includeDone=1 轮询。
//   - Basic 认证：用户名 open-design，密码 = OD_API_TOKEN。
//
// stdout 只承载 MCP JSON-RPC 帧，日志一律 stderr。
import http from 'node:http';
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { PNG } from 'pngjs';
import { z } from 'zod';

const OD_API_URL = (process.env.OD_API_URL || 'http://host.docker.internal:7456').replace(/\/$/, '');
const OD_API_TOKEN = process.env.OD_API_TOKEN || '';
const OD_HOST_HEADER = process.env.OD_HOST_HEADER || 'localhost:7456';
// 默认模型 = od 守护进程模型目录 id（非上游模型名）。"custom-image" 走 daemon 的
// OpenAI 兼容 custom-image provider，真实模型名由 daemon media-config.json 的
// providers.custom-image.model 决定（BMA 默认 doubao-seedream-5.0-lite，
// 火山方舟 Agent Plan 网关唯一可用图像模型）。
const OD_DEFAULT_MODEL = process.env.OD_DEFAULT_MODEL || 'custom-image';
const OD_PROJECT_ID = process.env.OD_PROJECT_ID || 'bma-agent-artifacts';
// 批量生成并发上限：daemon 每 task 独立异步（无串行限制），上限只防上游配额打爆。
const OD_BATCH_CONCURRENCY = Math.max(1, Number(process.env.OD_BATCH_CONCURRENCY || 3));
// 白底抠除阈值：RGB 三通道均 >= 阈值的边缘连通区视为背景置透明。
// 240 档容忍 seedream 的近白底（250+）；图内白色本体（眼睛/高光/白萝卜身）因与边缘
// 不连通而保留。实证 2026-08-31：seedream 对"透明背景"提示词执行不稳定，
// tower/monster 系列整图白底，纯 prompt 不可靠，必须落盘前后处理。
const WHITE_BG_THRESHOLD = 240;
const OD_TIMEOUT_MS = Number(process.env.OD_TIMEOUT_MS || 180000);
// 容器内挂载的工作目录根（plugins.yaml volumes: ${WORKDIR}:/workspace）。
// save_as 相对路径以此解析；默认产物落其子目录 .bma/od-artifacts。
const WORKSPACE_DIR = (process.env.OD_WORKSPACE_DIR || '/workspace').replace(/\/$/, '');
const OUTPUT_DIR = process.env.OD_OUTPUT_DIR || `${WORKSPACE_DIR}/.bma/od-artifacts`;
// 返回给 Agent 的宿主侧相对路径前缀（相对 Agent 工作目录，与 plugins.yaml 的 volumes 映射对应）。
const OUTPUT_REL_PREFIX = (process.env.OD_OUTPUT_REL_PREFIX || '.bma/od-artifacts').replace(/\/$/, '');
const POLL_INTERVAL_MS = 2000;
const IMAGE_EXTS = ['.png', '.jpg', '.jpeg', '.webp', '.gif'];
const VIDEO_EXTS = ['.mp4', '.webm', '.mov'];

// ---- 视频生成（od_video_generate，2026-09-04）----
// 双协议兼容（设计 docs/superpowers/specs/2026-09-04-video-generation-design.md）：
//   - volcengine：daemon 内置 renderVolcengineVideo（方舟 tasks 协议），模型须为目录 id
//     （doubao-seedance-*）；wire 模型名可被 daemon 的 OD_MEDIA_MODEL_ALIASES 别名改写
//     （自定义接入点场景，env 在 open_design service 插件侧注入）。
//   - openai：daemon 内置 renderAIHubMixVideo 讲 OpenAI /videos 协议；模型 id 带
//     aihubmix- 前缀触发 daemon 目录旁路（任意模型名），用户给裸名时桥自动补前缀；
//     网关 baseUrl/key 由 daemon media-config providers.aihubmix 提供（桥侧不可见，
//     缺失时 daemon 任务 failed、错误原文透传）。
const OD_VIDEO_PROVIDER = (process.env.OD_VIDEO_PROVIDER || 'volcengine').toLowerCase();
const OD_VOLCENGINE_VIDEO_MODEL = process.env.OD_VOLCENGINE_VIDEO_MODEL || 'doubao-seedance-2-0-fast-260128';
const OD_OPENAI_VIDEO_MODEL = process.env.OD_OPENAI_VIDEO_MODEL || '';
// 视频 30s–5min（ark 队列峰值更久），远超图片的 180s 上限，独立超时。
const OD_VIDEO_TIMEOUT_MS = Number(process.env.OD_VIDEO_TIMEOUT_MS || 720000);

// resolveSaveAs 把 Agent 给的 save_as（宿主工作目录相对路径）解析为容器内绝对路径。
// 防路径穿越：拒绝绝对路径、盘符、`..` 上跳；统一为正斜杠。
function resolveSaveAs(saveAs) {
  const rel = String(saveAs).replace(/\\/g, '/').replace(/^\/+/, '').replace(/\/+$/, '');
  if (!rel || rel.startsWith('..') || rel.includes('/../') || /^[a-zA-Z]:/.test(rel)) {
    throw new Error(`save_as 非法（须为工作目录内的相对路径，不可含 .. 或盘符）: ${saveAs}`);
  }
  return { rel, abs: `${WORKSPACE_DIR}/${rel}` };
}

function log(...args) {
  console.error('[open-design-mcp]', ...args);
}

// odRequest 调用守护进程 REST API（node:http 原生实现，确保 Host 头可改写）。
// 返回 { status, buffer }；json() 解析 JSON，文本错误取 OD 的 error.message。
function odRequest(path, { method = 'GET', body, timeoutMs = 30000 } = {}) {
  return new Promise((resolve, reject) => {
    const url = new URL(OD_API_URL + path);
    const headers = { host: OD_HOST_HEADER };
    if (OD_API_TOKEN) {
      headers.authorization = 'Basic ' + Buffer.from(`open-design:${OD_API_TOKEN}`).toString('base64');
    }
    if (body !== undefined) {
      headers['content-type'] = 'application/json';
    }
    const req = http.request({
      hostname: url.hostname,
      port: url.port,
      path: url.pathname + url.search,
      method,
      headers,
      timeout: timeoutMs,
    }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({ status: res.statusCode, buffer: Buffer.concat(chunks) }));
    });
    req.on('timeout', () => req.destroy(new Error(`请求超时（${timeoutMs}ms）`)));
    req.on('error', reject);
    if (body !== undefined) req.write(JSON.stringify(body));
    req.end();
  });
}

async function odJson(path, opts = {}) {
  const { status, buffer } = await odRequest(path, opts);
  let data = null;
  try {
    data = JSON.parse(buffer.toString('utf8'));
  } catch {
    // 非 JSON 响应：保留原文截断。
  }
  if (status < 200 || status >= 300) {
    const raw = buffer.toString('utf8').slice(0, 400);
    const msg = data?.error?.message || data?.error || data?.message || raw || `HTTP ${status}`;
    throw new Error(`OD ${opts.method || 'GET'} ${path} → ${status}: ${typeof msg === 'string' ? msg : JSON.stringify(msg)}`);
  }
  return data;
}

// ensureProject 幂等建项目（409/重复时回查列表确认存在即可）。
async function ensureProject() {
  try {
    await odJson('/api/projects', {
      method: 'POST',
      body: { id: OD_PROJECT_ID, name: 'BMA Agent Artifacts' },
    });
    log('项目已创建:', OD_PROJECT_ID);
  } catch (err) {
    const list = await odJson('/api/projects');
    const projects = Array.isArray(list?.projects) ? list.projects : [];
    if (!projects.some((p) => p && p.id === OD_PROJECT_ID)) {
      throw err;
    }
  }
}

// extractTaskFile 从已完成任务的 file 元数据提取文件名（OD 各版本字段不固定，做宽容回退）。
function extractTaskFile(task) {
  const meta = task?.file;
  if (!meta) return null;
  if (typeof meta === 'string') return meta;
  return meta.fileName || meta.filename || meta.name || meta.path || null;
}

// newestImageFallback task.file 缺文件名时，取项目里 task 启动后最新修改的对应类型文件。
async function newestImageFallback(sinceMs, exts = IMAGE_EXTS) {
  const data = await odJson(`/api/projects/${OD_PROJECT_ID}/files`);
  const files = Array.isArray(data?.files) ? data.files : [];
  const images = files
    .filter((f) => exts.some((ext) => String(f?.name || f?.fileName || '').toLowerCase().endsWith(ext)))
    .filter((f) => !sinceMs || Number(f?.mtime || f?.updatedAt || f?.modifiedAt || 0) >= sinceMs - 5000)
    .sort((a, b) => Number(b?.mtime || b?.updatedAt || b?.modifiedAt || 0) - Number(a?.mtime || a?.updatedAt || a?.modifiedAt || 0));
  const first = images[0];
  return first ? String(first.name || first.fileName) : null;
}

// waitTask 轮询任务直至 done/failed。label 用于错误文案（"图像"/"视频"），
// listTool 为超时后引导查询的列表工具名。视频生成 30s–5min，超时由调用方传入。
async function waitTask(taskId, { timeoutMs = OD_TIMEOUT_MS, label = '图像', listTool = 'od_image_list' } = {}) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const data = await odJson(`/api/projects/${OD_PROJECT_ID}/media/tasks?includeDone=1`);
    const tasks = Array.isArray(data?.tasks) ? data.tasks : [];
    const task = tasks.find((t) => t?.taskId === taskId);
    if (task?.status === 'done') return task;
    if (task?.status === 'failed' || task?.status === 'interrupted') {
      const msg = task?.error?.message || JSON.stringify(task?.error || '未知错误');
      throw new Error(`${label}生成失败（${task.status}）：${msg}`);
    }
    if (Date.now() > deadline) {
      throw new Error(`等待生成超时（${timeoutMs}ms），taskId=${taskId}；可稍后调 ${listTool} 查看结果`);
    }
    await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
  }
}

// downloadFile 下载项目文件字节（依次尝试候选端点，OD 各版本路由有差异）。
async function downloadFile(name) {
  const encoded = encodeURIComponent(name);
  const candidates = [
    `/api/projects/${OD_PROJECT_ID}/files/${encoded}?download=1`,
    `/api/projects/${OD_PROJECT_ID}/files/${encoded}`,
    `/api/projects/${OD_PROJECT_ID}/raw/${encoded}`,
  ];
  let lastErr = null;
  for (const path of candidates) {
    try {
      const { status, buffer } = await odRequest(path, { timeoutMs: 60000 });
      const ctypeLooksJson = buffer.length < 4096 && buffer.toString('utf8', 0, 1) === '{';
      if (status >= 200 && status < 300 && !ctypeLooksJson) {
        return buffer;
      }
      lastErr = new Error(`${path} → ${status}（疑似 JSON 错误响应或非文件内容）`);
    } catch (err) {
      lastErr = err;
    }
  }
  throw lastErr || new Error('全部下载端点不可用');
}

function slugify(prompt) {
  const slug = prompt.slice(0, 24).replace(/[^\w一-鿿-]+/g, '_').replace(/^_+|_+$/g, '');
  return slug || 'image';
}

// removeWhiteBackground 白底抠除：从图片四边出发 flood-fill，凡 RGB 三通道均
// >= WHITE_BG_THRESHOLD 且与边缘连通的像素置全透明。边缘连通保证只抠"背景"，
// 图内白色本体（白萝卜身/眼睛高光/白色图案）与边缘隔着描边不连通、得以保留。
// 非透明 PNG / 解码失败抛错由调用方兜底保留原图。
function removeWhiteBackground(buf) {
  const png = PNG.sync.read(buf);
  const { width: w, height: h, data } = png;
  const nearWhite = (i) =>
    data[i] >= WHITE_BG_THRESHOLD && data[i + 1] >= WHITE_BG_THRESHOLD &&
    data[i + 2] >= WHITE_BG_THRESHOLD && data[i + 3] !== 0;
  const visited = new Uint8Array(w * h);
  const stack = [];
  for (let x = 0; x < w; x++) stack.push(x, 0, x, h - 1);
  for (let y = 0; y < h; y++) stack.push(0, y, w - 1, y);
  while (stack.length) {
    const y = stack.pop();
    const x = stack.pop();
    if (x < 0 || y < 0 || x >= w || y >= h) continue;
    const p = y * w + x;
    if (visited[p]) continue;
    visited[p] = 1;
    const i = p * 4;
    if (!nearWhite(i)) continue;
    data[i + 3] = 0;
    stack.push(x + 1, y, x - 1, y, x, y + 1, x, y - 1);
  }
  return PNG.sync.write(png);
}

async function generateImage({ prompt, aspect, model, save_as: saveAs, remove_bg: removeBg }) {
  if (!OD_API_TOKEN) {
    throw new Error('未配置 OD_API_TOKEN（插件 settings.env 缺失）；请检查 .env 后重启后端');
  }
  // 先校验 save_as（fail fast：路径非法不必浪费一次生成）。
  const saveTarget = saveAs ? resolveSaveAs(saveAs) : null;
  const useModel = model || OD_DEFAULT_MODEL;
  const startedAtMs = Date.now();
  await ensureProject();
  const gen = await odJson(`/api/projects/${OD_PROJECT_ID}/media/generate`, {
    method: 'POST',
    body: { surface: 'image', model: useModel, prompt, ...(aspect ? { aspect } : {}) },
  });
  const taskId = gen?.taskId;
  if (!taskId) throw new Error(`OD 未返回 taskId：${JSON.stringify(gen).slice(0, 300)}`);
  log('生成任务已受理:', taskId, 'model=', useModel);
  const task = await waitTask(taskId);
  const fileName = (await extractTaskFile(task)) || (await newestImageFallback(startedAtMs));
  if (!fileName) throw new Error('任务完成但未找到产物文件名');
  const bytes = await downloadFile(fileName);
  // 白底抠除（默认开）：seedream 对"透明背景"提示词执行不稳定（实证 tower/monster
  // 系列整图白底），纯 prompt 不可靠，落盘前 flood-fill 边缘连通白区置透明。
  // remove_bg=false 跳过（满幅背景图如 9:16 场景底图必须保留背景，勿开）。
  let outBytes = bytes;
  let bgRemoved = false;
  if (removeBg !== false) {
    try {
      outBytes = removeWhiteBackground(bytes);
      bgRemoved = outBytes !== bytes;
    } catch (err) {
      log('背景去除失败（保留原图）:', err.message);
      outBytes = bytes;
    }
  }
  const ext = (fileName.match(/\.[a-z0-9]+$/i)?.[0]) || '.png';
  let absPath;
  let relPath;
  if (saveTarget) {
    // 正式素材：直落 Agent 指定的工作目录相对路径（如 assets/img/mon-zombie.png），
    // 一图一份，无需事后复制重命名。无扩展名时补生成文件的实际扩展名。
    relPath = /\.[a-z0-9]+$/i.test(saveTarget.rel) ? saveTarget.rel : saveTarget.rel + ext;
    absPath = `${WORKSPACE_DIR}/${relPath}`;
  } else {
    // 缺省（草稿/临时）：时间戳 + prompt slug 落 .bma/od-artifacts。
    const outName = `${new Date().toISOString().replace(/[:.]/g, '').slice(0, 15)}-${slugify(prompt)}${ext}`;
    relPath = `${OUTPUT_REL_PREFIX}/${outName}`;
    absPath = join(OUTPUT_DIR, outName);
  }
  mkdirSync(dirname(absPath), { recursive: true });
  writeFileSync(absPath, outBytes);
  const elapsedSec = Math.round((Date.now() - startedAtMs) / 1000);
  log('产物已落盘:', relPath, `${outBytes.length}B${bgRemoved ? '（白底已抠除）' : ''}`, `${elapsedSec}s`);
  return {
    path: relPath,
    filename: relPath.split('/').pop(),
    bytes: outBytes.length,
    bg_removed: bgRemoved,
    model: useModel,
    aspect: aspect || null,
    elapsedSec,
    hint: '在 HTML/CSS/JS 中以该相对路径（path 字段）引用此图片',
  };
}

// generateImageBatch 批量生成：对 prompts 数组并发生成，上限 OD_BATCH_CONCURRENCY
//（默认 3，防打爆上游配额）。实证 2026-08-31：daemon /media/generate 建 task 后
// generateMedia 后台异步跑、HTTP 立即回 taskId（routes/media.js），无队列无锁——
// 旧"daemon 单任务语义"注释不成立，纯串行白等（23 张 × 30s ≈ 11.5min，并发 3 压到 ~4min）。
// 单张失败不中断整批，结果逐项标注 path 或 error，由 Agent 决定重试/降级。
async function generateImageBatch({ prompts, aspect, model, save_as_list: saveAsList, remove_bg: removeBg }) {
  if (!OD_API_TOKEN) {
    throw new Error('未配置 OD_API_TOKEN（插件 settings.env 缺失）；请检查 .env 后重启后端');
  }
  if (!Array.isArray(prompts) || prompts.length === 0) {
    throw new Error('prompts 为空数组：批量模式至少提供 1 个 prompt');
  }
  if (saveAsList !== undefined && saveAsList.length !== prompts.length) {
    throw new Error(`save_as_list 长度（${saveAsList.length}）须与 prompts（${prompts.length}）等长`);
  }
  const startedAtMs = Date.now();
  const results = new Array(prompts.length);
  let next = 0;
  let settled = 0;
  async function worker() {
    while (next < prompts.length) {
      const i = next++;
      const prompt = prompts[i];
      // save_as_list 元素为空串 = 该项走默认 .bma/od-artifacts 命名。
      const saveAs = saveAsList?.[i] || undefined;
      try {
        const r = await generateImage({ prompt, aspect, model, save_as: saveAs, remove_bg: removeBg });
        results[i] = { index: i, prompt, path: r.path, filename: r.filename, bytes: r.bytes };
        log(`批量进度 ${++settled}/${prompts.length} 完成:`, r.path);
      } catch (err) {
        log(`批量进度 ${++settled}/${prompts.length} 失败:`, err.message);
        results[i] = { index: i, prompt, error: err.message };
      }
    }
  }
  await Promise.all(
    Array.from({ length: Math.min(OD_BATCH_CONCURRENCY, prompts.length) }, () => worker())
  );
  const succeeded = results.filter((r) => r.path).length;
  return {
    total: prompts.length,
    succeeded,
    failed: prompts.length - succeeded,
    model: model || OD_DEFAULT_MODEL,
    aspect: aspect || null,
    elapsedSec: Math.round((Date.now() - startedAtMs) / 1000),
    results,
    hint: '在 HTML/CSS/JS 中以各项结果的相对路径（path 字段）引用图片；带 error 的项需换 prompt 重试或人工处理',
  };
}

function resolveVideoProvider(provider) {
  const p = String(provider || OD_VIDEO_PROVIDER).toLowerCase();
  if (p !== 'volcengine' && p !== 'openai') {
    throw new Error(`未知视频 provider：${p}（可选 volcengine / openai；对应网关协议分别为方舟 tasks 与 OpenAI /videos）`);
  }
  return p;
}

function resolveVideoModel(provider, model) {
  if (provider === 'openai') {
    const m = model || OD_OPENAI_VIDEO_MODEL;
    if (!m) {
      throw new Error('provider=openai 但未配置视频模型：.env 填 VIDEO_OPENAI_MODEL（你的 /videos 网关模型名，如 veo-3.1-fast）后 POST /api/plugins/reload');
    }
    return m.startsWith('aihubmix-') ? m : `aihubmix-${m}`;
  }
  return model || OD_VOLCENGINE_VIDEO_MODEL;
}

// generateVideo 文生视频（t2v）：daemon media/generate surface='video'，length 传时长秒数。
// 无白底抠除（图片专属）；产物 mp4 经 downloadFile 落盘，路径语义与 generateImage 一致。
async function generateVideo({ prompt, aspect, duration_sec: durationSec, provider, model, save_as: saveAs }) {
  if (!OD_API_TOKEN) {
    throw new Error('未配置 OD_API_TOKEN（插件 settings.env 缺失）；请检查 .env 后重启后端');
  }
  // 先校验 save_as（fail fast：路径非法不必浪费一次生成）。
  const saveTarget = saveAs ? resolveSaveAs(saveAs) : null;
  const useProvider = resolveVideoProvider(provider);
  const useModel = resolveVideoModel(useProvider, model);
  const startedAtMs = Date.now();
  await ensureProject();
  const gen = await odJson(`/api/projects/${OD_PROJECT_ID}/media/generate`, {
    method: 'POST',
    body: {
      surface: 'video',
      model: useModel,
      prompt,
      ...(aspect ? { aspect } : {}),
      ...(durationSec ? { length: durationSec } : {}),
    },
  });
  const taskId = gen?.taskId;
  if (!taskId) throw new Error(`OD 未返回 taskId：${JSON.stringify(gen).slice(0, 300)}`);
  log('视频任务已受理:', taskId, 'provider=', useProvider, 'model=', useModel);
  const task = await waitTask(taskId, { timeoutMs: OD_VIDEO_TIMEOUT_MS, label: '视频', listTool: 'od_video_list' });
  const fileName = (await extractTaskFile(task)) || (await newestImageFallback(startedAtMs, VIDEO_EXTS));
  if (!fileName) throw new Error('任务完成但未找到产物文件名');
  const bytes = await downloadFile(fileName);
  const ext = (fileName.match(/\.[a-z0-9]+$/i)?.[0]) || '.mp4';
  let absPath;
  let relPath;
  if (saveTarget) {
    relPath = /\.[a-z0-9]+$/i.test(saveTarget.rel) ? saveTarget.rel : saveTarget.rel + ext;
    absPath = `${WORKSPACE_DIR}/${relPath}`;
  } else {
    const outName = `${new Date().toISOString().replace(/[:.]/g, '').slice(0, 15)}-${slugify(prompt)}${ext}`;
    relPath = `${OUTPUT_REL_PREFIX}/${outName}`;
    absPath = join(OUTPUT_DIR, outName);
  }
  mkdirSync(dirname(absPath), { recursive: true });
  writeFileSync(absPath, bytes);
  const elapsedSec = Math.round((Date.now() - startedAtMs) / 1000);
  log('视频已落盘:', relPath, `${bytes.length}B`, `${elapsedSec}s`);
  return {
    path: relPath,
    filename: relPath.split('/').pop(),
    bytes: bytes.length,
    provider: useProvider,
    model: useModel,
    duration_sec: durationSec || 5,
    elapsedSec,
    hint: '在 HTML 中以该相对路径（path 字段）用 <video src> 引用此视频',
  };
}

// listMediaFiles 列项目文件按扩展名过滤（图片/视频共用）。
async function listMediaFiles({ limit, exts, key }) {
  if (!OD_API_TOKEN) {
    throw new Error('未配置 OD_API_TOKEN（插件 settings.env 缺失）；请检查 .env 后重启后端');
  }
  await ensureProject();
  const data = await odJson(`/api/projects/${OD_PROJECT_ID}/files`);
  const files = Array.isArray(data?.files) ? data.files : [];
  const items = files
    .filter((f) => exts.some((ext) => String(f?.name || f?.fileName || '').toLowerCase().endsWith(ext)))
    .map((f) => ({
      name: String(f.name || f.fileName),
      size: Number(f.size || f.bytes || 0),
      updatedAt: f.mtime || f.updatedAt || f.modifiedAt || null,
    }))
    .sort((a, b) => Number(b.updatedAt || 0) - Number(a.updatedAt || 0))
    .slice(0, limit || 20);
  return { project: OD_PROJECT_ID, count: items.length, [key]: items };
}

async function listImages({ limit }) {
  return listMediaFiles({ limit, exts: IMAGE_EXTS, key: 'images' });
}

async function listVideos({ limit }) {
  return listMediaFiles({ limit, exts: VIDEO_EXTS, key: 'videos' });
}

const server = new McpServer({ name: 'open-design-mcp', version: '0.1.0' });

server.registerTool('od_image_generate', {
  title: 'Open Design 图像生成',
  description:
    '经本地 open_design 守护进程生成图片（默认火山方舟 Seedream）。' +
    '【批量优先】帧序列/多素材场景（如塔防游戏的 6 塔 × 4 帧动画、整套怪物贴图）' +
    '务必用 prompts 数组一次提交整批：桥内并发生成（上限 ' + OD_BATCH_CONCURRENCY + '，' +
    'OD_BATCH_CONCURRENCY 可调），一次调用拿全部结果；' +
    '逐张单独调用会在每两张之间多夹一轮 LLM 往返，墙钟数倍放大。' +
    '单张失败不中断整批，结果 results 数组逐项标注 path 或 error。' +
    '正式素材务必指定落盘路径：单张用 save_as，批量用 save_as_list（与 prompts 等长，' +
    '元素空串表示该项走默认命名），给工作目录相对路径（如 assets/img/mon-zombie.png），' +
    '图片直落该路径、一图一份，结果 path 字段即可在 HTML/CSS/JS 中直接引用；' +
    '禁止事后再复制/重命名出第二份。缺省落盘 .bma/od-artifacts（时间戳命名，仅适合草稿/临时用途）。' +
    '产物默认白底抠除（remove_bg=true）：边缘连通白色背景自动置透明，贴图/图标/精灵开箱即用；' +
    '满幅背景图（9:16 场景底图等）必须传 remove_bg=false 保留背景。' +
    '单张生成约需 10–60 秒，请勿重复提交相同 prompt。',
  inputSchema: {
    prompt: z.string().min(1).optional().describe(
      '单张模式的图像描述（建议具体描述主体/风格/配色/构图）。与 prompts 二选一；同时给 prompts 时以 prompts 为准'),
    prompts: z.array(z.string().min(1)).min(1).optional().describe(
      '批量模式：prompt 数组，每个元素各生成一张（桥内并发生成，上限 OD_BATCH_CONCURRENCY 默认 3）。帧序列/多素材场景优先用此参数一次提交，' +
      '避免逐张调用各产生一轮 LLM 往返。与 prompt 二选一；空数组报错'),
    aspect: z.enum(['1:1', '16:9', '9:16', '4:3', '3:4']).optional().describe('画幅比例（批量时整批共用），缺省由模型决定'),
    model: z.string().optional().describe(`覆盖默认模型（缺省 ${OD_DEFAULT_MODEL}）`),
    save_as: z.string().optional().describe(
      '单张模式产物落盘的工作目录相对路径（如 assets/img/mon-zombie.png）。目录自动创建；' +
      '无扩展名时按生成文件实际格式补齐；不可含 .. 或盘符。命名须与代码加载约定一致'),
    save_as_list: z.array(z.string()).optional().describe(
      '批量模式落盘路径数组，须与 prompts 等长；元素为工作目录相对路径（同 save_as 规则），' +
      '空串表示该项用默认 .bma/od-artifacts 命名。仅配合 prompts 使用'),
    remove_bg: z.boolean().optional().describe(
      '白底抠除（默认 true）：落盘前把边缘连通的白色背景置为透明（贴图/图标/精灵类需要）。' +
      '满幅背景图（如 9:16 场景底图、整页背景）必须传 false 保留背景，否则会被抠透。' +
      '仅对 PNG 生效，非 PNG 产物自动跳过'),
  },
}, async (args) => {
  try {
    let result;
    if (args.prompts !== undefined) {
      // 批量模式：prompts 优先于 prompt（两者同时给时按批量处理）。
      result = await generateImageBatch(args);
    } else if (args.prompt) {
      if (args.save_as_list !== undefined) {
        throw new Error('save_as_list 仅配合 prompts 批量使用；单张请用 save_as');
      }
      result = await generateImage(args);
    } else {
      throw new Error('prompt 与 prompts 至少提供其一（帧序列/多素材场景优先 prompts 一次提交整批）');
    }
    return { content: [{ type: 'text', text: JSON.stringify(result, null, 2) }] };
  } catch (err) {
    return { content: [{ type: 'text', text: `od_image_generate 失败: ${err.message}` }], isError: true };
  }
});

server.registerTool('od_image_list', {
  title: 'Open Design 已生成图片列表',
  description: '列出 BMA 专用 OD 项目中已生成的图片文件（名称/大小/更新时间），用于复用历史产物。',
  inputSchema: {
    limit: z.number().int().min(1).max(100).optional().describe('返回条数上限，缺省 20'),
  },
}, async (args) => {
  try {
    const result = await listImages(args);
    return { content: [{ type: 'text', text: JSON.stringify(result, null, 2) }] };
  } catch (err) {
    return { content: [{ type: 'text', text: `od_image_list 失败: ${err.message}` }], isError: true };
  }
});

server.registerTool('od_video_generate', {
  title: 'Open Design 视频生成',
  description:
    '经本地 open_design 守护进程生成短视频（文生视频 t2v）。' +
    'provider 二选一：volcengine=方舟 tasks 协议网关（模型为 daemon 目录 id，自定义接入点由 daemon 的 OD_MEDIA_MODEL_ALIASES 别名改写）；' +
    'openai=OpenAI /videos 协议网关（模型名任意，需 .env 配 VIDEO_OPENAI_URL/VIDEO_OPENAI_MODEL/VIDEO_OPENAI_API_KEY）。' +
    '正式素材务必用 save_as 直落工作目录相对路径（如 assets/video/intro.mp4），一视频一份；缺省落 .bma/od-artifacts（时间戳命名，仅草稿）。' +
    '单条约 30 秒–5 分钟，请勿重复提交相同 prompt；超时（默认 720s）后可调 od_video_list 查看结果。',
  inputSchema: {
    prompt: z.string().min(1).describe('视频描述（建议具体描述主体/动作/镜头运动/风格）'),
    aspect: z.enum(['1:1', '16:9', '9:16', '4:3', '3:4']).optional().describe('画幅比例，缺省由模型决定'),
    duration_sec: z.number().int().min(2).max(15).optional().describe('时长秒数，缺省 5；daemon 自动 snap 到模型允许桶（Veo 4/6/8、Sora 4/8/12、Seedance 常用 5/10）'),
    provider: z.enum(['volcengine', 'openai']).optional().describe(`网关协议，缺省 ${OD_VIDEO_PROVIDER}（env OD_VIDEO_PROVIDER 可改）`),
    model: z.string().optional().describe('覆盖默认模型 id（openai 侧给裸模型名即可，桥自动补 aihubmix- 前缀；volcengine 侧须为 daemon 目录 id）'),
    save_as: z.string().optional().describe('产物落盘的工作目录相对路径（如 assets/video/intro.mp4）。目录自动创建；无扩展名时补实际格式；不可含 .. 或盘符'),
  },
}, async (args) => {
  try {
    const result = await generateVideo(args);
    return { content: [{ type: 'text', text: JSON.stringify(result, null, 2) }] };
  } catch (err) {
    return { content: [{ type: 'text', text: `od_video_generate 失败: ${err.message}` }], isError: true };
  }
});

server.registerTool('od_video_list', {
  title: 'Open Design 已生成视频列表',
  description: '列出 BMA 专用 OD 项目中已生成的视频文件（名称/大小/更新时间），用于复用历史产物。',
  inputSchema: {
    limit: z.number().int().min(1).max(100).optional().describe('返回条数上限，缺省 20'),
  },
}, async (args) => {
  try {
    const result = await listVideos(args);
    return { content: [{ type: 'text', text: JSON.stringify(result, null, 2) }] };
  } catch (err) {
    return { content: [{ type: 'text', text: `od_video_list 失败: ${err.message}` }], isError: true };
  }
});

await server.connect(new StdioServerTransport());
log(`stdio MCP server 已就绪（OD_API_URL=${OD_API_URL}, project=${OD_PROJECT_ID}）`);
