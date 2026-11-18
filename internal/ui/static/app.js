// app.js — экраны панели (§10.2): вход, шапка, модели, редактор, поиск .gguf, баннеры, журнал. Текст в DOM — только
// textContent и createElement (§10.1): имя модели, путь и вывод llama-server не становятся кодом страницы. Язык —
// lang.js: постоянные подписи помечены data-i18n и переводятся при смене языка все сразу (translatePage).
import { createApi } from './api.js';
import { LANGS, LANG_NAMES, lang, pickLang, setLang, t } from './lang.js';
import { Poller } from './poller.js';
import {
  brokenFileConfirmText, canForceUnload, contextLabel, docFromView, errorText, forceConfirmText, formatArgs, gb, groupByRoot, kindText,
  localTime, parseArgs, parseField, projectorOf, relPath, rowsKey, savedText, setProjector, statusLine, statusText,
  suggestName,
} from './logic.js';

const LANG_KEY = 'locallm.lang'; // выбранный язык в localStorage

let storage = null;
try {
  storage = window.sessionStorage;
} catch {
  storage = null;
}
const api = createApi((url, init) => fetch(url, init), storage);

const ui = {
  poller: null,
  st: null, // последний ответ /api/admin/state
  rows: '', // rowsKey последней отрисованной таблицы
  banners: '',
  view: null, // вид GET /api/admin/models
  viewEtag: '',
  applied: undefined,
  disk: undefined,
  editor: null, // {index, doc, etag, vm, mmproj}
  pickForEditor: false,
  scan: null,
  logModel: '',
  pollFailed: false, // в сообщении — ошибка опроса: снять при следующем ответе
};
const lockedArgs = new WeakMap(); // строка профиля hand_only → её аргументы без изменений
let profileSeq = 0; // номера id полей профилей: подписи связаны с полями

const el = (id) => document.getElementById(id);

function make(tag, text, cls) {
  const node = document.createElement(tag);
  if (text !== undefined && text !== null) node.textContent = String(text);
  if (cls) node.className = cls;
  return node;
}

function button(text, onClick, cls) {
  const b = make('button', text, cls);
  b.type = 'button';
  b.addEventListener('click', onClick);
  return b;
}

// say — текст ключа key на текущем языке; ключ и подстановки запоминаются в data-i18n, и translatePage переводит
// узел при смене языка. key '' — пустой текст без перевода.
function say(node, key, ...args) {
  if (!key) {
    delete node.dataset.i18n;
    delete node.dataset.i18nArgs;
    node.textContent = '';
    return node;
  }
  node.dataset.i18n = key;
  if (args.length > 0) node.dataset.i18nArgs = JSON.stringify(args);
  else delete node.dataset.i18nArgs;
  node.textContent = t(key, ...args);
  return node;
}

// raw — текст не из словаря (ошибка хоста, имя): узел больше не переводится.
function raw(node, text) {
  delete node.dataset.i18n;
  delete node.dataset.i18nArgs;
  node.textContent = text || '';
}

// translatePage — все узлы с data-i18n на текущем языке; lang у <html> — для озвучки и переносов.
function translatePage() {
  for (const n of document.querySelectorAll('[data-i18n]')) {
    n.textContent = t(n.dataset.i18n, ...(n.dataset.i18nArgs ? JSON.parse(n.dataset.i18nArgs) : []));
  }
  document.documentElement.lang = lang();
}

function storedLang() {
  try {
    return window.localStorage.getItem(LANG_KEY) || '';
  } catch {
    return '';
  }
}

function storeLang(code) {
  try {
    window.localStorage.setItem(LANG_KEY, code);
  } catch {
    // хранилище недоступно: язык действует до закрытия вкладки
  }
}

// initLang — язык при открытии: из адреса (#lang= от трея), выбранный ранее, язык браузера, иначе русский.
function initLang() {
  const hash = new URLSearchParams(window.location.hash.replace(/^#/, ''));
  const fromTray = pickLang([hash.get('lang')]);
  if (fromTray) storeLang(fromTray);
  setLang(fromTray || pickLang([storedLang()]) || pickLang(navigator.languages || [navigator.language]) || 'ru');
}

// chooseLang — выбор в списке языков: панель перерисовывается на новом языке.
function chooseLang(code) {
  setLang(code);
  storeLang(lang());
  el('lang').value = lang();
  translatePage();
  if (ui.st) {
    ui.rows = '';
    ui.banners = '';
    applyState(ui.st);
  }
  if (ui.scan && !el('scan').hidden) renderScan();
  if (ui.editor) fillProjectors();
}

function show(id, on) {
  el(id).hidden = !on;
}

// lampOf — цвет лампы в шапке, как круг значка в трее (§11.1): пауза — синий, Empty — серый, Ready — зелёный,
// остальные фазы — жёлтый.
function lampOf(st) {
  if (st.paused) return 'blue';
  if (st.state === 'empty') return 'gray';
  if (st.state === 'ready') return 'green';
  return 'yellow';
}

function setMessage(text, isError) {
  ui.pollFailed = false;
  const m = el('message');
  m.textContent = text || '';
  m.classList.toggle('error', Boolean(isError));
}

function stopPoller() {
  if (ui.poller) {
    ui.poller.stop();
    ui.poller = null;
  }
}

// ——— вход ———

// showLogin — экран входа; key — сообщение из словаря, text — готовый текст (ошибка хоста).
function showLogin(key, text) {
  stopPoller();
  closeEditor();
  show('scan', false);
  show('log', false);
  show('main', false);
  show('login', true);
  if (key) say(el('login-error'), key);
  else raw(el('login-error'), text);
  el('admin-key').focus();
}

async function onLogin() {
  const key = el('admin-key').value.trim();
  if (key === '') {
    say(el('login-error'), 'enterKey');
    return;
  }
  el('login-btn').disabled = true;
  try {
    await api.login({ admin_key: key });
    el('admin-key').value = '';
    enterMain();
  } catch (err) {
    if (err.status === 401) say(el('login-error'), 'badKey');
    else raw(el('login-error'), errorText(err));
  } finally {
    el('login-btn').disabled = false;
  }
}

function enterMain() {
  stopPoller();
  show('login', false);
  show('main', true);
  raw(el('login-error'), '');
  setMessage('');
  ui.rows = '';
  ui.banners = '';
  ui.applied = undefined;
  ui.disk = undefined;
  ui.pollFailed = false;
  el('bar').dataset.lamp = '';
  el('status-text').textContent = t('gettingState');
  ui.poller = new Poller({
    fetchState: () => api.state(),
    onState: applyState,
    onError: (err) => {
      setMessage(errorText(err), true);
      ui.pollFailed = true;
    },
    onAuthLost: () => showLogin('authLost'),
    env: {
      setTimer: (fn, ms) => window.setTimeout(fn, ms),
      clearTimer: (id) => window.clearTimeout(id),
      visible: () => document.visibilityState === 'visible',
    },
  });
  ui.poller.start();
  loadModels();
}

// fail — ошибка действия: 401 — экран входа (F3-D1), иначе сообщение.
function fail(err) {
  if (err && err.status === 401) {
    showLogin('authLost');
    return;
  }
  setMessage(errorText(err), true);
}

async function onLogout() {
  stopPoller();
  try {
    await api.logout();
  } catch {
    // сессия уже закрыта или хост недоступен: токен всё равно стёрт
  }
  showLogin('');
}

// takeOtp — код из #otp= или ?otp= (§9.4); адрес сразу очищается.
function takeOtp() {
  const hash = new URLSearchParams(window.location.hash.replace(/^#/, ''));
  const query = new URLSearchParams(window.location.search);
  const otp = hash.get('otp') || query.get('otp') || '';
  if (hash.has('otp') || query.has('otp')) window.history.replaceState(null, '', window.location.pathname);
  return otp;
}

// ——— состояние, шапка, баннеры, таблица ———

function applyState(st) {
  ui.st = st;
  if (ui.pollFailed) {
    ui.pollFailed = false;
    setMessage('');
  }
  el('bar').dataset.lamp = lampOf(st);
  el('status-text').textContent = statusLine(st);
  const since = localTime(st.state_since);
  el('since-text').textContent = since ? t('since', since) : '';
  const vram = st.vram && typeof st.vram.usage_mib === 'number' ? gb(st.vram.usage_mib) : '';
  el('vram-text').textContent = vram ? t('vram', vram) : '';
  say(el('pause-btn'), st.paused ? 'resume' : 'pause');
  show('pause-force-label', !st.paused);
  el('unload-btn').disabled = !st.model && !st.target;
  el('force-btn').disabled = !canForceUnload(st);
  el('scan-btn').disabled = st.config.mode !== 'file';
  renderBanners();
  const key = rowsKey(st);
  if (key !== ui.rows) {
    ui.rows = key;
    renderRows(st);
  }
  const applied = st.config.applied_etag;
  const disk = st.config.disk_etag;
  if (ui.applied !== undefined && (applied !== ui.applied || disk !== ui.disk)) loadModels();
  ui.applied = applied;
  ui.disk = disk;
}

function renderBanners() {
  const st = ui.st;
  if (!st) return;
  const diskErrors = ui.view && Array.isArray(ui.view.disk_errors) ? ui.view.disk_errors : [];
  const key = JSON.stringify([st.config.mode, st.config.out_of_sync, st.pause_saved, diskErrors]);
  if (key === ui.banners) return;
  ui.banners = key;
  const box = el('banners');
  box.replaceChildren();
  if (st.config.mode === 'legacy') {
    const b = make('div', null, 'banner');
    b.append(make('span', t('bannerLegacy')), button(t('migrate'), onMigrate));
    box.append(b);
  }
  if (st.config.out_of_sync) {
    const b = make('div', null, 'banner');
    b.append(make('span', t('bannerOutOfSync')), button(t('applyFile'), onReload));
    if (diskErrors.length > 0) {
      const list = make('ul', null, 'error');
      for (const e of diskErrors) list.append(make('li', e.field ? `${e.field}: ${e.message}` : e.message));
      b.append(list);
    }
    box.append(b);
  }
  if (st.pause_saved === false) {
    box.append(make('div', t('pauseNotSaved'), 'banner'));
  }
}

function renderRows(st) {
  const body = el('models-body');
  body.replaceChildren();
  const editable = st.config.mode === 'file';
  for (const m of st.models) {
    const tr = make('tr');
    tr.append(make('td', m.id, 'mono'), make('td', statusText(m.status), `st st-${m.status}`), make('td', contextLabel(m)));
    const labels = make('td');
    if (m.default) labels.append(make('span', t('tagDefault'), 'tag'));
    if (m.next) labels.append(make('span', t('tagRestart'), 'tag warn'));
    if (m.failed) labels.append(make('span', t('tagFailed', m.failed.message), 'tag bad'));
    tr.append(labels);
    const acts = make('td', null, 'acts');
    if (m.status === 'loaded' || m.status === 'loading') {
      acts.append(button(t('unload'), () => command('unload', { model: m.id, force: false }, t('unloadStarted', m.id))));
    } else {
      const load = button(t('load'), () => command('load', { model: m.id }, t('loadStarted', m.id)));
      load.disabled = st.paused;
      acts.append(load);
    }
    if (m.status === 'loaded') {
      acts.append(button(t('restart'), () => command('restart', { model: m.id }, t('restartStarted', m.id))));
    }
    acts.append(button(t('log'), () => openLog(m.id)));
    if (editable && m.profile === '') {
      acts.append(button(t('edit'), () => editModel(m.base)), button(t('delete'), () => onDelete(m.base), 'danger'));
    }
    for (const b of acts.children) b.setAttribute('aria-label', `${b.textContent}: ${m.id}`);
    tr.append(acts);
    body.append(tr);
  }
  // F3-D2 основы: недоступный сетевой models_file выглядит отсутствующим и даёт пустой каталог — показываем путь
  el('models-file').textContent = st.config.models_file || '';
  show('models-empty', st.models.length === 0);
  body.closest('.scroll').hidden = st.models.length === 0;
}

// ——— команды ———

async function command(name, body, done) {
  try {
    const r = await api.command(name, body);
    setMessage(done || '');
    return r;
  } catch (err) {
    fail(err);
    return null;
  }
}

function onPause() {
  if (!ui.st) return;
  if (ui.st.paused) {
    command('resume', {}, t('resumed'));
    return;
  }
  const force = el('pause-force').checked;
  command('pause', { force }, force ? t('pausedForce') : t('pausedSoft'));
}

function onForce() {
  const inflight = ui.st ? ui.st.inflight : 0;
  if (!window.confirm(forceConfirmText(inflight))) return;
  command('unload', { force: true }, t('forced'));
}

// ——— модели: вид, перенос, «Применить файл» ———

async function loadModels() {
  try {
    const { data, etag } = await api.models();
    ui.view = data;
    ui.viewEtag = etag;
    renderBanners();
  } catch (err) {
    fail(err);
  }
}

function detailsText(err) {
  const d = err.detail && Array.isArray(err.detail.details) ? err.detail.details : [];
  if (d.length === 0) return errorText(err);
  return d.map((e) => (e.field ? `${e.field}: ${e.message}` : e.message)).join('; ');
}

async function onMigrate() {
  if (!window.confirm(t('confirmMigrate'))) return;
  try {
    const r = await api.migrate();
    setMessage(t('migrated', r.models_file, r.backup));
    await loadModels();
  } catch (err) {
    fail(err);
  }
}

async function onReload() {
  try {
    const { data } = await api.reload();
    setMessage(savedText(data, t('fileApplied')));
    await loadModels();
  } catch (err) {
    if (err.status === 422) {
      setMessage(t('yamlErrors', detailsText(err)), true);
      return;
    }
    fail(err);
  }
}

// ——— редактор ———

function takenIds(exceptIndex) {
  const out = [];
  const models = ui.view ? ui.view.models : [];
  models.forEach((m, i) => {
    if (i === exceptIndex) return;
    out.push(m.id);
    for (const p of m.profiles || []) out.push(`${m.id}-${p.name}`);
  });
  return out;
}

function editModel(id) {
  if (!ui.view) return;
  const index = ui.view.models.findIndex((m) => m.id === id);
  if (index < 0) {
    setMessage(t('modelNotFound'), true);
    loadModels();
    return;
  }
  startEditor(index, null);
}

// startEditor — редактор модели index вида; -1 — новая модель, preset — {id, path, mmproj} из поиска.
function startEditor(index, preset) {
  if (!ui.view || ui.view.mode !== 'file') {
    setMessage(t('migrateFirst'), true);
    return;
  }
  const doc = docFromView(ui.view);
  const vm = index >= 0 ? ui.view.models[index] : null;
  const m = index >= 0 ? doc.models[index]
    : { id: preset ? preset.id : '', path: preset ? preset.path : '', args: [], profiles: [] };
  ui.editor = {
    index, doc, etag: ui.viewEtag, vm, mmproj: preset ? preset.mmproj : [], brokenConfirm: brokenFileConfirmText(ui.view),
  };
  el('ed-profiles').replaceChildren();
  clearFieldErrors();
  if (index >= 0) say(el('ed-title'), 'modelTitle', m.id);
  else say(el('ed-title'), 'newModel');
  el('ed-id').value = m.id;
  const pathLocked = Boolean(vm && vm.path_hand_only);
  el('ed-path').value = m.path;
  el('ed-path').readOnly = pathLocked;
  el('ed-pick').disabled = pathLocked;
  say(el('ed-path-note'), pathLocked ? 'handOnly' : '');
  const argsLocked = Boolean(vm && vm.args_hand_only);
  el('ed-args').value = formatArgs(m.args);
  el('ed-args').readOnly = argsLocked;
  say(el('ed-args-note'), argsLocked ? 'handOnly' : '');
  const reasons = vm && vm.hand_only_reasons ? vm.hand_only_reasons : [];
  if (reasons.length > 0) say(el('ed-reasons'), 'handOnlyReasons', reasons.join(', '));
  else say(el('ed-reasons'), '');
  m.profiles.forEach((p, j) => addProfileRow(p, Boolean(vm && vm.profiles[j] && vm.profiles[j].hand_only)));
  const defaults = formatArgs(ui.view.default_args);
  if (defaults) raw(el('ed-defaults'), defaults);
  else say(el('ed-defaults'), 'none');
  const warns = el('ed-warnings');
  warns.replaceChildren();
  for (const w of vm && vm.warnings ? vm.warnings : []) warns.append(make('li', w.field ? `${w.field}: ${w.message}` : w.message));
  fillProjectors();
  show('scan', false);
  show('editor', true);
  el('ed-id').focus();
}

function closeEditor() {
  ui.editor = null;
  ui.pickForEditor = false;
  show('editor', false);
}

function addProfileRow(p, locked) {
  const row = make('div', null, 'profile');
  const name = make('input', null, 'p-name');
  name.type = 'text';
  name.spellcheck = false;
  name.value = p.name;
  const args = make('textarea', null, 'p-args');
  args.rows = 3;
  args.spellcheck = false;
  args.value = formatArgs(p.args);
  args.readOnly = locked;
  if (locked) lockedArgs.set(row, [...p.args]);
  profileSeq += 1;
  name.id = `p-name-${profileSeq}`;
  args.id = `p-args-${profileSeq}`;
  const nameLabel = say(make('label'), 'profileName');
  nameLabel.htmlFor = name.id;
  const argsLabel = say(make('label'), 'profileArgs');
  argsLabel.htmlFor = args.id;
  row.append(nameLabel, name, argsLabel, args, say(make('p', null, 'note'), locked ? 'handOnly' : ''),
    say(button('', () => row.remove()), 'deleteProfile'), make('p', '', 'error p-err'));
  el('ed-profiles').append(row);
}

function scanFile(path) {
  if (!ui.scan) return null;
  const p = path.trim().toLowerCase();
  return ui.scan.files.find((f) => f.path.toLowerCase() === p) || null;
}

// fillProjectors — выбор проектора из mmproj найденного файла (§10.2 п. 5) и текущего --mmproj.
function fillProjectors() {
  const ed = ui.editor;
  if (!ed) return;
  const file = scanFile(el('ed-path').value);
  const found = file ? file.mmproj || [] : ed.mmproj || [];
  const current = projectorOf(parseArgs(el('ed-args').value));
  const options = [...new Set([...found, ...(current ? [current] : [])])];
  const sel = el('ed-proj');
  sel.replaceChildren();
  const none = make('option', t('noProjector'));
  none.value = '';
  sel.append(none);
  for (const p of options) {
    const o = make('option', p);
    o.value = p;
    sel.append(o);
  }
  sel.value = current;
  sel.disabled = el('ed-args').readOnly;
  show('ed-proj-row', options.length > 0);
}

function onProjector() {
  el('ed-args').value = formatArgs(setProjector(parseArgs(el('ed-args').value), el('ed-proj').value));
}

function collectModel() {
  const ed = ui.editor;
  const vm = ed.vm;
  const orig = ed.index >= 0 ? ed.doc.models[ed.index] : null;
  const m = {
    id: el('ed-id').value.trim(),
    path: vm && vm.path_hand_only ? orig.path : el('ed-path').value.trim(),
    args: vm && vm.args_hand_only ? [...orig.args] : parseArgs(el('ed-args').value),
    profiles: [],
  };
  for (const row of el('ed-profiles').children) {
    const locked = lockedArgs.get(row);
    m.profiles.push({ name: row.querySelector('.p-name').value.trim(),
      args: locked ? [...locked] : parseArgs(row.querySelector('.p-args').value) });
  }
  return m;
}

function clearFieldErrors() {
  for (const id of ['ed-id-err', 'ed-path-err', 'ed-args-err']) el(id).textContent = '';
  el('ed-errors').replaceChildren();
  for (const p of el('ed-profiles').querySelectorAll('.p-err')) p.textContent = '';
}

// showFieldErrors — ошибки 422 у полей по details.field (§10.2 п. 4); прочие — списком под редактором.
function showFieldErrors(err, index) {
  const details = err.detail && Array.isArray(err.detail.details) ? err.detail.details : [];
  const general = el('ed-errors');
  const rows = [...el('ed-profiles').children];
  let first = null; // поле первой ошибки: туда переходит фокус, чтобы ошибку было видно
  for (const d of details) {
    const f = parseField(d.field);
    let target = null;
    let input = null;
    if (f && f.model === index) {
      if (f.part === 'id') [target, input] = [el('ed-id-err'), el('ed-id')];
      else if (f.part === 'path') [target, input] = [el('ed-path-err'), el('ed-path')];
      else if (f.part === 'args') [target, input] = [el('ed-args-err'), el('ed-args')];
      else if ((f.part === 'profile-name' || f.part === 'profile-args') && rows[f.profile]) {
        target = rows[f.profile].querySelector('.p-err');
        input = rows[f.profile].querySelector(f.part === 'profile-name' ? '.p-name' : '.p-args');
      }
    }
    if (target) target.textContent = target.textContent ? `${target.textContent}; ${d.message}` : d.message;
    else general.append(make('li', d.field ? `${d.field}: ${d.message}` : d.message));
    if (!first) first = input;
  }
  if (details.length === 0) general.append(make('li', errorText(err)));
  setMessage(t('notSaved'), true);
  if (first) first.focus();
}

async function onSave() {
  const ed = ui.editor;
  if (!ed) return;
  if (ed.brokenConfirm && !window.confirm(ed.brokenConfirm)) return;
  clearFieldErrors();
  const doc = { models: [...ed.doc.models] };
  const m = collectModel();
  const index = ed.index >= 0 ? ed.index : doc.models.length;
  doc.models[index] = m;
  el('ed-save').disabled = true;
  try {
    const { data } = await api.putModels(doc, ed.etag);
    closeEditor();
    setMessage(savedText(data, t('saved', m.id)));
    await loadModels();
  } catch (err) {
    if (err.status === 412) {
      if (window.confirm(t('fileChanged'))) {
        const id = ed.index >= 0 ? ed.doc.models[ed.index].id : '';
        await loadModels();
        const again = ui.view && id ? ui.view.models.findIndex((x) => x.id === id) : -1;
        startEditor(again, again < 0 ? { id: m.id, path: m.path, mmproj: ed.mmproj } : null);
      }
      return;
    }
    if (err.status === 422) {
      showFieldErrors(err, index);
      return;
    }
    fail(err);
  } finally {
    el('ed-save').disabled = false;
  }
}

async function onDelete(id) {
  if (!ui.view) return;
  const index = ui.view.models.findIndex((m) => m.id === id);
  if (index < 0) return;
  if (!window.confirm(t('confirmDelete', id))) return;
  const broken = brokenFileConfirmText(ui.view);
  if (broken && !window.confirm(broken)) return;
  const doc = docFromView(ui.view);
  doc.models.splice(index, 1);
  try {
    const { data } = await api.putModels(doc, ui.viewEtag);
    setMessage(savedText(data, t('deleted', id)));
    await loadModels();
  } catch (err) {
    if (err.status === 412) {
      setMessage(t('deleteRetry'), true);
      await loadModels();
      return;
    }
    if (err.status === 422) {
      setMessage(t('notDeleted', detailsText(err)), true);
      return;
    }
    fail(err);
  }
}

// ——— поиск .gguf ———

async function openScan(forEditor) {
  ui.pickForEditor = forEditor;
  show('scan', true);
  el('scan').scrollIntoView({ block: 'nearest' });
  el('scan-status').textContent = t('scanning');
  el('scan-list').replaceChildren();
  el('scan-errors').replaceChildren();
  try {
    ui.scan = await api.scan();
    renderScan();
    el('scan').scrollIntoView({ block: 'start' });
  } catch (err) {
    el('scan-status').textContent = '';
    fail(err);
  }
}

function renderScan() {
  const s = ui.scan;
  let status = t('scanFound', s.files.length, s.elapsed_ms);
  if (s.truncated) status += t('scanTruncated');
  if (s.roots.length === 0) status = t('scanNoRoots');
  el('scan-status').textContent = status;
  const list = el('scan-list');
  list.replaceChildren();
  for (const g of groupByRoot(s.roots, s.files)) {
    list.append(make('h3', g.root || t('outsideRoots'), 'mono'));
    if (g.files.length === 0) {
      list.append(make('p', t('noGguf'), 'muted'));
      continue;
    }
    const table = make('table');
    const head = make('tr');
    for (const h of ['colFile', 'colKind', 'colArch', 'colContext', 'colSize', 'colUsedBy']) head.append(make('th', t(h)));
    head.append(make('th'));
    table.append(head);
    for (const f of g.files) {
      const tr = make('tr');
      tr.append(make('td', relPath(g.root, f.path), 'mono'), make('td', kindText(f.kind)), make('td', f.architecture),
        make('td', f.context_train > 0 ? f.context_train : ''), make('td', gb(f.size_bytes / 1048576)),
        make('td', (f.used_by || []).join(', ')));
      const act = make('td');
      if (f.kind === 'model') act.append(button(t('pick'), () => pickFile(f)));
      tr.append(act);
      table.append(tr);
    }
    list.append(table);
  }
  const errs = el('scan-errors');
  errs.replaceChildren();
  for (const e of s.errors) errs.append(make('li', `${e.path}: ${e.message}`));
}

function pickFile(f) {
  show('scan', false);
  if (ui.pickForEditor && ui.editor) {
    ui.pickForEditor = false;
    el('ed-path').value = f.path;
    ui.editor.mmproj = f.mmproj || [];
    fillProjectors();
    return;
  }
  ui.pickForEditor = false;
  startEditor(-1, { id: suggestName(f.path, takenIds(-1)), path: f.path, mmproj: f.mmproj || [] });
}

// ——— журнал модели ———

async function openLog(id) {
  ui.logModel = id;
  say(el('log-title'), 'logTitle', id);
  el('log-text').textContent = '';
  show('log', true);
  el('log-text').focus();
  await refreshLog();
}

async function refreshLog() {
  if (!ui.logModel) return;
  try {
    const text = await api.logs(ui.logModel);
    const pre = el('log-text');
    pre.textContent = text || t('noOutput');
    pre.scrollTop = pre.scrollHeight;
  } catch (err) {
    fail(err);
  }
}

// ——— запуск ———

function wire() {
  const langSel = el('lang');
  for (const code of LANGS) {
    const o = make('option', LANG_NAMES[code]);
    o.value = code;
    o.lang = code;
    langSel.append(o);
  }
  langSel.value = lang();
  langSel.addEventListener('change', () => chooseLang(langSel.value));
  el('login-btn').addEventListener('click', onLogin);
  el('admin-key').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') onLogin();
  });
  el('pause-btn').addEventListener('click', onPause);
  el('unload-btn').addEventListener('click', () => command('unload', { force: false }, t('unloadStartedAll')));
  el('force-btn').addEventListener('click', onForce);
  el('logout-btn').addEventListener('click', onLogout);
  el('scan-btn').addEventListener('click', () => openScan(false));
  el('scan-close').addEventListener('click', () => {
    ui.pickForEditor = false;
    show('scan', false);
  });
  el('ed-pick').addEventListener('click', () => openScan(true));
  el('ed-path').addEventListener('change', fillProjectors);
  el('ed-args').addEventListener('change', fillProjectors);
  el('ed-proj').addEventListener('change', onProjector);
  el('ed-add-profile').addEventListener('click', () => addProfileRow({ name: '', args: [] }, false));
  el('ed-save').addEventListener('click', onSave);
  el('ed-cancel').addEventListener('click', closeEditor);
  el('log-refresh').addEventListener('click', refreshLog);
  el('log-close').addEventListener('click', () => {
    ui.logModel = '';
    show('log', false);
  });
  document.addEventListener('visibilitychange', () => {
    if (ui.poller) ui.poller.visibilityChanged();
  });
}

async function boot() {
  initLang();
  translatePage();
  wire();
  const otp = takeOtp();
  if (otp) {
    try {
      await api.login({ otp });
      enterMain();
    } catch (err) {
      if (err.status === 401) showLogin('otpFailed');
      else showLogin('', errorText(err));
    }
    return;
  }
  if (api.token()) {
    enterMain();
    return;
  }
  showLogin('');
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
else boot();
