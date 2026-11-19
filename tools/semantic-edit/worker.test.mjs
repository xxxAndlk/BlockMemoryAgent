// worker.test.mjs 语义编辑 worker 的行为测试（计划 §8.5 必测场景的 Node 侧部分）：
//   - 同文件两个相似节点，只命中唯一目标；目标数 0 / 2 一律拒绝（不猜、不批量替换）；
//   - 共享 CSS 多消费者时按 selector 精确定位，未授权声明不被改动；
//   - 指纹过期拒绝；
//   - 未修改区域 byte-for-byte 相同（只返回 value 段 TextEdit）；
//   - 解析失败/未知方法返回结构化错误，不静默回退。
//
// 跑法：cd tools/semantic-edit && node --test

import test from 'node:test';
import assert from 'node:assert/strict';
import { handleLine, applyEditsForTest } from './worker.mjs';

async function call(method, params) {
  const res = await handleLine(JSON.stringify({ request_id: 't', method, params }));
  return res;
}

const SFC = `<template>
  <div class="band">
    <span class="item header-item-name"><span class="label">商品名称：</span>移动流量套餐</span>
    <span class="item header-item-time"><span class="label">下单时间：</span>2026-11-18</span>
  </div>
</template>

<style scoped>
.band { display: flex; }
.header-item-name { width: 360px; margin-left: 30px; }
.header-item-time { width: 220px; margin-left: 60px; }
.shared-label { min-width: 4.2em; }
</style>
`;

test('ping reports no writes', async () => {
  const res = await call('ping', {});
  assert.equal(res.ok, true);
  assert.equal(res.result.writes_files, false);
});

test('parse_sfc returns block ranges and content hash', async () => {
  const res = await call('parse_sfc', { file: 'x.vue', source: SFC });
  assert.equal(res.ok, true);
  assert.equal(res.result.ok, true);
  assert.ok(res.result.template.start < res.result.template.end);
  assert.ok(res.result.styles.length >= 1);
  assert.match(res.result.content_hash, /^[0-9a-f]{64}$/);
});

test('plan_css_edit replaces only the declaration value', async () => {
  const res = await call('plan_css_edit', {
    file: 'x.vue', source: SFC,
    selector: '.header-item-name', property: 'width',
    expected_value: '360px', new_value: '420px',
  });
  assert.equal(res.ok, true, JSON.stringify(res.error));
  assert.equal(res.result.edits.length, 1);
  const applied = applyEditsForTest(SFC, res.result.edits);
  assert.ok(applied.includes('.header-item-name { width: 420px; margin-left: 30px; }'));
  // 未授权声明必须逐字不变
  assert.ok(applied.includes('.header-item-time { width: 220px; margin-left: 60px; }'));
  assert.ok(applied.includes('.shared-label { min-width: 4.2em; }'));
  // 模板与其它内容不动
  assert.ok(applied.includes('移动流量套餐'));
});

test('plan_css_edit rejects ambiguous targets', async () => {
  const dup = `<style>
.a { width: 1px; }
.a { width: 2px; }
</style>`;
  const res = await call('plan_css_edit', {
    file: 'x.vue', source: dup, selector: '.a', property: 'width', new_value: '3px',
  });
  assert.equal(res.ok, false);
  assert.equal(res.error.code, -32006);
  assert.match(res.error.message, /target_ambiguous/);
});

test('plan_css_edit rejects missing targets', async () => {
  const res = await call('plan_css_edit', {
    file: 'x.vue', source: SFC, selector: '.nope', property: 'width', new_value: '1px',
  });
  assert.equal(res.ok, false);
  assert.equal(res.error.code, -32005);
});

test('plan_css_edit rejects stale fingerprint', async () => {
  const res = await call('plan_css_edit', {
    file: 'x.vue', source: SFC, selector: '.header-item-name', property: 'width',
    expected_value: '999px', new_value: '420px',
  });
  assert.equal(res.ok, false);
  assert.equal(res.error.code, -32007);
  assert.match(res.error.message, /fingerprint_mismatch/);
});

test('plan_css_edit rejects bulk replace intent', async () => {
  const res = await call('plan_css_edit', {
    file: 'x.vue', source: SFC, selector: '.header-item-name', property: 'width',
    expected_matches: 2, new_value: '420px',
  });
  assert.equal(res.ok, false);
  assert.equal(res.error.code, -32004);
});

test('find_template_node matches unique node by anchor', async () => {
  const res = await call('find_template_node', {
    file: 'x.vue', source: SFC, tag: 'span', anchor: 'header-item-time',
  });
  assert.equal(res.ok, true);
  assert.equal(res.result.count, 1);
  assert.ok(res.result.matches[0].end > res.result.matches[0].start);
});

test('find_template_node reports ambiguity instead of guessing', async () => {
  const res = await call('find_template_node', {
    file: 'x.vue', source: SFC, tag: 'span', anchor: 'class="item',
  });
  assert.equal(res.ok, true);
  assert.ok(res.result.count >= 2, `expected multiple matches, got ${res.result.count}`);
});

test('plan_template_edit rejects ambiguity', async () => {
  const res = await call('plan_template_edit', {
    file: 'x.vue', source: SFC, tag: 'span', anchor: 'class="item', new_text: '<span/>',
  });
  assert.equal(res.ok, false);
  assert.equal(res.error.code, -32006);
});

test('find_ts_symbol locates declarations with syntax diagnostics', async () => {
  const ts = `export function add(a: number, b: number) { return a + b }
const remove = (x: number) => x - 1
`;
  const res = await call('find_ts_symbol', { file: 'x.ts', source: ts, symbol: 'add' });
  assert.equal(res.ok, true);
  assert.equal(res.result.count, 1);
  assert.equal(res.result.diagnostics.length, 0);
});

test('find_ts_symbol reports parse diagnostics', async () => {
  const res = await call('find_ts_symbol', { file: 'x.ts', source: 'function (', symbol: 'x' });
  assert.equal(res.ok, true);
  assert.ok(res.result.diagnostics.length > 0);
});

test('unknown method fails closed', async () => {
  const res = await call('do_magic', {});
  assert.equal(res.ok, false);
  assert.equal(res.error.code, -32601);
});

test('malformed json returns parse error without crashing', async () => {
  const res = await handleLine('{not json');
  assert.equal(res.ok, false);
  assert.equal(res.error.code, -32700);
});

test('css parse failure returns structured error (no regex fallback)', async () => {
  const res = await call('find_css_declaration', {
    file: 'x.css', source: '.a { color: ', selector: '.a', property: 'color',
  });
  assert.equal(res.ok, false);
  assert.equal(res.error.code, -32003);
});
