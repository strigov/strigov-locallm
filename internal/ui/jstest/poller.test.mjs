import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Poller } from '../static/poller.js';

// harness — поддельные таймеры и видимость вкладки.
function harness() {
  const timers = new Map();
  let next = 1;
  const env = {
    visibleNow: true,
    setTimer(fn, ms) {
      const id = next++;
      timers.set(id, { fn, ms });
      return id;
    },
    clearTimer(id) { timers.delete(id); },
    visible() { return env.visibleNow; },
  };
  const fire = () => {
    const [id, t] = timers.entries().next().value;
    timers.delete(id);
    t.fn();
  };
  return { env, timers, fire };
}

function deferred() {
  let resolve;
  let reject;
  const p = new Promise((a, b) => { resolve = a; reject = b; });
  return { p, resolve, reject };
}

const flush = () => new Promise((resolve) => setImmediate(resolve));

test('раз в секунду и без наложения запросов', async () => {
  const h = harness();
  const pending = [];
  const states = [];
  const p = new Poller({ fetchState: () => { const d = deferred(); pending.push(d); return d.p; },
    onState: (s) => states.push(s), env: h.env });
  p.start();
  assert.equal(pending.length, 1);
  assert.equal(h.timers.size, 0);
  p.visibilityChanged();
  p.start();
  assert.equal(pending.length, 1);
  pending[0].resolve({ n: 1 });
  await flush();
  assert.deepEqual(states, [{ n: 1 }]);
  assert.equal(h.timers.size, 1);
  assert.equal([...h.timers.values()][0].ms, 1000);
  h.fire();
  assert.equal(pending.length, 2);
  pending[1].resolve({ n: 2 });
  await flush();
  assert.deepEqual(states, [{ n: 1 }, { n: 2 }]);
  p.stop();
  assert.equal(h.timers.size, 0);
});

test('скрытая вкладка не опрашивает и продолжает, когда снова видна', async () => {
  const h = harness();
  let calls = 0;
  const p = new Poller({ fetchState: async () => { calls++; return {}; }, onState: () => {}, env: h.env });
  h.env.visibleNow = false;
  p.start();
  await flush();
  assert.equal(calls, 0);
  assert.equal(h.timers.size, 0);
  h.env.visibleNow = true;
  p.visibilityChanged();
  await flush();
  assert.equal(calls, 1);
  assert.equal(h.timers.size, 1);
  h.env.visibleNow = false;
  h.fire();
  await flush();
  assert.equal(calls, 1);
  assert.equal(h.timers.size, 0);
  h.env.visibleNow = true;
  p.visibilityChanged();
  await flush();
  assert.equal(calls, 2);
  p.stop();
});

test('первый 401 останавливает опрос навсегда (F3-D1)', async () => {
  const h = harness();
  let calls = 0;
  let lost = 0;
  let errors = 0;
  const p = new Poller({
    fetchState: async () => {
      calls++;
      const e = new Error('unauthorized');
      e.status = 401;
      throw e;
    },
    onState: () => {}, onError: () => { errors++; }, onAuthLost: () => { lost++; }, env: h.env,
  });
  p.start();
  await flush();
  assert.deepEqual([calls, lost, errors, h.timers.size], [1, 1, 0, 0]);
  p.visibilityChanged();
  p.start();
  await flush();
  assert.deepEqual([calls, lost], [1, 1]);
});

test('прочие ошибки — опрос продолжается', async () => {
  const h = harness();
  const seen = [];
  let n = 0;
  const p = new Poller({
    fetchState: async () => {
      n++;
      if (n === 1) {
        const e = new Error('down');
        e.status = 0;
        throw e;
      }
      return { ok: true };
    },
    onState: (s) => seen.push(s), onError: (e) => seen.push(e.message), env: h.env,
  });
  p.start();
  await flush();
  assert.deepEqual(seen, ['down']);
  assert.equal(h.timers.size, 1);
  h.fire();
  await flush();
  assert.deepEqual(seen, ['down', { ok: true }]);
  p.stop();
});

test('stop снимает таймер и отбрасывает поздний ответ', async () => {
  const h = harness();
  const d = deferred();
  const states = [];
  const p = new Poller({ fetchState: () => d.p, onState: (s) => states.push(s), env: h.env });
  p.start();
  p.stop();
  d.resolve({ late: true });
  await flush();
  assert.deepEqual(states, []);
  assert.equal(h.timers.size, 0);
});
