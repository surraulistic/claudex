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

// Цель приходит и как session_id из herdr, и как id разговора из выдачи find.
function convIdFor(target) {
  return `coalesce(` +
    `(select id from conv where session_id = ${target} limit 1),` +
    `(select id from conv where cast(id as text) = ${target} limit 1))`;
}

// Время cass в миллисекундах, наше — в секундах: toIso ждёт unix-секунды.
const TS = 'm.created_at / 1000';

export function prefixChars(chars) {
  return Math.max(4096, chars * 4);
}

function entriesJson(convExpr, limit, cut) {
  return `(select json_group_array(json_object(` +
    `'id',r.id,'kind',r.role,'ts',r.ts,'len',r.len,'text',r.text)) from (` +
    `select m.id id, m.role role, ${TS} ts, m.len len,` +
    ` substr((select content from msg_fts where rowid = m.id),1,${cut}) text` +
    ` from msg m where m.conv_id = ${convExpr} and m.is_tool = 0` +
    ` order by m.created_at desc, m.id desc limit ${limit}) r)`;
}

function convJson(convExpr, limit, cut) {
  return `json_object(` +
    `'transcript_id',(select id from conv where id = ${convExpr}),` +
    `'agent',(select agent from conv where id = ${convExpr}),` +
    `'source_path',(select source_path from conv where id = ${convExpr}),` +
    `'entry_count',(select count(*) from msg where conv_id = ${convExpr} and is_tool = 0),` +
    `'last_ts',(select max(created_at) / 1000 from msg where conv_id = ${convExpr}),` +
    `'entries',${entriesJson(convExpr, limit, cut)})`;
}

export function digestSql(sessionId, limit, chars) {
  const target = lit(sessionId);
  const conv = convIdFor(target);
  return `select ${convJson(conv, limit, prefixChars(chars))} where ${conv} is not null;`;
}

export function digestManySql(sessionIds, limit, chars) {
  const values = sessionIds.map((s) => `(${lit(s)})`).join(',');
  const cut = prefixChars(chars);
  return `with want(sid) as (values ${values}),` +
    ` tr as (select w.sid sid, ${convIdFor('w.sid')} id from want w)` +
    ` select json_group_array(json_patch(json_object('session_id',tr.sid),` +
    `case when tr.id is null then json_object('transcript_id',null,'entry_count',0)` +
    ` else ${convJson('tr.id', limit, cut)} end)) from tr;`;
}

export function searchSql(match, { convId = null, limit = 8, snippetTokens = 24, days = null } = {}) {
  const scope = convId ? ` and m.conv_id = ${lit(convId)}` : '';
  const since = days ? ` and m.created_at >= (strftime('%s','now') - ${days} * 86400) * 1000` : '';
  return `select json_group_array(json_object(` +
    `'id',id,'kind',kind,'ts',ts,'rank',rank,'session_id',session_id,` +
    `'transcript_id',transcript_id,'agent',agent,'cwd',cwd,'project',project,` +
    `'branch',branch,'text',text)) from (select m.id id, m.role kind,` +
    ` m.created_at / 1000 ts, f.rank rank,` +
    ` c.session_id session_id, c.id transcript_id, c.agent agent,` +
    ` c.workspace cwd, c.workspace project, null branch, f.snip text` +
    ` from (select rowid rid, rank, snippet(msg_fts,0,'','','…',${snippetTokens}) snip` +
    ` from msg_fts where msg_fts match ${lit(match)} order by rank limit ${limit * 4}) f` +
    ` join msg m on m.id = f.rid` +
    ` join conv c on c.id = m.conv_id` +
    ` where 1 = 1${scope}${since} order by f.rank limit ${limit});`;
}

export function entrySql(entryId) {
  const id = lit(entryId);
  return `select json_object('id',m.id,'kind',m.role,'ts',${TS},` +
    `'transcript_id',m.conv_id,'session_id',c.session_id,'agent',c.agent,` +
    `'cwd',c.workspace,'project',c.workspace,'branch',null,'len',m.len,` +
    `'text',(select content from msg_fts where rowid = m.id))` +
    ` from msg m join conv c on c.id = m.conv_id where m.id = ${id};`;
}

export function contextSql(entryId, before, after) {
  const id = lit(entryId);
  const anchor = `(select conv_id from msg where id = ${id})`;
  const pos = `(select count(*) from msg x where x.conv_id = ${anchor}` +
    ` and (x.created_at, x.id) <= ((select created_at from msg where id = ${id}),` +
    ` (select id from msg where id = ${id})))`;
  return `with ordered as (select m.id, m.role, ${TS} ts, m.len,` +
    ` row_number() over (order by m.created_at, m.id) rn` +
    ` from msg m where m.conv_id = ${anchor})` +
    ` select json_object('anchor',${id},'transcript_id',${anchor},'entries',` +
    `(select json_group_array(json_object('id',o.id,'kind',o.role,'ts',o.ts,'len',o.len,` +
    `'text',(select content from msg_fts where rowid = o.id),'anchor',o.id = ${id}))` +
    ` from (select * from ordered where rn between ${pos} - ${before} and ${pos} + ${after}` +
    ` order by rn) o)) where ${anchor} is not null;`;
}
