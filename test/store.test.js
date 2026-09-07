import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { run } from '../lib/exec.js';
import { readDigest, readMany, searchEntries, readEntry, readContext } from '../lib/store.js';
import { ftsQuery } from '../lib/sql.js';

const CC = '11111111-2222-3333-4444-555555555555';
const CODEX = '66666666-7777-8888-9999-aaaaaaaaaaaa';

let dir;
let db;

// Настоящая sqlite вместо мока: резолв, фильтр заглушек и FTS живут в SQL,
// и подделанный run их не проверяет.
const SCHEMA = `
create table conv (id integer primary key, agent text, session_id text,
  source_path text, workspace text, title text, started_at integer, ended_at integer);
create index conv_session on conv(session_id);
create table msg (id integer primary key, conv_id integer, idx integer,
  role text, created_at integer, len integer, is_tool integer not null default 0);
create index msg_conv on msg(conv_id, is_tool, created_at desc, id desc);
create virtual table msg_fts using fts5(content, tokenize='unicode61 remove_diacritics 2');

insert into conv values
  (1,'claude_code','${CC}','/p/${CC}.jsonl','/g','Работа',1000,2000),
  (2,'codex','${CODEX}','/c/rollout-2026-09-05T10-00-00-${CODEX}.jsonl','/g/wt','Codex',1000,2000);

insert into msg (id,conv_id,idx,role,created_at,len,is_tool) values
  (1,1,0,'user',1788164300000,20,0),
  (2,1,1,'assistant',1788164338000,16,0),
  (3,1,2,'assistant',1788164400000,18,1),
  (4,2,0,'user',1788164500000,18,0);
insert into msg_fts (rowid,content) values
  (1,'подними River на dev'),
  (2,'влит bonuses!874'),
  (3,'[Tool: ToolSearch]'),
  (4,'кэшбек config-service');
`;

before(() => {
  dir = mkdtempSync(join(tmpdir(), 'claudex-'));
  db = join(dir, 'index.db');
  const r = run('sqlite3', [db, SCHEMA]);
  assert.equal(r.ok, true, r.stderr);
});

after(() => rmSync(dir, { recursive: true, force: true }));

test('сессия Claude Code резолвится по session_id из herdr', () => {
  const got = readDigest(run, db, CC, { limit: 8, chars: 400 });
  assert.equal(got.ok, true, got.reason);
  assert.equal(got.transcriptId, 1);
  assert.equal(got.agent, 'claude_code');
});

test('сессия Codex резолвится по хвосту имени rollout-файла', () => {
  const got = readDigest(run, db, CODEX, { limit: 8, chars: 400 });
  assert.equal(got.ok, true, got.reason);
  assert.equal(got.agent, 'codex');
});

test('цель принимается и как id разговора', () => {
  assert.equal(readDigest(run, db, '2', {}).transcriptId, 2);
});

test('заглушки инструментов не попадают ни в ленту, ни в счётчик', () => {
  const got = readDigest(run, db, CC, { limit: 8, chars: 400 });
  assert.equal(got.entryCount, 2, 'третья запись — [Tool: …], в разговоре её нет');
  assert.deepEqual(got.entries.map((e) => e.id), [2, 1]);
});

test('записи приходят свежими вперёд, время в секундах', () => {
  const got = readDigest(run, db, CC, { limit: 8, chars: 400 });
  assert.equal(got.entries[0].timestamp, 1788164338, 'миллисекунды cass переводятся в секунды');
});

// Намеренное расхождение: лента и счётчик считают разговор, а «последняя
// активность» — любую запись. Панель, которая последний час гоняла инструменты,
//не должна выглядеть простаивающей.
test('последняя активность учитывает и вызовы инструментов', () => {
  const got = readDigest(run, db, CC, { limit: 8, chars: 400 });
  assert.equal(got.lastTs, 1788164400);
  assert.equal(got.entryCount, 2);
});

test('роль agent приводится к assistant', () => {
  assert.equal(readDigest(run, db, CC, {}).entries[0].kind, 'assistant');
});

test('limit режет ленту', () => {
  assert.equal(readDigest(run, db, CC, { limit: 1, chars: 400 }).entries.length, 1);
});

test('несуществующая сессия не выдаётся за пустую', () => {
  const got = readDigest(run, db, '00000000-0000-0000-0000-000000000000', {});
  assert.equal(got.ok, false);
  assert.match(got.reason, /нет в индексе/);
});

test('readMany отвечает за каждую запрошенную сессию', () => {
  const got = readMany(run, db, [CC, CODEX, 'нет-такой'], { limit: 2, chars: 400 });
  assert.equal(got.get(CC).transcriptId, 1);
  assert.equal(got.get(CODEX).transcriptId, 2);
  assert.equal(got.get('нет-такой').ok, false);
});

test('readMany не зовёт sqlite, когда валидных целей нет', () => {
  const got = readMany(() => {
    throw new Error('sqlite не должен вызываться');
  }, db, ["плохой'"]);
  assert.equal(got.get("плохой'").ok, false);
});

test('поиск находит кириллицу — то, чего не умеет лексический индекс cass', () => {
  const got = searchEntries(run, db, ftsQuery('кэшбек'), { limit: 5 });
  assert.equal(got.ok, true);
  assert.equal(got.results.length, 1);
  assert.equal(got.results[0].agent, 'codex');
  assert.equal(got.results[0].session_id, CODEX);
});

test('поиск с префиксом и объединением слов', () => {
  assert.equal(searchEntries(run, db, ftsQuery('River dev'), { limit: 5 }).results.length, 1);
  assert.equal(searchEntries(run, db, ftsQuery('кэшб*'), { limit: 5 }).results.length, 1);
});

test('поиск по одной сессии не выходит за её пределы', () => {
  const got = searchEntries(run, db, ftsQuery('кэшбек'), { convId: 1, limit: 5 });
  assert.deepEqual(got.results, []);
});

test('limit в поиске соблюдается', () => {
  const match = ftsQuery('River OR кэшбек', { raw: true });
  assert.equal(searchEntries(run, db, match, { limit: 5 }).results.length, 2);
  assert.equal(searchEntries(run, db, match, { limit: 1 }).results.length, 1);
});

test('запрос с кавычкой не ломает SQL', () => {
  const got = searchEntries(run, db, ftsQuery("x' or '1'='1"), { limit: 5 });
  assert.equal(got.ok, true);
  assert.deepEqual(got.results, []);
});

test('entry отдаёт запись целиком', () => {
  const got = readEntry(run, db, '2');
  assert.equal(got.ok, true, got.reason);
  assert.equal(got.entry.text, 'влит bonuses!874');
  assert.equal(got.entry.agent, 'claude_code');
});

test('entry по несуществующему id объясняет, а не молчит', () => {
  const got = readEntry(run, db, '9999');
  assert.equal(got.ok, false);
  assert.match(got.reason, /нет в индексе/);
});

test('entry с кавычкой в id отсекается до обращения к базе', () => {
  const got = readEntry(() => {
    throw new Error('sqlite не должен вызываться');
  }, db, "1' or '1'='1");
  assert.equal(got.reason, 'некорректный entry_id');
});

test('context отдаёт окно и помечает якорь', () => {
  const got = readContext(run, db, '1', { before: 5, after: 5 });
  assert.equal(got.ok, true, got.reason);
  assert.deepEqual(got.context.entries.map((e) => e.id), [1, 2, 3]);
  assert.equal(got.context.entries.find((e) => e.id === 1).anchor, 1);
});

test('context держит границы окна', () => {
  const got = readContext(run, db, '2', { before: 0, after: 0 });
  assert.deepEqual(got.context.entries.map((e) => e.id), [2]);
});

test('без собранного индекса объясняется, что делать', () => {
  const got = readDigest(run, join(dir, 'нет.db'), CC, {});
  assert.equal(got.ok, false);
  assert.match(got.reason, /claudex index/);
});
