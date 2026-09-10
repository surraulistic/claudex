import { listAgents } from './herdr.js';
import { readMany } from './store.js';
import { cleanTail, toIso } from './normalize.js';
import { extractSignals } from './signals.js';
import { readGauges } from './gauge.js';
import { listHeld } from './lock.js';
import { rank } from './sessions.js';

export const BRIEF_DEFAULTS = { limit: 4, chars: 400, tailLines: 8 };

function tailJob(agent, maxTail) {
  return ['herdr', [
    'agent', 'read', agent.pane_id,
    '--source', agent.status === 'working' ? 'visible' : 'recent',
    '--lines', String(Math.max(maxTail * 3, 30)),
    '--format', 'text',
  ]];
}

// Один процесс на все панели: старт node и вызов sqlite амортизируются, хвосты
// читаются параллельно. Отдельный claudex на каждую панель стоит вдесятеро.
export async function buildBrief(run, runAll, dbPath, opts = {}) {
  const limit = opts.limit ?? BRIEF_DEFAULTS.limit;
  const chars = opts.chars ?? BRIEF_DEFAULTS.chars;
  const maxTail = opts.tailLines ?? BRIEF_DEFAULTS.tailLines;

  const listed = listAgents(run, { withLabels: true });
  const held = listHeld(opts.lock ?? {});
  if (!listed.ok) return { ok: false, error: listed.error };

  const agents = opts.cwd ? listed.agents.filter((a) => a.cwd === opts.cwd) : listed.agents;
  if (!agents.length) return { ok: true, brief: { generated_at: toIso(Math.floor(Date.now() / 1000)), panes: [] } };

  const history = readMany(run, dbPath, agents.map((a) => a.session_id).filter(Boolean), { limit, chars });
  const tails = await runAll(agents.map((a) => tailJob(a, maxTail)));

  const panes = agents.map((agent, i) => {
    const h = history.get(agent.session_id) ?? { ok: false, reason: 'у панели нет session_id' };
    const rawTail = tails[i]?.ok ? tails[i].stdout : '';
    const cleaned = cleanTail(rawTail, maxTail);
    const gauges = readGauges(rawTail);
    return {
      target: agent.label ?? agent.alias ?? agent.pane_id,
      label: agent.label,
      kind: agent.kind,
      alias: agent.alias,
      pane_id: agent.pane_id,
      status: agent.status,
      context_pct: gauges.context_pct ?? null,
      limits: gauges.limits ?? null,
      watched: held.has(agent.label ?? agent.alias ?? agent.pane_id),
      title: agent.title,
      cwd: agent.cwd,
      focused: agent.focused,
      history: {
        transcript_id: h.transcriptId ?? null,
        entry_count: h.ok ? h.entryCount : null,
        last_activity: h.ok ? toIso(h.lastTs) : null,
        unindexed_mb: h.unindexedMb,
        reason: h.ok ? undefined : h.reason,
      },
      tail: cleaned.length ? cleaned : null,
      signals: extractSignals({ entries: h.ok ? h.entries : [], rawTail }),
      last_ts: h.ok ? h.lastTs : null,
    };
  });

  panes.sort(rank);
  for (const p of panes) delete p.last_ts;
  return { ok: true, brief: { generated_at: toIso(Math.floor(Date.now() / 1000)), panes } };
}
