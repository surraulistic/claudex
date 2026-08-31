const SAFE_ID = /^[A-Za-z0-9._-]{1,128}$/;

function parseJsonRows(stdout) {
  const out = String(stdout ?? '').trim();
  if (!out) return [];
  return JSON.parse(out);
}

export function resolveTranscript(run, dbPath, sessionId) {
  if (!SAFE_ID.test(String(sessionId ?? ''))) {
    return { ok: false, reason: 'некорректный session_id' };
  }
  const sql =
    'select t.id as id, t.file_size as file_size, t.last_error as last_error,' +
    " (select count(*) from transcript_entries e where e.transcript_id = t.id) as entry_count," +
    ' (select max(timestamp) from transcript_entries e where e.transcript_id = t.id) as last_ts' +
    ' from transcripts t' +
    ` where t.provider_session_id = '${sessionId}' limit 1;`;
  const res = run('sqlite3', ['-readonly', '-json', dbPath, sql]);
  if (!res.ok) {
    const msg = String(res.stderr ?? '').trim() || 'sqlite3 недоступен';
    return { ok: false, reason: msg.slice(0, 200) };
  }
  let rows;
  try {
    rows = parseJsonRows(res.stdout);
  } catch {
    return { ok: false, reason: 'нечитаемый ответ sqlite3' };
  }
  if (!rows.length) {
    return { ok: false, reason: 'транскрипт для этой сессии ещё не проиндексирован' };
  }
  const row = rows[0];
  const entryCount = row.entry_count ?? 0;
  if (entryCount === 0) {
    const megabytes = Math.round(((row.file_size ?? 0) / 1048576) * 10) / 10;
    const detail = String(row.last_error ?? '').trim();
    const reason = detail
      ? `Contextify не смог проиндексировать сессию (файл ${megabytes} МБ): ${detail}`
      : `сессия ещё не проиндексирована Contextify (файл ${megabytes} МБ, записей 0)`;
    return { ok: false, transcriptId: row.id, reason };
  }
  return {
    ok: true,
    transcriptId: row.id,
    entryCount,
    lastTs: row.last_ts ?? null,
  };
}

function runContextify(run, dbPath, args) {
  const res = run('contextify', ['--db-path', dbPath, ...args]);
  if (!res.ok) {
    const msg = String(res.stderr ?? '').trim() || `contextify завершился с кодом ${res.code}`;
    return { ok: false, reason: msg.slice(0, 300) };
  }
  try {
    return { ok: true, body: JSON.parse(res.stdout) };
  } catch {
    return { ok: false, reason: 'нечитаемый JSON от contextify' };
  }
}

export function activity(run, dbPath, transcriptId, limit) {
  const got = runContextify(run, dbPath, [
    '--transcript-id', transcriptId,
    '--limit', String(limit),
    '--json', 'activity',
  ]);
  if (!got.ok) return got;
  const data = Array.isArray(got.body?.data) ? got.body.data : [];
  return { ok: true, entries: data.map((row) => row.entry).filter(Boolean) };
}

export function search(run, dbPath, transcriptId, query, limit, snippetTokens) {
  const got = runContextify(run, dbPath, [
    '--transcript-id', transcriptId,
    '--limit', String(limit),
    '--snippet-tokens', String(snippetTokens),
    '--json', 'search', query,
  ]);
  if (!got.ok) return got;
  const data = Array.isArray(got.body?.data) ? got.body.data : [];
  return { ok: true, results: data };
}
