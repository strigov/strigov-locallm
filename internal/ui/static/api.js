// api.js — клиент /api/admin/* (§9.4, §9.5): токен сессии в sessionStorage (закрыть вкладку — выйти), Bearer, ошибки
// {"error":{…}} — ApiError. Cookie не используются (credentials: 'omit').

export const TOKEN_KEY = 'locallm.token';
const COMMANDS = new Set(['load', 'unload', 'restart', 'pause', 'resume']);

// ApiError — ответ хоста не 2xx (status 0 — нет связи); detail — объект "error" целиком (details, etag, models).
export class ApiError extends Error {
  constructor(status, code, message, detail) {
    super(message || code || `HTTP ${status}`);
    this.name = 'ApiError';
    this.status = status;
    this.code = code || '';
    this.detail = detail || {};
  }
}

function memoryStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: (k) => { m.delete(k); },
  };
}

// createApi — клиент поверх fetchFn (как fetch) и storage (как sessionStorage; null или хранилище с ошибками —
// токен в памяти страницы).
export function createApi(fetchFn, storage) {
  const store = storage || memoryStorage();
  let mem = null;

  function getToken() {
    if (mem) return mem.getItem(TOKEN_KEY) || '';
    try {
      return store.getItem(TOKEN_KEY) || '';
    } catch {
      mem = memoryStorage();
      return '';
    }
  }

  function setToken(token) {
    if (!mem) {
      try {
        store.setItem(TOKEN_KEY, token);
        return;
      } catch {
        mem = memoryStorage();
      }
    }
    mem.setItem(TOKEN_KEY, token);
  }

  function clearToken() {
    if (mem) mem.removeItem(TOKEN_KEY);
    try {
      store.removeItem(TOKEN_KEY);
    } catch {
      // хранилище недоступно: токен был только в памяти
    }
  }

  async function request(method, path, { body, ifMatch, auth = true, text = false } = {}) {
    const headers = {};
    if (auth) {
      const token = getToken();
      if (token) headers.Authorization = `Bearer ${token}`;
    }
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (ifMatch) headers['If-Match'] = ifMatch;
    let resp;
    try {
      resp = await fetchFn(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body),
        cache: 'no-store', credentials: 'omit' });
    } catch {
      throw new ApiError(0, 'network_error', 'Нет связи с хостом');
    }
    if (resp.status === 401 && auth) clearToken();
    if (!resp.ok) {
      let detail = {};
      try {
        const data = await resp.json();
        if (data && typeof data.error === 'object' && data.error !== null) detail = data.error;
      } catch {
        // тело не JSON
      }
      throw new ApiError(resp.status, detail.code || '', detail.message || `HTTP ${resp.status}`, detail);
    }
    const etag = resp.headers.get('ETag') || '';
    if (resp.status === 204) return { data: null, etag };
    return { data: text ? await resp.text() : await resp.json(), etag };
  }

  return {
    token: getToken,
    setToken,
    clearToken,
    // login — {otp} или {admin_key}; без Authorization (§9.4).
    async login(credentials) {
      const { data } = await request('POST', '/api/admin/session', { body: credentials, auth: false });
      setToken(data.token);
      return data;
    },
    async logout() {
      try {
        await request('DELETE', '/api/admin/session');
      } finally {
        clearToken();
      }
    },
    async state() {
      return (await request('GET', '/api/admin/state')).data;
    },
    // models — {data: вид, etag}.
    models() {
      return request('GET', '/api/admin/models');
    },
    putModels(doc, etag) {
      return request('PUT', '/api/admin/models', { body: doc, ifMatch: etag });
    },
    reload() {
      return request('POST', '/api/admin/models/reload', { body: {} });
    },
    async migrate() {
      return (await request('POST', '/api/admin/models/migrate', { body: {} })).data;
    },
    async scan() {
      return (await request('GET', '/api/admin/scan')).data;
    },
    async logs(model) {
      return (await request('GET', `/api/admin/logs?model=${encodeURIComponent(model)}&tail=200`, { text: true })).data;
    },
    // command — load, unload, restart, pause или resume; другое имя не отправляется.
    async command(name, body) {
      if (!COMMANDS.has(name)) throw new Error(`unknown command ${name}`);
      return (await request('POST', `/api/admin/${name}`, { body: body || {} })).data;
    },
  };
}
