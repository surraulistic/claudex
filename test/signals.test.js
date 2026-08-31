import { test } from 'node:test';
import assert from 'node:assert/strict';
import { extractSignals } from '../lib/signals.js';

const RAW_TAIL = [
  '⏺ Running 3 shell commands…',
  '  …/GolandProjects/casino-payment-service  SNEW-711-cpf-gate-rea…  Opus 5·1M  ░░░░8%',
  '  ⏵⏵ auto mode on (shift+tab to cycle) · ← 4 agents · MR !480 · SNEW-1403 · SD-7615',
].join('\n');

test('MR и тикеты собираются из statusline и из текста записей', () => {
  const s = extractSignals({
    entries: [{ kind: 'assistant', content: 'bonuses!874/!873 влиты, SD-8208 закрыт' }],
    rawTail: RAW_TAIL,
  });
  assert.deepEqual(s.mr, ['!480', 'bonuses!874', '!873']);
  assert.deepEqual(s.tickets, ['SNEW-711', 'SNEW-1403', 'SD-7615', 'SD-8208']);
});

test('repo берётся из строки статус-бара без ведущего многоточия', () => {
  const s = extractSignals({ entries: [], rawTail: RAW_TAIL });
  assert.equal(s.repo, 'GolandProjects/casino-payment-service');
});

test('repo понимает форму воркtree с разделителем', () => {
  const bar = '  casino-balancer⟩SD-8208-consolidate-dev  SD-8208-balancer-cons…  Opus 5·1M  ████░81%';
  assert.equal(extractSignals({ entries: [], rawTail: bar }).repo, 'casino-balancer⟩SD-8208-consolidate-dev');
});

test('current_tool_call берёт последнюю строку Running', () => {
  const s = extractSignals({ entries: [], rawTail: RAW_TAIL });
  assert.equal(s.current_tool_call, '⏺ Running 3 shell commands…');
});

test('last_user_prompt — самая свежая запись пользователя', () => {
  const s = extractSignals({
    entries: [
      { kind: 'assistant', content: 'готово' },
      { kind: 'user', content: 'продолжай Payment' },
      { kind: 'user', content: 'старое' },
    ],
    rawTail: '',
  });
  assert.equal(s.last_user_prompt, 'продолжай Payment');
});

test('без хвоста repo и current_tool_call пусты, списки не падают', () => {
  const s = extractSignals({ entries: [], rawTail: '' });
  assert.deepEqual(s, {
    mr: [],
    tickets: [],
    repo: null,
    last_user_prompt: null,
    current_tool_call: null,
  });
});

test('одиночный !1 не считается номером MR', () => {
  const s = extractSignals({ entries: [{ kind: 'assistant', content: 'ура!1 и !42' }], rawTail: '' });
  assert.deepEqual(s.mr, ['!42']);
});
