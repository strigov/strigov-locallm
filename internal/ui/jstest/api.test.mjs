import { test } from 'node:test';
import assert from 'node:assert/strict';
import { ApiError, TOKEN_KEY, createApi } from '../static/api.js';

function memoryStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: (k) => { m.delete(k); },
  };
}

// fakeFetch — ответы по очереди (Error — отказ сети); calls — [{url, init}].
function fakeFetch(...responses) {
  const calls = [];
  const fn = async (url, init) => {
    calls.push({ url, init });
    const r = responses.shift();
    if (r instanceof Error) throw r;
    return r;
  };
  fn.calls = calls;
  return fn;
}

const json = (status, body, headers = {}) => new Response(JSON.stringify(body),
  { status, headers: { 'Content-Type': 'application/json', ...headers } });
const apiErr = (status, code) => json(status, { error: { message: 'no', type: 'authentication_error', code } });

test('вход без Authorization сохраняет токен', async () => {
  const storage = memoryStorage();
  storage.setItem(TOKEN_KEY, 'old');
  const f = fakeFetch(json(200, { token: 'tok', expires_at: '2026-11-16T00:00:00Z' }));
  const api = createApi(f, storage);
  const data = await api.login({ otp: 'code' });
  assert.equal(data.token, 'tok');
  assert.equal(storage.getItem(TOKEN_KEY), 'tok');
  assert.equal(api.token(), 'tok');
  const { url, init } = f.calls[0];
  assert.equal(url, '/api/admin/session');
  assert.equal(init.method, 'POST');
  assert.equal(init.headers.Authorization, undefined);
  assert.equal(init.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(init.body), { otp: 'code' });
  assert.equal(init.credentials, 'omit');
  assert.equal(init.cache, 'no-store');
});

test('запросы несут Bearer', async () => {
  const storage = memoryStorage();
  storage.setItem(TOKEN_KEY, 'tok');
  const f = fakeFetch(json(200, { state: 'empty' }));
  const api = createApi(f, storage);
  assert.deepEqual(await api.state(), { state: 'empty' });
  assert.equal(f.calls[0].url, '/api/admin/state');
  assert.equal(f.calls[0].init.method, 'GET');
  assert.equal(f.calls[0].init.headers.Authorization, 'Bearer tok');
  assert.equal(f.calls[0].init.body, undefined);
});

test('ошибка — ApiError с кодом и доп. полями; PUT несёт If-Match', async () => {
  const f = fakeFetch(json(412, { error: { message: 'changed', type: 'invalid_request_error',
    code: 'config_changed_on_disk', etag: '"e2"' } }));
  const api = createApi(f, memoryStorage());
  await assert.rejects(api.putModels({ models: [] }, '"e1"'), (err) => {
    assert.ok(err instanceof ApiError);
    assert.equal(err.status, 412);
    assert.equal(err.code, 'config_changed_on_disk');
    assert.equal(err.message, 'changed');
    assert.equal(err.detail.etag, '"e2"');
    return true;
  });
  assert.equal(f.calls[0].init.method, 'PUT');
  assert.equal(f.calls[0].url, '/api/admin/models');
  assert.equal(f.calls[0].init.headers['If-Match'], '"e1"');
  assert.deepEqual(JSON.parse(f.calls[0].init.body), { models: [] });
});

test('422: details доступны в err.detail', async () => {
  const f = fakeFetch(json(422, { error: { message: 'invalid', type: 'invalid_request_error', code: 'config_invalid',
    details: [{ field: 'models[0].id', message: 'bad name' }] } }));
  await assert.rejects(createApi(f, memoryStorage()).putModels({ models: [] }, '"e"'), (err) => {
    assert.deepEqual(err.detail.details, [{ field: 'models[0].id', message: 'bad name' }]);
    return true;
  });
});

test('нет связи — статус 0; тело не JSON — код пустой', async () => {
  const api = createApi(fakeFetch(new TypeError('fetch failed'), new Response('oops', { status: 502 })), memoryStorage());
  await assert.rejects(api.state(), (err) => err instanceof ApiError && err.status === 0 && err.code === 'network_error');
  await assert.rejects(api.state(), (err) => err.status === 502 && err.code === '' && err.message === 'HTTP 502');
});

test('models отдаёт вид и ETag', async () => {
  const api = createApi(fakeFetch(json(200, { mode: 'file', models: [] }, { ETag: '"abc"' })), memoryStorage());
  const r = await api.models();
  assert.equal(r.etag, '"abc"');
  assert.equal(r.data.mode, 'file');
});

test('reload и migrate шлют пустой объект', async () => {
  const f = fakeFetch(json(200, { etag: '"x"', restart_required: [], warnings: [] }, { ETag: '"x"' }),
    json(200, { models_file: 'C:\\m\\models.yaml', backup: 'C:\\m\\locallm.yaml.bak' }));
  const api = createApi(f, memoryStorage());
  assert.equal((await api.reload()).data.etag, '"x"');
  assert.equal((await api.migrate()).backup, 'C:\\m\\locallm.yaml.bak');
  assert.deepEqual(f.calls.map((c) => [c.init.method, c.url, c.init.body]),
    [['POST', '/api/admin/models/reload', '{}'], ['POST', '/api/admin/models/migrate', '{}']]);
});

test('журнал — текст, имя модели экранируется', async () => {
  const f = fakeFetch(new Response('line 1\nline 2\n', { status: 200, headers: { 'Content-Type': 'text/plain; charset=utf-8' } }));
  const api = createApi(f, memoryStorage());
  assert.equal(await api.logs('a b&c'), 'line 1\nline 2\n');
  assert.equal(f.calls[0].url, '/api/admin/logs?model=a%20b%26c&tail=200');
});

test('выход стирает токен, даже если хост отказал', async () => {
  const storage = memoryStorage();
  storage.setItem(TOKEN_KEY, 'tok');
  const f = fakeFetch(new Response(null, { status: 204 }));
  await createApi(f, storage).logout();
  assert.equal(storage.getItem(TOKEN_KEY), null);
  assert.equal(f.calls[0].init.method, 'DELETE');
  assert.equal(f.calls[0].url, '/api/admin/session');
  storage.setItem(TOKEN_KEY, 'tok2');
  await assert.rejects(createApi(fakeFetch(apiErr(401, 'invalid_admin_credentials')), storage).logout(),
    (err) => err.status === 401);
  assert.equal(storage.getItem(TOKEN_KEY), null);
});

test('401 на любом запросе стирает токен', async () => {
  const storage = memoryStorage();
  storage.setItem(TOKEN_KEY, 'tok');
  const api = createApi(fakeFetch(apiErr(401, 'invalid_admin_credentials')), storage);
  await assert.rejects(api.state(), (err) => err.status === 401 && err.code === 'invalid_admin_credentials');
  assert.equal(storage.getItem(TOKEN_KEY), null);
  assert.equal(api.token(), '');
});

test('хранилище с ошибками — токен в памяти', async () => {
  const broken = {
    getItem() { throw new Error('denied'); },
    setItem() { throw new Error('denied'); },
    removeItem() { throw new Error('denied'); },
  };
  const f = fakeFetch(json(200, { token: 'tok', expires_at: 'x' }), json(200, { state: 'ready' }));
  const api = createApi(f, broken);
  await api.login({ admin_key: 'k' });
  assert.equal(api.token(), 'tok');
  await api.state();
  assert.equal(f.calls[1].init.headers.Authorization, 'Bearer tok');
  const plain = createApi(fakeFetch(json(200, { token: 't2', expires_at: 'x' })), null);
  await plain.login({ admin_key: 'k' });
  assert.equal(plain.token(), 't2');
});

test('только пять команд', async () => {
  const f = fakeFetch(json(202, { accepted: true, state: 'loading' }), json(200, { paused: true, persisted: true }));
  const api = createApi(f, memoryStorage());
  assert.deepEqual(await api.command('load', { model: 'a' }), { accepted: true, state: 'loading' });
  assert.equal(f.calls[0].url, '/api/admin/load');
  assert.deepEqual(JSON.parse(f.calls[0].init.body), { model: 'a' });
  await api.command('pause');
  assert.equal(f.calls[1].url, '/api/admin/pause');
  assert.equal(f.calls[1].init.body, '{}');
  await assert.rejects(api.command('migrate'), /unknown command/);
  await assert.rejects(api.command('session'), /unknown command/);
  assert.equal(f.calls.length, 2);
});
