import { getAgent, listAgents } from './herdr.js';

function describe(agents) {
  return agents.map((a) => a.alias ?? a.pane_id).join(', ') || 'нет';
}

// Herdr сам понимает и алиас, и pane_id; заголовок и cwd добираем списком —
// но только когда прямое обращение не нашло агента.
export function resolveTarget(run, target) {
  const direct = getAgent(run, target);
  if (direct.ok) return { ok: true, agent: direct.agent, matched: 'herdr' };
  if (direct.error?.code !== 'agent_not_found') return direct;

  const listed = listAgents(run);
  if (!listed.ok) return listed;

  const needle = String(target ?? '').toLowerCase();
  const has = (value) => String(value ?? '').toLowerCase().includes(needle);
  const byTitle = listed.agents.filter((a) => has(a.title));
  const byCwd = listed.agents.filter((a) => has(a.cwd));
  const matched = byTitle.length ? 'title' : 'cwd';
  const hits = byTitle.length ? byTitle : byCwd;

  if (hits.length === 1) return { ok: true, agent: hits[0], matched };
  if (hits.length > 1) {
    return {
      ok: false,
      error: {
        code: 'ambiguous_target',
        message: `под «${target}» подходит несколько панелей: ${describe(hits)}`,
      },
    };
  }
  return {
    ok: false,
    error: {
      code: 'agent_not_found',
      message: `панель «${target}» не найдена. Живые цели: ${describe(listed.agents)}`,
    },
  };
}
