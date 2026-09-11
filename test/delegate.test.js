import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildDelegate } from '../lib/delegate.js';
import { buildWatch } from '../lib/watch.js';
import { acquire } from '../lib/lock.js';

const HEALTHY = {
  transcript_id: 1, agent: 'claude', entry_count: 9, last_ts: 1788164338,
  entries: [{ id: 7, kind: 'assistant', ts: 1788164338, len: 16, text: 'влит bonuses!874' }],
};

let locks;
const dirs = [];
function lockDir() {
  locks = mkdtempSync(join(tmpdir(), 'claudex-lock-'));
  dirs.push(locks);
  return { dir: locks };
}
process.on('exit', () => dirs.forEach((d) => rmSync(d, { recursive: true, force: true })));

// waits — сценарий ответов herdr agent wait по порядку вызовов.
// 'working' | 'idle' | 'blocked' | {error: {code}} — последний повторяется.
function harness({ status = 'idle', waits = ['working', 'idle'], promptFails = false, notifyFails = 0 } = {}) {
  const agent = (st) => ({
    name: null, tab_id: 'wE:t13', pane_id: 'wE:p13', agent: 'claude',
    agent_session: { value: 's1' }, agent_status: st,
    terminal_title_stripped: 'config', cwd: '/g',
  });
  const calls = [];
  let waitN = 0;
  let notifyN = 0;
  const run = (cmd, args) => {
    calls.push(`${cmd} ${args.slice(0, 2).join(' ')}`);
    if (cmd === 'herdr' && args[0] === 'tab') {
      return { ok: true, code: 0, stdout: JSON.stringify({ result: { tabs: [{ tab_id: 'wE:t13', label: 'install' }] } }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'prompt') {
      const toCodex = args[2] !== 'wE:p13';
      if (toCodex) {
        notifyN += 1;
        if (notifyN <= notifyFails) {
          return { ok: true, code: 0, stdout: JSON.stringify({ error: { code: 'agent_blocked', message: 'занят' } }), stderr: '' };
        }
      } else if (promptFails) {
        return { ok: true, code: 0, stdout: JSON.stringify({ error: { code: 'agent_blocked', message: 'занят' } }), stderr: '' };
      }
      return { ok: true, code: 0, stdout: JSON.stringify({ result: {} }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'wait') {
      const step = waits[Math.min(waitN, waits.length - 1)];
      waitN += 1;
      if (typeof step === 'object') {
        return { ok: true, code: 0, stdout: JSON.stringify({ error: step.error }), stderr: '' };
      }
      return { ok: true, code: 0, stdout: JSON.stringify({ result: { agent: agent(step) } }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'get') {
      return { ok: true, code: 0, stdout: JSON.stringify({ result: { agent: agent(status) } }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'read') return { ok: true, code: 0, stdout: '⏺ готово', stderr: '' };
    if (cmd === 'herdr') return { ok: true, code: 0, stdout: JSON.stringify({ result: { agents: [agent(status)] } }), stderr: '' };
    return { ok: true, code: 0, stdout: JSON.stringify(HEALTHY), stderr: '' };
  };
  return { run, calls, opts: { lock: lockDir(), sleep: () => {} } };
}

for (const status of ['working', 'blocked', 'done']) {
  test(`в панель со статусом ${status} промпт не отправляется`, () => {
    const { run, calls, opts } = harness({ status });
    const got = buildDelegate(run, '/db', 'install', 'задача', opts);
    assert.equal(got.ok, false);
    assert.equal(got.error.code, 'target_busy');
    assert.equal(calls.some((c) => c.includes('agent prompt')), false, 'herdr поставил бы его в очередь');
  });
}

test('отказ объясняет, что делать вместо ожидания', () => {
  const { run, opts } = harness({ status: 'working' });
  assert.match(buildDelegate(run, '/db', 'install', 'з', opts).error.message, /herdr tab create/);
});

test('обычный ход: working -> idle, задача ушла, результат дождался', () => {
  const { run, calls, opts } = harness();
  const got = buildDelegate(run, '/db', 'install', 'подними River', opts);
  assert.equal(got.outcome, 'settled');
  assert.equal(got.delegate.final_status, 'idle');
  assert.equal(calls.filter((c) => c.includes('agent prompt')).length, 1);
});

// Идея взведения — из наблюдателя, который Codex написал себе сам.
test('сначала ждём, что панель начала, и только потом — что закончила', () => {
  const { run, calls, opts } = harness();
  const got = buildDelegate(run, '/db', 'install', 'з', opts);
  assert.equal(got.delegate.armed, true);
  assert.equal(calls.filter((c) => c.includes('agent wait')).length, 2);
});

test('панель уже работала до отправки — фаза взведения не нужна', () => {
  const { run, calls, opts } = harness({ status: 'idle', waits: ['idle'] });
  const got = buildWatch(run, '/db', 'wE:p13', { ...opts, armMs: 0, timeoutMs: 1000 });
  assert.equal(got.outcome, 'settled');
  assert.equal(calls.filter((c) => c.includes('agent wait')).length, 0, 'ждать нечего');
});

test('быстрое завершение: панель не успела попасть в working', () => {
  const { run, opts } = harness({ waits: [{ error: { code: 'timeout', message: 'timeout' } }] });
  const got = buildDelegate(run, '/db', 'install', 'з', { ...opts, armMs: 500 });
  assert.equal(got.delegate.armed, false);
  assert.equal(got.outcome, 'never_started');
  assert.ok(got.delegate.digest, 'дайджест всё равно собран — работа могла успеть пройти');
});

test('таймаут ожидания — отдельный исход, не успех', () => {
  const { run, opts } = harness({ waits: ['working', { error: { code: 'timeout', message: 'timeout' } }] });
  const got = buildDelegate(run, '/db', 'install', 'з', opts);
  assert.equal(got.outcome, 'timeout');
  assert.equal(got.delegate.final_status, 'unknown', 'состояние неизвестно, а не idle');
  assert.equal(got.delegate.digest, null, 'дайджест не выдаётся за результат задачи');
});

test('blocked — законный конец ожидания, ведущему видно, что панель ждёт ответа', () => {
  const { run, opts } = harness({ waits: ['working', 'blocked'] });
  const got = buildDelegate(run, '/db', 'install', 'з', opts);
  assert.equal(got.outcome, 'settled');
  assert.equal(got.delegate.final_status, 'blocked');
});

test('моргание herdr пересматривается, а не считается таймаутом', () => {
  const { run, calls, opts } = harness({
    waits: ['working', { error: { code: 'bad_json', message: 'мусор' } }, 'idle'],
  });
  const got = buildDelegate(run, '/db', 'install', 'з', opts);
  assert.equal(got.outcome, 'settled', 'transient-сбой не должен читаться как «не дошла»');
  assert.equal(calls.filter((c) => c.includes('agent wait')).length, 3);
});

test('устойчивый сбой herdr — исход «не знаем», отличный от таймаута', () => {
  const { run, opts } = harness({ waits: ['working', { error: { code: 'bad_json', message: 'мусор' } }] });
  const got = buildDelegate(run, '/db', 'install', 'з', opts);
  assert.equal(got.outcome, 'herdr_error');
  assert.ok(got.delegate.log.some((l) => l.phase === 'settle'), 'причина попадает в лог');
});

test('второй наблюдатель на ту же панель не запускается', () => {
  const { run, opts } = harness();
  const outer = acquire('install', { ...opts.lock, pid: process.pid });
  assert.equal(outer.ok, true);
  try {
    const got = buildDelegate(run, '/db', 'install', 'з', opts);
    assert.equal(got.ok, false);
    assert.equal(got.error.code, 'already_watched');
    assert.match(got.error.message, /уже под наблюдением/);
  } finally {
    outer.release();
  }
});

test('запись мёртвого процесса не блокирует навсегда', () => {
  const { opts } = harness();
  const stale = acquire('ghost', { ...opts.lock, pid: 999999 });
  assert.equal(stale.ok, true);
  const again = acquire('ghost', { ...opts.lock, pid: process.pid });
  assert.equal(again.ok, true, 'лок мёртвого pid снимается');
  again.release();
});

test('провал уведомления не глушится и виден в ответе', () => {
  const { run, opts } = harness({ notifyFails: 99 });
  const got = buildDelegate(run, '/db', 'install', 'з', {
    ...opts, notifyTarget: 'codex', notify: { attempts: 2, pauseMs: 0 },
  });
  assert.equal(got.delegate.notified.ok, false);
  assert.equal(got.delegate.notified.attempts, 2);
  assert.equal(got.delegate.notified.log.length, 2, 'каждая попытка с причиной');
});

test('уведомление со второй попытки — успех, попытки посчитаны', () => {
  const { run, opts } = harness({ notifyFails: 1 });
  const got = buildDelegate(run, '/db', 'install', 'з', {
    ...opts, notifyTarget: 'codex', notify: { attempts: 3, pauseMs: 0 },
  });
  assert.equal(got.delegate.notified.ok, true);
  assert.equal(got.delegate.notified.attempts, 2);
});

test('--no-wait отправляет и возвращает управление сразу', () => {
  const { run, calls, opts } = harness();
  const got = buildDelegate(run, '/db', 'install', 'з', { ...opts, noWait: true });
  assert.equal(got.delegate.waited, false);
  assert.equal(calls.some((c) => c.includes('agent wait')), false);
});

test('итог несёт дайджест, а не только статус', () => {
  const { run, opts } = harness();
  const d = buildDelegate(run, '/db', 'install', 'з', opts).delegate.digest;
  assert.deepEqual(d.signals.mr, ['bonuses!874']);
});

// Прямой herdr agent prompt не оставляет наблюдателя, и узнать об этом можно
// только там, где ведущий и так смотрит.
test('панель под наблюдением помечена в sessions', async () => {
  const { buildPanes } = await import('../lib/sessions.js');
  const { acquire } = await import('../lib/lock.js');
  const dir = lockDir();
  const agent = { name: null, tab_id: 'wE:t13', pane_id: 'wE:p13', agent: 'claude',
    agent_session: { value: 's1' }, agent_status: 'working', terminal_title_stripped: 'c', cwd: '/g' };
  const run = (cmd, args) => {
    if (cmd === 'herdr' && args[0] === 'tab') {
      return { ok: true, code: 0, stdout: JSON.stringify({ result: { tabs: [{ tab_id: 'wE:t13', label: 'install' }] } }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'read') return { ok: true, code: 0, stdout: '', stderr: '' };
    if (cmd === 'herdr') return { ok: true, code: 0, stdout: JSON.stringify({ result: { agents: [agent] } }), stderr: '' };
    return { ok: true, code: 0, stdout: '[]', stderr: '' };
  };
  const runAll = async (jobs) => jobs.map(() => ({ ok: true, code: 0, stdout: '', stderr: '' }));
  const before = await buildPanes(run, runAll, '/db', { lock: dir });
  assert.equal(before.panes[0].watched, false, 'прямой prompt оставил бы панель без наблюдения');

  const held = acquire('install', { ...dir, pid: process.pid });
  try {
    const after = await buildPanes(run, runAll, '/db', { lock: dir });
    assert.equal(after.panes[0].watched, true);
  } finally {
    held.release();
  }
});

test('delegate доказывает привязку к задаче, потому что видел старт панели', () => {
  const { run, opts } = harness();
  assert.equal(buildDelegate(run, '/db', 'install', 'з', opts).delegate.correlated, true);
});

test('присоединение к уже идущей работе привязку не доказывает', () => {
  const { run, opts } = harness({ status: 'idle', waits: ['idle'] });
  const got = buildWatch(run, '/db', 'wE:p13', { ...opts, armMs: 0, timeoutMs: 500 });
  assert.equal(got.watch.correlated, false, 'какая именно задача идёт — неизвестно, и врать нельзя');
});
