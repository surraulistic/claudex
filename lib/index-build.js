import { mkdirSync, statSync, rmSync } from 'node:fs';
import { dirname } from 'node:path';

export const CASS_DB = process.env.CLAUDEX_CASS_DB ||
  `${process.env.HOME}/Library/Application Support/com.coding-agent-search.coding-agent-search/agent_search.db`;

// Своя база, а не запись в чужую: cass владеет своей и может перестраивать её в
// любой момент. Позавчерашний урок с Contextify — два писателя в один файл.
export const SCHEMA = `
create table if not exists meta (k text primary key, v text);
create table if not exists conv (
  id integer primary key, agent text, session_id text, source_path text,
  workspace text, title text, started_at integer, ended_at integer
);
create index if not exists conv_session on conv(session_id);
create index if not exists conv_path on conv(source_path);
create table if not exists msg (
  id integer primary key, conv_id integer, idx integer,
  role text, created_at integer, len integer, is_tool integer not null default 0
);
create index if not exists msg_conv on msg(conv_id, is_tool, created_at desc, id desc);
create virtual table if not exists msg_fts using fts5(
  content, tokenize='unicode61 remove_diacritics 2'
);`;

// Идентификатор сессии — последние 36 символов имени файла: у Claude Code это
// весь basename, у Codex — хвост после rollout-<метка времени>-.
const SESSION_ID = `
  case when c.source_path like '%.jsonl'
    then substr(replace(c.source_path,'.jsonl',''), length(replace(c.source_path,'.jsonl','')) - 35)
    else null end`;

// Роли cass не совпадают с нашими: agent — это assistant, developer и tool
// в ленте разговора не участвуют, но выбрасывать их нельзя, иначе idx поплывёт.
const ROLE = `case m.role when 'agent' then 'assistant' else m.role end`;

// 148 тысяч записей из 215 — заглушки вызовов инструментов с ролью assistant.
// Отличить их можно только по тексту, а тянуть текст на каждом запросе дорого,
// поэтому признак считается один раз при сборке.
const IS_TOOL = `case when m.content like '[Tool:%' or m.role in ('tool','developer') then 1 else 0 end`;

export function refreshSql(watermark) {
  return `
attach database 'file:${CASS_DB}?mode=ro' as cass;
begin;
insert or replace into conv (id, agent, session_id, source_path, workspace, title, started_at, ended_at)
select c.id, a.name, ${SESSION_ID}, c.source_path, w.path, c.title, c.started_at, c.ended_at
from cass.conversations c
join cass.agents a on a.id = c.agent_id
left join cass.workspaces w on w.id = c.workspace_id;

insert into msg (id, conv_id, idx, role, created_at, len, is_tool)
select m.id, m.conversation_id, m.idx, ${ROLE}, m.created_at, length(m.content), ${IS_TOOL}
from cass.messages m where m.id > ${watermark};

insert into msg_fts (rowid, content)
select m.id, m.content from cass.messages m where m.id > ${watermark};

insert or replace into meta (k, v)
select 'watermark', coalesce(max(id), ${watermark}) from msg;
commit;
detach database cass;`;
}

export function statOr(path, fallback = null) {
  try {
    return statSync(path).size;
  } catch {
    return fallback;
  }
}

function scalar(run, dbPath, sql) {
  const res = run('sqlite3', ['-readonly', dbPath, sql]);
  return res.ok ? String(res.stdout ?? '').trim() : null;
}

// Инкрементально по watermark: пересобирать 215 тысяч записей ради последних
// двадцати незачем — полный проход стоит 23 секунды, догон почти ничего.
export function buildIndex(run, dbPath, { full = false } = {}) {
  if (statOr(CASS_DB) === null) {
    return { ok: false, reason: `базы cass нет по пути ${CASS_DB} — выполните \`cass index --full\`` };
  }
  mkdirSync(dirname(dbPath), { recursive: true });
  if (full) for (const suffix of ['', '-wal', '-shm']) rmSync(dbPath + suffix, { force: true });

  const created = run('sqlite3', [dbPath, SCHEMA]);
  if (!created.ok) return { ok: false, reason: String(created.stderr ?? '').slice(0, 200) };

  const before = Number.parseInt(scalar(run, dbPath, "select coalesce(v,0) from meta where k='watermark';") || '0', 10) || 0;
  const started = Date.now();
  const res = run('sqlite3', [dbPath, refreshSql(before)], { timeoutMs: 900000 });
  if (!res.ok) return { ok: false, reason: String(res.stderr ?? '').slice(0, 300) };

  const stats = {
    index: dbPath,
    source: CASS_DB,
    conversations: Number(scalar(run, dbPath, 'select count(*) from conv;')),
    messages: Number(scalar(run, dbPath, 'select count(*) from msg;')),
    conversational: Number(scalar(run, dbPath, 'select count(*) from msg where is_tool = 0;')),
    added: Number(scalar(run, dbPath, `select count(*) from msg where id > ${before};`)),
    elapsed_ms: Date.now() - started,
  };
  return { ok: true, stats };
}
