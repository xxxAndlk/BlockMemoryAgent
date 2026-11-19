#!/usr/bin/env node
// BMA semantic-edit worker（Harness 根治计划 WP4 §8.3）。
//
// 职责边界（硬约束）：
//   - 只解析、只返回 TextEdit / 诊断，**从不写用户文件**（写盘一律由 Go 侧按 byte range 执行）；
//   - 只通过 stdin/stdout 的 JSON-RPC（行分隔 JSON）通信，每条请求带 request_id；
//   - 不支持 eval / 动态 import 用户指定模块 / 任意命令参数；
//   - 输出上限由 Go 侧与本地 MAX_OUTPUT_CHARS 双重保护（防一条响应打爆管道）。
//
// 协议（每行一个 JSON 对象，响应一行）：
//   请求  {"request_id":"r1","method":"parse_sfc","params":{...}}
//   响应  {"request_id":"r1","ok":true,"result":{...}} / {"request_id":"r1","ok":false,"error":{"code":...,"message":...}}
//
// 方法：
//   ping                → {version, node, languages}
//   parse_sfc           → 解析 Vue SFC，返回 template/script/style 块范围 + 诊断
//   find_css_declaration→ 定位唯一 CSS 声明（selector + property），返回 byte range + 指纹
//   find_template_node  → 定位唯一模板节点（tag + 精确锚点/文本），返回 byte range + 指纹
//   find_ts_symbol      → 用 TypeScript AST 定位声明（symbol name），返回 byte range + 指纹
//   plan_css_edit       → 组合操作：定位唯一声明并生成最小 TextEdit（新值）
//   plan_template_edit  → 组合操作：定位唯一节点并生成最小 TextEdit（替换整段/属性值）
//
// 一律**拒绝**的输入：ExpectedMatches != 1 的批量替换、解析失败的静默回退（返回错误，
// 由 Go 侧按 semantic_edit_mode 决定 fail closed 还是记录）。

import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import * as readline from 'node:readline';
import { pathToFileURL } from 'node:url';
import path from 'node:path';

const VERSION = '1.0.0';
const MAX_OUTPUT_CHARS = 4 * 1024 * 1024; // 4MB 单条响应上限
const SUPPORTED = ['vue', 'css', 'ts', 'js'];

// --- 懒加载依赖（缺失时明确报错，绝不静默降级） ------------------------------
let depsPromise = null;
async function loadDeps() {
  if (depsPromise) return depsPromise;
  depsPromise = (async () => {
    const req = (name) => {
      try {
        return import(name);
      } catch (err) {
        throw new RPCError(-32001, `dependency_missing: ${name} (${err.message})`);
      }
    };
    // @vue/compiler-sfc 与 postcss 是 CJS/ESM 双形态，动态 import 后取 default 兜底。
    const sfcMod = await req('@vue/compiler-sfc');
    const postcssMod = await req('postcss');
    const tsMod = await req('typescript');
    return {
      sfc: sfcMod.default ?? sfcMod,
      postcss: postcssMod.default ?? postcssMod,
      ts: tsMod.default ?? tsMod,
    };
  })();
  return depsPromise;
}

// --- 错误与工具 ---------------------------------------------------------------
class RPCError extends Error {
  constructor(code, message) {
    super(message);
    this.code = code;
  }
}

function sha256(text) {
  return createHash('sha256').update(text, 'utf8').digest('hex');
}

// lineStarts 计算每行起始字节/字符偏移（JS 字符串索引是 UTF-16 code unit，
// 与 Go 侧的 byte range 不同——统一在响应里带 unit:"utf16"，由 Go 侧换算）。
function lineStartsOf(src) {
  const starts = [0];
  for (let i = 0; i < src.length; i++) {
    if (src[i] === '\n') starts.push(i + 1);
  }
  return starts;
}

function offsetToPosition(src, offset) {
  const starts = lineStartsOf(src);
  let lo = 0;
  let hi = starts.length - 1;
  while (lo < hi) {
    const mid = Math.floor((lo + hi + 1) / 2);
    if (starts[mid] <= offset) lo = mid;
    else hi = mid - 1;
  }
  return { line: lo, character: offset - starts[lo] };
}

function positionToOffset(src, position) {
  const starts = lineStartsOf(src);
  const line = Math.max(0, Math.min(position.line ?? 0, starts.length - 1));
  return starts[line] + (position.character ?? 0);
}

// detectLanguage 由显式语言或扩展名推断。
function detectLanguage(params) {
  const lang = String(params.language || '').toLowerCase();
  if (lang) return lang;
  const file = String(params.file || '').toLowerCase();
  if (file.endsWith('.vue')) return 'vue';
  if (file.endsWith('.css') || file.endsWith('.scss') || file.endsWith('.less')) return 'css';
  if (file.endsWith('.ts') || file.endsWith('.tsx')) return 'ts';
  if (file.endsWith('.js') || file.endsWith('.mjs') || file.endsWith('.jsx')) return 'js';
  return '';
}

function readSource(params) {
  if (typeof params.source === 'string') return params.source;
  const file = params.file;
  if (!file) throw new RPCError(-32602, 'params.file or params.source required');
  try {
    return readFileSync(file, 'utf8');
  } catch (err) {
    throw new RPCError(-32002, `read_failed: ${file} (${err.message})`);
  }
}

// --- Vue SFC ----------------------------------------------------------------
async function parseSFC(params) {
  const { sfc } = await loadDeps();
  const src = readSource(params);
  const filename = params.file || 'anonymous.vue';
  const parsed = sfc.parse(src, { filename });
  const errors = (parsed.errors || []).map((e) =>
    typeof e === 'string' ? { message: e } : { message: e.message || String(e), loc: e.loc && e.loc.start });
  const descriptor = parsed.descriptor;
  const block = (b) => (b ? { start: b.loc.start.offset, end: b.loc.end.offset, lang: b.lang || '' } : null);
  return {
    unit: 'utf16',
    content_hash: sha256(src),
    template: block(descriptor.template),
    script: block(descriptor.script || descriptor.scriptSetup),
    script_setup: block(descriptor.scriptSetup),
    styles: (descriptor.styles || []).map((s) => ({ start: s.loc.start.offset, end: s.loc.end.offset,
      lang: s.lang || '', scoped: !!s.scoped })),
    diagnostics: errors,
    ok: errors.length === 0,
  };
}

// --- CSS --------------------------------------------------------------------
// resolveCssSource 返回待解析的 CSS 文本与其在原始文件中的起始偏移。
// .vue 文件先经 SFC 解析取 <style> 块内容：直接把整份 SFC 交给 postcss 会把
// 模板/脚本当 CSS 语法错误（这正是事故里"模型盲写"最容易踩的坑，必须显式处理）。
async function resolveCssSource(params) {
  const src = readSource(params);
  const language = detectLanguage(params);
  if (language !== 'vue') {
    return { src, css: src, baseOffset: 0 };
  }
  const { sfc } = await loadDeps();
  const parsed = sfc.parse(src, { filename: params.file || 'anonymous.vue' });
  const style = (parsed.descriptor.styles || [])[0];
  if (!style) throw new RPCError(-32011, 'no_style_block');
  return { src, css: style.content, baseOffset: style.loc.start.offset };
}

// findCssDeclaration 定位 selector 下唯一的 property 声明，返回范围与指纹。
// 规则（计划 §8.4）：目标数 0 / >=2 一律拒绝，绝不"自动全部替换"。
async function findCssDeclaration(params) {
  const { postcss } = await loadDeps();
  const resolved = await resolveCssSource(params);
  const src = resolved.src;
  const css = resolved.css;
  const base = resolved.baseOffset;
  const selector = String(params.selector || '').trim();
  const property = String(params.property || '').trim().toLowerCase();
  if (!selector || !property) throw new RPCError(-32602, 'selector and property required');
  let root;
  try {
    root = postcss.parse(css, { from: undefined });
  } catch (err) {
    throw new RPCError(-32003, `css_parse_failed: ${err.message}`);
  }
  const matches = [];
  root.walkDecls((decl) => {
    if (decl.prop.toLowerCase() !== property) return;
    const parent = decl.parent;
    if (!parent || !parent.selector) return;
    const normalized = parent.selector.replace(/\s+/g, ' ').trim();
    if (normalized !== selector.replace(/\s+/g, ' ').trim()) return;
    matches.push({
      selector: normalized,
      property: decl.prop,
      value: decl.value,
      important: !!decl.important,
      // postcss 的 source.start.offset 在部分语法下为 undefined，回退到位置换算；
      // 两种情况都要加上 style 块在原始文件中的基址（.vue 场景），否则范围会错位。
      start: base + (decl.source?.start?.offset ?? positionToOffset(css, decl.source?.start)),
      end: base + (decl.source?.end?.offset ?? positionToOffset(css, decl.source?.end)),
    });
  });
  return {
    unit: 'utf16',
    content_hash: sha256(src),
    matches,
    count: matches.length,
  };
}

// normalizeValue 去掉首尾空白与分号，供指纹比对。
function normalizeValue(v) {
  return String(v).trim().replace(/;+$/, '').trim();
}

// planCssEdit 组合定位 + 生成最小 TextEdit（只替换 value 段，不动其余字节）。
async function planCssEdit(params) {
  const found = await findCssDeclaration(params);
  const expected = params.expected_matches ?? 1;
  if (expected !== 1) {
    throw new RPCError(-32004, `expected_matches must be 1 (got ${expected}); bulk replace is forbidden`);
  }
  if (found.count === 0) {
    throw new RPCError(-32005, `target_not_found: ${params.selector} { ${params.property} }`);
  }
  if (found.count > 1) {
    throw new RPCError(-32006, `target_ambiguous: ${found.count} declarations match; refusing to guess`);
  }
  const m = found.matches[0];
  const expectedValue = params.expected_value;
  if (expectedValue != null && normalizeValue(expectedValue) !== normalizeValue(m.value)) {
    throw new RPCError(-32007, `fingerprint_mismatch: expected ${JSON.stringify(expectedValue)}, actual ${JSON.stringify(m.value)}`);
  }
  const newValue = normalizeValue(params.new_value ?? '');
  if (!newValue) throw new RPCError(-32602, 'new_value required');
  const src = readSource(params);
  // postcss 的 decl.source.end.offset 是**开区间且已含结尾分号**（slice(start,end) =
  // "width: 360px;"）：替换文本必须自己补回分号，否则会把
  // `width: 420px; margin-left` 粘成 `width: 420px margin-left`（语法破坏，
  // 且是最难在 diff 里一眼看出的回归）。这里以原文实际内容为准，不假设 postcss 口径。
  const end = m.end;
  const beforeText = src.slice(m.start, end);
  const keepSemicolon = /;\s*$/.test(beforeText);
  const newText = `${m.property}: ${newValue}${m.important ? ' !important' : ''}${keepSemicolon ? ';' : ''}`;
  return {
    unit: 'utf16',
    content_hash: found.content_hash,
    edits: [{
      start: m.start,
      end,
      new_text: newText,
      // before_hash = 被替换段的原文指纹（Go 侧应用前可二次校验，防并发改写）。
      before_hash: sha256(beforeText),
      target_kind: 'css_declaration',
      selector: m.selector,
      property: m.property,
      from_value: m.value,
      to_value: newValue,
    }],
    diagnostics: [],
  };
}

// --- 模板节点 ----------------------------------------------------------------
// 模板解析用 @vue/compiler-dom（由 compiler-sfc 传递依赖提供）：只做结构定位，
// 不重新打印（打印会改写空白/引号，违反"未修改区域 byte-for-byte 相同"）。
async function findTemplateNode(params) {
  const { sfc } = await loadDeps();
  const src = readSource(params);
  const filename = params.file || 'anonymous.vue';
  const parsed = sfc.parse(src, { filename });
  if (!parsed.descriptor.template) throw new RPCError(-32010, 'no_template_block');
  const template = parsed.descriptor.template;
  const tag = String(params.tag || '').trim();
  const anchor = String(params.anchor || '').trim();
  const text = String(params.text || '').trim();
  if (!tag) throw new RPCError(-32602, 'tag required');
  if (!anchor && !text) throw new RPCError(-32602, 'anchor or text required (ambiguous targets are rejected)');

  const tplSrc = template.content;
  const tplStart = template.loc.start.offset;
  const re = new RegExp(`<${tag}\\b`, 'g');
  const candidates = [];
  let match;
  while ((match = re.exec(tplSrc)) !== null) {
    // 找到对应闭合标签（同 tag 的简单配对；嵌套同 tag 用计数处理）。
    const startIdx = match.index;
    const endIdx = findMatchingClose(tplSrc, startIdx, tag);
    if (endIdx < 0) continue;
    const snippet = tplSrc.slice(startIdx, endIdx);
    if (anchor && !snippet.includes(anchor)) continue;
    if (text && !snippet.includes(text)) continue;
    candidates.push({
      start: tplStart + startIdx,
      end: tplStart + endIdx,
      snippet_hash: sha256(snippet),
      tag,
    });
  }
  return {
    unit: 'utf16',
    content_hash: sha256(src),
    matches: candidates,
    count: candidates.length,
  };
}

// findMatchingClose 从 startIdx 起找到同 tag 的配对闭合标签结束位置（不含结束标签之后的偏移）。
function findMatchingClose(src, startIdx, tag) {
  const open = `<${tag}`;
  const close = `</${tag}>`;
  let depth = 0;
  let i = startIdx;
  while (i < src.length) {
    const nextOpen = src.indexOf(open, i);
    const nextClose = src.indexOf(close, i);
    if (nextClose < 0) return -1;
    if (nextOpen >= 0 && nextOpen < nextClose) {
      // 自闭合标签不计深度
      const afterOpen = src.slice(nextOpen, nextOpen + open.length + 200);
      const selfClosing = /\/>/.test(afterOpen.slice(0, afterOpen.indexOf('>') + 1));
      if (!selfClosing) depth++;
      i = nextOpen + open.length;
      continue;
    }
    depth--;
    i = nextClose + close.length;
    if (depth === 0) return i;
  }
  return -1;
}

async function planTemplateEdit(params) {
  const found = await findTemplateNode(params);
  const expected = params.expected_matches ?? 1;
  if (expected !== 1) {
    throw new RPCError(-32004, `expected_matches must be 1 (got ${expected}); bulk replace is forbidden`);
  }
  if (found.count === 0) throw new RPCError(-32005, 'target_not_found');
  if (found.count > 1) {
    throw new RPCError(-32006, `target_ambiguous: ${found.count} template nodes match; refusing to guess`);
  }
  const m = found.matches[0];
  if (params.expected_fingerprint && params.expected_fingerprint !== m.snippet_hash) {
    throw new RPCError(-32007, `fingerprint_mismatch: expected ${params.expected_fingerprint}, actual ${m.snippet_hash}`);
  }
  const newText = params.new_text;
  if (typeof newText !== 'string' || newText === '') {
    throw new RPCError(-32602, 'new_text required');
  }
  return {
    unit: 'utf16',
    content_hash: found.content_hash,
    edits: [{ start: m.start, end: m.end, new_text: newText, target_kind: 'template_node', tag: m.tag }],
    diagnostics: [],
  };
}

// --- TypeScript / JavaScript -------------------------------------------------
async function findTsSymbol(params) {
  const { ts } = await loadDeps();
  const src = readSource(params);
  const name = String(params.symbol || '').trim();
  if (!name) throw new RPCError(-32602, 'symbol required');
  const fileName = params.file || 'anonymous.ts';
  const sf = ts.createSourceFile(fileName, src, ts.ScriptTarget.Latest, true,
    fileName.endsWith('.tsx') ? ts.ScriptKind.TSX : fileName.endsWith('.js') ? ts.ScriptKind.JS : ts.ScriptKind.TS);
  const matches = [];
  const visit = (node) => {
    const ids = [];
    if (node.name && ts.isIdentifier(node.name)) ids.push(node.name.text);
    for (const d of node.declarationList?.declarations || []) {
      if (d.name && ts.isIdentifier(d.name)) ids.push(d.name.text);
    }
    if (ids.includes(name)) {
      matches.push({
        kind: ts.SyntaxKind[node.kind],
        start: node.getStart(sf),
        end: node.getEnd(),
        snippet_hash: sha256(src.slice(node.getStart(sf), node.getEnd())),
      });
    }
    ts.forEachChild(node, visit);
  };
  visit(sf);
  const diagnostics = [];
  // 语法诊断：只报语法级（不做类型检查，避免需要 tsconfig/项目上下文）。
  const sfDiag = sf.parseDiagnostics || [];
  for (const d of sfDiag) {
    diagnostics.push({
      category: 'error',
      code: d.code,
      message: ts.flattenDiagnosticMessageText(d.messageText, '\n'),
      start: d.start ?? 0,
    });
  }
  return {
    unit: 'utf16',
    content_hash: sha256(src),
    matches,
    count: matches.length,
    diagnostics,
  };
}

// --- 分发 ---------------------------------------------------------------------
const methods = {
  ping: async () => ({
    version: VERSION,
    node: process.version,
    languages: SUPPORTED,
    writes_files: false,
  }),
  parse_sfc: parseSFC,
  find_css_declaration: findCssDeclaration,
  plan_css_edit: planCssEdit,
  find_template_node: findTemplateNode,
  plan_template_edit: planTemplateEdit,
  find_ts_symbol: findTsSymbol,
};

async function handleLine(line) {
  let req;
  try {
    req = JSON.parse(line);
  } catch (err) {
    return { request_id: null, ok: false, error: { code: -32700, message: `parse_error: ${err.message}` } };
  }
  const id = req.request_id ?? null;
  const method = String(req.method || '');
  const fn = methods[method];
  if (!fn) {
    return { request_id: id, ok: false, error: { code: -32601, message: `unknown_method: ${method}` } };
  }
  try {
    const result = await fn(req.params || {});
    return { request_id: id, ok: true, result };
  } catch (err) {
    const code = err instanceof RPCError ? err.code : -32000;
    return { request_id: id, ok: false, error: { code, message: err.message || String(err) } };
  }
}

function emit(obj) {
  let line;
  try {
    line = JSON.stringify(obj);
  } catch (err) {
    line = JSON.stringify({ request_id: null, ok: false, error: { code: -32000, message: `encode_failed: ${err.message}` } });
  }
  if (line.length > MAX_OUTPUT_CHARS) {
    line = JSON.stringify({
      request_id: obj.request_id ?? null,
      ok: false,
      error: { code: -32008, message: `response_too_large: ${line.length} chars > ${MAX_OUTPUT_CHARS}` },
    });
  }
  process.stdout.write(line + '\n');
}

async function main() {
  // 单请求模式（调试/一次性调用）：--once 从 argv 读 JSON
  const onceIdx = process.argv.indexOf('--once');
  if (onceIdx >= 0) {
    const payload = process.argv[onceIdx + 1] || '';
    emit(await handleLine(payload));
    return;
  }
  const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
  for await (const line of rl) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    emit(await handleLine(trimmed));
  }
}

// applyEditsForTest 测试辅助：按 TextEdit 数组把文本应用（从后往前应用，避免偏移漂移）。
// 生产写盘由 Go 侧执行，本函数仅供单元测试验证"最小编辑"语义。
function applyEditsForTest(src, edits) {
  const sorted = [...edits].sort((a, b) => b.start - a.start);
  let out = src;
  for (const e of sorted) {
    out = out.slice(0, e.start) + e.new_text + out.slice(e.end);
  }
  return out;
}

// 导出给单元测试直接调用（不启动 stdin 循环）。
export { handleLine, methods, sha256, positionToOffset, offsetToPosition, applyEditsForTest };

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href;
if (invokedDirectly) {
  main().catch((err) => {
    process.stderr.write(`semantic-edit worker fatal: ${err && err.stack ? err.stack : err}\n`);
    process.exit(1);
  });
}
