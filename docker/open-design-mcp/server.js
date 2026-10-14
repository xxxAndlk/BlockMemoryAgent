// open-design-mcp：open_design 守护进程 REST API 的 stdio MCP 桥（BMA ui_design 插件）。
//
// 调研实测结论（2026-08-17，doc/设计文档_插件范式.md §9）：
//   - 守护进程无 MCP 端点；媒体生成走 /api/projects/:id/media/generate（异步任务）。
//   - 媒体路由有 isLocalSameOrigin 校验：Host 头端口必须等于守护进程自身端口（7456），
//     与宿主映射端口无关——跨容器调用必须改写 Host 头（OD_HOST_HEADER）。
//   - /api/media/tasks/:id/wait 需 odtt_ 工具令牌（仅 OD 内部 chat run 可铸），外部不可用；
//     改用 GET /api/projects/:id/media/tasks?includeDone=1 轮询。
//   - Basic 认证：用户名 open-design，密码 = OD_API_TOKEN。
//   - 参考图/首帧（2026-10-14）：daemon /media/generate 原生接收 image（单张，项目内
//     相对路径）→ 图片路径 renderCustomOpenAIImage 自动切 /images/edits（图生图）、
//     视频两条 renderer 把它当首帧（i2v）。桥须先把工作区文件经
//     POST /api/projects/:id/files 上传进 daemon 项目——**必须 multipart**（全局
//     express.json 上限 4mb，JSON base64 传不了大图）。
//
// stdout 只承载 MCP JSON-RPC 帧，日志一律 stderr。
import http from 'node:http';
import { createHash, randomBytes } from 'node:crypto';
import { mkdirSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { dirname, extname, join } from 'node:path';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { PNG } from 'pngjs';
import { z } from 'zod';

const OD_API_URL = (process.env.OD_API_URL || 'http://host.docker.internal:7456').replace(/\/$/, '');
const OD_API_TOKEN = process.env.OD_API_TOKEN || '';
const OD_HOST_HEADER = process.env.OD_HOST_HEADER || 'localhost:7456';
// 图像默认模型（daemon 模型目录 id，非上游模型名）：env 显式设置最优先（逃生舱）；
// 未设置时走 fetchMediaDefaults() 向 daemon 探测（media-config 槽位 model 非空 =
// "用户选了这条通道"）。"custom-image" = OpenAI 兼容 custom-image provider 的占位条目，
// 真实模型名由 providers.custom-image.model 决定（配置优先型渲染器）。
// 2026-10-14 媒体配置单点化：真实模型/网关/key 的唯一定义处 = 仓库根 .env 媒体段。
const OD_DEFAULT_MODEL_ENV = (process.env.OD_DEFAULT_MODEL || '').trim();
// 单点探测的硬兜底（daemon 不可达/槽位未配置时 = 单点化之前的默认行为）。
const IMAGE_FALLBACK_MODEL_ID = 'custom-image';
const NANOBANANA_FALLBACK_MODEL_ID = 'gemini-3.1-flash-image-preview'; // 对齐 daemon NANOBANANA_DEFAULT_MODEL
const VIDEO_FALLBACK_PROVIDER = 'volcengine';
const VOLCENGINE_FALLBACK_VIDEO_MODEL = 'doubao-seedance-2-0-fast-260128';
const AIHUBMIX_PLACEHOLDER_VIDEO_MODEL = 'aihubmix-bma';
const OD_PROJECT_ID = process.env.OD_PROJECT_ID || 'bma-agent-artifacts';
// 批量生成并发上限：daemon 每 task 独立异步（无串行限制），上限只防上游配额打爆。
const OD_BATCH_CONCURRENCY = Math.max(1, Number(process.env.OD_BATCH_CONCURRENCY || 3));
// 白底抠除阈值：RGB 三通道均 >= 阈值的边缘连通区视为背景置透明。
// 240 档容忍 seedream 的近白底（250+）；图内白色本体（眼睛/高光/白萝卜身）因与边缘
// 不连通而保留。实证 2026-08-31：seedream 对"透明背景"提示词执行不稳定，
// tower/monster 系列整图白底，纯 prompt 不可靠，必须落盘前后处理。
const WHITE_BG_THRESHOLD = 240;
// 单张生图等待上限：seedream-5.0-lite 时代实测 10–60s（默认曾为 180s），2026-10-14
// 切 5.0-pro 后实测 1.5–4.5 分钟（含排队），180s 会掐断——默认提到 10 分钟
//（仍 < plugins.yaml 的 exec_timeout_sec 1200）。
const OD_TIMEOUT_MS = Number(process.env.OD_TIMEOUT_MS || 600000);
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
// 2026-10-14 单点化后：三项 env 仅作逃生舱（显式设置最优先）；未设置时向 daemon 探测
// （providers.aihubmix.model / providers.volcengine.model 非空即选定对应 wire）。
const OD_VIDEO_PROVIDER_ENV = (process.env.OD_VIDEO_PROVIDER || '').trim().toLowerCase();
const OD_VOLCENGINE_VIDEO_MODEL_ENV = (process.env.OD_VOLCENGINE_VIDEO_MODEL || '').trim();
const OD_OPENAI_VIDEO_MODEL_ENV = (process.env.OD_OPENAI_VIDEO_MODEL || '').trim();
// 视频 30s–5min（ark 队列峰值更久），远超图片的 180s 上限，独立超时。
const OD_VIDEO_TIMEOUT_MS = Number(process.env.OD_VIDEO_TIMEOUT_MS || 720000);

// ---- 媒体默认值探测（2026-10-14 配置单点化）----
// 真实模型/网关/key 的唯一定义处 = 仓库根 .env 媒体段（compose env_file → entrypoint
// 排他写入 media-config.json）。桥在 env 未显式设置时向 daemon 探测"用户选了哪条通道"
// ——判定键是 providers.<槽>.model 非空，**不能用 configured**（LLM 的 OPENAI_API_KEY
// 会让 openai.configured 恒为 true 假阳性）。60s 进程内缓存 + 并发去重 + 失败负缓存；
// 失败静默回落硬默认（绝不阻断生成）。
const MEDIA_DEFAULTS_TTL_MS = 60000;
let mediaDefaultsLastGood = null; // { value, at }
let mediaDefaultsRetryAfter = 0;
let mediaDefaultsInFlight = null;

async function probeMediaDefaults() {
  const [cfg, models] = await Promise.all([
    odJson('/api/media/config', { timeoutMs: 5000 }),
    odJson('/api/media/models', { timeoutMs: 5000 }).catch(() => null), // 目录失败不致命
  ]);
  const providers = (cfg && cfg.providers) || {};
  const modelOf = (id) => String((providers[id] && providers[id].model) || '').trim();
  let imageModel = '';
  if (modelOf('custom-image')) {
    imageModel = 'custom-image';
  } else if (modelOf('nanobanana')) {
    // nanobanana 是 provider id 不是模型 id；桥须传具体目录模型 id（随 daemon 版本变化 → 动态取）。
    const list = Array.isArray(models && models.image) ? models.image : [];
    const hit = list.find((m) => m && m.provider === 'nanobanana' && Array.isArray(m.caps) && m.caps.includes('t2i'));
    imageModel = (hit && hit.id) || NANOBANANA_FALLBACK_MODEL_ID;
  }
  let videoProvider = '';
  let videoModel = '';
  if (modelOf('aihubmix')) {
    // openai wire：renderAIHubMixVideo 配置优先（credentials.model 恒胜于桥传值），桥传
    // 占位即可。禁止用目录 id（如 veo-3）当占位——provider=google 会落 stub 假成功；
    // 占位自带 aihubmix- 前缀，被清空时上游报 model 'bma' not found，失败可见。
    videoProvider = 'openai';
    videoModel = AIHUBMIX_PLACEHOLDER_VIDEO_MODEL;
  } else if (modelOf('volcengine')) {
    videoProvider = 'volcengine';
    videoModel = modelOf('volcengine'); // volcengine 视频纯用桥传值，须目录 id/别名可解析
  }
  return { imageModel, videoProvider, videoModel };
}

// fetchMediaDefaults 带缓存/去重/静默失败的探测入口；永不抛（daemon 未起时返回 null）。
async function fetchMediaDefaults() {
  const now = Date.now();
  if (mediaDefaultsLastGood && now - mediaDefaultsLastGood.at < MEDIA_DEFAULTS_TTL_MS) {
    return mediaDefaultsLastGood.value;
  }
  if (now < mediaDefaultsRetryAfter) {
    return mediaDefaultsLastGood ? mediaDefaultsLastGood.value : null;
  }
  if (mediaDefaultsInFlight) return mediaDefaultsInFlight;
  mediaDefaultsInFlight = (async () => {
    try {
      const value = await probeMediaDefaults();
      mediaDefaultsLastGood = { value, at: Date.now() };
      return value;
    } catch (err) {
      mediaDefaultsRetryAfter = Date.now() + MEDIA_DEFAULTS_TTL_MS;
      log('媒体默认值探测失败（静默回退硬默认）:', err.message);
      return mediaDefaultsLastGood ? mediaDefaultsLastGood.value : null;
    } finally {
      mediaDefaultsInFlight = null;
    }
  })();
  return mediaDefaultsInFlight;
}

// resolveWorkspaceRel 把工作目录相对路径解析为容器内绝对路径（save_as 与
// reference/first_frame 读侧共用）。防路径穿越：拒绝绝对路径、盘符、`..` 上跳；
// 统一为正斜杠。label 进错误文案（save_as / reference / first_frame）。
function resolveWorkspaceRel(raw, label) {
  const rel = String(raw).replace(/\\/g, '/').replace(/^\/+/, '').replace(/\/+$/, '');
  if (!rel || rel.startsWith('..') || rel.includes('/../') || /^[a-zA-Z]:/.test(rel)) {
    throw new Error(`${label} 非法（须为工作目录内的相对路径，不可含 .. 或盘符）: ${raw}`);
  }
  return { rel, abs: `${WORKSPACE_DIR}/${rel}` };
}

// resolveSaveAs 把 Agent 给的 save_as（宿主工作目录相对路径）解析为容器内绝对路径。
function resolveSaveAs(saveAs) {
  return resolveWorkspaceRel(saveAs, 'save_as');
}

// ---- 参考图/首帧（2026-10-14）----
// daemon /media/generate 接收 image（单张，daemon 项目内相对路径）：图片路径走
// renderCustomOpenAIImage → 自动切 /images/edits（图生图）；视频两条 renderer 作
// 首帧（i2v）。参考图必须先在 daemon 项目里，桥负责把工作区文件搬运进去。
const REF_IMAGE_EXTS = ['.png', '.jpg', '.jpeg', '.webp', '.gif']; // 对齐 daemon resolveProjectImage 白名单
const REF_MIME = { '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.webp': 'image/webp', '.gif': 'image/gif' };
const REF_MAX_BYTES = 16 * 1024 * 1024; // 对齐 daemon 上限（其上传路由 multer 侧 20MB，取小值）
const OD_UPLOAD_TIMEOUT_MS = 120000;
// 上传结果缓存：key=abs|size|mtimeMs → Promise<项目相对名>。内容寻址命名 + daemon 同名
// 覆盖幂等，正确性不依赖缓存；缓存只省重复 HTTP——并发批量（同参考图）只上传一次。
const uploadedRefs = new Map();

// resolveReferenceImagePath 读侧校验：路径防穿越 + 扩展名白名单 + 存在 + 常规文件
// + ≤16MB。label 进错误文案（reference / first_frame）。
function resolveReferenceImagePath(raw, label) {
  const { rel, abs } = resolveWorkspaceRel(raw, label);
  const ext = extname(rel).toLowerCase();
  if (!REF_IMAGE_EXTS.includes(ext)) {
    throw new Error(`${label} 不支持的文件类型（${ext || '无扩展名'}）：须为 ${REF_IMAGE_EXTS.join(' / ')} 之一`);
  }
  let st;
  try {
    st = statSync(abs);
  } catch {
    throw new Error(`${label} 文件不存在: ${rel}（路径相对工作目录，例如 assets/img/anchor.png）`);
  }
  if (!st.isFile()) {
    throw new Error(`${label} 不是常规文件: ${rel}`);
  }
  if (st.size > REF_MAX_BYTES) {
    throw new Error(`${label} 文件过大（${(st.size / 1048576).toFixed(1)}MB，上限 16MB）：请先压缩或换更小的图`);
  }
  return { rel, abs, ext, size: st.size, mtimeMs: st.mtimeMs };
}

// buildRefMultipart 手写 multipart（daemon /files 路由是 upload.single('file')，
// 同名文本字段 name 决定项目内最终文件名；JSON base64 路径受全局 express.json
// 4mb 上限，大图必须走 multipart）。
function buildRefMultipart(name, mime, buf) {
  const boundary = `----bmaod${randomBytes(10).toString('hex')}`;
  const head = Buffer.from(
    `--${boundary}\r\nContent-Disposition: form-data; name="name"\r\n\r\n${name}\r\n` +
    `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="${name}"\r\n` +
    `Content-Type: ${mime}\r\n\r\n`, 'utf8');
  const tail = Buffer.from(`\r\n--${boundary}--\r\n`, 'utf8');
  return { body: Buffer.concat([head, buf, tail]), contentType: `multipart/form-data; boundary=${boundary}` };
}

// ensureProjectFileUploaded 工作区图片 → OD 项目文件，返回项目相对名
//（ref-<内容 sha1[:16]>.<ext>；内容寻址天然去重，同名重传 daemon 侧覆盖 = 幂等）。
async function ensureProjectFileUploaded(refFile) {
  const key = `${refFile.abs}|${refFile.size}|${refFile.mtimeMs}`;
  const hit = uploadedRefs.get(key);
  if (hit) return hit;
  const p = (async () => {
    await ensureProject();
    const buf = readFileSync(refFile.abs);
    const name = `ref-${createHash('sha1').update(buf).digest('hex').slice(0, 16)}${refFile.ext}`;
    const { body, contentType } = buildRefMultipart(name, REF_MIME[refFile.ext], buf);
    const res = await odJson(`/api/projects/${OD_PROJECT_ID}/files`, {
      method: 'POST', rawBody: body, contentType, timeoutMs: OD_UPLOAD_TIMEOUT_MS,
    });
    const uploaded = String(res?.file?.name || res?.file?.fileName || name);
    log('参考图已上传:', uploaded, `${refFile.size}B`);
    return uploaded;
  })();
  uploadedRefs.set(key, p);
  try {
    return await p;
  } catch (err) {
    uploadedRefs.delete(key); // 失败不留毒缓存
    throw err;
  }
}

// assertRefNotOutput 拒绝"输出路径 = 参考图路径"（生成会覆盖锚点图）：比对去扩展名
// 后的相对路径（兼容 save_as 无扩展名写法）；宿主 Windows 大小写不敏感，统一小写比对。
function assertRefNotOutput(saveTarget, refFile) {
  const strip = (p) => p.replace(/\.[a-z0-9]+$/i, '').toLowerCase();
  if (strip(saveTarget.rel) === strip(refFile.rel)) {
    throw new Error(`输出路径与参考图指向同一文件（${refFile.rel}）：生成会覆盖参考图，请改 save_as 或先复制一份锚点图`);
  }
}

function log(...args) {
  console.error('[open-design-mcp]', ...args);
}

// odRequest 调用守护进程 REST API（node:http 原生实现，确保 Host 头可改写）。
// 返回 { status, buffer }；json() 解析 JSON，文本错误取 OD 的 error.message。
// rawBody/contentType：multipart 等原始字节体（参考图上传用）；JSON 调用不受影响。
function odRequest(path, { method = 'GET', body, rawBody, contentType, timeoutMs = 30000 } = {}) {
  return new Promise((resolve, reject) => {
    const url = new URL(OD_API_URL + path);
    const headers = { host: OD_HOST_HEADER };
    if (OD_API_TOKEN) {
      headers.authorization = 'Basic ' + Buffer.from(`open-design:${OD_API_TOKEN}`).toString('base64');
    }
    if (rawBody !== undefined) {
      headers['content-type'] = contentType;
      headers['content-length'] = String(rawBody.length);
    } else if (body !== undefined) {
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
    if (rawBody !== undefined) req.write(rawBody);
    else if (body !== undefined) req.write(JSON.stringify(body));
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

async function generateImage({ prompt, aspect, model, save_as: saveAs, remove_bg: removeBg, reference }) {
  if (!OD_API_TOKEN) {
    throw new Error('未配置 OD_API_TOKEN（插件 settings.env 缺失）；请检查 .env 后重启后端');
  }
  // 先校验 save_as 与参考图（fail fast：路径非法不必浪费一次生成/上传）。
  const saveTarget = saveAs ? resolveSaveAs(saveAs) : null;
  let refFile = null;
  if (reference !== undefined && reference !== null && String(reference).trim() !== '') {
    refFile = resolveReferenceImagePath(reference, 'reference');
    if (saveTarget) assertRefNotOutput(saveTarget, refFile);
  }
  const useModel = await resolveImageModel(model);
  const startedAtMs = Date.now();
  await ensureProject();
  // 参考图上传（幂等 + 缓存）：daemon 只认项目内文件，body.image 传项目相对名。
  const refName = refFile ? await ensureProjectFileUploaded(refFile) : null;
  const gen = await odJson(`/api/projects/${OD_PROJECT_ID}/media/generate`, {
    method: 'POST',
    body: { surface: 'image', model: useModel, prompt, ...(aspect ? { aspect } : {}), ...(refName ? { image: refName } : {}) },
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
    reference: refName,
    elapsedSec,
    hint: '在 HTML/CSS/JS 中以该相对路径（path 字段）引用此图片',
  };
}

// generateImageBatch 批量生成：对 prompts 数组并发生成，上限 OD_BATCH_CONCURRENCY
//（默认 3，防打爆上游配额）。实证 2026-08-31：daemon /media/generate 建 task 后
// generateMedia 后台异步跑、HTTP 立即回 taskId（routes/media.js），无队列无锁——
// 旧"daemon 单任务语义"注释不成立，纯串行白等（23 张 × 30s ≈ 11.5min，并发 3 压到 ~4min）。
// 单张失败不中断整批，结果逐项标注 path 或 error，由 Agent 决定重试/降级。
async function generateImageBatch({ prompts, aspect, model, save_as_list: saveAsList, remove_bg: removeBg, reference }) {
  if (!OD_API_TOKEN) {
    throw new Error('未配置 OD_API_TOKEN（插件 settings.env 缺失）；请检查 .env 后重启后端');
  }
  if (!Array.isArray(prompts) || prompts.length === 0) {
    throw new Error('prompts 为空数组：批量模式至少提供 1 个 prompt');
  }
  if (saveAsList !== undefined && saveAsList.length !== prompts.length) {
    throw new Error(`save_as_list 长度（${saveAsList.length}）须与 prompts（${prompts.length}）等长`);
  }
  // 参考图（整批共用一张）：先整体校验一次（fail fast——非法参考图不让整批变 N 条
  // 相同 item error）；逐张生成时经缓存上传只发一次。save_as_list 逐项防覆盖锚点图。
  let refFile = null;
  if (reference !== undefined && reference !== null && String(reference).trim() !== '') {
    refFile = resolveReferenceImagePath(reference, 'reference');
    if (Array.isArray(saveAsList)) {
      for (const s of saveAsList) {
        if (s) assertRefNotOutput(resolveSaveAs(s), refFile);
      }
    }
  }
  // 默认模型整批解析一次（探测走缓存；避免每张各探测）。
  const useModel = await resolveImageModel(model);
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
        const r = await generateImage({ prompt, aspect, model: useModel, save_as: saveAs, remove_bg: removeBg, reference: refFile ? refFile.rel : undefined });
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
    model: useModel,
    aspect: aspect || null,
    reference: refFile ? refFile.rel : null,
    elapsedSec: Math.round((Date.now() - startedAtMs) / 1000),
    results,
    hint: '在 HTML/CSS/JS 中以各项结果的相对路径（path 字段）引用图片；带 error 的项需换 prompt 重试或人工处理',
  };
}

// resolveImageModel 图像默认模型：参数 > env OD_DEFAULT_MODEL > daemon 单点探测 > 硬默认。
async function resolveImageModel(explicitModel) {
  if (explicitModel) return explicitModel;
  if (OD_DEFAULT_MODEL_ENV) return OD_DEFAULT_MODEL_ENV;
  const d = await fetchMediaDefaults();
  return (d && d.imageModel) || IMAGE_FALLBACK_MODEL_ID;
}

function normalizeVideoProvider(raw) {
  const p = String(raw || '').toLowerCase();
  if (p !== 'volcengine' && p !== 'openai') {
    throw new Error(`未知视频 provider：${p}（可选 volcengine / openai；对应网关协议分别为方舟 tasks 与 OpenAI /videos）`);
  }
  return p;
}

// resolveVideoTarget 视频默认目标：参数 > env > daemon 单点探测 > 硬默认。
// openai wire 侧模型统一补 aihubmix- 前缀（探测给的占位自带前缀=no-op；真实裸名由
// 用户在仓库根 .env 填）。volcengine 侧纯用桥传值，须目录 id / 别名可解析。
async function resolveVideoTarget(providerArg, modelArg) {
  const explicitProvider = providerArg ? normalizeVideoProvider(providerArg) : '';
  const explicitModel = String(modelArg || '').trim();
  const d = await fetchMediaDefaults();
  const provider = explicitProvider || OD_VIDEO_PROVIDER_ENV || (d && d.videoProvider) || VIDEO_FALLBACK_PROVIDER;
  const envModel = provider === 'openai' ? OD_OPENAI_VIDEO_MODEL_ENV : OD_VOLCENGINE_VIDEO_MODEL_ENV;
  let model = explicitModel || envModel || (d && d.videoProvider === provider ? d.videoModel : '');
  if (provider === 'openai') {
    if (!model) {
      throw new Error('provider=openai 但未解析到视频模型：在仓库根 .env 媒体段填 VIDEO_MODEL（你的 /videos 网关模型名）后运行 bash docker/bma-plugin.sh media；或临时设桥 env OD_OPENAI_VIDEO_MODEL');
    }
    model = model.startsWith('aihubmix-') ? model : `aihubmix-${model}`;
  } else {
    model = model || VOLCENGINE_FALLBACK_VIDEO_MODEL;
  }
  return { provider, model };
}

// generateVideo 文生视频（t2v）/ 首帧图生视频（i2v，first_frame）：daemon media/generate
// surface='video'，length 传时长秒数；带 image 时两条 renderer（volcengine tasks 协议 /
// aihubmix /videos 协议）均把它当首帧。无白底抠除（图片专属）；产物 mp4 经 downloadFile
// 落盘，路径语义与 generateImage 一致。
async function generateVideo({ prompt, aspect, duration_sec: durationSec, provider, model, save_as: saveAs, first_frame: firstFrame }) {
  if (!OD_API_TOKEN) {
    throw new Error('未配置 OD_API_TOKEN（插件 settings.env 缺失）；请检查 .env 后重启后端');
  }
  // 先校验 save_as 与首帧图（fail fast：路径非法不必浪费一次生成/上传）。
  const saveTarget = saveAs ? resolveSaveAs(saveAs) : null;
  let frameFile = null;
  if (firstFrame !== undefined && firstFrame !== null && String(firstFrame).trim() !== '') {
    frameFile = resolveReferenceImagePath(firstFrame, 'first_frame');
    if (saveTarget) assertRefNotOutput(saveTarget, frameFile);
  }
  const { provider: useProvider, model: useModel } = await resolveVideoTarget(provider, model);
  const startedAtMs = Date.now();
  await ensureProject();
  const frameName = frameFile ? await ensureProjectFileUploaded(frameFile) : null;
  const gen = await odJson(`/api/projects/${OD_PROJECT_ID}/media/generate`, {
    method: 'POST',
    body: {
      surface: 'video',
      model: useModel,
      prompt,
      ...(aspect ? { aspect } : {}),
      ...(durationSec ? { length: durationSec } : {}),
      ...(frameName ? { image: frameName } : {}),
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
    first_frame: frameName,
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
    '经本地 open_design 守护进程生成图片（默认模型按 daemon 单点配置自动选择——真实模型/网关在仓库根 .env 媒体段，一般无需传 model）。' +
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
    '图生图（reference）：系列素材先出锚点样图再批量锁一致性——第一张单独生成并保存为锚点（如 assets/img/<系列>-anchor.png），' +
    '确认后其余图带 reference=锚点路径（批量整批共用一张，桥只上传一次），prompt 里用「图一」指代参考图；不传 reference 逐张重绘风格易漂移。' +
    '单张生成约需 1–5 分钟（pro 档较慢），请勿重复提交相同 prompt。',
  inputSchema: {
    prompt: z.string().min(1).optional().describe(
      '单张模式的图像描述（建议具体描述主体/风格/配色/构图）。与 prompts 二选一；同时给 prompts 时以 prompts 为准'),
    prompts: z.array(z.string().min(1)).min(1).optional().describe(
      '批量模式：prompt 数组，每个元素各生成一张（桥内并发生成，上限 OD_BATCH_CONCURRENCY 默认 3）。帧序列/多素材场景优先用此参数一次提交，' +
      '避免逐张调用各产生一轮 LLM 往返。与 prompt 二选一；空数组报错'),
    aspect: z.enum(['1:1', '16:9', '9:16', '4:3', '3:4']).optional().describe('画幅比例（批量时整批共用），缺省由模型决定'),
    model: z.string().optional().describe('覆盖默认模型（缺省按 daemon 单点配置自动选择，一般无需指定）'),
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
    reference: z.string().optional().describe(
      '参考图（图生图，可选）：工作目录相对路径（如 assets/img/anchor.png），生成时作为风格/主体锚点。' +
      '【系列素材一致性】先单独生成一张锚点样图并确认，再整批传同一 reference 锁一致性；' +
      'prompt 里用「图一」指代参考图（如「图一中的角色，改为侧面奔跑姿势，保持配色与描边」）。' +
      '约束：png/jpg/jpeg/webp/gif、≤16MB、文件须已存在于工作目录；批量模式整批共用一张。' +
      'save_as/save_as_list 中与参考图同路径的条目会被拒绝（防覆盖锚点图）。'),
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
    '经本地 open_design 守护进程生成短视频（文生视频 t2v / 首帧图生视频 i2v）。' +
    'provider 二选一：volcengine=方舟 tasks 协议网关（模型为 daemon 目录 id，自定义接入点由 daemon 的 OD_MEDIA_MODEL_ALIASES 别名改写）；' +
    'openai=OpenAI /videos 协议网关（模型名任意，.env 配 VIDEO_URL/VIDEO_MODEL；裸名桥自动补 aihubmix- 前缀）。' +
    '正式素材务必用 save_as 直落工作目录相对路径（如 assets/video/intro.mp4），一视频一份；缺省落 .bma/od-artifacts（时间戳命名，仅草稿）。' +
    '首帧图生视频（i2v）：先出定稿画面（od_image_generate），再传 first_frame 让它动起来，prompt 里用「图一」指代首帧。' +
    '单条约 30 秒–5 分钟，请勿重复提交相同 prompt；超时（默认 720s）后可调 od_video_list 查看结果。',
  inputSchema: {
    prompt: z.string().min(1).describe('视频描述（建议具体描述主体/动作/镜头运动/风格）'),
    aspect: z.enum(['1:1', '16:9', '9:16', '4:3', '3:4']).optional().describe('画幅比例，缺省由模型决定'),
    duration_sec: z.number().int().min(2).max(15).optional().describe('时长秒数，缺省 5；daemon 自动 snap 到模型允许桶（Veo 4/6/8、Sora 4/8/12、Seedance 常用 5/10）'),
    provider: z.enum(['volcengine', 'openai']).optional().describe('网关协议（缺省按 daemon 单点配置自动选择，一般无需指定）'),
    model: z.string().optional().describe('覆盖默认模型 id（openai 侧给裸模型名即可，桥自动补 aihubmix- 前缀；volcengine 侧须为 daemon 目录 id）'),
    save_as: z.string().optional().describe('产物落盘的工作目录相对路径（如 assets/video/intro.mp4）。目录自动创建；无扩展名时补实际格式；不可含 .. 或盘符'),
    first_frame: z.string().optional().describe(
      '首帧图（图生视频 i2v，可选）：工作目录相对路径，作为视频第一帧；prompt 里用「图一」指代该首帧' +
      '（如「图一中的角色向前走两步，镜头缓慢推进」）。约束同 od_image_generate 的 reference' +
      '（png/jpg/jpeg/webp/gif、≤16MB、须已存在）。网关不支持 i2v 时任务失败并透传上游错误原文，去掉本参数即回退纯 t2v。'),
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
