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
const OD_TIMEOUT_MS = Number(process.env.OD_TIMEOUT_MS || 180000);
// 容器内挂载的工作目录根（plugins.yaml volumes: ${WORKDIR}:/workspace）。
// save_as 相对路径以此解析；默认产物落其子目录 .bma/od-artifacts。
const WORKSPACE_DIR = (process.env.OD_WORKSPACE_DIR || '/workspace').replace(/\/$/, '');
const OUTPUT_DIR = process.env.OD_OUTPUT_DIR || `${WORKSPACE_DIR}/.bma/od-artifacts`;
// 返回给 Agent 的宿主侧相对路径前缀（相对 Agent 工作目录，与 plugins.yaml 的 volumes 映射对应）。
const OUTPUT_REL_PREFIX = (process.env.OD_OUTPUT_REL_PREFIX || '.bma/od-artifacts').replace(/\/$/, '');
const POLL_INTERVAL_MS = 2000;
const IMAGE_EXTS = ['.png', '.jpg', '.jpeg', '.webp', '.gif'];

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

// newestImageFallback task.file 缺文件名时，取项目里 task 启动后最新修改的图片。
async function newestImageFallback(sinceMs) {
  const data = await odJson(`/api/projects/${OD_PROJECT_ID}/files`);
  const files = Array.isArray(data?.files) ? data.files : [];
  const images = files
    .filter((f) => IMAGE_EXTS.some((ext) => String(f?.name || f?.fileName || '').toLowerCase().endsWith(ext)))
    .filter((f) => !sinceMs || Number(f?.mtime || f?.updatedAt || f?.modifiedAt || 0) >= sinceMs - 5000)
    .sort((a, b) => Number(b?.mtime || b?.updatedAt || b?.modifiedAt || 0) - Number(a?.mtime || a?.updatedAt || a?.modifiedAt || 0));
  const first = images[0];
  return first ? String(first.name || first.fileName) : null;
}

async function waitTask(taskId, startedAtMs) {
  const deadline = Date.now() + OD_TIMEOUT_MS;
  for (;;) {
    const data = await odJson(`/api/projects/${OD_PROJECT_ID}/media/tasks?includeDone=1`);
    const tasks = Array.isArray(data?.tasks) ? data.tasks : [];
    const task = tasks.find((t) => t?.taskId === taskId);
    if (task?.status === 'done') return task;
    if (task?.status === 'failed' || task?.status === 'interrupted') {
      const msg = task?.error?.message || JSON.stringify(task?.error || '未知错误');
      throw new Error(`图像生成失败（${task.status}）：${msg}`);
    }
    if (Date.now() > deadline) {
      throw new Error(`等待生成超时（${OD_TIMEOUT_MS}ms），taskId=${taskId}；可稍后调 od_image_list 查看结果`);
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

async function generateImage({ prompt, aspect, model, save_as: saveAs }) {
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
  const task = await waitTask(taskId, startedAtMs);
  const fileName = (await extractTaskFile(task)) || (await newestImageFallback(startedAtMs));
  if (!fileName) throw new Error('任务完成但未找到产物文件名');
  const bytes = await downloadFile(fileName);
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
  writeFileSync(absPath, bytes);
  const elapsedSec = Math.round((Date.now() - startedAtMs) / 1000);
  log('产物已落盘:', relPath, `${bytes.length}B`, `${elapsedSec}s`);
  return {
    path: relPath,
    filename: relPath.split('/').pop(),
    bytes: bytes.length,
    model: useModel,
    aspect: aspect || null,
    elapsedSec,
    hint: '在 HTML/CSS/JS 中以该相对路径（path 字段）引用此图片',
  };
}

async function listImages({ limit }) {
  if (!OD_API_TOKEN) {
    throw new Error('未配置 OD_API_TOKEN（插件 settings.env 缺失）；请检查 .env 后重启后端');
  }
  await ensureProject();
  const data = await odJson(`/api/projects/${OD_PROJECT_ID}/files`);
  const files = Array.isArray(data?.files) ? data.files : [];
  const images = files
    .filter((f) => IMAGE_EXTS.some((ext) => String(f?.name || f?.fileName || '').toLowerCase().endsWith(ext)))
    .map((f) => ({
      name: String(f.name || f.fileName),
      size: Number(f.size || f.bytes || 0),
      updatedAt: f.mtime || f.updatedAt || f.modifiedAt || null,
    }))
    .sort((a, b) => Number(b.updatedAt || 0) - Number(a.updatedAt || 0))
    .slice(0, limit || 20);
  return { project: OD_PROJECT_ID, count: images.length, images };
}

const server = new McpServer({ name: 'open-design-mcp', version: '0.1.0' });

server.registerTool('od_image_generate', {
  title: 'Open Design 图像生成',
  description:
    '经本地 open_design 守护进程生成图片（默认火山方舟 Seedream）。' +
    '正式素材务必用 save_as 指定工作目录相对路径（如 assets/img/mon-zombie.png），图片直落该路径、' +
    '一图一份，结果 path 字段即可在 HTML/CSS/JS 中直接引用；禁止事后再复制/重命名出第二份。' +
    '缺省 save_as 时落 .bma/od-artifacts（时间戳命名，仅适合草稿/临时用途）。' +
    '生成约需 10–60 秒，请勿重复提交相同 prompt。',
  inputSchema: {
    prompt: z.string().min(1).describe('图像描述（建议具体描述主体/风格/配色/构图）'),
    aspect: z.enum(['1:1', '16:9', '9:16', '4:3', '3:4']).optional().describe('画幅比例，缺省由模型决定'),
    model: z.string().optional().describe(`覆盖默认模型（缺省 ${OD_DEFAULT_MODEL}）`),
    save_as: z.string().optional().describe(
      '产物落盘的工作目录相对路径（如 assets/img/mon-zombie.png）。目录自动创建；' +
      '无扩展名时按生成文件实际格式补齐；不可含 .. 或盘符。命名须与代码加载约定一致'),
  },
}, async (args) => {
  try {
    const result = await generateImage(args);
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

await server.connect(new StdioServerTransport());
log(`stdio MCP server 已就绪（OD_API_URL=${OD_API_URL}, project=${OD_PROJECT_ID}）`);
