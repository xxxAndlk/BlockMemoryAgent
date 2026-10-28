// media-core.test.mjs —— 媒体桥确定性策略层单测（2026-10-24 P0/P1，计划 §8.2）。
//
// 运行：node --test docker/open-design-mcp/    （零第三方依赖，不产生任何费用）
// 覆盖：能力/参数/画幅本地拒绝、provider auto 与未配置拒绝、task 清单内容、
// duplicate_suspect、阶段审批门。
import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, mkdirSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import {
  IMAGE_CAPABILITY_DEFAULTS,
  VIDEO_CAPABILITY_DEFAULTS,
  CapabilityError,
  ProviderNotConfigured,
  normalizeCapabilities,
  resolveCapabilities,
  resolveOperation,
  validateStrictParams,
  validateAspect,
  normalizeStage,
  pngSize,
  hashString,
  buildImageRequestManifest,
  buildImageResultManifest,
  writeTaskManifests,
  readTaskManifest,
  rememberOutput,
  resetOutputIndex,
  approveGate,
  gateState,
  assertGateAllows,
  selectVideoProvider,
} from './media-core.js';

function tempWorkdir() {
  return mkdtempSync(join(tmpdir(), 'bma-media-core-'));
}

// ---- 能力路由 -------------------------------------------------------------

test('默认能力集不宣称 image_edit / inpaint 可用', () => {
  assert.ok(!IMAGE_CAPABILITY_DEFAULTS.includes('image_edit'));
  assert.ok(!IMAGE_CAPABILITY_DEFAULTS.includes('inpaint'));
  assert.ok(IMAGE_CAPABILITY_DEFAULTS.includes('subject_reference'));
});

test('未支持的操作在本地拒绝（capability_unsupported）', () => {
  assert.throws(
    () => resolveOperation({ operation: 'image_edit', reference: 'a.png', capabilities: IMAGE_CAPABILITY_DEFAULTS }),
    (err) => err instanceof CapabilityError && err.code === 'capability_unsupported' && /不支持/.test(err.message),
  );
  assert.throws(
    () => resolveOperation({ operation: 'inpaint', reference: 'a.png', capabilities: IMAGE_CAPABILITY_DEFAULTS }),
    (err) => err instanceof CapabilityError,
  );
});

test('只给 reference 时按能力映射到 subject_reference', () => {
  assert.equal(resolveOperation({ reference: 'a.png', capabilities: IMAGE_CAPABILITY_DEFAULTS }), 'subject_reference');
  // 有 image_edit 无 subject_reference 时映射到编辑。
  assert.equal(resolveOperation({ reference: 'a.png', capabilities: ['t2i', 'image_edit'] }), 'image_edit');
  // 两者都不可用 → 本地报错，不静默丢参考图。
  assert.throws(() => resolveOperation({ reference: 'a.png', capabilities: ['t2i'] }), CapabilityError);
  // 无参考图 = 纯文生图。
  assert.equal(resolveOperation({ capabilities: ['t2i'] }), 't2i');
});

test('operation=t2i 带 reference 明确报错', () => {
  assert.throws(() => resolveOperation({ operation: 't2i', reference: 'a.png', capabilities: ['t2i'] }), CapabilityError);
});

test('不支持的严格参数必须报错，不得静默丢弃', () => {
  for (const param of ['input_fidelity', 'negative_prompt', 'seed']) {
    assert.throws(
      () => validateStrictParams({ [param]: 'x' }, IMAGE_CAPABILITY_DEFAULTS),
      (err) => err instanceof CapabilityError && err.message.includes(param),
    );
    // 声明具备该能力后允许。
    assert.doesNotThrow(() => validateStrictParams({ [param]: 'x' }, [...IMAGE_CAPABILITY_DEFAULTS, param]));
  }
  // 未传则不校验。
  assert.doesNotThrow(() => validateStrictParams({}, ['t2i']));
});

test('扩展画幅需要显式能力，未知画幅本地报错', () => {
  assert.equal(validateAspect('4:3', IMAGE_CAPABILITY_DEFAULTS), '4:3');
  assert.equal(validateAspect(undefined, IMAGE_CAPABILITY_DEFAULTS), null);
  assert.throws(() => validateAspect('3:2', IMAGE_CAPABILITY_DEFAULTS), CapabilityError);
  assert.equal(validateAspect('3:2', [...IMAGE_CAPABILITY_DEFAULTS, 'aspect_3:2']), '3:2');
  assert.throws(() => validateAspect('7:5', IMAGE_CAPABILITY_DEFAULTS), CapabilityError);
});

test('stage 归一与非法值拒绝', () => {
  assert.equal(normalizeStage(undefined), 'asset');
  assert.equal(normalizeStage('KeyFrame'), 'keyframe');
  assert.throws(() => normalizeStage('final'), CapabilityError);
});

test('能力声明解析：逗号串/数组/空值', () => {
  assert.deepEqual(normalizeCapabilities('t2i, image_edit', VIDEO_CAPABILITY_DEFAULTS), ['t2i', 'image_edit']);
  assert.deepEqual(normalizeCapabilities(['A', 'a', ' b '], []), ['a', 'b']);
  assert.deepEqual(normalizeCapabilities(undefined, ['x']), ['x']);
  assert.deepEqual(normalizeCapabilities('', ['x']), ['x']);
});


test('能力来源优先级：env > daemon 探测 > 保守默认', () => {
  // env 声明优先（仓库根 .env 是唯一手改点）
  assert.deepEqual(
    resolveCapabilities({ envDeclared: 't2i,image_edit', detected: ['t2i', 'subject_reference'], defaults: ['t2i'] }),
    ['t2i', 'image_edit'],
  );
  // env 未声明 → 用 daemon 探测值
  assert.deepEqual(
    resolveCapabilities({ envDeclared: '', detected: ['t2i', 'inpaint'], defaults: ['t2i'] }),
    ['t2i', 'inpaint'],
  );
  // 都没有 → 保守默认
  assert.deepEqual(
    resolveCapabilities({ envDeclared: undefined, detected: [], defaults: ['t2i', 'aspect'] }),
    ['t2i', 'aspect'],
  );
});

// ---- provider 路由 --------------------------------------------------------

test('auto 选择当前唯一配置通道', () => {
  const r = selectVideoProvider({ explicit: undefined, detected: ['grok'], fallback: 'volcengine' });
  assert.equal(r.provider, 'grok');
  assert.equal(r.reason, 'auto');
  assert.equal(selectVideoProvider({ explicit: 'auto', detected: ['grok'] }).provider, 'grok');
});

test('显式未配置 provider 在发请求前拒绝', () => {
  assert.throws(
    () => selectVideoProvider({ explicit: 'volcengine', detected: ['grok'] }),
    (err) => err instanceof ProviderNotConfigured && err.code === 'provider_not_configured' && /grok/.test(err.message),
  );
  assert.throws(
    () => selectVideoProvider({ explicit: 'openai', detected: ['grok'] }),
    ProviderNotConfigured,
  );
  // 显式且已配置 → 放行。
  assert.equal(selectVideoProvider({ explicit: 'grok', detected: ['grok'] }).provider, 'grok');
  // 未知协议名。
  assert.throws(() => selectVideoProvider({ explicit: 'minimax', detected: ['grok'] }), ProviderNotConfigured);
});

test('探测失败（无 configured）时退回 env/兜底，不误拒', () => {
  assert.equal(selectVideoProvider({ explicit: 'volcengine', detected: [] }).provider, 'volcengine');
  assert.equal(selectVideoProvider({ envProvider: 'grok', detected: [] }).provider, 'grok');
  assert.equal(selectVideoProvider({ detected: [], fallback: 'volcengine' }).provider, 'volcengine');
});

// ---- 产物清单 -------------------------------------------------------------

test('request/result 清单含 hash 且不含 key/base64', () => {
  const wd = tempWorkdir();
  const request = buildImageRequestManifest({
    taskId: 't-1', operation: 'subject_reference', provider: 'custom-image', model: 'image-01',
    prompt: '图一中的角色改成侧视', reference: 'assets/img/anchor.png', aspect: '4:3',
    stage: 'keyframe', requestedAt: '2026-10-24T00:00:00.000Z',
  });
  const result = buildImageResultManifest({
    taskId: 't-1', file: 'x.png', workspacePath: 'assets/img/out.png',
    rawHash: 'rawhash', outputHash: 'outhash', postprocess: true, width: 2048, height: 1536,
  });
  writeTaskManifests({ workdir: wd, taskId: 't-1', request, result });
  const rq = readTaskManifest(wd, 't-1', 'request');
  const rs = readTaskManifest(wd, 't-1', 'result');
  assert.equal(rq.operation, 'subject_reference');
  assert.equal(rq.prompt_hash, hashString('图一中的角色改成侧视'));
  assert.equal(rq.stage, 'keyframe');
  assert.equal(rs.width, 2048);
  assert.equal(rs.postprocess_hash, 'outhash');
  assert.equal(rs.raw_hash, 'rawhash');
  const raw = readFileSync(join(wd, '.bma/media/t-1/request.json'), 'utf8') +
    readFileSync(join(wd, '.bma/media/t-1/result.json'), 'utf8');
  for (const banned of ['apiKey', 'API_KEY', 'Authorization', 'base64', 'data:image']) {
    assert.ok(!raw.includes(banned), `manifest 不应含 ${banned}`);
  }
});

test('不同 prompt/reference 得到相同 output hash → duplicate_suspect', () => {
  resetOutputIndex();
  const first = rememberOutput('same-out', { promptHash: 'p1', referenceHash: 'r1', taskId: 't1' });
  assert.equal(first.duplicate, false);
  // 同 prompt+reference 重跑（合法重抽）不算 suspect。
  assert.equal(rememberOutput('same-out', { promptHash: 'p1', referenceHash: 'r1', taskId: 't1b' }).duplicate, false);
  // 不同 prompt（或参考图）却同输出 → 上游忽视提示词/并发串产物。
  assert.equal(rememberOutput('same-out', { promptHash: 'p2', referenceHash: 'r1', taskId: 't2' }).duplicate, true);
  assert.equal(rememberOutput('same-out', { promptHash: 'p1', referenceHash: 'r2', taskId: 't3' }).duplicate, true);
});

test('pngSize 读取真实像素，非 PNG 返回 null', () => {
  const wd = tempWorkdir();
  const p = join(wd, 'a.png');
  // 手工拼最小 PNG 头（IHDR 宽高）。
  const head = Buffer.alloc(33);
  Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]).copy(head, 0);
  head.write('IHDR', 12, 'ascii');
  head.writeUInt32BE(2048, 16);
  head.writeUInt32BE(1536, 20);
  writeFileSync(p, head);
  assert.deepEqual(pngSize(head), { width: 2048, height: 1536 });
  assert.equal(pngSize(Buffer.from('not a png at all')), null);
});

// ---- 阶段审批门 -----------------------------------------------------------

test('未批准锚点时 keyframe/inbetween 阶段本地拒绝', () => {
  const wd = tempWorkdir();
  assert.throws(
    () => assertGateAllows({ workdir: wd, stage: 'keyframe' }),
    (err) => err instanceof CapabilityError && /未通过「anchor」审批/.test(err.message),
  );
  assert.throws(() => assertGateAllows({ workdir: wd, stage: 'inbetween' }), CapabilityError);
  // asset / anchor 阶段不需要前置审批。
  assert.doesNotThrow(() => assertGateAllows({ workdir: wd, stage: 'asset' }));
  assert.doesNotThrow(() => assertGateAllows({ workdir: wd, stage: 'anchor' }));
});

test('批准锚点后放行；换参考图（hash 变）使批准失效', () => {
  const wd = tempWorkdir();
  mkdirSync(join(wd, 'assets/img'), { recursive: true });
  writeFileSync(join(wd, 'assets/img/anchor-a.png'), Buffer.from('A'));
  approveGate({ workdir: wd, stage: 'anchor', ref: 'assets/img/anchor-a.png', approved: true });
  assert.equal(gateState(wd, 'anchor').approved, true);
  assert.doesNotThrow(() => assertGateAllows({ workdir: wd, stage: 'keyframe' }));

  // 同一参考图（hash 相同）继续可用。
  const { ref_hash: hashA } = gateState(wd, 'anchor');
  assert.doesNotThrow(() => assertGateAllows({ workdir: wd, stage: 'keyframe', refHash: hashA }));

  // 换参考图 → hash 不同 → 旧批准不生效。
  writeFileSync(join(wd, 'assets/img/anchor-b.png'), Buffer.from('B'));
  const otherHash = hashString('something-else');
  assert.throws(
    () => assertGateAllows({ workdir: wd, stage: 'keyframe', refHash: otherHash }),
    /旧「anchor」批准失效/,
  );
});

test('拒绝锚点后仍禁止进入 keyframe', () => {
  const wd = tempWorkdir();
  writeFileSync(join(wd, 'anchor.png'), Buffer.from('A'));
  approveGate({ workdir: wd, stage: 'anchor', ref: 'anchor.png', approved: false, note: '角色漂了' });
  assert.equal(gateState(wd, 'anchor').approved, false);
  assert.throws(() => assertGateAllows({ workdir: wd, stage: 'keyframe' }), CapabilityError);
});

test('审批未知 stage 报错', () => {
  const wd = tempWorkdir();
  assert.throws(() => approveGate({ workdir: wd, stage: 'inbetween', ref: 'x.png', approved: true }), CapabilityError);
});
