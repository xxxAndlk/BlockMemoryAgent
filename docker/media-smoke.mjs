#!/usr/bin/env node
// media-smoke.mjs —— 媒体桥冒烟（计划 §9「真实媒体冒烟」的执行器）。
//
// 分两层：
//   1) free（默认）：**零额度**。通过真实 stdio MCP 桥（bma/open-design-mcp:local）调用
//      应当被本地拒绝的用例，验证"能力/路由/审批门在发上游请求之前就拦住"：
//        - od_image_generate(operation=image_edit)      → capability_unsupported
//        - od_video_generate(provider=<未配置通道>)     → provider_not_configured
//        - od_image_generate(stage=keyframe)            → 「anchor」审批门本地拒绝
//        - od_media_approve(ref=<不存在>)               → 报错（不静默成功）
//      这些用例若能通过说明：桥的新逻辑生效、且**没有产生任何上游调用/费用**。
//   2) live（需显式 --live --yes-spend）：真实调上游（花钱）。按计划 §9 最小集合：
//        a. 一张 t2i：核对 model/aspect/实际像素/task 清单
//        b. 一张 subject_reference：核对 reference_hash 与清单
//        c. 一条 H3 4 秒首帧视频（provider=auto）：确认不再走错网关
//        d. 四个关键姿态：未批准时**不得**继续中间帧（free 层已覆盖拒绝语义）
//      每步保留 request/result manifest，失败时打印上游错误原文，不盲目重试。
//
// 用法：
//   node docker/media-smoke.mjs                 # 免费层（默认）
//   node docker/media-smoke.mjs --live --yes-spend [--ref=assets/img/anchor.png]
//
// 前置：docker build -t bma/open-design-mcp:local docker/open-design-mcp
//      （桥代码改动后必须重建，否则测的是旧逻辑）

import { spawn } from 'node:child_process';
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, existsSync, readdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const IMAGE = process.env.MEDIA_SMOKE_IMAGE || 'bma/open-design-mcp:local';
const DAEMON_URL = process.env.OD_API_URL || 'http://host.docker.internal:7456';
const OD_HOST_HEADER = process.env.OD_HOST_HEADER || 'localhost:7456';

// 从仓库根 .env 读 token（只用于桥容器鉴权，绝不打印）。
function envToken() {
  if (process.env.OD_API_TOKEN) return process.env.OD_API_TOKEN;
  for (const p of ['.env', '../.env', '../../.env']) {
    if (!existsSync(p)) continue;
    const line = readFileSync(p, 'utf8').split(/\r?\n/).find((l) => /^\s*OD_API_TOKEN\s*=/.test(l));
    if (line) return line.split('=').slice(1).join('=').trim();
  }
  return '';
}

const flags = new Set(process.argv.slice(2));
const argOf = (name, def = '') => {
  const hit = process.argv.slice(2).find((a) => a.startsWith(`--${name}=`));
  return hit ? hit.slice(name.length + 3) : def;
};

// ---------------------------------------------------------------- MCP stdio 客户端

// callBridge 起一个桥容器，走最小 JSON-RPC 握手后调用一个工具。
// 超时即杀容器（避免冒烟把 CI 挂住）。
function callBridge(toolName, args, { timeoutMs = 180000 } = {}) {
  const workdir = smokeWorkdir;
  const env = {
    OD_API_URL: DAEMON_URL,
    OD_API_HOST_HEADER: OD_HOST_HEADER,
    OD_HOST_HEADER,
    OD_API_TOKEN: envToken(),
    OD_WORKSPACE_DIR: '/workspace',
    OD_OUTPUT_REL_PREFIX: '.bma/od-artifacts',
    OD_IMAGE_CAPABILITIES: process.env.IMAGE_CAPABILITIES || '',
    OD_VIDEO_CAPABILITIES: process.env.VIDEO_CAPABILITIES || '',
  };
  const dockerArgs = [
    'run', '-i', '--rm', '--add-host=host.docker.internal:host-gateway',
    '-v', `${workdir}:/workspace`,
    ...Object.entries(env).flatMap(([k, v]) => ['-e', `${k}=${v}`]),
    IMAGE,
  ];
  return new Promise((resolve) => {
    const child = spawn('docker', dockerArgs, { stdio: ['pipe', 'pipe', 'pipe'] });
    let buf = '';
    let stderr = '';
    let settled = false;
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      try { child.kill('SIGKILL'); } catch { /* ignore */ }
      resolve({ ok: false, text: `超时（${timeoutMs}ms）`, stderr });
    }, timeoutMs);
    const finish = (res) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try { child.stdin.end(); } catch { /* ignore */ }
      try { child.kill('SIGTERM'); } catch { /* ignore */ }
      resolve(res);
    };
    child.stdout.on('data', (d) => {
      buf += d.toString('utf8');
      let idx;
      while ((idx = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, idx).trim();
        buf = buf.slice(idx + 1);
        if (!line) continue;
        let msg;
        try { msg = JSON.parse(line); } catch { continue; }
        if (msg.id === 2) {
          const out = msg.result?.content?.map((c) => c.text || '').join('\n') ?? '';
          finish({ ok: !msg.result?.isError && !msg.error, text: out || JSON.stringify(msg.error || msg.result), stderr });
        }
      }
    });
    child.stderr.on('data', (d) => { stderr += d.toString('utf8'); });
    child.on('error', (err) => finish({ ok: false, text: `docker 启动失败: ${err.message}`, stderr }));
    // 握手：initialize → initialized → tools/call
    child.stdin.write(JSON.stringify({
      jsonrpc: '2.0', id: 1, method: 'initialize',
      params: { protocolVersion: '2024-11-05', capabilities: {}, clientInfo: { name: 'bma-media-smoke', version: '1' } },
    }) + '\n');
    child.stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n');
    child.stdin.write(JSON.stringify({
      jsonrpc: '2.0', id: 2, method: 'tools/call',
      params: { name: toolName, arguments: args },
    }) + '\n');
  });
}

// ---------------------------------------------------------------- 断言工具

let pass = 0, fail = 0;
const results = [];

function report(name, ok, detail) {
  results.push({ name, ok, detail });
  if (ok) pass++; else fail++;
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? `\n      ${detail.replace(/\n/g, '\n      ')}` : ''}`);
}

function expectLocalReject(name, res, needles) {
  const text = (res.text || '') + (res.stderr || '');
  const hit = needles.some((n) => text.includes(n));
  report(name, !res.ok && hit, `期望本地拒绝（含 ${needles.join(' / ')}）；实际: ${text.slice(0, 400)}`);
}

function listManifests(dir) {
  const root = join(dir, '.bma', 'media');
  if (!existsSync(root)) return [];
  const out = [];
  for (const name of readdirSync(root)) {
    const p = join(root, name);
    if (existsSync(join(p, 'result.json'))) out.push(name);
  }
  return out;
}

// ---------------------------------------------------------------- 主流程

const smokeWorkdir = mkdtempSync(join(tmpdir(), 'bma-media-smoke-'));
console.log(`media smoke · 桥镜像=${IMAGE} · 工作目录=${smokeWorkdir}`);

// 1×1 透明 PNG：给"能力拒绝"用例一个真实存在的参考图，否则桥会先报路径不存在
// （路径校验先于能力校验，那是正确的 fail-fast 顺序，但测不到能力路由）。
const TINY_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64');
const anchor = argOf('ref', 'assets/img/anchor.png');
mkdirSync(join(smokeWorkdir, 'assets', 'img'), { recursive: true });
writeFileSync(join(smokeWorkdir, 'assets', 'img', 'anchor.png'), TINY_PNG);

// ---- 免费层：必须在 HTTP 之前拒绝，零额度 ----
const r1 = await callBridge('od_image_generate', {
  prompt: 'smoke: local capability reject', operation: 'image_edit', reference: anchor, stage: 'asset',
});
expectLocalReject('image_edit 能力不满足 → capability_unsupported（零上游调用）', r1,
  ['capability_unsupported', '不支持 operation=image_edit', '不支持']);

const r2 = await callBridge('od_video_generate', {
  prompt: 'smoke: provider reject', provider: 'volcengine',
});
expectLocalReject('显式未配置 provider → provider_not_configured（零上游调用）', r2,
  ['provider_not_configured', '未配置']);

const r3 = await callBridge('od_image_generate', {
  prompt: 'smoke: gate reject', stage: 'keyframe',
});
expectLocalReject('未批准锚点 → keyframe 阶段本地拒绝（零额度）', r3,
  ['「anchor」审批', '未通过']);

const r4 = await callBridge('od_media_approve', {
  stage: 'anchor', ref: 'assets/img/does-not-exist.png', approved: true,
});
expectLocalReject('审批登记拒绝不存在的产物（不静默成功）', r4, ['不存在', '失败']);

// 审批门：批准真实存在的锚点 → gates.json 落盘；换参考图（内容 hash 变）→ 旧批准失效。
const r5 = await callBridge('od_media_approve', {
  stage: 'anchor', ref: 'assets/img/anchor.png', approved: true, note: 'smoke approve',
});
report('审批登记写 gates.json', r5.ok && r5.text.includes('"approved": true'),
  (r5.text || r5.stderr).slice(0, 400));

writeFileSync(join(smokeWorkdir, 'assets', 'img', 'other.png'),
  Buffer.concat([TINY_PNG, Buffer.from('x')])); // 内容不同 → hash 不同
const r6 = await callBridge('od_image_generate', {
  prompt: 'smoke: hash invalidation', stage: 'keyframe', reference: 'assets/img/other.png',
});
expectLocalReject('换参考图 → 旧锚点批准失效（本地拒绝）', r6, ['旧「anchor」批准失效', '批准失效']);

// 免费层收尾：确认没有任何产物清单/文件被写入（说明确实没发上游请求）。
report('免费层未产生任何 task 产物', listManifests(smokeWorkdir).length === 0,
  `产物清单: ${listManifests(smokeWorkdir).join(', ') || '(空)'}`);

// ---- 真实层：需显式确认额度 ----
if (flags.has('--live')) {
  if (!flags.has('--yes-spend')) {
    console.log('\n已跳过真实层：需要同时传 --live 与 --yes-spend（会产生上游费用）。');
  } else {
    console.log('\n真实层（花钱）：t2i → subject_reference → H3 4s i2v');
    const live1 = await callBridge('od_image_generate', {
      prompt: 'smoke: a single ink-wash quadruped beast, front view 45 degrees above, plain light grey background',
      aspect: '4:3', stage: 'asset', save_as: 'assets/img/smoke-t2i.png',
    }, { timeoutMs: 600000 });
    report('t2i 落盘 + 清单', live1.ok && live1.text.includes('"task_id"'),
      (live1.text || live1.stderr).slice(0, 600));

    const live2 = await callBridge('od_image_generate', {
      prompt: 'Same creature as the reference image — identical design, identical colours. Keep it unchanged, only slightly different camera height.',
      reference: 'assets/img/smoke-t2i.png', operation: 'subject_reference',
      stage: 'anchor', save_as: 'assets/img/smoke-ref.png',
    }, { timeoutMs: 600000 });
    report('subject_reference 落盘（核对 reference_hash）', live2.ok && live2.text.includes('"task_id"'),
      (live2.text || live2.stderr).slice(0, 600));

    const live3 = await callBridge('od_video_generate', {
      prompt: 'The creature takes two slow steps forward, camera locked, same lighting.',
      provider: 'auto', first_frame: 'assets/img/smoke-ref.png',
      duration_sec: 4, save_as: 'assets/video/smoke-h3.mp4',
    }, { timeoutMs: 900000 });
    report('H3 4s 首帧视频（provider=auto，未走错网关）',
      live3.ok && live3.text.includes('"provider"') && live3.text.includes('"task_id"'),
      (live3.text || live3.stderr).slice(0, 800));

    const manifests = listManifests(smokeWorkdir);
    report('至少 3 个 task 级清单落盘', manifests.length >= 3, manifests.join(', '));
    console.log(`\n产物目录：${join(smokeWorkdir, '.bma', 'media')}`);
    console.log('下一步（人工）：核对每个 result.json 的 width/height/output_hash，');
    console.log('并检查 duplicate_suspect 是否为 false；关键姿态/中间帧仍需走审批门。');
  }
}

console.log(`\n合计 PASS=${pass} FAIL=${fail}`);
process.exit(fail === 0 ? 0 : 1);
