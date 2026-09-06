import { isSafeId, digestSql, digestManySql, searchSql, prefixChars } from './sql.js';

function query(run, dbPath, sql) {
  const res = run('sqlite3', ['-readonly', dbPath, sql]);
  if (!res.ok) {
    const msg = String(res.stderr ?? '').trim() || 'sqlite3 недоступен';
    return { ok: false, reason: msg.slice(0, 200) };
  }
  const out = String(res.stdout ?? '').trim();
  if (!out) return { ok: true, value: null };
  try {
    return { ok: true, value: JSON.parse(out) };
  } catch {
    return { ok: false, reason: 'нечитаемый ответ sqlite3' };
  }
}

function shapeEntry(row, chars) {
  return {
    id: row.id ?? null,
    kind: row.kind ?? null,
    timestamp: row.ts ?? null,
    content: row.text ?? '',
    contentFullSize: row.len ?? String(row.text ?? '').length,
    contentTruncated: (row.len ?? 0) > prefixChars(chars),
  };
}

function interpret(row, chars) {
  if (!row || row.transcript_id == null) {
    return { ok: false, reason: 'транскрипт для этой сессии ещё не проиндексирован' };
  }
  const entryCount = row.entry_count ?? 0;
  if (entryCount === 0) {
    const megabytes = Math.round(((row.file_size ?? 0) / 1048576) * 10) / 10;
    const detail = String(row.last_error ?? '').trim();
    const reason = detail
      ? `Contextify не смог проиндексировать сессию (файл ${megabytes} МБ): ${detail}`
      : `сессия ещё не проиндексирована Contextify (файл ${megabytes} МБ, записей 0)`;
    return { ok: false, transcriptId: row.transcript_id, reason };
  }
  const raw = Array.isArray(row.entries) ? row.entries : [];
  return {
    ok: true,
    transcriptId: row.transcript_id,
    entryCount,
    lastTs: row.last_ts ?? null,
    entries: raw.map((e) => shapeEntry(e, chars)),
  };
}

export function readDigest(run, dbPath, sessionId, { limit = 8, chars = 400 } = {}) {
  if (!isSafeId(sessionId)) return { ok: false, reason: 'некорректный session_id' };
  const got = query(run, dbPath, digestSql(sessionId, limit, chars));
  if (!got.ok) return got;
  return interpret(got.value, chars);
}

export function readMany(run, dbPath, sessionIds, { limit = 0, chars = 400 } = {}) {
  const safe = sessionIds.filter(isSafeId);
  const byId = new Map();
  for (const id of sessionIds) {
    if (!isSafeId(id)) byId.set(id, { ok: false, reason: 'некорректный session_id' });
  }
  if (!safe.length) return byId;

  const got = query(run, dbPath, digestManySql(safe, limit, chars));
  if (!got.ok) {
    for (const id of safe) byId.set(id, { ok: false, reason: got.reason });
    return byId;
  }
  const rows = Array.isArray(got.value) ? got.value : [];
  for (const row of rows) byId.set(row.session_id, interpret(row, chars));
  for (const id of safe) {
    if (!byId.has(id)) byId.set(id, { ok: false, reason: 'транскрипт для этой сессии ещё не проиндексирован' });
  }
  return byId;
}

export function searchEntries(run, dbPath, match, opts = {}) {
  if (opts.transcriptId && !isSafeId(opts.transcriptId)) {
    return { ok: false, reason: 'некорректный transcript_id' };
  }
  const got = query(run, dbPath, searchSql(match, opts));
  if (!got.ok) return got;
  return { ok: true, results: Array.isArray(got.value) ? got.value : [] };
}
