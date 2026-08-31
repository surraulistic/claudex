import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildDigest } from '../lib/digest.js';

function fakeRun({ status = 'working', sqliteRows, tail = 'хвост' } = {}) {
  const agent = {
    name: 'river',
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
      return { ok: true, code: 0, stdout: JSON.stringify(sqliteRows), stderr: '' };
    }
    if (cmd === 'contextify') {
      return {
        ok: true,
        code: 0,
        stderr: '',
        stdout: JSON.stringify({
          data: [
            { entry: { id: 'e1', kind: 'assistant', timestamp: 1788164338, content: 'bonuses!874 влит' } },
            { entry: { id: 'e2', kind: 'user', timestamp: 1788164300, content: 'продолжай' } },
          ],
        }),
      };
    }
    throw new Error(`неожиданный вызов ${cmd}`);
  };
  return { run, calls };
}

const HEALTHY = [{ id: 'T1', file_size: 10, last_error: null, entry_count: 11900, last_ts: 1788164338 }];

test('digest собирает live, history, tail и signals', () => {
  const { run } = fakeRun({ sqliteRows: HEALTHY });
  const got = buildDigest(run, '/db', 'river');
  assert.equal(got.ok, true);
  const d = got.digest;
  assert.equal(d.alias, 'river');
  assert.equal(d.live.session_id, '846a1bcf');
  assert.equal(d.history.transcript_id, 'T1');
  assert.equal(d.history.entry_count, 11900);
  assert.equal(d.history.entries.length, 2);
  assert.equal(d.history.entries[0].id, 'e1');
  assert.deepEqual(d.signals.mr, ['bonuses!874']);
  assert.equal(d.signals.last_user_prompt, 'продолжай');
});

test('на простаивающей панели хвост не читается', () => {
  const { run, calls } = fakeRun({ status: 'idle', sqliteRows: HEALTHY });
  const got = buildDigest(run, '/db', 'river');
  assert.equal(got.digest.tail, null);
  assert.equal(calls.some((c) => c.includes('agent read')), false);
});

test('непроиндексированная сессия отдаёт причину, а не пустую историю', () => {
  const rows = [{ id: 'T9', file_size: 7149096, last_error: 'Invalid transcript format', entry_count: 0, last_ts: null }];
  const { run } = fakeRun({ sqliteRows: rows });
  const d = buildDigest(run, '/db', 'river').digest;
  assert.equal(d.history.transcript_id, 'T9');
  assert.equal(d.history.entry_count, 0);
  assert.match(d.history.reason, /Invalid transcript format/);
  assert.equal('entries' in d.history, false);
});

test('неизвестный алиас возвращает ошибку, а не бросает', () => {
  const run = () => ({
    ok: true,
    code: 0,
    stderr: '',
    stdout: JSON.stringify({ error: { code: 'agent_not_found', message: 'нет такого' } }),
  });
  const got = buildDigest(run, '/db', 'zzz');
  assert.equal(got.ok, false);
  assert.equal(got.error.code, 'agent_not_found');
});

test('длина записи режется, исходный размер сохраняется', () => {
  const { run } = fakeRun({ sqliteRows: HEALTHY });
  const d = buildDigest(run, '/db', 'river', { chars: 5 }).digest;
  assert.equal(d.history.entries[0].truncated, true);
  assert.equal(d.history.entries[0].chars, 'bonuses!874 влит'.length);
});
