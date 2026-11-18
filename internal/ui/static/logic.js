// logic.js — чистые функции панели: тексты, числа, разбор полей. Без DOM и сети: их проверяет node --test. Тексты —
// на текущем языке lang.js.
import { lang, locale, t } from './lang.js';

const STATUSES = ['loaded', 'loading', 'unloaded', 'failed'];
const KINDS = ['model', 'mmproj', 'adapter', 'unknown'];
const ERROR_CODES = ['invalid_admin_credentials', 'host_not_allowed', 'model_in_use', 'model_is_default', 'models_not_migrated',
  'already_migrated', 'config_changed_on_disk', 'config_invalid', 'config_write_failed', 'host_paused', 'model_not_found'];

// freshestFailed — имя модели с действующей меткой failed и самой поздней ошибкой (у неё больше всего retry_in_ms:
// срок метки у всех одинаковый); при равенстве — первое по алфавиту; нет — ''.
export function freshestFailed(failed) {
  let best = '';
  let bestMs = -1;
  for (const f of failed || []) {
    if (!(f.retry_in_ms > 0)) continue;
    if (f.retry_in_ms > bestMs || (f.retry_in_ms === bestMs && f.model < best)) {
      best = f.model;
      bestMs = f.retry_in_ms;
    }
  }
  return best;
}

// statusLine — строка состояния, как в трее (§11.1), по ответу GET /api/admin/state.
export function statusLine(st) {
  let text;
  switch (st.state) {
    case 'loading':
      text = t('sLoading', st.model);
      break;
    case 'ready':
      text = t('sReady', st.model);
      if (st.inflight > 0) text += t('sAnswering', st.inflight);
      break;
    case 'draining': {
      let target = st.target;
      if (!target) target = t('sDrainUnload');
      else if (target === st.model) target = t('sDrainRestart');
      text = t('sDraining', st.model, target);
      break;
    }
    case 'stopping':
      text = t('sStopping', st.model);
      break;
    case 'wait_vram':
      text = t('sWaitVram');
      break;
    default: {
      text = t('sEmpty');
      const m = freshestFailed(st.failed);
      if (m) text = t('sFailed', m);
    }
  }
  return st.paused ? t('sPaused', text) : text;
}

// gb — МиБ в ГБ с одним знаком после запятой (§10.1): 12288 → «12,0 ГБ» (у английского и китайского — «12.0 GB»);
// не число — ''.
export function gb(mib) {
  if (typeof mib !== 'number' || !Number.isFinite(mib)) return '';
  const n = (mib / 1024).toFixed(1);
  return t('gb', lang() === 'ru' ? n.replace('.', ',') : n);
}

// parseArgs — поле флагов (§10.3): строка — один аргумент; пробелы по краям обрезаются, пустые строки пропускаются,
// пробелы внутри строки сохраняются.
export function parseArgs(text) {
  return String(text).split(/\r?\n/).map((s) => s.trim()).filter((s) => s !== '');
}

// formatArgs — список аргументов для поля флагов: один на строку.
export function formatArgs(args) {
  return (args || []).join('\n');
}

// suggestName — имя модели по пути файла (§10.2 п. 5) по правилу имени ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$: без
// каталога, .gguf и суффикса шарда -00001-of-00003; нижний регистр; прочие символы — «-»; не длиннее 64. Занятое имя
// (без учёта регистра) получает суффикс -2, -3, …
export function suggestName(path, taken) {
  let base = String(path).split(/[\\/]/).pop() || '';
  base = base.replace(/\.gguf$/i, '').replace(/-\d{5}-of-\d{5}$/i, '');
  let name = base.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/-{2,}/g, '-');
  name = name.replace(/^[^a-z0-9]+/, '').slice(0, 64).replace(/[-._]+$/, '');
  if (name === '') name = 'model';
  const used = new Set((taken || []).map((s) => String(s).toLowerCase()));
  if (!used.has(name)) return name;
  for (let n = 2; ; n++) {
    const suffix = `-${n}`;
    const candidate = name.slice(0, 64 - suffix.length).replace(/[-._]+$/, '') + suffix;
    if (!used.has(candidate)) return candidate;
  }
}

// contextLabel — контекст строки модели (§10.2 п. 3): «8192» или «8192 → 65536 после перезапуска»; 0 — «—».
export function contextLabel(m) {
  const now = m.context_length > 0 ? String(m.context_length) : '—';
  if (m.next && m.next.context_length !== m.context_length) {
    const next = m.next.context_length > 0 ? String(m.next.context_length) : '—';
    return t('ctxAfterRestart', now, next);
  }
  return now;
}

const FIELD_RE = /^models\[(\d+)\](.*)$/;
const ARGS_RE = /^\.args(?:\[(\d+)\])?$/;
const PROFILE_RE = /^\.profiles\[(\d+)\](?:\.(name|args)(?:\[(\d+)\])?)?$/;

// parseField — поле ошибки 422 после PUT (§9.5): {model, part, profile, arg}; part — 'model', 'id', 'path', 'args',
// 'profiles', 'profile-name' или 'profile-args'; нет индекса — -1. Другие поля (например, ошибки файла) — null.
export function parseField(field) {
  const m = FIELD_RE.exec(String(field));
  if (!m) return null;
  const model = Number(m[1]);
  const rest = m[2];
  if (rest === '') return { model, part: 'model', profile: -1, arg: -1 };
  if (rest === '.id' || rest === '.path' || rest === '.profiles') return { model, part: rest.slice(1), profile: -1, arg: -1 };
  let a = ARGS_RE.exec(rest);
  if (a) return { model, part: 'args', profile: -1, arg: a[1] === undefined ? -1 : Number(a[1]) };
  a = PROFILE_RE.exec(rest);
  if (a) {
    return { model, part: a[2] === 'args' ? 'profile-args' : 'profile-name', profile: Number(a[1]),
      arg: a[3] === undefined ? -1 : Number(a[3]) };
  }
  return null;
}

// docFromView — документ PUT из вида GET /api/admin/models: id, path, args и профили моделей в том же порядке;
// списки — копии.
export function docFromView(view) {
  return {
    models: (view.models || []).map((m) => ({
      id: m.id,
      path: m.path,
      args: [...(m.args || [])],
      profiles: (m.profiles || []).map((p) => ({ name: p.name, args: [...(p.args || [])] })),
    })),
  };
}

// projectorOf — значение последнего --mmproj или -mm в аргументах; нет — ''.
export function projectorOf(args) {
  let value = '';
  for (let i = 0; i < args.length; i++) {
    if ((args[i] === '--mmproj' || args[i] === '-mm') && i + 1 < args.length) value = args[i + 1];
  }
  return value;
}

// setProjector — новые аргументы: без --mmproj и -mm (с их значениями), а если path не пуст — с «--mmproj path» в конце.
export function setProjector(args, path) {
  const out = [];
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--mmproj' || args[i] === '-mm') {
      i++;
      continue;
    }
    out.push(args[i]);
  }
  if (path) out.push('--mmproj', path);
  return out;
}

// statusText — статус модели на текущем языке.
export function statusText(status) {
  return STATUSES.includes(status) ? t(`st_${status}`) : String(status);
}

// kindText — вид файла GGUF на текущем языке.
export function kindText(kind) {
  return KINDS.includes(kind) ? t(`kind_${kind}`) : String(kind);
}

// errorText — ошибка API для человека (§10.1): 429 — про попытки; нет связи; известный код — текст на текущем языке и
// текст хоста; иначе текст хоста и код.
export function errorText(err) {
  if (!err) return '';
  if (err.status === 429 || err.code === 'too_many_attempts') return t('tooMany');
  if (err.status === 0) return t('noConnection');
  const known = ERROR_CODES.includes(err.code) ? t(`err_${err.code}`) : '';
  if (known) return err.message ? `${known} (${err.message})` : known;
  if (err.message) return `${err.message} (${err.code || err.status})`;
  return t('errorN', err.status);
}

// rowsKey — отпечаток таблицы моделей без обратного отсчёта retry_in_ms: таблица перерисовывается, только когда он
// меняется (иначе кнопки пересоздаются каждую секунду и щелчок может пропасть).
export function rowsKey(st) {
  const models = (st.models || []).map((m) => [m.id, m.base, m.profile, m.status, m.context_length, m.default,
    m.failed ? [m.failed.exit_code, m.failed.message] : null, m.next ? m.next.context_length : null]);
  return JSON.stringify([Boolean(st.paused), st.config ? st.config.mode : '', models]);
}

// canForceUnload — «Прервать и выгрузить» активна только в Ready и Draining (§10.2 п. 2).
export function canForceUnload(st) {
  return st.state === 'ready' || st.state === 'draining';
}

// forceConfirmText — вопрос перед «Прервать и выгрузить» с числом незаконченных ответов.
export function forceConfirmText(inflight) {
  return inflight > 0 ? t('confirmForce', inflight) : t('confirmUnloadIdle');
}

// brokenFileConfirmText — вопрос перед записью, если models.yaml на диске с ошибками (вид несёт disk_errors);
// исправный файл — ''.
export function brokenFileConfirmText(view) {
  return view && Array.isArray(view.disk_errors) && view.disk_errors.length > 0
    ? t('brokenFile')
    : '';
}

// savedText — итог PUT или «Применить файл»: перезапуск каких моделей нужен и сколько предупреждений.
export function savedText(res, verb) {
  let text = verb;
  if (res.restart_required && res.restart_required.length > 0) text += t('needRestart', res.restart_required.join(', '));
  if (res.warnings && res.warnings.length > 0) text += t('warningsN', res.warnings.length);
  return text;
}

// localTime — время RFC 3339 по местным часам (§10.1); неверная строка — ''.
export function localTime(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleString(locale());
}

function underRoot(root, path) {
  const r = root.replace(/\\+$/, '').toLowerCase();
  return r !== '' && path.toLowerCase().startsWith(`${r}\\`);
}

// groupByRoot — файлы поиска по каталогам (§10.2 п. 5): [{root, files}] в порядке roots; файл — в первом каталоге, внутри
// которого лежит (без учёта регистра, по границе «\»); остальные — в группе root ''.
export function groupByRoot(roots, files) {
  const groups = roots.map((root) => ({ root, files: [] }));
  const rest = { root: '', files: [] };
  for (const f of files) {
    const g = groups.find((x) => underRoot(x.root, f.path));
    (g || rest).files.push(f);
  }
  if (rest.files.length > 0) groups.push(rest);
  return groups;
}

// relPath — путь файла относительно каталога root; вне каталога — путь целиком.
export function relPath(root, path) {
  if (!underRoot(root, path)) return path;
  return path.slice(root.replace(/\\+$/, '').length + 1);
}
