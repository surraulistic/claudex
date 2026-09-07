import { homedir } from 'node:os';
import { join } from 'node:path';
import {
  isSafeId, digestSql, digestManySql, searchSql, entrySql, contextSql, prefixChars,
} from './sql.js';

export const DEFAULT_INDEX = process.env.CLAUDEX_INDEX ||
  join(homedir(), '.claudex', 'index.db');

function query(run, dbPath, sql) {
  const res = run('sqlite3', ['-readonly', dbPath, sql]);
  if (!res.ok) {
    const msg = String(res.stderr ?? '').trim() || 'sqlite3 недоступен';
    const missing = /unable to open database|no such table/i.test(msg);
    return {
      ok: false,
      reason: missing
        ? 'индекс не собран — выполните `claudex index`'
        : msg.slice(0, 200),
    };
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
    return { ok: false, reason: 'сессии нет в индексе — возможно, он не пересобирался' };
  }
  const entryCount = row.entry_count ?? 0;
  if (entryCount === 0) {
    return {
      ok: false,
      transcriptId: row.transcript_id,
      reason: 'сессия проиндексирована, но разговорных записей в ней нет',
    };
  }
  const raw = Array.isArray(row.entries) ? row.entries : [];
  return {
    ok: true,
    transcriptId: row.transcript_id,
    agent: row.agent ?? null,
    entryCount,
    lastTs: row.last_ts ?? null,
    entries: raw.map((e) => shapeEntry(e, chars)),
  };
}

export function readDigest(run, dbPath, sessionId, { limit = 8, chars = 400 } = {}) {
  if (!isSafeId(sessionId)) return { ok: false, reason: 'некорректный идентификатор сессии' };
  const got = query(run, dbPath, digestSql(sessionId, limit, chars));
  if (!got.ok) return got;
  return interpret(got.value, chars);
}

export function readMany(run, dbPath, sessionIds, { limit = 0, chars = 400 } = {}) {
  const safe = sessionIds.filter(isSafeId);
  const byId = new Map();
  for (const id of sessionIds) {
    if (!isSafeId(id)) byId.set(id, { ok: false, reason: 'некорректный идентификатор сессии' });
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
    if (!byId.has(id)) byId.set(id, { ok: false, reason: 'сессии нет в индексе' });
  }
  return byId;
}

export function searchEntries(run, dbPath, match, opts = {}) {
  if (opts.convId != null && !isSafeId(String(opts.convId))) {
    return { ok: false, reason: 'некорректный идентификатор разговора' };
  }
  const got = query(run, dbPath, searchSql(match, opts));
  if (!got.ok) return got;
  return { ok: true, results: Array.isArray(got.value) ? got.value : [] };
}

export function readEntry(run, dbPath, entryId) {
  if (!isSafeId(entryId)) return { ok: false, reason: 'некорректный entry_id' };
  const got = query(run, dbPath, entrySql(entryId));
  if (!got.ok) return got;
  if (!got.value) return { ok: false, reason: `записи ${entryId} нет в индексе` };
  return { ok: true, entry: got.value };
}

export function readContext(run, dbPath, entryId, { before = 10, after = 20 } = {}) {
  if (!isSafeId(entryId)) return { ok: false, reason: 'некорректный entry_id' };
  const got = query(run, dbPath, contextSql(entryId, before, after));
  if (!got.ok) return got;
  if (!got.value) return { ok: false, reason: `записи ${entryId} нет в индексе` };
  return { ok: true, context: got.value };
}
