import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { run } from '../lib/exec.js';
import { readDigest, readMany, searchEntries, readEntry, readContext } from '../lib/contextify.js';
import { ftsQuery } from '../lib/sql.js';

const INDEXED = '11111111-2222-3333-4444-555555555555';
const CLI_INGESTED = '66666666-7777-8888-9999-aaaaaaaaaaaa';

let dir;
let db;

// Настоящая sqlite вместо мока: обе ветки резолва — по provider_session_id и по
// имени файла — живут в SQL, и подделанный run их не проверяет.
const SCHEMA = `
create table projects (id text primary key, name text, root_path text not null);
create table transcripts (
  id text primary key, project_id text not null, file_path text not null,
  provider text not null, provider_session_id text, file_size integer,
  last_error text, updated_at integer not null);
create table transcript_entries (
  id text primary key, transcript_id text not null, project_id text not null,
  kind text not null, timestamp integer not null, content text not null,
  cwd text, git_branch text, is_sidechain integer not null default 0,
  display_in_timeline integer not null default 1);
create virtual table transcript_entries_fts using fts5(content, entry_id unindexed);

insert into projects values ('p1','GolandProjects','/g');
insert into transcripts values
  ('T-OK','p1','/g/${INDEXED}.jsonl','claude.code','${INDEXED}',100,null,10),
  ('T-CLI','p1','/g/${CLI_INGESTED}.jsonl','claude.code',null,200,null,20),
  ('T-BROKEN','p1','/g/deadbeef.jsonl','claude.code','broken-1',7149096,'Invalid transcript format',30);
insert into transcript_entries values
  ('e1','T-OK','p1','user',1788164300,'подними River на dev','/g/wt','main',0,1),
  ('e2','T-OK','p1','assistant',1788164338,'влит bonuses!874','/g/wt','main',0,1),
  ('e3','T-OK','p1','assistant',1788164400,'скрытая',null,null,0,0),
  ('e4','T-CLI','p1','user',1788164500,'кэш config-service','/g/cfg','stage',0,1);
insert into transcript_entries_fts (rowid, content, entry_id)
  select rowid, content, id from transcript_entries;
`;

before(() => {
  dir = mkdtempSync(join(tmpdir(), 'claudex-'));
  db = join(dir, 'contextify.db');
  const r = run('sqlite3', [db, SCHEMA]);
  assert.equal(r.ok, true, r.stderr);
});

after(() => rmSync(dir, { recursive: true, force: true }));

test('сессия с provider_session_id резолвится точным ключом', () => {
  const got = readDigest(run, db, INDEXED, { limit: 8, chars: 400 });
  assert.equal(got.ok, true);
  assert.equal(got.transcriptId, 'T-OK');
  assert.equal(got.entryCount, 3);
});

test('entry_count считает всю сессию, entries — только видимое в таймлайне', () => {
  const got = readDigest(run, db, INDEXED, { limit: 8, chars: 400 });
  assert.equal(got.entryCount, 3);
  assert.equal(got.entries.length, 2);
});

test('сессия после contextify ingest резолвится по имени файла', () => {
  const got = readDigest(run, db, CLI_INGESTED, { limit: 8, chars: 400 });
  assert.equal(got.ok, true, got.reason);
  assert.equal(got.transcriptId, 'T-CLI');
  assert.equal(got.entries[0].content, 'кэш config-service');
});

test('записи приходят свежими вперёд, скрытые из таймлайна не попадают', () => {
  const got = readDigest(run, db, INDEXED, { limit: 8, chars: 400 });
  assert.deepEqual(got.entries.map((e) => e.id), ['e2', 'e1']);
});

test('limit режет историю', () => {
  assert.equal(readDigest(run, db, INDEXED, { limit: 1, chars: 400 }).entries.length, 1);
});

test('несуществующая сессия не выдаётся за пустую', () => {
  const got = readDigest(run, db, '00000000-0000-0000-0000-000000000000', {});
  assert.equal(got.ok, false);
  assert.match(got.reason, /не проиндексирован/);
});

test('сломанный транскрипт объясняет причину и отдаёт свой id', () => {
  const got = readDigest(run, db, 'broken-1', {});
  assert.equal(got.ok, false);
  assert.equal(got.transcriptId, 'T-BROKEN');
  assert.match(got.reason, /Invalid transcript format/);
});

test('readMany резолвит оба ключа за один вызов', () => {
  const got = readMany(run, db, [INDEXED, CLI_INGESTED, 'нет-такой'], { limit: 2, chars: 400 });
  assert.equal(got.get(INDEXED).transcriptId, 'T-OK');
  assert.equal(got.get(CLI_INGESTED).transcriptId, 'T-CLI');
  assert.equal(got.get('нет-такой').ok, false);
});

test('find находит по всем транскриптам и приносит ветку с cwd', () => {
  const got = searchEntries(run, db, ftsQuery('config-service'), { limit: 5 });
  assert.equal(got.ok, true);
  assert.equal(got.results.length, 1);
  assert.equal(got.results[0].branch, 'stage');
  assert.equal(got.results[0].transcript_id, 'T-CLI');
});

test('search внутри одной сессии не выходит за её пределы', () => {
  const got = searchEntries(run, db, ftsQuery('config-service'), { transcriptId: 'T-OK', limit: 5 });
  assert.deepEqual(got.results, []);
});

test('запрос с кавычкой не ломает SQL и ничего не находит', () => {
  const got = searchEntries(run, db, ftsQuery("x' or '1'='1"), { limit: 5 });
  assert.equal(got.ok, true);
  assert.deepEqual(got.results, []);
});

test('entry отдаёт запись целиком, без обрезки на 2 КБ', () => {
  const got = readEntry(run, db, 'e2');
  assert.equal(got.ok, true, got.reason);
  assert.equal(got.entry.text, 'влит bonuses!874');
  assert.equal(got.entry.len, got.entry.text.length);
  assert.equal(got.entry.session_id, INDEXED);
  assert.equal(got.entry.project, 'GolandProjects');
});

test('entry по несуществующему id объясняет, а не отдаёт пустоту', () => {
  const got = readEntry(run, db, 'e404');
  assert.equal(got.ok, false);
  assert.match(got.reason, /нет в базе/);
});

test('entry с кавычкой в id отсекается до обращения к базе', () => {
  const got = readEntry(() => {
    throw new Error('sqlite не должен вызываться');
  }, db, "e1' or '1'='1");
  assert.equal(got.reason, 'некорректный entry_id');
});

test('context отдаёт окно вокруг записи и помечает якорь', () => {
  const got = readContext(run, db, 'e1', { before: 5, after: 5 });
  assert.equal(got.ok, true, got.reason);
  const ids = got.context.entries.map((e) => e.id);
  assert.deepEqual(ids, ['e1', 'e2'], 'скрытая из таймлайна e3 в окно не входит');
  assert.equal(got.context.entries.find((e) => e.id === 'e1').anchor, 1);
});

test('context держит границы окна', () => {
  const got = readContext(run, db, 'e2', { before: 0, after: 0 });
  assert.deepEqual(got.context.entries.map((e) => e.id), ['e2']);
});

test('запись адресуется по transcript_id, когда provider_session_id пуст', () => {
  const got = readDigest(run, db, 'T-CLI', { limit: 8, chars: 400 });
  assert.equal(got.ok, true, got.reason);
  assert.equal(got.transcriptId, 'T-CLI');
});

test('нечитаемый файл на диске не мешает объяснить причину', () => {
  const got = readDigest(run, db, 'broken-1', {});
  assert.equal(got.ok, false);
  assert.equal(typeof got.unindexedMb, 'number');
  assert.match(got.reason, /МБ на диске/);
});
