import { listAgents, readTail } from './herdr.js';
import { readGauges } from './gauge.js';
import { readMany } from './store.js';
import { toIso } from './normalize.js';

const ACTIVE_FIRST = { working: 0, done: 1, idle: 2 };

export function rank(a, b) {
  const byStatus = (ACTIVE_FIRST[a.status] ?? 3) - (ACTIVE_FIRST[b.status] ?? 3);
  if (byStatus !== 0) return byStatus;
  const byActivity = (b.last_ts ?? 0) - (a.last_ts ?? 0);
  if (byActivity !== 0) return byActivity;
  return String(a.pane_id).localeCompare(String(b.pane_id));
}

export function buildPanes(run, dbPath, { cwd = null } = {}) {
  const listed = listAgents(run, { withLabels: true });
  if (!listed.ok) return { ok: false, error: listed.error };

  const agents = cwd ? listed.agents.filter((a) => a.cwd === cwd) : listed.agents;
  const history = readMany(run, dbPath, agents.map((a) => a.session_id).filter(Boolean));

  const panes = agents.map((a) => {
    const h = history.get(a.session_id) ?? { ok: false, reason: 'у панели нет session_id' };
    const read = readTail(run, a.pane_id, 40, { status: a.status });
    const gauges = read.ok ? readGauges(read.raw) : {};
    return {
      target: a.label ?? a.alias ?? a.pane_id,
      label: a.label,
      kind: a.kind,
      alias: a.alias,
      pane_id: a.pane_id,
      context_pct: gauges.context_pct ?? null,
      limits: gauges.limits ?? null,
      session_id: a.session_id,
      status: a.status,
      title: a.title,
      cwd: a.cwd,
      focused: a.focused,
      transcript_id: h.transcriptId ?? null,
      entry_count: h.ok ? h.entryCount : null,
      last_activity: h.ok ? toIso(h.lastTs) : null,
      unindexed_mb: h.unindexedMb,
      history_reason: h.ok ? undefined : h.reason,
      last_ts: h.ok ? h.lastTs : null,
    };
  });

  panes.sort(rank);
  for (const p of panes) delete p.last_ts;
  return { ok: true, panes };
}
