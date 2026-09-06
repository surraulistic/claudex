import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildDigest } from '../lib/digest.js';

const ENTRIES = [
  { id: 'e1', kind: 'assistant', ts: 1788164338, len: 16, text: 'bonuses!874 влит' },
  { id: 'e2', kind: 'user', ts: 1788164300, len: 9, text: 'продолжай' },
];

const HEALTHY = {
  transcript_id: 'T1', file_size: 10, last_error: null,
  entry_count: 11900, last_ts: 1788164338, entries: ENTRIES,
};

function fakeRun({ status = 'working', sqlite = HEALTHY, tail = 'хвост', alias = 'river' } = {}) {
  const agent = {
    name: alias,
    pane_id: 'wE:pB',
    agent_session: { value: '846a1bcf' },
    agent_status: status,
    terminal_title_stripped: 'Centrifugo vs riverqueue',
    cwd: '/Users/surraulistic/GolandProjects',
    focused: false,
  };
  const calls = [];
  const run = (cmd, args) => {
    calls.push(`${cmd} ${args.slice(0, 3).join(' ')}`);
    if (cmd === 'herdr' && args[1] === 'get') {
      return { ok: true, code: 0, stdout: JSON.stringify({ result: { agent } }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'read') {
      return { ok: true, code: 0, stdout: tail, stderr: '' };
    }
    if (cmd === 'sqlite3') {
      return { ok: true, code: 0, stdout: sqlite ? JSON.stringify(sqlite) : '', stderr: '' };
    }
    throw new Error(`неожиданный вызов ${cmd}`);
  };
  return { run, calls };
}

test('digest собирает live, history, tail и signals', () => {
  const { run } = fakeRun();
  const got = buildDigest(run, '/db', 'river');
  assert.equal(got.ok, true);
  const d = got.digest;
  assert.equal(d.target, 'river');
  assert.equal(d.live.session_id, '846a1bcf');
  assert.equal(d.history.transcript_id, 'T1');
  assert.equal(d.history.entry_count, 11900);
  assert.equal(d.history.entries.length, 2);
  assert.equal(d.history.entries[0].id, 'e1');
  assert.deepEqual(d.tail, ['хвост']);
  assert.deepEqual(d.signals.mr, ['bonuses!874']);
  assert.equal(d.signals.last_user_prompt, 'продолжай');
});

for (const status of ['idle', 'done']) {
  test(`хвост читается и на панели в состоянии ${status}`, () => {
    const { run, calls } = fakeRun({ status, tail: '⏺ Готово, влит bonuses!874' });
    const d = buildDigest(run, '/db', 'river').digest;
    assert.equal(calls.some((c) => c.includes('agent read')), true);
    assert.deepEqual(d.tail, ['⏺ Готово, влит bonuses!874']);
  });
}

test('дайджест адресуется по pane_id безымянной панели', () => {
  const { run } = fakeRun({ alias: null });
  const d = buildDigest(run, '/db', 'wE:pB').digest;
  assert.equal(d.target, 'wE:pB');
  assert.equal(d.alias, null);
});

test('недоступный хвост не роняет дайджест', () => {
  const agent = { name: 'river', pane_id: 'wE:pB', agent_session: { value: '846a1bcf' }, agent_status: 'working' };
  const run = (cmd, args) => {
    if (cmd === 'herdr' && args[1] === 'get') {
      return { ok: true, code: 0, stdout: JSON.stringify({ result: { agent } }), stderr: '' };
    }
    if (cmd === 'herdr' && args[1] === 'read') return { ok: false, code: 1, stdout: '', stderr: 'pane closed' };
    return { ok: true, code: 0, stdout: JSON.stringify(HEALTHY), stderr: '' };
  };
  const d = buildDigest(run, '/db', 'river').digest;
  assert.equal(d.tail, null);
  assert.equal(d.history.entry_count, 11900);
});

test('непроиндексированная сессия отдаёт причину, а не пустую историю', () => {
  const sqlite = { transcript_id: 'T9', file_size: 7149096, last_error: 'Invalid transcript format', entry_count: 0, last_ts: null, entries: [] };
  const { run } = fakeRun({ sqlite });
  const d = buildDigest(run, '/db', 'river').digest;
  assert.equal(d.history.transcript_id, 'T9');
  assert.equal(d.history.entry_count, 0);
  assert.match(d.history.reason, /Invalid transcript format/);
  assert.equal('entries' in d.history, false);
});

test('история одной панели стоит ровно один вызов sqlite', () => {
  const { run, calls } = fakeRun();
  buildDigest(run, '/db', 'river');
  assert.equal(calls.filter((c) => c.startsWith('sqlite3')).length, 1);
});

test('длина записи режется, исходный размер сохраняется', () => {
  const { run } = fakeRun();
  const d = buildDigest(run, '/db', 'river', { chars: 5 }).digest;
  assert.equal(d.history.entries[0].truncated, true);
  assert.equal(d.history.entries[0].chars, 'bonuses!874 влит'.length);
});
