const SAFE_ID = /^[A-Za-z0-9._-]{1,128}$/;

export function isSafeId(value) {
  return SAFE_ID.test(String(value ?? ''));
}

export function lit(value) {
  return `'${String(value ?? '').replace(/'/g, "''")}'`;
}

// Пользовательский текст в FTS5 — не выражение, а набор слов: любой токен,
// попавший в синтаксис MATCH нераскавыченным, роняет запрос синтаксической ошибкой.
export function ftsQuery(input, { raw = false } = {}) {
  const text = String(input ?? '').trim();
  if (!text) return null;
  if (raw) return text;
  const terms = text
    .split(/\s+/)
    .map((word) => {
      const prefix = word.endsWith('*');
      const core = (prefix ? word.slice(0, -1) : word).replace(/"/g, '');
      if (!core) return null;
      return `"${core}"${prefix ? '*' : ''}`;
    })
    .filter(Boolean);
  return terms.length ? terms.join(' AND ') : null;
}

const UUID = "'????????-????-????-????-????????????'";

// Contextify заполняет provider_session_id только когда индексирует приложение;
// после `contextify ingest` из CLI поле остаётся NULL (514 строк из 1278 на
// 2026-09-06), и join по нему теряет сессию целиком. Имя файла — второй ключ:
// транскрипт сессии всегда лежит как <session-id>.jsonl. Сверку по имени
// включаем только для UUID, иначе journal.jsonl из 24 проектов схлопнется в одну.
function transcriptIdFor(sid) {
  const exact = `(select t.id from transcripts t where t.provider = 'claude.code'` +
    ` and t.provider_session_id = ${sid}` +
    " and t.provider_session_id is not null and t.provider_session_id <> '' limit 1)";
  const byFile = `(select t.id from transcripts t where t.provider = 'claude.code'` +
    ` and ${sid} glob ${UUID} and t.file_path like '%/' || ${sid} || '.jsonl'` +
    ' order by t.updated_at desc limit 1)';
  return `coalesce(${exact}, ${byFile})`;
}

function entryJson(alias) {
  return `json_object('id',${alias}.id,'kind',${alias}.kind,'ts',${alias}.timestamp,` +
    `'len',${alias}.len,'text',${alias}.text)`;
}

// Префикс берём с запасом: cutText схлопывает пробелы, и обрезка ровно по chars
// выдала бы truncated:false там, где текст на деле обрезан.
export function prefixChars(chars) {
  return Math.max(4096, chars * 4);
}

function entriesJson(idExpr, limit, cut) {
  return `(select json_group_array(${entryJson('r')}) from (` +
    `select id,kind,timestamp,length(content) len,substr(content,1,${cut}) text` +
    ` from transcript_entries where transcript_id=${idExpr} and display_in_timeline=1` +
    ` order by timestamp desc, rowid desc limit ${limit}) r)`;
}

export function digestSql(sessionId, limit, chars) {
  const cut = prefixChars(chars);
  return `with tr as (select ${transcriptIdFor(lit(sessionId))} id)` +
    ` select json_object(` +
    `'transcript_id',(select id from tr),` +
    `'file_size',(select file_size from transcripts where id=(select id from tr)),` +
    `'last_error',(select last_error from transcripts where id=(select id from tr)),` +
    `'entry_count',(select count(*) from transcript_entries e where e.transcript_id=(select id from tr)),` +
    `'last_ts',(select max(timestamp) from transcript_entries e where e.transcript_id=(select id from tr)),` +
    `'entries',${entriesJson('(select id from tr)', limit, cut)}` +
    `) where (select id from tr) is not null;`;
}

export function digestManySql(sessionIds, limit, chars) {
  const values = sessionIds.map((s) => `(${lit(s)})`).join(',');
  const cut = prefixChars(chars);
  return `with want(sid) as (values ${values}),` +
    ` tr as (select w.sid sid, ${transcriptIdFor('w.sid')} id from want w)` +
    ` select json_group_array(json_object(` +
    `'session_id',tr.sid,'transcript_id',tr.id,` +
    `'file_size',(select file_size from transcripts where id=tr.id),` +
    `'last_error',(select last_error from transcripts where id=tr.id),` +
    `'entry_count',(select count(*) from transcript_entries e where e.transcript_id=tr.id),` +
    `'last_ts',(select max(timestamp) from transcript_entries e where e.transcript_id=tr.id),` +
    `'entries',${entriesJson('tr.id', limit, cut)}` +
    `)) from tr;`;
}

export function searchSql(match, { transcriptId = null, limit = 8, snippetTokens = 18, days = null } = {}) {
  const scope = transcriptId ? ` and e.transcript_id = ${lit(transcriptId)}` : '';
  const since = days ? ` and e.timestamp >= strftime('%s','now') - ${days} * 86400` : '';
  return `select json_group_array(json_object(` +
    `'id',e.id,'kind',e.kind,'ts',e.timestamp,'rank',f.rank,` +
    `'session_id',t.provider_session_id,'transcript_id',t.id,` +
    `'cwd',e.cwd,'branch',e.git_branch,'project',p.name,'text',f.snip)) from (` +
    `select e.id id, e.kind kind, e.timestamp timestamp, e.transcript_id transcript_id,` +
    ` e.cwd cwd, e.git_branch git_branch, rank rank,` +
    ` snippet(transcript_entries_fts,0,'','','…',${snippetTokens}) snip` +
    ` from transcript_entries_fts` +
    ` join transcript_entries e on e.id = transcript_entries_fts.entry_id` +
    ` where transcript_entries_fts match ${lit(match)} and e.is_sidechain = 0${scope}${since}` +
    ` order by rank limit ${limit}) f` +
    ` join transcript_entries e on e.id = f.id` +
    ` join transcripts t on t.id = f.transcript_id` +
    ` join projects p on p.id = t.project_id` +
    ` where t.file_path not like '%/subagents/%';`;
}
