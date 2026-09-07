import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildBrief } from '../lib/brief.js';

const TABS = [
  { tab_id: 'wE:tX', label: 'media-unification' },
  { tab_id: 'wE:tB', label: 'river-impl' },
];

const AGENTS = [
  { name: 'media', tab_id: 'wE:tX', pane_id: 'wE:pX', agent_session: { value: 's1' }, agent_status: 'idle', terminal_title_stripped: 'media', cwd: '/a' },
  { name: null, tab_id: 'wE:tB', pane_id: 'wE:pB', agent_session: { value: 's2' }, agent_status: 'working', terminal_title_stripped: 'river', cwd: '/b' },
];

const ROWS = [
  { session_id: 's1', transcript_id: 'T1', entry_count: 7, last_ts: 1788164338, entries: [{ id: 'e1', kind: 'user', ts: 1788164338, len: 9, text: 'продолжай' }] },
  { session_id: 's2', transcript_id: null, entry_count: 0, last_ts: null, entries: [] },
];

function harness({ tails = ['⏺ Готово, влит bonuses!874', '⏺ Running тесты'] } = {}) {
  const calls = [];
  const run = (cmd, args) => {
    calls.push(cmd);
    if (cmd === 'herdr' && args[0] === 'tab') {
      return { ok: true, code: 0, stdout: JSON.stringify({ result: { tabs: TABS } }), stderr: '' };
    }
    if (cmd === 'herdr') return { ok: true, code: 0, stdout: JSON.stringify({ result: { agents: AGENTS } }), stderr: '' };
    return { ok: true, code: 0, stdout: JSON.stringify(ROWS), stderr: '' };
  };
  const jobs = [];
  const runAll = async (list) => {
    jobs.push(list);
    return list.map((_, i) => ({ ok: true, code: 0, stdout: tails[i] ?? '', stderr: '' }));
  };
  return { run, runAll, calls, jobs };
}

test('brief покрывает все панели за один вызов sqlite, хвосты — одной пачкой', async () => {
  const { run, runAll, calls } = harness();
  const { brief } = await buildBrief(run, runAll, '/db');
  assert.equal(brief.panes.length, 2);
  assert.equal(calls.filter((c) => c === 'herdr').length, 2, 'agent list + tab list');
  assert.equal(calls.filter((c) => c === 'sqlite3').length, 1);
});

test('панель зовётся меткой вкладки, а не pane_id', async () => {
  const { run, runAll } = harness();
  const { brief } = await buildBrief(run, runAll, '/db');
  assert.deepEqual(brief.panes.map((p) => p.target).sort(), ['media-unification', 'river-impl']);
});

test('хвосты всех панелей читаются одной пачкой', async () => {
  const { run, runAll, jobs } = harness();
  await buildBrief(run, runAll, '/db');
  assert.equal(jobs.length, 1);
  assert.deepEqual(jobs[0].map(([, args]) => args[2]), ['wE:pX', 'wE:pB']);
});

test('хвост и сигналы собираются для каждой панели', async () => {
  const { run, runAll } = harness();
  const { brief } = await buildBrief(run, runAll, '/db');
  const media = brief.panes.find((p) => p.target === 'media-unification');
  assert.deepEqual(media.tail, ['⏺ Готово, влит bonuses!874']);
  assert.deepEqual(media.signals.mr, ['bonuses!874']);
  assert.equal(media.signals.last_user_prompt, 'продолжай');
  assert.equal(media.history.entry_count, 7);
});

test('работающая панель идёт первой', async () => {
  const { run, runAll } = harness();
  const { brief } = await buildBrief(run, runAll, '/db');
  assert.deepEqual(brief.panes.map((p) => p.target), ['river-impl', 'media-unification']);
});

test('упавшее чтение хвоста не роняет остальные панели', async () => {
  const { run } = harness();
  const runAll = async (list) => list.map((_, i) => (i === 0
    ? { ok: false, code: 1, stdout: '', stderr: 'pane closed' }
    : { ok: true, code: 0, stdout: '⏺ жив', stderr: '' }));
  const { brief } = await buildBrief(run, runAll, '/db');
  assert.equal(brief.panes.find((p) => p.target === 'media-unification').tail, null);
  assert.deepEqual(brief.panes.find((p) => p.target === 'river-impl').tail, ['⏺ жив']);
});

test('пустой herdr даёт пустой список, а не обращение к базе', async () => {
  const run = (cmd) => {
    if (cmd === 'sqlite3') throw new Error('sqlite не должен вызываться');
    return { ok: true, code: 0, stdout: JSON.stringify({ result: { agents: [] } }), stderr: '' };
  };
  const { brief } = await buildBrief(run, async () => [], '/db');
  assert.deepEqual(brief.panes, []);
});

test('недоступный herdr отдаёт ошибку', async () => {
  const run = () => ({ ok: false, code: 1, stdout: '', stderr: 'connection refused' });
  const got = await buildBrief(run, async () => [], '/db');
  assert.equal(got.ok, false);
});
