// media-core：open-design MCP 桥的确定性媒体策略层（2026-10-24 P0/P1）。
//
// 为什么要单独一层：桥原先直接拼 prompt 调 daemon，没有能力校验、没有 task 级产物
// 清单、任务缺文件名时还会"取项目里最新文件"兜底——并发任务可能拿到同一张图，
// Agent 只能靠 MD5 猜"提示词是不是被忽略"。本模块把三类确定性逻辑抽出来，既不依赖
// daemon（可单测），也不依赖 MCP SDK：
//
//   1. 能力路由：operation / input_fidelity / negative_prompt / seed / aspect / stage
//      先按已声明能力校验，**不支持就在发上游请求之前报错**（零费用、错误可见）；
//   2. 产物对账：每个 task 落 <workdir>/.bma/media/<task_id>/{request,result}.json，
//      记录 prompt/reference/output/postprocess 四类 hash 与真实像素尺寸；
//      同一输出 hash 对应不同 request hash → duplicate_suspect；
//   3. 阶段审批：anchor / keyframe 两道人审门（gates.json，按参考图 hash 记忆）——
//      未批准锚点时 keyframe/inbetween/运动视频一律本地拒绝。
//
// 约定：不记录 API key、Authorization、参考图 base64（request.json 只有 hash 与元数据）。

import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { dirname, extname, join } from 'node:path';

// ---- 能力模型 -------------------------------------------------------------

// 保守默认能力集（未声明时）：只承诺当前链路真实具备的能力。
// 不默认宣称 image_edit / inpaint —— 那需要真正的局部编辑通道（遮罩/保真度），
// 提示词里的"其它部分逐像素不变"没有接口保障（计划 §2.2）。
export const IMAGE_CAPABILITY_DEFAULTS = ['t2i', 'subject_reference', 'aspect'];
export const VIDEO_CAPABILITY_DEFAULTS = ['t2v', 'i2v', 'aspect', 'duration'];

// 当前认识的图像操作（od_image_generate.operation 取值）。
export const IMAGE_OPERATIONS = ['t2i', 'subject_reference', 'image_edit', 'inpaint'];
// 可选的严格参数 → 需要的能力名。
export const IMAGE_PARAM_CAPABILITIES = {
  input_fidelity: 'input_fidelity',
  negative_prompt: 'negative_prompt',
  seed: 'seed',
};

// 基础画幅；扩展画幅需要显式能力声明（避免未知比例被上游静默改成方图）。
export const BASE_ASPECTS = ['1:1', '16:9', '9:16', '4:3', '3:4'];
export const EXTENDED_ASPECTS = { '3:2': 'aspect_3:2', '2:3': 'aspect_2:3' };

// 媒体阶段（决定审批门）。
export const STAGES = ['asset', 'anchor', 'keyframe', 'inbetween'];

// 需要"锚点已批准"才能进入的阶段。
export const ANCHOR_GATED_STAGES = ['keyframe', 'inbetween'];

// normalizeCapabilities 把声明值（逗号分隔字符串 / 数组 / 空）归一为小写数组。
// 未声明（空）时用默认集；声明过则**只认声明**（显式关闭某能力是使用者的权利）。
export function normalizeCapabilities(declared, defaults) {
  if (declared === undefined || declared === null) return [...defaults];
  const raw = Array.isArray(declared) ? declared : String(declared).split(',');
  const out = [];
  for (const item of raw) {
    const v = String(item).trim().toLowerCase();
    if (v && !out.includes(v)) out.push(v);
  }
  if (out.length === 0) return [...defaults];
  return out;
}

// resolveCapabilities 能力来源优先级（2026-10-24）：
//   1. 桥 env（OD_IMAGE_CAPABILITIES / OD_VIDEO_CAPABILITIES，由仓库根 .env 的
//      IMAGE_CAPABILITIES / VIDEO_CAPABILITIES 经 plugins.yaml ${VAR:} 插值进来）；
//   2. daemon 探测值（providers.<插槽>.capabilities——entrypoint 写入，留作未来 daemon
//      若在 /api/media/config 暴露该字段时的自动生效路径；当前 daemon 的 readMaskedConfig
//      会丢弃未知字段，所以实际上读不到）；
//   3. 保守默认集（不宣称 image_edit/inpaint）。
// 显式声明过就只认声明（关闭某能力是使用者的权利）。
export function resolveCapabilities({ envDeclared, detected, defaults }) {
  const env = normalizeCapabilities(envDeclared, []);
  if (env.length > 0) return env;
  const det = normalizeCapabilities(detected, []);
  if (det.length > 0) return det;
  return [...defaults];
}

// CapabilityError：能力/参数不满足，调用方必须在上游请求之前抛出。
export class CapabilityError extends Error {
  constructor(message, detail = {}) {
    super(message);
    this.name = 'CapabilityError';
    this.code = 'capability_unsupported';
    this.detail = detail;
  }
}

// resolveOperation 决定本次图像操作，并校验能力是否具备。
// 兼容旧参数：只给 reference 时，有 subject_reference 就走它，其次 image_edit，
// 两者都没有 → 本地报错（不再"静默丢弃参考图"）。
export function resolveOperation({ operation, reference, capabilities }) {
  const caps = capabilities || IMAGE_CAPABILITY_DEFAULTS;
  const hasRef = reference !== undefined && reference !== null && String(reference).trim() !== '';
  const explicit = String(operation || '').trim().toLowerCase();
  if (explicit) {
    if (!IMAGE_OPERATIONS.includes(explicit)) {
      throw new CapabilityError(
        `未知 operation: ${explicit}（可选 ${IMAGE_OPERATIONS.join(' / ')}）`,
        { requested: explicit, available: caps });
    }
    if (!caps.includes(explicit)) {
      throw new CapabilityError(
        `当前媒体通道不支持 operation=${explicit}：已声明能力 [${caps.join(',')}]。` +
        '需要该能力请配置具备它的通道（如真正的局部编辑/遮罩通道）——这与模型强弱无关。',
        { requested: explicit, available: caps });
    }
    if (hasRef && explicit === 't2i') {
      throw new CapabilityError(
        'operation=t2i 不接受 reference（纯文生图）；参考图请用 subject_reference 或 image_edit',
        { requested: explicit, available: caps });
    }
    if (!hasRef && (explicit === 'subject_reference' || explicit === 'image_edit' || explicit === 'inpaint')) {
      throw new CapabilityError(`operation=${explicit} 需要 reference（工作目录相对路径）`,
        { requested: explicit, available: caps });
    }
    return explicit;
  }
  if (!hasRef) return 't2i';
  if (caps.includes('subject_reference')) return 'subject_reference';
  if (caps.includes('image_edit')) return 'image_edit';
  throw new CapabilityError(
    '当前媒体通道不支持带参考图生成（既无 subject_reference 也无 image_edit）；' +
    '请配置相应通道，或去掉 reference 走纯文生图。',
    { requested: 'subject_reference|image_edit', available: caps });
}

// validateStrictParams 校验"只在上游支持时才允许传"的参数。
// 不支持的参数**必须报错**，不允许改 schema 后静默丢弃（计划 §5.3）。
export function validateStrictParams(args, capabilities) {
  const caps = capabilities || IMAGE_CAPABILITY_DEFAULTS;
  for (const [param, cap] of Object.entries(IMAGE_PARAM_CAPABILITIES)) {
    const v = args[param];
    const provided = v !== undefined && v !== null && String(v).trim() !== '';
    if (provided && !caps.includes(cap)) {
      throw new CapabilityError(
        `当前媒体通道不支持 ${param}：已声明能力 [${caps.join(',')}]。` +
        '请去掉该参数或改用具备该能力的通道（静默忽略会让"只改腿、其它不变"这类要求无法兑现）。',
        { requested: param, available: caps });
    }
  }
}

// validateAspect 画幅校验：基础集永远可用；扩展集需显式能力（否则本地报错，
// 不允许上游把未知比例静默改成方图）。
export function validateAspect(aspect, capabilities) {
  if (aspect === undefined || aspect === null || String(aspect).trim() === '') return null;
  const a = String(aspect).trim();
  const caps = capabilities || IMAGE_CAPABILITY_DEFAULTS;
  if (BASE_ASPECTS.includes(a)) return a;
  const cap = EXTENDED_ASPECTS[a];
  if (!cap) {
    throw new CapabilityError(
      `未知画幅 ${a}（可选 ${[...BASE_ASPECTS, ...Object.keys(EXTENDED_ASPECTS)].join(' / ')}）`,
      { requested: a, available: caps });
  }
  if (!caps.includes(cap)) {
    throw new CapabilityError(
      `当前媒体通道未声明画幅 ${a}（需能力 ${cap}）；请改用已声明画幅或用纯文生图通道。`,
      { requested: a, available: caps });
  }
  return a;
}

// validateStage 归一媒体阶段。
export function normalizeStage(stage) {
  const s = String(stage || 'asset').trim().toLowerCase();
  if (!STAGES.includes(s)) {
    throw new CapabilityError(`未知 stage: ${s}（可选 ${STAGES.join(' / ')}）`,
      { requested: s, available: STAGES });
  }
  return s;
}

// ---- hash 与图像尺寸 ------------------------------------------------------

export function hashBuffer(buf) {
  return createHash('sha256').update(buf).digest('hex');
}

export function hashString(s) {
  return createHash('sha256').update(String(s == null ? '' : s), 'utf8').digest('hex');
}

// pngSize 从 PNG IHDR 读取真实像素尺寸（零依赖：不引入图像库）。
// 非 PNG 返回 null（调用方按 null 记，不猜）。
export function pngSize(buf) {
  if (!buf || buf.length < 24) return null;
  const sig = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
  for (let i = 0; i < 8; i++) {
    if (buf[i] !== sig[i]) return null;
  }
  if (buf.toString('ascii', 12, 16) !== 'IHDR') return null;
  const width = buf.readUInt32BE(16);
  const height = buf.readUInt32BE(20);
  if (!width || !height) return null;
  return { width, height };
}

// ---- task 级产物清单 ------------------------------------------------------

export const MEDIA_ROOT_REL = '.bma/media';

export function taskDir(workdir, taskId) {
  return join(workdir, MEDIA_ROOT_REL, String(taskId));
}

// writeTaskManifests 写 request.json / result.json（原子性不重要：JSON 小文件，
// 读侧只做审计；写入失败由调用方决定是否致命）。
export function writeTaskManifests({ workdir, taskId, request, result }) {
  const dir = taskDir(workdir, taskId);
  mkdirSync(dir, { recursive: true });
  const reqPath = join(dir, 'request.json');
  const resPath = join(dir, 'result.json');
  if (request) writeFileSync(reqPath, JSON.stringify(request, null, 2) + '\n', 'utf8');
  if (result) writeFileSync(resPath, JSON.stringify(result, null, 2) + '\n', 'utf8');
  return { dir, requestPath: reqPath, resultPath: resPath };
}

export function readTaskManifest(workdir, taskId, kind) {
  const p = join(taskDir(workdir, taskId), `${kind}.json`);
  try {
    return JSON.parse(readFileSync(p, 'utf8'));
  } catch {
    return null;
  }
}

// buildImageRequestManifest 请求清单（绝不写 key/base64：只留 hash 与元数据）。
export function buildImageRequestManifest({ taskId, operation, provider, model, prompt, reference, aspect, stage, requestedAt }) {
  return {
    task_id: String(taskId),
    operation,
    provider: provider || null,
    model: model || null,
    prompt_hash: hashString(prompt || ''),
    reference_hash: reference ? hashString(reference) : null,
    has_reference: Boolean(reference),
    aspect: aspect || null,
    stage: stage || 'asset',
    requested_at: requestedAt || new Date().toISOString(),
  };
}

// buildImageResultManifest 结果清单：原图与后处理图分别记 hash（区分上游重复与本地处理重复）。
export function buildImageResultManifest({ taskId, file, workspacePath, rawHash, outputHash, postprocess, width, height, duplicate, model }) {
  return {
    task_id: String(taskId),
    status: 'done',
    file: file || null,
    workspace_path: workspacePath || null,
    output_hash: outputHash || null,
    raw_hash: rawHash || null,
    postprocess_hash: postprocess ? outputHash : null,
    width: width || null,
    height: height || null,
    duplicate_suspect: Boolean(duplicate),
    model: model || null,
  };
}

// ---- 重复产物检测 ---------------------------------------------------------

// 进程内输出 hash 索引。同一 output hash 若对应**不同** prompt_hash/reference_hash，
// 说明上游忽略提示词或并发任务串了产物 → 标 duplicate_suspect，不计为可接受新帧。
const outputIndex = new Map();

export function rememberOutput(outputHash, { promptHash, referenceHash, taskId }) {
  if (!outputHash) return { duplicate: false, first: null };
  const prev = outputIndex.get(outputHash);
  if (!prev) {
    outputIndex.set(outputHash, { promptHash, referenceHash, taskId });
    return { duplicate: false, first: null };
  }
  const sameRequest = prev.promptHash === promptHash && (prev.referenceHash || null) === (referenceHash || null);
  return { duplicate: !sameRequest, first: prev };
}

// resetOutputIndex 仅供测试复位。
export function resetOutputIndex() {
  outputIndex.clear();
}

// ---- 阶段审批门 -----------------------------------------------------------

export function gatesPath(workdir) {
  return join(workdir, MEDIA_ROOT_REL, 'gates.json');
}

export function readGates(workdir) {
  try {
    const raw = readFileSync(gatesPath(workdir), 'utf8');
    const parsed = JSON.parse(raw);
    return parsed && typeof parsed === 'object' ? parsed : {};
  } catch {
    return {};
  }
}

// approveGate 记录一次人审结果（stage=anchor|keyframe）。ref 是本次被审的产物路径，
// 记录其内容 hash：换图即 hash 变 → 旧批准自动失效。
export function approveGate({ workdir, stage, ref, approved, note }) {
  const s = String(stage || '').trim().toLowerCase();
  if (s !== 'anchor' && s !== 'keyframe') {
    throw new CapabilityError(`未知审批阶段: ${stage}（仅支持 anchor / keyframe）`, { requested: stage });
  }
  const abs = join(workdir, String(ref || '').replace(/\\/g, '/'));
  let refHash = null;
  try {
    refHash = hashBuffer(readFileSync(abs));
  } catch {
    throw new Error(`审批产物不存在或不可读: ${ref}（须为工作目录内相对路径）`);
  }
  const gates = readGates(workdir);
  gates[s] = {
    approved: Boolean(approved),
    ref: String(ref),
    ref_hash: refHash,
    at: new Date().toISOString(),
    ...(note ? { note: String(note) } : {}),
  };
  const p = gatesPath(workdir);
  mkdirSync(dirname(p), { recursive: true });
  writeFileSync(p, JSON.stringify(gates, null, 2) + '\n', 'utf8');
  return gates[s];
}

// gateState 读取某阶段审批状态。
export function gateState(workdir, stage) {
  const g = readGates(workdir)[String(stage || '').trim().toLowerCase()];
  if (!g) return { approved: false, ref: null, ref_hash: null, missing: true };
  return { approved: Boolean(g.approved), ref: g.ref || null, ref_hash: g.ref_hash || null, at: g.at || null };
}

// assertGateAllows 确定性硬门：未批准锚点禁止进入 keyframe/inbetween，
// 以及任何带首帧的运动视频（防止"未批准就一口气 16 帧"）。
// 参考图 hash 与被批准的不一致 = 旧批准失效。
export function assertGateAllows({ workdir, stage, refHash, need }) {
  const required = need || (ANCHOR_GATED_STAGES.includes(String(stage || '').trim().toLowerCase()) ? 'anchor' : null);
  if (!required) return;
  const state = gateState(workdir, required);
  if (!state.approved) {
    throw new CapabilityError(
      `未通过「${required}」审批，禁止进入 ${stage || 'next'} 阶段：先生成 ${required === 'anchor' ? '单张锚点' : '关键姿态'}，` +
      '用 ShowArtifact 展示并 ask_user 审批，批准后调用 od_media_approve 再继续（本地硬门，不发上游请求）。',
      { requested: stage, gate: required, gate_state: state });
  }
  if (refHash && state.ref_hash && refHash !== state.ref_hash) {
    throw new CapabilityError(
      `参考图已更换（当前 hash=${String(refHash).slice(0, 12)}…，已批准的=${String(state.ref_hash).slice(0, 12)}…），` +
      `旧「${required}」批准失效：请重新展示并审批。`,
      { requested: stage, gate: required, gate_state: state });
  }
}

// ---- 视频 provider 路由（2026-10-24 P0）-------------------------------------
//
// 事故形态（会话 #66）：当前唯一可用链路是 grok→minimax-media，但 Agent 先强制
// provider=volcengine（401），又强制 openai（AIHubMix key 缺失）——全是路由错误，
// 不是模型质量结论。修正：
//   - provider 缺省/auto → 用 daemon 当前已配置的通道（桥主动探测，Agent 不必知道
//     MiniMax 被包装成 grok 协议）；
//   - 显式指定未配置 provider → 提交前本地拒绝（provider_not_configured），
//     并列出当前可用能力，绝不发上游请求。

export const VIDEO_PROVIDERS = ['volcengine', 'openai', 'grok'];

export class ProviderNotConfigured extends Error {
  constructor(message, detail = {}) {
    super(message);
    this.name = 'ProviderNotConfigured';
    this.code = 'provider_not_configured';
    this.detail = detail;
  }
}

// selectVideoProvider 纯函数路由：explicit > env > 探测（auto）> 兜底。
// 任何显式 provider 不在 configured 里（且 configured 非空）→ 本地报错。
// configured 为空（daemon 探测失败）时无法证伪，退回 env/兜底（生成本身也依赖 daemon）。
export function selectVideoProvider({ explicit, envProvider, detected, fallback }) {
  const configured = Array.isArray(detected) ? detected.filter(Boolean) : [];
  const checkConfigured = (p) => {
    if (configured.length > 0 && !configured.includes(p)) {
      throw new ProviderNotConfigured(
        `provider=${p} 未配置（当前 daemon 已配置通道：[${configured.join(',')}]）。` +
        '请用 provider=auto（或省略该参数）让桥按现有通道路由——协议名属于适配层，Agent 无需指定。',
        { requested: p, available: configured });
    }
    return p;
  };
  const explicitP = String(explicit || '').trim().toLowerCase();
  if (explicitP && explicitP !== 'auto') {
    if (!VIDEO_PROVIDERS.includes(explicitP)) {
      throw new ProviderNotConfigured(
        `未知视频 provider: ${explicitP}（可选 auto / ${VIDEO_PROVIDERS.join(' / ')}）`,
        { requested: explicitP, available: configured });
    }
    return { provider: checkConfigured(explicitP), reason: 'explicit' };
  }
  const envP = String(envProvider || '').trim().toLowerCase();
  if (envP) {
    if (!VIDEO_PROVIDERS.includes(envP)) {
      throw new ProviderNotConfigured(`桥 env OD_VIDEO_PROVIDER 非法: ${envP}`,
        { requested: envP, available: configured });
    }
    return { provider: checkConfigured(envP), reason: 'env' };
  }
  if (configured.length > 0) {
    return { provider: configured[0], reason: 'auto' };
  }
  return { provider: fallback || VIDEO_PROVIDERS[0], reason: 'fallback' };
}

// fileExists 小工具（server.js 的读侧校验复用）。
export function fileExists(p) {
  try {
    return statSync(p).isFile();
  } catch {
    return false;
  }
}

// extOf 取扩展名（小写，含点）。
export function extOf(name, fallback) {
  const e = extname(String(name || '')).toLowerCase();
  return e || fallback || '';
}

export function ensureExists(path) {
  return existsSync(path);
}
