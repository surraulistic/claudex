import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildWatch } from '../lib/watch.js';

const HEALTHY = {
  transcript_id: 1, agent: 'claude', entry_count: 12, last_ts: 1788164338,
  entries: [{ id: 7, kind: 'assistant', ts: 1788164338, len: 16, text: 'влит bonuses!874' }],
};

function harness({ status = 'working', afterWait = 'idle', waitFails = false } = {}) {
  const agent = (st) => ({
    name: null, tab_id: 'wE:t13', pane_id: 'wE:p13', agent: 'claude',
    agent_session: { value: 's1' }, agent_status: st,
    terminal_title_stripped: 'config', cwd: '/g',
  });
  const calls = [];
  const run = (cmd, args) => {
    calls.push(`${cmd} ${args.slice(0, 2).join(' ')}`);
    if (cmd === 'herdr' && args[0] === 'tab') {
      return { ok: true, code: 0, stdout: JSON.stringify({ result: { tabs: [{ tab_id: 'wE:t13', label: 'install' }] } }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'wait') {
      return waitFails
        ? { ok: false, code: 1, stdout: '', stderr: 'timeout' }
        : { ok: true, code: 0, stdout: JSON.stringify({ result: { agent: agent(afterWait) } }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'get') {
      return { ok: true, code: 0, stdout: JSON.stringify({ result: { agent: agent(status) } }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'read') return { ok: true, code: 0, stdout: '⏺ готово', stderr: '' };
    if (cmd === 'herdr') return { ok: true, code: 0, stdout: JSON.stringify({ result: { agents: [agent(status)] } }), stderr: '' };
    return { ok: true, code: 0, stdout: JSON.stringify(HEALTHY), stderr: '' };
  };
  return { run, calls };
}

test('на занятой панели наблюдатель ждёт и возвращает итог', () => {
  const { run, calls } = harness({ status: 'working' });
  const got = buildWatch(run, '/db', 'install');
  assert.equal(got.ok, true);
  assert.equal(got.watch.waited, true);
  assert.equal(got.watch.final_status, 'idle');
  assert.equal(calls.some((c) => c.includes('agent wait')), true);
});

test('на свободной панели не ждёт вовсе', () => {
  const { run, calls } = harness({ status: 'idle' });
  const got = buildWatch(run, '/db', 'install');
  assert.equal(got.watch.waited, false);
  assert.equal(calls.some((c) => c.includes('agent wait')), false, 'ждать нечего — сразу итог');
});

test('итог включает дайджест панели, а не только статус', () => {
  const { run } = harness();
  const d = buildWatch(run, '/db', 'install').watch.digest;
  assert.equal(d.history.entry_count, 12);
  assert.deepEqual(d.signals.mr, ['bonuses!874']);
});

test('не дождался — это отдельный исход, а не успех', () => {
  const { run } = harness({ waitFails: true });
  const got = buildWatch(run, '/db', 'install');
  assert.equal(got.timedOut, true);
  assert.equal(got.watch.final_status, 'unknown', 'состояние неизвестно, а не idle');
});

test('заблокированная панель — тоже конец ожидания', () => {
  const { run } = harness({ afterWait: 'blocked' });
  assert.equal(buildWatch(run, '/db', 'install').watch.final_status, 'blocked');
});

test('панель зовётся меткой вкладки', () => {
  const { run } = harness({ status: 'idle' });
  assert.equal(buildWatch(run, '/db', 'install').watch.target, 'install');
});

test('несуществующая цель — ошибка, а не пустое ожидание', () => {
  const run = (cmd, args) => {
    if (cmd === 'herdr' && args[0] === 'tab') return { ok: true, code: 0, stdout: JSON.stringify({ result: { tabs: [] } }), stderr: '' };
    if (cmd === 'herdr' && args[1] === 'get') {
      return { ok: true, code: 0, stdout: JSON.stringify({ error: { code: 'agent_not_found', message: 'нет' } }), stderr: '' };
    }
    return { ok: true, code: 0, stdout: JSON.stringify({ result: { agents: [] } }), stderr: '' };
  };
  const got = buildWatch(run, '/db', 'нетакой');
  assert.equal(got.ok, false);
  assert.equal(got.error.code, 'agent_not_found');
});
