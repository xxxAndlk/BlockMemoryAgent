// 上游 od 守护进程补丁（BMA，2026-08-18；2026-10-14 增 i2i 形状 + 恢复 aspect 映射）。
//
// ① size（2026-08-18 起）：火山方舟 Agent Plan 网关图像模型对 size 有约束，上游两条
// OpenAI 兼容生图路径的 size 兜底恒 1024x1024，必被 InvalidParameter 拒绝。原补丁把
// 两处钉死 2048x2048（seedream-5.0-lite 要求 ≥3686400px）；2026-10-14 切
// doubao-seedream-5.0-pro 后恢复按 aspect 出图（pro 显式尺寸 1280×720..2048×2048、
// 边长为 16 的倍数）——此前 aspect 参数被钉死方图吞掉，9:16 海报也出方图。
//   - RESTORE_ASPECT=true ：size = bmaImageSizeFor(ctx.aspect)（16:9→2048x1152 等）
//   - RESTORE_ASPECT=false：回退恒 2048x2048（网关拒非方图时一行切回后重建镜像）
//   替换点两处：
//   1) renderVolcengineImage（volcengine provider）
//   2) renderCustomOpenAIImage（custom-image provider——BMA 实际走这条）
//
// ② i2i 形状（2026-10-14，实测）：上游 custom-image 的图生图切到 OpenAI 风格
// /images/edits（body.images=[{image_url}]），而火山方舟 plan 网关无该端点（404，
// 实测）。方舟原生形状 = POST /images/generations + image 参数（data URI）——直连
// 实测 HTTP 200 返回 data[0].url。本补丁把该分支改为保持 generations URL、设
// body.image。代价：custom-image 的 i2i 固定为方舟形状；若换真 OpenAI 网关需回退
// 本替换（或另配 provider）。响应若为 URL，daemon 的 bytesFromOpenAICompatibleData
// 会自动下载（文生图路径同款，已验证）。
//
// 锚点校验：上游 dist 变更导致锚点缺失时直接 throw（docker build 失败，
// 响亮失败不静默），此时需人工复核上游的新实现。
const fs = require('node:fs');

const FILE = '/app/apps/daemon/dist/media/index.js';
const RESTORE_ASPECT = true;

const SIZE_ASPECT = "size: bmaImageSizeFor(ctx.aspect), // BMA patch: aspect→尺寸映射（seedream-5.0-pro 显式尺寸 1280x720..2048x2048、16 倍数）";
const SIZE_CONST = "size: '2048x2048', // BMA patch: 恒方图（aspect 恢复被网关拒绝时的回退）";
const SIZE = RESTORE_ASPECT ? SIZE_ASPECT : SIZE_CONST;

// 函数声明提升：追加到文件尾即可在任意位置被调用。
const HELPER = `
// BMA patch: aspect → 显式尺寸（全部 ≤2048² 像素积、边长为 16 的倍数）
function bmaImageSizeFor(aspect) {
  switch (aspect) {
    case '16:9': return '2048x1152';
    case '9:16': return '1152x2048';
    case '4:3':  return '2048x1536';
    case '3:4':  return '1536x2048';
    default:     return '2048x2048';
  }
}
`;

const I2I_NEEDLE = `        if (!wireModel.startsWith('gpt-image-')) {
            body.response_format = 'b64_json';
        }
        body.images = [{ image_url: ctx.imageRef.dataUrl }];
        url = buildOpenAIImageEditUrl(baseUrl);`;
const I2I_REPLACEMENT = `        // BMA patch: Agent Plan 网关无 /images/edits——i2i 保持 generations URL，
        // 参考图走方舟原生 image 参数（data URI）。
        body.image = ctx.imageRef.dataUrl;`;

// [anchor 函数, 原文 needle, 替换文本]；needle 在 anchor 之后的首次出现即补丁点。
const PATCHES = [
  ['async function renderVolcengineImage', 'size: openaiSizeFor(ctx.model, ctx.aspect),', SIZE],
  // custom-image 的 needle 全文件唯一（上游仅此一处用 'gpt-image-1' 字面量）。
  ['async function renderCustomOpenAIImage', "size: openaiSizeFor('gpt-image-1', ctx.aspect),", SIZE],
  // i2i 形状补丁（同函数内，anchor 之后查找）。
  ['async function renderCustomOpenAIImage', I2I_NEEDLE, I2I_REPLACEMENT],
];

let src = fs.readFileSync(FILE, 'utf8');
for (const [anchor, needle, replacement] of PATCHES) {
  const fi = src.indexOf(anchor);
  if (fi < 0) throw new Error(`锚点缺失: ${anchor}`);
  const ni = src.indexOf(needle, fi);
  if (ni < 0) throw new Error(`锚点缺失: ${anchor} 之后未找到 ${needle.slice(0, 60)}…`);
  src = src.slice(0, ni) + replacement + src.slice(ni + needle.length);
}
console.log(`[bma-patch] size → ${RESTORE_ASPECT ? 'aspect 映射' : '2048x2048'}；i2i → 方舟 generations/image 形状`);
if (RESTORE_ASPECT && !src.includes('function bmaImageSizeFor')) {
  src += HELPER;
  console.log('[bma-patch] bmaImageSizeFor 已注入');
}
fs.writeFileSync(FILE, src);
