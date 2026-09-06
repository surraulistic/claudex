import { test } from 'node:test';
import assert from 'node:assert/strict';
import { listAgents, getAgent, readTail, mapAgent } from '../lib/herdr.js';
import { readDigest, readMany, searchEntries } from '../lib/contextify.js';
import { ftsQuery, lit, digestSql, searchSql } from '../lib/sql.js';

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

const HEALTHY = {
  transcript_id: 'T2',
  file_size: 100,
  last_error: null,
  entry_count: 11900,
  last_ts: 1788164338,
  entries: [{ id: 'e1', kind: 'user', ts: 1788164338, len: 2, text: 'да' }],
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
  const got = listAgents(okRun(JSON.stringify({ result: { agents: [AGENT] } })));
  assert.equal(got.ok, true);
  assert.equal(got.agents[0].alias, 'river');
});

test('getAgent превращает конверт ошибки в error без броска', () => {
  const got = getAgent(okRun(JSON.stringify({
    error: { code: 'agent_not_found', message: 'agent target zzz not found' },
  })), 'zzz');
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

test('readDigest отвергает session_id с кавычкой, не доходя до sqlite', () => {
  const got = readDigest(() => {
    throw new Error('sqlite не должен вызываться');
  }, '/db', "x' or '1'='1");
  assert.equal(got.ok, false);
  assert.equal(got.reason, 'некорректный session_id');
});

test('пустой вывод sqlite означает непроиндексированную сессию', () => {
  const got = readDigest(okRun(''), '/db', 'abc-123');
  assert.equal(got.ok, false);
  assert.match(got.reason, /не проиндексирован/);
});

test('ноль записей с ошибкой парсера объясняется, а не выдаётся за тишину', () => {
  const run = okRun(JSON.stringify({
    transcript_id: 'T1', file_size: 7149096, last_error: 'Invalid transcript format',
    entry_count: 0, last_ts: null, entries: [],
  }));
  const got = readDigest(run, '/db', 'abc-123');
  assert.equal(got.ok, false);
  assert.equal(got.transcriptId, 'T1');
  assert.match(got.reason, /Invalid transcript format/);
  assert.match(got.reason, /6\.8 МБ/);
});

test('нормальный транскрипт резолвится вместе с записями за один вызов', () => {
  const calls = [];
  const run = (cmd, args) => {
    calls.push(cmd);
    return { ok: true, code: 0, stdout: JSON.stringify(HEALTHY), stderr: '' };
  };
  const got = readDigest(run, '/db', 'abc-123');
  assert.deepEqual(calls, ['sqlite3']);
  assert.equal(got.transcriptId, 'T2');
  assert.equal(got.entryCount, 11900);
  assert.equal(got.entries[0].content, 'да');
  assert.equal(got.entries[0].contentFullSize, 2);
});

test('длинная запись помечается обрезанной по реальному размеру', () => {
  const run = okRun(JSON.stringify({ ...HEALTHY, entries: [{ id: 'e1', kind: 'user', ts: 1, len: 99999, text: 'начало' }] }));
  assert.equal(readDigest(run, '/db', 'abc-123').entries[0].contentTruncated, true);
});

test('readMany отвечает за каждую запрошенную сессию, включая ненайденные', () => {
  const run = okRun(JSON.stringify([{ ...HEALTHY, session_id: 'a-1' }]));
  const got = readMany(run, '/db', ['a-1', 'b-2', "плохой'"]);
  assert.equal(got.get('a-1').ok, true);
  assert.match(got.get('b-2').reason, /не проиндексирован/);
  assert.equal(got.get("плохой'").reason, 'некорректный session_id');
});

test('readMany не зовёт sqlite, когда валидных сессий нет', () => {
  const got = readMany(() => {
    throw new Error('sqlite не должен вызываться');
  }, '/db', ["плохой'"]);
  assert.equal(got.get("плохой'").ok, false);
});

test('searchEntries возвращает пустой список, а не падает, когда ничего не нашлось', () => {
  const got = searchEntries(okRun(''), '/db', '"river"');
  assert.deepEqual(got, { ok: true, results: [] });
});

test('ftsQuery раскавычивает пользовательский текст и держит префикс', () => {
  assert.equal(ftsQuery('river миграц*'), '"river" AND "миграц"*');
  assert.equal(ftsQuery('a "OR" b'), '"a" AND "OR" AND "b"');
  assert.equal(ftsQuery('   '), null);
  assert.equal(ftsQuery('NEAR(a b)', { raw: true }), 'NEAR(a b)');
});

test('одинарная кавычка удваивается, а не разрывает литерал', () => {
  assert.equal(lit("it's"), "'it''s'");
  assert.ok(digestSql("x''y", 8, 400).includes("'x''''y'"));
  assert.ok(searchSql('"a"', { transcriptId: 'T1' }).includes("'T1'"));
});
