import { test } from 'node:test';
import assert from 'node:assert/strict';
import { listAgents, getAgent, readTail, mapAgent } from '../lib/herdr.js';
import { resolveTranscript, activity } from '../lib/contextify.js';

const okRun = (stdout) => () => ({ ok: true, code: 0, stdout, stderr: '' });
const failRun = (stderr, code = 1) => () => ({ ok: false, code, stdout: '', stderr });

const AGENT = {
  name: 'river',
  pane_id: 'wE:pB',
  agent_session: { value: '846a1bcf-bad5-4ffa-8366-9c5e31205ac7' },
  agent_status: 'working',
  terminal_title_stripped: 'Centrifugo vs riverqueue',
  cwd: '/Users/surraulistic/GolandProjects',
  focused: false,
};

test('mapAgent вытаскивает session_id из вложенного agent_session', () => {
  assert.deepEqual(mapAgent(AGENT), {
    alias: 'river',
    pane_id: 'wE:pB',
    session_id: '846a1bcf-bad5-4ffa-8366-9c5e31205ac7',
    status: 'working',
    title: 'Centrifugo vs riverqueue',
    cwd: '/Users/surraulistic/GolandProjects',
    focused: false,
  });
});

test('агент без имени даёт alias null', () => {
  assert.equal(mapAgent({ ...AGENT, name: undefined }).alias, null);
});

test('listAgents разбирает конверт herdr', () => {
  const run = okRun(JSON.stringify({ result: { agents: [AGENT] } }));
  const got = listAgents(run);
  assert.equal(got.ok, true);
  assert.equal(got.agents[0].alias, 'river');
});

test('getAgent превращает конверт ошибки в error без броска', () => {
  const run = okRun(JSON.stringify({
    error: { code: 'agent_not_found', message: 'agent target zzz not found' },
  }));
  const got = getAgent(run, 'zzz');
  assert.equal(got.ok, false);
  assert.equal(got.error.code, 'agent_not_found');
});

test('нечитаемый ответ herdr не роняет процесс', () => {
  const got = getAgent(okRun('не json'), 'river');
  assert.equal(got.ok, false);
  assert.equal(got.error.code, 'bad_json');
});

test('readTail сообщает об ошибке чтения', () => {
  const got = readTail(failRun('pane closed'), 'river', 30);
  assert.equal(got.ok, false);
  assert.match(got.error.message, /pane closed/);
});

test('resolveTranscript отвергает session_id с кавычкой', () => {
  const got = resolveTranscript(() => {
    throw new Error('sqlite не должен вызываться');
  }, '/db', "x' or '1'='1");
  assert.equal(got.ok, false);
  assert.equal(got.reason, 'некорректный session_id');
});

test('пустой вывод sqlite означает непроиндексированную сессию', () => {
  const got = resolveTranscript(okRun(''), '/db', 'abc-123');
  assert.equal(got.ok, false);
  assert.match(got.reason, /не проиндексирован/);
});

test('ноль записей с ошибкой парсера объясняется, а не выдаётся за тишину', () => {
  const run = okRun(JSON.stringify([
    { id: 'T1', file_size: 7149096, last_error: 'Invalid transcript format', entry_count: 0, last_ts: null },
  ]));
  const got = resolveTranscript(run, '/db', 'abc-123');
  assert.equal(got.ok, false);
  assert.equal(got.transcriptId, 'T1');
  assert.match(got.reason, /Invalid transcript format/);
  assert.match(got.reason, /6\.8 МБ/);
});

test('нормальный транскрипт резолвится с количеством записей', () => {
  const run = okRun(JSON.stringify([
    { id: 'T2', file_size: 100, last_error: null, entry_count: 11900, last_ts: 1788164338 },
  ]));
  const got = resolveTranscript(run, '/db', 'abc-123');
  assert.deepEqual(got, { ok: true, transcriptId: 'T2', entryCount: 11900, lastTs: 1788164338 });
});

test('activity разворачивает обёртку data[].entry', () => {
  const run = okRun(JSON.stringify({ data: [{ entry: { id: 'e1', kind: 'user', content: 'да' } }] }));
  const got = activity(run, '/db', 'T2', 8);
  assert.equal(got.ok, true);
  assert.deepEqual(got.entries, [{ id: 'e1', kind: 'user', content: 'да' }]);
});
