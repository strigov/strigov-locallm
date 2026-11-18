import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  brokenFileConfirmText, canForceUnload, contextLabel, docFromView, errorText, forceConfirmText, formatArgs, freshestFailed, gb,
  groupByRoot, kindText, localTime, parseArgs, parseField, projectorOf, relPath, rowsKey, savedText, setProjector,
  statusLine, statusText, suggestName,
} from '../static/logic.js';

const MODEL_ID_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

test('statusLine повторяет строку трея (§11.1)', () => {
  const st = (extra) => ({ state: 'empty', model: null, target: null, inflight: 0, paused: false, failed: [], ...extra });
  assert.equal(statusLine(st({})), 'Модель не загружена');
  assert.equal(statusLine(st({ failed: [{ model: 'b', retry_in_ms: 5000 }, { model: 'a', retry_in_ms: 90000 }] })),
    'Ошибка загрузки: a');
  assert.equal(statusLine(st({ state: 'loading', model: 'a' })), 'Загружается: a…');
  assert.equal(statusLine(st({ state: 'ready', model: 'a' })), 'Готова: a');
  assert.equal(statusLine(st({ state: 'ready', model: 'a', inflight: 2 })), 'Готова: a · отвечает (2)');
  assert.equal(statusLine(st({ state: 'draining', model: 'a', target: 'b' })), 'Заканчивает ответы: a → b');
  assert.equal(statusLine(st({ state: 'draining', model: 'a' })), 'Заканчивает ответы: a → выгрузка');
  assert.equal(statusLine(st({ state: 'draining', model: 'a', target: 'a' })), 'Заканчивает ответы: a → перезапуск');
  assert.equal(statusLine(st({ state: 'stopping', model: 'a' })), 'Выгружается: a…');
  assert.equal(statusLine(st({ state: 'wait_vram' })), 'Освобождается видеопамять…');
  assert.equal(statusLine(st({ state: 'ready', model: 'a', paused: true })), 'Пауза · Готова: a');
});

test('freshestFailed: больше всего retry_in_ms, при равенстве — первая по алфавиту', () => {
  assert.equal(freshestFailed([]), '');
  assert.equal(freshestFailed([{ model: 'c', retry_in_ms: 0 }]), '');
  assert.equal(freshestFailed([{ model: 'b', retry_in_ms: 1000 }, { model: 'a', retry_in_ms: 1000 }]), 'a');
  assert.equal(freshestFailed([{ model: 'a', retry_in_ms: 10 }, { model: 'z', retry_in_ms: 20 }]), 'z');
});

test('gb: ГБ с одним знаком после запятой', () => {
  assert.equal(gb(12288), '12,0 ГБ');
  assert.equal(gb(1536), '1,5 ГБ');
  assert.equal(gb(15974), '15,6 ГБ');
  assert.equal(gb(0), '0,0 ГБ');
  assert.equal(gb(null), '');
  assert.equal(gb(undefined), '');
});

test('parseArgs и formatArgs: строка — один аргумент (§10.3)', () => {
  assert.deepEqual(parseArgs('  -c \n\n 8192\r\n--chat-template-file\n C:\\models\\a b\\t.jinja  \n'),
    ['-c', '8192', '--chat-template-file', 'C:\\models\\a b\\t.jinja']);
  assert.deepEqual(parseArgs(''), []);
  assert.equal(formatArgs(['-c', '8192']), '-c\n8192');
  assert.equal(formatArgs(undefined), '');
});

test('suggestName: имя по файлу по правилу имени модели', () => {
  const cases = [
    ['C:\\models\\unsloth--Qwen3.8-27B-GGUF\\Qwen3.8-27B-UD-Q3_K_XL.gguf', [], 'qwen3.8-27b-ud-q3_k_xl'],
    ['D:\\m\\Big-Model-00001-of-00003.gguf', [], 'big-model'],
    ['C:\\m\\  Мой файл!!.gguf', [], 'model'],
    ['C:\\m\\-._abc.gguf', [], 'abc'],
    ['C:/m/my model (v2).GGUF', [], 'my-model-v2'],
    ['C:\\m\\Tiny.gguf', ['tiny', 'TINY-2'], 'tiny-3'],
    ['C:\\m\\' + 'a'.repeat(70) + '.gguf', [], 'a'.repeat(64)],
    ['C:\\m\\' + 'a'.repeat(70) + '.gguf', ['a'.repeat(64)], 'a'.repeat(62) + '-2'],
  ];
  for (const [path, taken, want] of cases) {
    const got = suggestName(path, taken);
    assert.equal(got, want, path);
    assert.match(got, MODEL_ID_RE, path);
  }
});

test('contextLabel: действующий контекст и значение после перезапуска', () => {
  assert.equal(contextLabel({ context_length: 8192, next: null }), '8192');
  assert.equal(contextLabel({ context_length: 8192, next: { context_length: 65536 } }), '8192 → 65536 после перезапуска');
  assert.equal(contextLabel({ context_length: 8192, next: { context_length: 8192 } }), '8192');
  assert.equal(contextLabel({ context_length: 0, next: null }), '—');
});

test('parseField: поля 422 после PUT', () => {
  const f = (model, part, profile = -1, arg = -1) => ({ model, part, profile, arg });
  assert.deepEqual(parseField('models[2]'), f(2, 'model'));
  assert.deepEqual(parseField('models[0].id'), f(0, 'id'));
  assert.deepEqual(parseField('models[1].path'), f(1, 'path'));
  assert.deepEqual(parseField('models[0].args'), f(0, 'args'));
  assert.deepEqual(parseField('models[0].args[3]'), f(0, 'args', -1, 3));
  assert.deepEqual(parseField('models[0].profiles'), f(0, 'profiles'));
  assert.deepEqual(parseField('models[0].profiles[1].name'), f(0, 'profile-name', 1));
  assert.deepEqual(parseField('models[0].profiles[1].args[0]'), f(0, 'profile-args', 1, 0));
  for (const bad of ['models.gemma.args[3]', '', 'models[x]', 'models[0].unknown']) {
    assert.equal(parseField(bad), null, bad);
  }
});

test('docFromView: документ PUT — копия вида в том же порядке', () => {
  const view = {
    models: [
      { id: 'a', path: 'C:\\m\\a.gguf', path_hand_only: false, args: ['-c', '8192'], args_hand_only: false,
        profiles: [{ name: 'long', args: ['-c', '65536'], hand_only: false }], hand_only_reasons: [], info: null, warnings: [] },
      { id: 'b', path: 'C:\\m\\b.gguf', args: [], profiles: [] },
    ],
  };
  const doc = docFromView(view);
  assert.deepEqual(doc, {
    models: [
      { id: 'a', path: 'C:\\m\\a.gguf', args: ['-c', '8192'], profiles: [{ name: 'long', args: ['-c', '65536'] }] },
      { id: 'b', path: 'C:\\m\\b.gguf', args: [], profiles: [] },
    ],
  });
  doc.models[0].args.push('x');
  doc.models[0].profiles[0].args.push('y');
  assert.deepEqual(view.models[0].args, ['-c', '8192']);
  assert.deepEqual(view.models[0].profiles[0].args, ['-c', '65536']);
});

test('projectorOf и setProjector', () => {
  assert.equal(projectorOf(['-c', '1', '--mmproj', 'C:\\p.gguf']), 'C:\\p.gguf');
  assert.equal(projectorOf(['-c', '1', '-mm', 'C:\\q.gguf']), 'C:\\q.gguf');
  assert.equal(projectorOf(['-c']), '');
  assert.equal(projectorOf(['--mmproj']), '');
  const args = ['-c', '8192', '--mmproj', 'C:\\old.gguf', '-mm', 'C:\\x.gguf'];
  assert.deepEqual(setProjector(args, 'C:\\new.gguf'), ['-c', '8192', '--mmproj', 'C:\\new.gguf']);
  assert.deepEqual(setProjector(args, ''), ['-c', '8192']);
  assert.deepEqual(args, ['-c', '8192', '--mmproj', 'C:\\old.gguf', '-mm', 'C:\\x.gguf']);
});

test('statusText и kindText', () => {
  assert.equal(statusText('loaded'), 'загружена');
  assert.equal(statusText('loading'), 'загружается');
  assert.equal(statusText('unloaded'), 'не загружена');
  assert.equal(statusText('failed'), 'ошибка загрузки');
  assert.equal(kindText('model'), 'модель');
  assert.equal(kindText('mmproj'), 'проектор');
  assert.equal(kindText('adapter'), 'адаптер');
  assert.equal(kindText('unknown'), 'неизвестно');
  assert.equal(kindText('other'), 'other');
});

test('errorText: 429, нет связи, известные и прочие коды', () => {
  assert.equal(errorText({ status: 429, code: 'too_many_attempts', message: 'x' }),
    'Слишком много неудачных попыток, подождите минуту');
  assert.equal(errorText({ status: 0, code: 'network_error', message: '' }), 'Нет связи с хостом');
  assert.equal(errorText({ status: 409, code: 'model_in_use', message: 'model "a" is in use' }),
    'Модель сейчас загружена или в очереди: сначала выгрузи её (model "a" is in use)');
  assert.equal(errorText({ status: 500, code: 'weird', message: 'boom' }), 'boom (weird)');
  assert.equal(errorText({ status: 502, code: '', message: '' }), 'Ошибка 502');
});

test('rowsKey не зависит от обратного отсчёта retry_in_ms', () => {
  const st = (retry, status) => ({ paused: false, config: { mode: 'file' }, models: [{ id: 'a', base: 'a', profile: '',
    status, context_length: 8192, default: true, failed: { exit_code: 1, message: 'boom', retry_in_ms: retry }, next: null }] });
  assert.equal(rowsKey(st(90000, 'failed')), rowsKey(st(89000, 'failed')));
  assert.notEqual(rowsKey(st(90000, 'failed')), rowsKey(st(90000, 'unloaded')));
  assert.notEqual(rowsKey({ ...st(1, 'failed'), paused: true }), rowsKey(st(1, 'failed')));
});

test('canForceUnload: только Ready и Draining', () => {
  assert.equal(canForceUnload({ state: 'ready' }), true);
  assert.equal(canForceUnload({ state: 'draining' }), true);
  for (const s of ['empty', 'loading', 'stopping', 'wait_vram']) assert.equal(canForceUnload({ state: s }), false, s);
});

test('localTime: местное время, неверная строка — пусто', () => {
  assert.equal(localTime('nope'), '');
  assert.match(localTime('2026-11-15T12:00:00Z'), /2026/);
});

test('groupByRoot и relPath: файлы по каталогам без учёта регистра', () => {
  const x = { path: 'C:\\Models\\a\\x.gguf' };
  const y = { path: 'D:\\m\\y.gguf' };
  const z = { path: 'C:\\models2\\z.gguf' };
  assert.deepEqual(groupByRoot(['C:\\models', 'D:\\m\\'], [x, y, z]),
    [{ root: 'C:\\models', files: [x] }, { root: 'D:\\m\\', files: [y] }, { root: '', files: [z] }]);
  assert.deepEqual(groupByRoot(['C:\\models'], []), [{ root: 'C:\\models', files: [] }]);
  assert.equal(relPath('C:\\models', 'C:\\Models\\a\\x.gguf'), 'a\\x.gguf');
  assert.equal(relPath('C:\\models', 'C:\\models2\\z.gguf'), 'C:\\models2\\z.gguf');
  assert.equal(relPath('', 'C:\\models2\\z.gguf'), 'C:\\models2\\z.gguf');
});

test('forceConfirmText и savedText', () => {
  assert.equal(forceConfirmText(3), 'Будут прерваны незаконченные ответы: 3. Продолжить?');
  assert.equal(forceConfirmText(0), 'Незаконченных ответов нет. Выгрузить модель?');
  assert.equal(savedText({ etag: 'x', restart_required: ['a', 'b'], warnings: [{ field: 'f', message: 'm' }] }, 'Сохранено'),
    'Сохранено. Нужен перезапуск: a, b. Предупреждений: 1');
  assert.equal(savedText({ etag: 'x', restart_required: [], warnings: [] }, 'Файл применён'), 'Файл применён');
});

test('brokenFileConfirmText: вопрос только при disk_errors (F2-B1)', () => {
  const text = 'Файл моделей на диске с ошибками. Запись заменит его последним рабочим вариантом, а испорченный файл останется рядом в копии .bak. Продолжить?';
  assert.equal(brokenFileConfirmText(null), '');
  assert.equal(brokenFileConfirmText({ mode: 'file', models: [] }), '');
  assert.equal(brokenFileConfirmText({ mode: 'file', models: [], disk_errors: [] }), '');
  assert.equal(brokenFileConfirmText({ mode: 'file', models: [], disk_errors: [{ field: '', message: 'yaml: line 3' }] }), text);
});
