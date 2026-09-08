import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildDelegate } from '../lib/delegate.js';

const HEALTHY = {
  transcript_id: 1, agent: 'claude', entry_count: 9, last_ts: 1788164338,
  entries: [{ id: 7, kind: 'assistant', ts: 1788164338, len: 16, text: 'влит bonuses!874' }],
};

function harness({ status = 'idle', arms = true, settles = 'idle' } = {}) {
  const agent = (st) => ({
    name: null, tab_id: 'wE:t13', pane_id: 'wE:p13', agent: 'claude',
    agent_session: { value: 's1' }, agent_status: st,
    terminal_title_stripped: 'config', cwd: '/g',
  });
  const calls = [];
  let waits = 0;
  const run = (cmd, args) => {
    calls.push(`${cmd} ${args.slice(0, 2).join(' ')}`);
    if (cmd === 'herdr' && args[0] === 'tab') {
      return { ok: true, code: 0, stdout: JSON.stringify({ result: { tabs: [{ tab_id: 'wE:t13', label: 'install' }] } }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'prompt') {
      return { ok: true, code: 0, stdout: JSON.stringify({ result: {} }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'wait') {
      waits += 1;
      // первое ожидание — взведение (панель начала), второе — завершение
      if (waits === 1 && !arms) return { ok: false, code: 1, stdout: '', stderr: 'timeout' };
      return { ok: true, code: 0, stdout: JSON.stringify({ result: { agent: agent(waits === 1 ? 'working' : settles) } }), stderr: '' };
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

for (const status of ['working', 'blocked', 'done']) {
  test(`в панель со статусом ${status} промпт не отправляется`, () => {
    const { run, calls } = harness({ status });
    const got = buildDelegate(run, '/db', 'install', 'задача');
    assert.equal(got.ok, false);
    assert.equal(got.error.code, 'target_busy');
    assert.equal(calls.some((c) => c.includes('agent prompt')), false, 'herdr поставил бы его в очередь');
  });
}

test('отказ объясняет, что делать вместо ожидания', () => {
  const { run } = harness({ status: 'working' });
  const got = buildDelegate(run, '/db', 'install', 'задача');
  assert.match(got.error.message, /herdr tab create/, 'подсказывает свежего агента');
});

test('в свободную панель задача уходит и результат дожидается', () => {
  const { run, calls } = harness();
  const got = buildDelegate(run, '/db', 'install', 'подними River');
  assert.equal(got.ok, true);
  assert.equal(got.delegate.sent, true);
  assert.equal(got.delegate.final_status, 'idle');
  assert.equal(calls.filter((c) => c.includes('agent prompt')).length, 1);
});

// Идея взведения — из наблюдателя, который Codex написал себе сам: без неё
// ожидание завершается мгновенно, отчитываясь о предыдущем ходе панели.
test('сначала ждём, что панель начала, и только потом — что закончила', () => {
  const { run, calls } = harness();
  const got = buildDelegate(run, '/db', 'install', 'задача');
  assert.equal(got.delegate.armed, true);
  assert.equal(calls.filter((c) => c.includes('agent wait')).length, 2, 'взведение и завершение');
});

test('панель не начала за отведённое время — это видно в ответе', () => {
  const { run } = harness({ arms: false });
  const got = buildDelegate(run, '/db', 'install', 'задача');
  assert.equal(got.delegate.armed, false);
});

test('заблокированная панель — законный исход ожидания, а не таймаут', () => {
  const { run } = harness({ settles: 'blocked' });
  const got = buildDelegate(run, '/db', 'install', 'задача');
  assert.equal(got.timedOut, false);
  assert.equal(got.delegate.final_status, 'blocked');
});

test('--no-wait отправляет и возвращает управление сразу', () => {
  const { run, calls } = harness();
  const got = buildDelegate(run, '/db', 'install', 'задача', { noWait: true });
  assert.equal(got.delegate.waited, false);
  assert.equal(calls.some((c) => c.includes('agent wait')), false);
});

test('итог несёт дайджест, а не только статус', () => {
  const { run } = harness();
  const d = buildDelegate(run, '/db', 'install', 'задача').delegate.digest;
  assert.deepEqual(d.signals.mr, ['bonuses!874'], 'ведущему не нужен второй заход за результатом');
});
