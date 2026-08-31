import { test } from 'node:test';
import assert from 'node:assert/strict';
import { toIso, cutText, cleanTail, stripAnsi } from '../lib/normalize.js';

const ESC = String.fromCharCode(27);

test('toIso обратим в исходную метку', () => {
  const ts = 1788164338;
  assert.equal(new Date(toIso(ts)).getTime() / 1000, ts);
  assert.match(toIso(ts), /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[+-]\d{2}:\d{2}$/);
});

test('toIso возвращает null на мусоре', () => {
  assert.equal(toIso(null), null);
  assert.equal(toIso('вчера'), null);
  assert.equal(toIso(Number.NaN), null);
});

test('cutText схлопывает переносы и режет по границе', () => {
  assert.deepEqual(cutText('a\n\nb   c', 40), { text: 'a b c', truncated: false });
  assert.deepEqual(cutText('абвгде', 3), { text: 'абв…', truncated: true });
});

test('stripAnsi снимает escape, но не трогает скобки в тексте', () => {
  assert.equal(stripAnsi(`${ESC}[31mкрасный${ESC}[0m`), 'красный');
  assert.equal(stripAnsi('см. [0] и [12]'), 'см. [0] и [12]');
});

test('cleanTail выбрасывает рамки, футер и пустой промпт', () => {
  const raw = [
    '──────────────────────',
    '⏺ Всё зелёное.   ',
    '',
    '❯',
    '  …/GolandProjects/casino-payment-service  SNEW-711  ░░░░8%',
    '  ⏵⏵ auto mode on (shift+tab to cycle) · MR !480',
  ].join('\n');
  assert.deepEqual(cleanTail(raw, 12), ['⏺ Всё зелёное.']);
});

test('cleanTail отдаёт только последние maxLines строк', () => {
  const raw = ['один', 'два', 'три', 'четыре'].join('\n');
  assert.deepEqual(cleanTail(raw, 2), ['три', 'четыре']);
});
