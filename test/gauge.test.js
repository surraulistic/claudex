import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readGauges } from '../lib/gauge.js';
import { readTail } from '../lib/herdr.js';

const BAR = '  ~/GolandProjects/license-platform  panel-real-data  Opus 5  ██░░░49%  5h░░░░░7% 12:00  7d███░░57% 10.09 02:00';

test('первый столбик без префикса — контекст, именованные — лимиты', () => {
  assert.deepEqual(readGauges(BAR), { context_pct: 49, limits: { '5h': 7, '7d': 57 } });
});

test('при высоком заполнении Claude Code пишет процент словами', () => {
  const line = '99% context used';
  assert.equal(readGauges(line).context_pct, 99);
});

test('текстовая форма важнее столбика, если есть обе', () => {
  assert.equal(readGauges(`${BAR}\n  97% context used`).context_pct, 97);
});

test('панель без статусной строки не выдумывает значений', () => {
  assert.deepEqual(readGauges('⏺ просто вывод'), { context_pct: null, limits: null });
});

test('лимит может быть один', () => {
  assert.deepEqual(readGauges('  репо  ветка  Opus 5  ██░░░56%  7d██░░░35%').limits, { '7d': 35 });
});

// На работающей панели herdr --source recent отдаёт пусто — измерено на всех
// working-панелях. Хвост при этом нужен именно тогда, когда панель занята.
function fakeRead(bySource) {
  const calls = [];
  const run = (cmd, args) => {
    const source = args[args.indexOf('--source') + 1];
    calls.push(source);
    return { ok: true, code: 0, stdout: bySource[source] ?? '', stderr: '' };
  };
  return { run, calls };
}

test('на работающей панели хвост берётся из visible', () => {
  const { run, calls } = fakeRead({ visible: 'экран', recent: '' });
  const got = readTail(run, 'wE:p13', 30, { status: 'working' });
  assert.equal(got.raw, 'экран');
  assert.equal(got.source, 'visible');
  assert.equal(calls[0], 'visible', 'visible пробуется первым, а не после пустого recent');
});

test('на покое хвост берётся из recent — там вчетверо больше строк', () => {
  const { run, calls } = fakeRead({ visible: 'экран', recent: 'скроллбэк' });
  const got = readTail(run, 'wE:pX', 30, { status: 'idle' });
  assert.equal(got.raw, 'скроллбэк');
  assert.equal(calls[0], 'recent');
});

test('пустой ответ первого источника добирается вторым', () => {
  const { run, calls } = fakeRead({ visible: 'экран', recent: '   ' });
  const got = readTail(run, 'wE:pX', 30, { status: 'idle' });
  assert.equal(got.raw, 'экран');
  assert.deepEqual(calls, ['recent', 'visible']);
});

test('оба источника пусты — это успех с пустым хвостом, а не ошибка', () => {
  const { run } = fakeRead({ visible: '', recent: '' });
  const got = readTail(run, 'wE:pX', 30, { status: 'idle' });
  assert.equal(got.ok, true);
  assert.equal(got.raw.trim(), '');
});

test('недоступная панель остаётся ошибкой', () => {
  const run = () => ({ ok: false, code: 1, stdout: '', stderr: 'pane closed' });
  const got = readTail(run, 'wE:pX', 30, { status: 'idle' });
  assert.equal(got.ok, false);
  assert.match(got.error.message, /pane closed/);
});
