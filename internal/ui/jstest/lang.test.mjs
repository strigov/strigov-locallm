import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { LANGS, keys, lang, locale, pickLang, setLang, t } from '../static/lang.js';
import { contextLabel, errorText, forceConfirmText, gb, kindText, localTime, savedText, statusLine, statusText } from '../static/logic.js';

const withLang = (code, fn) => {
  const prev = lang();
  setLang(code);
  try {
    fn();
  } finally {
    setLang(prev);
  }
};

test('у каждого языка те же ключи, что у русского, и те же подстановки', () => {
  const ru = keys('ru');
  const subs = (s) => (s.match(/\{\d\}/g) || []).sort().join();
  const ruSubs = {};
  withLang('ru', () => {
    for (const k of ru) ruSubs[k] = subs(t(k));
  });
  for (const code of LANGS) {
    assert.deepEqual([...keys(code)].sort(), [...ru].sort(), code);
    withLang(code, () => {
      for (const k of ru) {
        assert.notEqual(t(k), '', `${code}.${k}`);
        assert.equal(subs(t(k)), ruSubs[k], `${code}.${k}`);
      }
    });
  }
});

test('каждый data-i18n в index.html есть в словаре', () => {
  const html = readFileSync(new URL('../static/index.html', import.meta.url), 'utf8');
  const used = [...html.matchAll(/data-i18n="([^"]+)"/g)].map((m) => m[1]);
  assert.ok(used.length > 20, `data-i18n: ${used.length}`);
  for (const k of used) assert.ok(keys('ru').includes(k), k);
});

test('каждый t(\'…\') в app.js и logic.js есть в словаре', () => {
  for (const f of ['app.js', 'logic.js']) {
    const src = readFileSync(new URL(`../static/${f}`, import.meta.url), 'utf8');
    for (const m of src.matchAll(/\bt\('([A-Za-z_]+)'/g)) assert.ok(keys('ru').includes(m[1]), `${f}: ${m[1]}`);
  }
});

test('pickLang и setLang', () => {
  assert.equal(pickLang(['de-DE', 'zh-CN', 'en']), 'zh');
  assert.equal(pickLang(['en_US']), 'en');
  assert.equal(pickLang(['fr']), '');
  assert.equal(pickLang(undefined), '');
  withLang('en', () => {
    assert.equal(setLang('xx'), 'en');
    assert.equal(locale(), 'en-US');
  });
});

test('подстановки: {0} и {1}; отсутствующий аргумент остаётся как есть', () => {
  withLang('en', () => {
    assert.equal(t('migrated', 'a', 'b'), 'Models moved to a. Copy of the old file: b');
    assert.equal(t('since'), 'since {0}');
  });
});

test('логика панели на английском', () => {
  withLang('en', () => {
    const st = { state: 'ready', model: 'a', target: null, inflight: 2, paused: true, failed: [] };
    assert.equal(statusLine(st), 'Paused · Ready: a · answering (2)');
    assert.equal(statusLine({ ...st, state: 'draining', paused: false }), 'Finishing responses: a → unload');
    assert.equal(gb(1536), '1.5 GB');
    assert.equal(statusText('unloaded'), 'not loaded');
    assert.equal(kindText('mmproj'), 'projector');
    assert.equal(contextLabel({ context_length: 8192, next: { context_length: 0 } }), '8192 → — after restart');
    assert.equal(errorText({ status: 409, code: 'host_paused', message: 'x' }), 'The host is paused (x)');
    assert.equal(errorText({ status: 0 }), 'No connection to the host');
    assert.equal(forceConfirmText(0), 'No unfinished responses. Unload the model?');
    assert.equal(savedText({ restart_required: ['a'], warnings: [1, 2] }, 'Saved: a'), 'Saved: a. Restart needed: a. Warnings: 2');
    assert.notEqual(localTime('2026-11-12T12:00:00Z'), '');
  });
});

test('логика панели на китайском', () => {
  withLang('zh', () => {
    assert.equal(statusLine({ state: 'empty', paused: false, failed: [] }), '未加载模型');
    assert.equal(statusLine({ state: 'loading', model: 'q', paused: true, failed: [] }), '已暂停 · 正在加载：q…');
    assert.equal(gb(12288), '12.0 GB');
    assert.equal(statusText('loaded'), '已加载');
    assert.equal(errorText({ status: 429 }), '失败次数过多，请等待一分钟');
  });
});
