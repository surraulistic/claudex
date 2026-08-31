import { listAgents } from './herdr.js';
import { resolveTranscript } from './contextify.js';
import { toIso } from './normalize.js';

export function buildSessions(run, dbPath, { cwd = null } = {}) {
  const listed = listAgents(run);
  if (!listed.ok) return { ok: false, error: listed.error };

  const agents = cwd ? listed.agents.filter((a) => a.cwd === cwd) : listed.agents;
  const aliased = [];
  const unaliased = [];

  for (const a of agents) {
    if (!a.alias) {
      unaliased.push({ pane_id: a.pane_id, title: a.title, status: a.status, cwd: a.cwd });
      continue;
    }
    const resolved = resolveTranscript(run, dbPath, a.session_id);
    aliased.push({
      alias: a.alias,
      pane_id: a.pane_id,
      session_id: a.session_id,
      status: a.status,
      title: a.title,
      cwd: a.cwd,
      focused: a.focused,
      transcript_id: resolved.transcriptId ?? null,
      entry_count: resolved.ok ? resolved.entryCount : 0,
      last_activity: resolved.ok ? toIso(resolved.lastTs) : null,
      history_reason: resolved.ok ? undefined : resolved.reason,
    });
  }

  aliased.sort((x, y) => x.alias.localeCompare(y.alias));
  return { ok: true, sessions: { aliased, unaliased } };
}
