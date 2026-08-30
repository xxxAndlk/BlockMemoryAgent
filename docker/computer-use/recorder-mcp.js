#!/usr/bin/env node
/**
 * recorder-mcp.js — computer-use 沙箱的复合 MCP stdio 代理。
 *
 * 职责：
 *  1. spawn 原 @zavora-ai/computer-use-mcp server 作为子进程，按 MCP stdio 规范
 *     （行分隔 JSON-RPC）双向转发：initialize / notifications / 原工具调用原样透传。
 *  2. 合并本地工具 screen_record：ffmpeg x11grab 录制本容器 Xvfb :99 虚拟桌面
 *    （即 Agent 自己操作的同一个桌面），产物落 /workspace/.bma/videos/（经
 *     plugins.yaml volumes 挂载 ${WORKDIR}，宿主工作目录可见），并抽取 4 张
 *     关键帧以 MCP image content 返回（经 mcpbridge image_passthrough 透传给模型）。
 *
 * ID 映射：wrapper→child 用自增 counter 重编请求 ID，pending Map<childId, parentOriginalId>
 * 在响应方向换回原 ID。child 主动发起的请求（sampling/roots 等有 ID 但不在映射中）
 * 原样转发给 parent；无 ID 的 notification 原样透传。
 *
 * 退出保证：parent stdin 关闭 / SIGTERM / SIGINT → kill 子进程后退出
 *（mcpbridge docker transport `docker rm -f` 回收时不留孤儿进程）。
 */

'use strict';

const { spawn, execFile } = require('child_process');
const fs = require('fs');
const path = require('path');

// 原 server 路径由 entrypoint.sh export COMPUTER_USE_SERVER 传入（npm root -g 结果）；
// 直启（未走 entrypoint）时回退全局默认路径。
const CHILD_SERVER = process.env.COMPUTER_USE_SERVER ||
  '/usr/local/lib/node_modules/@zavora-ai/computer-use-mcp/dist/server.js';

// ---------- screen_record 工具描述符 ----------
const SCREEN_RECORD_TOOL = {
  name: 'screen_record',
  description:
    '录制本沙箱虚拟桌面（Xvfb :99，即你正在操作的同一个桌面）指定时长，' +
    '生成 mp4 视频并抽取 4 张关键帧返回。产物保存于 /workspace/.bma/videos/，' +
    '对应宿主工作目录 <workdir>/.bma/videos/，宿主侧可见可复用。' +
    '注意：该工具属破坏性插件，调用需要用户批准。',
  inputSchema: {
    type: 'object',
    properties: {
      duration_sec: {
        type: 'number',
        default: 10,
        maximum: 300,
        description: '录制时长（秒），默认 10，上限 300',
      },
      fps: {
        type: 'number',
        default: 15,
        maximum: 30,
        description: '录制帧率，默认 15（桌面操作足够，CPU 友好）',
      },
    },
  },
};

// ---------- 常量 ----------
const FRAME_COUNT = 4;          // 与 mcpbridge image_passthrough 上限（4 张/次）对齐
const FRAME_MAX_PIXELS = 1024;  // 帧长边像素上限（1024px JPEG q5 约 100-200KB < 4MiB 单张上限）
const RECORD_OUTPUT_DIR = '/workspace/.bma/videos';
const RECORD_OUTPUT_DIR_FALLBACK = '/tmp/bma-videos';

// ---------- 子进程管理 ----------
const child = spawn(process.execPath, [CHILD_SERVER], {
  stdio: ['pipe', 'pipe', 'inherit'], // stderr 直通容器日志，不污染 stdio JSON-RPC
  env: { ...process.env },
});

// ---------- ID 映射 ----------
let nextChildId = 1;
const pending = new Map(); // childId -> parent 原始 id

// ---------- parent（stdin/stdout）行分隔 JSON 流 ----------
let parentBuf = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (chunk) => {
  parentBuf += chunk;
  let idx;
  while ((idx = parentBuf.indexOf('\n')) >= 0) {
    const line = parentBuf.slice(0, idx).trim();
    parentBuf = parentBuf.slice(idx + 1);
    if (!line) continue;
    handleParentLine(line);
  }
});
process.stdin.on('close', () => shutdown(0));

function handleParentLine(line) {
  let msg;
  try {
    msg = JSON.parse(line);
  } catch {
    return; // 非 JSON 行（坏帧）静默丢弃，不让坏帧毒化子进程
  }

  // 本地工具调用：直接执行并回 parent 原 id，不经子进程。
  if (msg.id !== undefined && msg.id !== null &&
      msg.method === 'tools/call' && msg.params && msg.params.name === 'screen_record') {
    callScreenRecord(msg.params.arguments || {})
      .then((result) => sendToParent({ jsonrpc: '2.0', id: msg.id, result }))
      .catch((err) => sendToParent({
        jsonrpc: '2.0',
        id: msg.id,
        result: { isError: true, content: [{ type: 'text', text: `screen_record 失败: ${err && err.message ? err.message : String(err)}` }] },
      }));
    return;
  }

  // 无 id 的 notification 或其余请求 → 转发子进程；有 id 的请求重编 ID。
  if (msg.id !== undefined && msg.id !== null) {
    const childId = nextChildId++;
    pending.set(childId, msg.id);
    msg.id = childId;
  }
  sendToChild(msg);
}

// ---------- child（stdout）→ parent ----------
let childBuf = '';
child.stdout.setEncoding('utf8');
child.stdout.on('data', (chunk) => {
  childBuf += chunk;
  let idx;
  while ((idx = childBuf.indexOf('\n')) >= 0) {
    const line = childBuf.slice(0, idx).trim();
    childBuf = childBuf.slice(idx + 1);
    if (!line) continue;
    handleChildLine(line);
  }
});
child.on('exit', (code) => {
  // 子进程崩溃则整体退出（mcpbridge 有重连与退避逻辑，退出即向桥报告 down）。
  process.exit(code === 0 ? 0 : 1);
});

function handleChildLine(line) {
  let msg;
  try {
    msg = JSON.parse(line);
  } catch {
    return;
  }

  // 对 tools/list 响应：合并 screen_record 描述符。
  if (msg.id !== undefined && msg.id !== null && pending.has(msg.id)) {
    const parentId = pending.get(msg.id);
    pending.delete(msg.id);
    if (msg.result && Array.isArray(msg.result.tools) &&
        !msg.result.tools.some((t) => t.name === SCREEN_RECORD_TOOL.name)) {
      msg.result.tools.push(SCREEN_RECORD_TOOL);
    }
    msg.id = parentId;
    sendToParent(msg);
    return;
  }

  // 有 id 但不在映射（child 主动请求，如 sampling/create_message）→ 原样透传。
  // 无 id 的 notification（log/progress）→ 原样透传。
  sendToParent(msg);
}

function sendToParent(msg) {
  process.stdout.write(JSON.stringify(msg) + '\n');
}

function sendToChild(msg) {
  if (child.stdin.writable) {
    child.stdin.write(JSON.stringify(msg) + '\n');
  }
}

// ---------- screen_record 实现 ----------
function parseResolution() {
  // entrypoint export RESOLUTION=1280x800x24；解析 W/H 供 x11grab -video_size。
  const m = /^(\d+)x(\d+)/.exec(process.env.RESOLUTION || '1280x800x24');
  return { w: parseInt(m[1], 10), h: parseInt(m[2], 10) };
}

function ensureOutputDir() {
  try {
    fs.mkdirSync(RECORD_OUTPUT_DIR, { recursive: true });
    return { dir: RECORD_OUTPUT_DIR, mounted: true };
  } catch {
    // /workspace 未挂载（plugins.yaml 未配 volumes）：降级容器内 /tmp 并说明宿主不可见。
    fs.mkdirSync(RECORD_OUTPUT_DIR_FALLBACK, { recursive: true });
    return { dir: RECORD_OUTPUT_DIR_FALLBACK, mounted: false };
  }
}

function runFf(args, timeoutMs) {
  return new Promise((resolve, reject) => {
    execFile('ffmpeg', args, { timeout: timeoutMs }, (err, stdout, stderr) => {
      if (err) reject(new Error(`ffmpeg: ${err.message} ${stderr ? stderr.slice(-400) : ''}`));
      else resolve(stdout);
    });
  });
}

function runProbe(args, timeoutMs) {
  return new Promise((resolve, reject) => {
    execFile('ffprobe', args, { timeout: timeoutMs }, (err, stdout) => {
      if (err) reject(err);
      else resolve(stdout);
    });
  });
}

function ffprobeDurationSec(file, timeoutMs) {
  return runProbe(['-v', 'error', '-show_entries', 'format=duration', '-of', 'json', file], timeoutMs)
    .then((out) => {
      try {
        const d = JSON.parse(out).format.duration;
        const v = parseFloat(d);
        return Number.isFinite(v) ? v : 0;
      } catch {
        return 0;
      }
    })
    .catch(() => 0);
}

function extractFrame(file, tSec, outPath, timeoutMs) {
  const scale = `scale='min(${FRAME_MAX_PIXELS},iw)':'min(${FRAME_MAX_PIXELS},ih)':force_original_aspect_ratio=decrease`;
  return runFf([
    '-hide_banner', '-loglevel', 'error',
    '-ss', tSec.toFixed(2),
    '-i', file,
    '-frames:v', '1',
    '-vf', scale,
    '-q:v', '5',
    '-f', 'image2', '-y', outPath,
  ], timeoutMs).then(() => outPath);
}

async function callScreenRecord(args) {
  const duration = Math.min(Math.max(Number(args.duration_sec) || 10, 1), 300);
  const fps = Math.min(Math.max(Number(args.fps) || 15, 1), 30);
  const { w, h } = parseResolution();
  const { dir, mounted } = ensureOutputDir();
  const out = path.join(dir, `rec-${Date.now()}-${process.pid}.mp4`);
  const budgetMs = (duration + 60) * 1000; // 录制 + 抽帧硬超时

  // 录制：x11grab 抓 :99，libx264 veryfast + crf 28（沙箱 CPU 友好，1 分钟约几 MB）。
  await runFf([
    '-y',
    '-f', 'x11grab',
    '-video_size', `${w}x${h}`,
    '-framerate', String(fps),
    '-i', ':99',
    '-t', String(duration),
    '-c:v', 'libx264',
    '-preset', 'veryfast',
    '-crf', '28',
    '-pix_fmt', 'yuv420p',
    out,
  ], budgetMs);

  // 实测时长（失败用请求时长兜底）+ 等时间隔抽 4 帧（-ss 在 -i 前 = 输入侧 seek）。
  const realDur = (await ffprobeDurationSec(out, 15000)) || duration;
  const frames = [];
  for (let i = 0; i < FRAME_COUNT; i++) {
    const t = (realDur * i) / FRAME_COUNT;
    try {
      frames.push(await extractFrame(out, t, path.join(dir, `rec-${process.pid}-f${i}.jpg`), 30000));
    } catch {
      // 单帧失败不中断，尽力返回已成功帧。
    }
  }

  const sizeMB = (fs.statSync(out).size / (1024 * 1024)).toFixed(1);
  const hostNote = mounted
    ? `宿主工作目录 <workdir>/.bma/videos/ 下可见`
    : `/workspace 未挂载，产物在容器内 ${dir}，宿主不可见`;
  const content = [{
    type: 'text',
    text: `已录制 ${duration}s → ${out}（${sizeMB}MB，${w}x${h}@${fps}fps）；${hostNote}。` +
      `关键帧 ${frames.length}/${FRAME_COUNT} 如下：`,
  }];
  for (const f of frames) {
    content.push({
      type: 'image',
      data: fs.readFileSync(f).toString('base64'),
      mimeType: 'image/jpeg',
    });
  }
  return { content };
}

// ---------- 信号处理 ----------
function shutdown(code) {
  try { child.kill('SIGTERM'); } catch { /* ignore */ }
  setTimeout(() => process.exit(code), 500).unref();
}
process.on('SIGTERM', () => shutdown(0));
process.on('SIGINT', () => shutdown(0));
