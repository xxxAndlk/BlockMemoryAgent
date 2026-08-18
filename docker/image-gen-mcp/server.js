// image-gen-mcp：独立免费文生图 stdio MCP server（BMA ui_design 插件）。
//
// 背景（doc/设计文档_插件范式.md §9）：原 open_design 守护进程 + 火山方舟 Seedream
// 方案因按张付费（约 ¥0.26/张）被用户否决，2026-08-17 起替换为本独立桥——
// 无本地服务依赖，直连免费 provider：
//   - zhipu：智谱 CogView-3-Flash（官网标注长期免费，限 1 并发；需 ZHIPU_API_KEY，
//     open.bigmodel.cn 注册即得）。支持尺寸：1024x1024/1344x768/768x1344/1152x864/864x1152。
//   - pollinations：pollinations.ai 免 key 匿名档（限速，FLUX 系模型），零配置兜底。
// IMAGE_PROVIDER=auto（缺省）：有 ZHIPU_API_KEY 走 zhipu，否则 pollinations。
//
// 产物直接落盘 volumes 映射的宿主目录（容器内 /out），返回宿主相对路径。
// stdout 只承载 MCP JSON-RPC 帧，日志一律 stderr。
import https from 'node:https';
import { mkdirSync, writeFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { z } from 'zod';

const IMAGE_PROVIDER = (process.env.IMAGE_PROVIDER || 'auto').toLowerCase();
const ZHIPU_API_KEY = process.env.ZHIPU_API_KEY || '';
const ZHIPU_BASE_URL = (process.env.ZHIPU_BASE_URL || 'https://open.bigmodel.cn/api/paas/v4').replace(/\/$/, '');
const ZHIPU_MODEL = process.env.ZHIPU_MODEL || 'cogview-3-flash';
const POLLINATIONS_MODEL = process.env.POLLINATIONS_MODEL || 'flux';
const IMAGE_TIMEOUT_MS = Number(process.env.IMAGE_TIMEOUT_MS || 180000);
const OUTPUT_DIR = process.env.IMAGE_OUTPUT_DIR || '/out';
// 返回给 Agent 的宿主侧相对路径前缀（与 plugins.yaml 的 volumes 映射对应）。
const OUTPUT_REL_PREFIX = (process.env.IMAGE_OUTPUT_REL_PREFIX || 'workspace/image-artifacts').replace(/\/$/, '');
const IMAGE_EXTS = ['.png', '.jpg', '.jpeg', '.webp', '.gif'];

// 画幅 → 像素尺寸（取自 CogView-3-Flash 官方支持列表；pollinations 接受任意宽高，同用此表）。
const ASPECTS = {
  '1:1': [1024, 1024],
  '16:9': [1344, 768],
  '9:16': [768, 1344],
  '4:3': [1152, 864],
  '3:4': [864, 1152],
};

function log(...args) {
  console.error('[image-gen-mcp]', ...args);
}

// httpsRequest 原生 HTTPS 请求（最多跟随 3 次重定向；provider 均 https，无需 http 分支）。
// 返回 { status, buffer, headers }；json() 由调用方自行解析。
function httpsRequest(urlStr, { method = 'GET', headers = {}, body, timeoutMs = IMAGE_TIMEOUT_MS, redirects = 3 } = {}) {
  return new Promise((resolve, reject) => {
    const url = new URL(urlStr);
    const reqHeaders = { ...headers };
    if (body !== undefined) reqHeaders['content-type'] = 'application/json';
    const req = https.request({
      hostname: url.hostname,
      port: url.port || 443,
      path: url.pathname + url.search,
      method,
      headers: reqHeaders,
      timeout: timeoutMs,
    }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => {
        const status = res.statusCode;
        const location = res.headers.location;
        if (status >= 300 && status < 400 && location && redirects > 0) {
          resolve(httpsRequest(new URL(location, urlStr).toString(), { method, headers, body, timeoutMs, redirects: redirects - 1 }));
          return;
        }
        resolve({ status, buffer: Buffer.concat(chunks), headers: res.headers });
      });
    });
    req.on('timeout', () => req.destroy(new Error(`请求超时（${timeoutMs}ms）`)));
    req.on('error', reject);
    if (body !== undefined) req.write(JSON.stringify(body));
    req.end();
  });
}

// resolveProvider 决定本次调用走哪家；显式指定优先，其次 auto 规则。
function resolveProvider(requested) {
  const want = (requested && requested !== 'auto' ? requested : IMAGE_PROVIDER);
  if (want === 'auto') return ZHIPU_API_KEY ? 'zhipu' : 'pollinations';
  if (want === 'zhipu' && !ZHIPU_API_KEY) {
    throw new Error('指定了 zhipu 但未配置 ZHIPU_API_KEY（.env 填入 open.bigmodel.cn 的 key 后重启后端）；或不传 provider 走免 key 的 pollinations');
  }
  if (want !== 'zhipu' && want !== 'pollinations') {
    throw new Error(`未知 provider：${want}（可选 zhipu / pollinations / auto）`);
  }
  return want;
}

// genZhipu 调智谱 images/generations（OpenAI 兼容形状），返回图片字节与格式。
async function genZhipu(prompt, [w, h]) {
  const { status, buffer } = await httpsRequest(`${ZHIPU_BASE_URL}/images/generations`, {
    method: 'POST',
    headers: { authorization: `Bearer ${ZHIPU_API_KEY}` },
    body: { model: ZHIPU_MODEL, prompt, size: `${w}x${h}` },
  });
  let data = null;
  try { data = JSON.parse(buffer.toString('utf8')); } catch { /* 非 JSON 响应 */ }
  if (status < 200 || status >= 300) {
    const msg = data?.error?.message || buffer.toString('utf8').slice(0, 300) || `HTTP ${status}`;
    throw new Error(`智谱生图失败（${status}）：${msg}`);
  }
  const imageUrl = data?.data?.[0]?.url;
  if (!imageUrl) throw new Error(`智谱未返回图片 URL：${JSON.stringify(data).slice(0, 300)}`);
  // URL 短时有效，立即下载。
  const img = await httpsRequest(imageUrl, { timeoutMs: 60000 });
  if (img.status < 200 || img.status >= 300 || img.buffer.length < 512) {
    throw new Error(`智谱图片下载失败（${img.status}）`);
  }
  return { bytes: img.buffer, ext: extFromContentType(img.headers['content-type']) || '.png', model: ZHIPU_MODEL };
}

// genPollinations 免 key 直连，响应即图片字节。
async function genPollinations(prompt, [w, h], seed) {
  const qs = new URLSearchParams({ width: String(w), height: String(h), nologo: 'true', model: POLLINATIONS_MODEL });
  if (seed !== undefined) qs.set('seed', String(seed));
  const url = `https://image.pollinations.ai/prompt/${encodeURIComponent(prompt)}?${qs}`;
  const { status, buffer, headers } = await httpsRequest(url);
  const ctype = String(headers['content-type'] || '');
  if (status < 200 || status >= 300 || !ctype.startsWith('image/') || buffer.length < 512) {
    const raw = buffer.toString('utf8').slice(0, 300);
    throw new Error(`pollinations 生图失败（${status} ${ctype}）：${raw || '无响应体'}（匿名档限速中，稍后重试）`);
  }
  return { bytes: buffer, ext: extFromContentType(ctype) || '.jpg', model: POLLINATIONS_MODEL };
}

function extFromContentType(ctype) {
  const c = String(ctype || '').split(';')[0].trim().toLowerCase();
  if (c === 'image/png') return '.png';
  if (c === 'image/jpeg') return '.jpg';
  if (c === 'image/webp') return '.webp';
  if (c === 'image/gif') return '.gif';
  return null;
}

function slugify(prompt) {
  const slug = prompt.slice(0, 24).replace(/[^\w一-鿿-]+/g, '_').replace(/^_+|_+$/g, '');
  return slug || 'image';
}

async function generateImage({ prompt, aspect, provider, seed }) {
  const useProvider = resolveProvider(provider);
  const useAspect = aspect || '1:1';
  const dims = ASPECTS[useAspect];
  if (!dims) throw new Error(`未知画幅：${useAspect}（可选 ${Object.keys(ASPECTS).join(' / ')}）`);
  const startedAtMs = Date.now();
  log('生成请求:', useProvider, useAspect, JSON.stringify(prompt.slice(0, 60)));
  const gen = useProvider === 'zhipu'
    ? await genZhipu(prompt, dims)
    : await genPollinations(prompt, dims, seed);
  mkdirSync(OUTPUT_DIR, { recursive: true });
  const outName = `${new Date().toISOString().replace(/[:.]/g, '').slice(0, 15)}-${slugify(prompt)}${gen.ext}`;
  writeFileSync(join(OUTPUT_DIR, outName), gen.bytes);
  const elapsedSec = Math.round((Date.now() - startedAtMs) / 1000);
  log('产物已落盘:', outName, `${gen.bytes.length}B`, `${elapsedSec}s`);
  return {
    path: `${OUTPUT_REL_PREFIX}/${outName}`,
    filename: outName,
    bytes: gen.bytes.length,
    provider: useProvider,
    model: gen.model,
    aspect: useAspect,
    seed: seed ?? null,
    elapsedSec,
    hint: '在 HTML/CSS 中以该相对路径引用此图片',
  };
}

// listImages 列产物目录（本地文件系统，无服务依赖）。
function listImages({ limit }) {
  let entries = [];
  try {
    entries = readdirSync(OUTPUT_DIR);
  } catch {
    return { dir: OUTPUT_REL_PREFIX, count: 0, images: [] };
  }
  const images = entries
    .filter((name) => IMAGE_EXTS.some((ext) => name.toLowerCase().endsWith(ext)))
    .map((name) => {
      const st = statSync(join(OUTPUT_DIR, name));
      return { name, size: st.size, updatedAt: st.mtime.toISOString() };
    })
    .sort((a, b) => (a.updatedAt < b.updatedAt ? 1 : -1))
    .slice(0, limit || 20);
  return { dir: OUTPUT_REL_PREFIX, count: images.length, images };
}

const server = new McpServer({ name: 'image-gen-mcp', version: '0.1.0' });

server.registerTool('image_generate', {
  title: '免费文生图',
  description:
    '用免费模型生成图片（智谱 CogView-3-Flash / pollinations，无需付费）。' +
    '适合游戏卡通素材、装饰图、封面图等静态图片；产物保存到宿主 workspace 并在结果中' +
    '返回相对路径（path 字段），可直接在 HTML/CSS/文档中引用。' +
    '生成约需 5–60 秒，请勿重复提交相同 prompt。' +
    '提示词建议具体描述主体/风格/配色/背景（游戏 sprite 可注明"纯色背景 居中 卡通风格"便于后期抠图）。',
  inputSchema: {
    prompt: z.string().min(1).describe('图像描述（建议具体描述主体/风格/配色/构图）'),
    aspect: z.enum(['1:1', '16:9', '9:16', '4:3', '3:4']).optional().describe('画幅比例，缺省 1:1'),
    provider: z.enum(['auto', 'zhipu', 'pollinations']).optional()
      .describe('生图渠道，缺省 auto（有 ZHIPU_API_KEY 走智谱，否则免 key 的 pollinations）'),
    seed: z.number().int().optional().describe('随机种子（仅 pollinations 支持；固定后可复现同图）'),
  },
}, async (args) => {
  try {
    const result = await generateImage(args);
    return { content: [{ type: 'text', text: JSON.stringify(result, null, 2) }] };
  } catch (err) {
    return { content: [{ type: 'text', text: `image_generate 失败: ${err.message}` }], isError: true };
  }
});

server.registerTool('image_list', {
  title: '已生成图片列表',
  description: '列出产物目录中已生成的图片文件（名称/大小/更新时间），用于复用历史产物。',
  inputSchema: {
    limit: z.number().int().min(1).max(100).optional().describe('返回条数上限，缺省 20'),
  },
}, async (args) => {
  try {
    const result = listImages(args);
    return { content: [{ type: 'text', text: JSON.stringify(result, null, 2) }] };
  } catch (err) {
    return { content: [{ type: 'text', text: `image_list 失败: ${err.message}` }], isError: true };
  }
});

await server.connect(new StdioServerTransport());
log(`stdio MCP server 已就绪（provider=${IMAGE_PROVIDER}, out=${OUTPUT_DIR}）`);
