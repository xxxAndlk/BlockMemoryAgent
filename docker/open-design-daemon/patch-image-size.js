// 上游 od 守护进程生图 size 补丁（BMA，2026-08-18）。
//
// 背景（实测）：火山方舟 Agent Plan 网关（/api/plan/v3）图像模型仅支持
// doubao-seedream-5.0-lite，该模型要求图片总像素 ≥ 3686400（≈1920x1920）；
// 上游两条 OpenAI 兼容生图路径的 size 兜底都恒为 1024x1024，必被
// InvalidParameter 拒绝。本补丁把两处 size 钉为 2048x2048（4194304px）：
//   1) renderVolcengineImage（volcengine provider，size 走 openaiSizeFor 兜底）
//   2) renderCustomOpenAIImage（custom-image provider，size 硬编码
//      openaiSizeFor('gpt-image-1', ...)）——BMA 实际走的就是这条：
//      桥插件以目录 id "custom-image" 调用，wire 模型名由 media-config 的
//      providers.custom-image.model 提供（= doubao-seedream-5.0-lite）。
//
// 锚点校验：上游 dist 变更导致锚点缺失时直接 throw（docker build 失败，
// 响亮失败不静默），此时需人工复核上游的新实现。
const fs = require('node:fs');

const FILE = '/app/apps/daemon/dist/media/index.js';
const SIZE = "size: '2048x2048', // BMA patch: Agent Plan seedream-5.0-lite 要求 >=3686400px";

// [anchor 函数, 原文 needle]；needle 在 anchor 之后的首次出现即补丁点。
const PATCHES = [
  ['async function renderVolcengineImage', 'size: openaiSizeFor(ctx.model, ctx.aspect),'],
  // custom-image 的 needle 全文件唯一（上游仅此一处用 'gpt-image-1' 字面量）。
  ['async function renderCustomOpenAIImage', "size: openaiSizeFor('gpt-image-1', ctx.aspect),"],
];

let src = fs.readFileSync(FILE, 'utf8');
for (const [anchor, needle] of PATCHES) {
  const fi = src.indexOf(anchor);
  if (fi < 0) throw new Error(`锚点缺失: ${anchor}`);
  const ni = src.indexOf(needle, fi);
  if (ni < 0) throw new Error(`锚点缺失: ${anchor} 之后未找到 ${needle}`);
  src = src.slice(0, ni) + SIZE + src.slice(ni + needle.length);
  console.log(`[bma-patch] ${anchor} size → 2048x2048`);
}
fs.writeFileSync(FILE, src);
