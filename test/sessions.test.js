import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildPanes } from '../lib/sessions.js';

const AGENTS = [
  { name: null, pane_id: 'wE:p13', agent_session: { value: 's-idle-fresh' }, agent_status: 'idle', terminal_title_stripped: 'config', cwd: '/a' },
  { name: 'media', pane_id: 'wE:pX', agent_session: { value: 's-idle-old' }, agent_status: 'idle', terminal_title_stripped: 'media', cwd: '/a' },
  { name: null, pane_id: 'wE:pB', agent_session: { value: 's-working' }, agent_status: 'working', terminal_title_stripped: 'river', cwd: '/b' },
];

const ROWS = [
  { session_id: 's-idle-fresh', transcript_id: 1, entry_count: 10, last_ts: 1788164338, entries: [] },
  { session_id: 's-idle-old', transcript_id: 2, entry_count: 5, last_ts: 1788000000, entries: [] },
  { session_id: 's-working', transcript_id: null, entry_count: 0, last_ts: null, entries: [] },
];

function fakeRun(rows = ROWS) {
  const calls = [];
  const run = (cmd, args) => {
    calls.push(cmd);
    if (cmd === 'herdr') return { ok: true, code: 0, stdout: JSON.stringify({ result: { agents: AGENTS } }), stderr: '' };
    return { ok: true, code: 0, stdout: JSON.stringify(rows), stderr: '' };
  };
  return { run, calls };
}

test('безымянная панель попадает в список и адресуется своим pane_id', () => {
  const { run } = fakeRun();
  const { panes } = buildPanes(run, '/db');
  assert.equal(panes.length, 3);
  const config = panes.find((p) => p.pane_id === 'wE:p13');
  assert.equal(config.alias, null);
  assert.equal(config.target, 'wE:p13');
  assert.equal(config.session_id, 's-idle-fresh');
  assert.equal(config.entry_count, 10);
});

test('история всех панелей стоит один вызов sqlite, а не по вызову на панель', () => {
  const { run, calls } = fakeRun();
  buildPanes(run, '/db');
  assert.equal(calls.filter((c) => c === 'sqlite3').length, 1);
});

test('работающие панели идут первыми, дальше — по свежести', () => {
  const { run } = fakeRun();
  const { panes } = buildPanes(run, '/db');
  assert.deepEqual(panes.map((p) => p.pane_id), ['wE:pB', 'wE:p13', 'wE:pX']);
});

test('cwd фильтрует список', () => {
  const { run } = fakeRun();
  const { panes } = buildPanes(run, '/db', { cwd: '/b' });
  assert.deepEqual(panes.map((p) => p.pane_id), ['wE:pB']);
});

test('панель без истории несёт причину, а не молчаливый ноль', () => {
  const { run } = fakeRun();
  const { panes } = buildPanes(run, '/db');
  const working = panes.find((p) => p.pane_id === 'wE:pB');
  assert.equal(working.entry_count, null, 'ноль читается как «в сессии тихо» — тут неизвестно');
  assert.match(working.history_reason, /нет в индексе/);
});

test('служебный ключ сортировки не протекает наружу', () => {
  const { run } = fakeRun();
  const { panes } = buildPanes(run, '/db');
  assert.equal(panes.every((p) => !('last_ts' in p)), true);
});

test('недоступный herdr отдаёт ошибку, а не пустой список', () => {
  const run = () => ({ ok: false, code: 1, stdout: '', stderr: 'connection refused' });
  assert.equal(buildPanes(run, '/db').ok, false);
});
