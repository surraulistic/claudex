import { getAgent, listAgents, listTabs } from './herdr.js';

function describe(agents) {
  return agents.map((a) => a.label ?? a.alias ?? a.pane_id).join(', ') || 'нет';
}

// Herdr сам понимает и алиас, и pane_id; заголовок и cwd добираем списком —
// но только когда прямое обращение не нашло агента.
export function resolveTarget(run, target) {
  const direct = getAgent(run, target);
  if (direct.ok) {
    // Метка — основное имя панели в выводе, поэтому она не должна зависеть от
    // того, каким путём цель нашлась: agent get о вкладках не знает.
    const agent = direct.agent.label || !direct.agent.tab_id
      ? direct.agent
      : { ...direct.agent, label: listTabs(run).get(direct.agent.tab_id) ?? null };
    return { ok: true, agent, matched: 'herdr' };
  }
  if (direct.error?.code !== 'agent_not_found') return direct;

  const listed = listAgents(run, { withLabels: true });
  if (!listed.ok) return listed;

  const needle = String(target ?? '').toLowerCase();
  const has = (value) => String(value ?? '').toLowerCase().includes(needle);

  // find отдаёт session_id, и передать его обратно в claudex — первое, что
  // делает читатель. Живая панель с этой сессией лучше её же истории: у неё
  // есть хвост.
  const bySession = listed.agents.filter((a) => String(a.session_id ?? '').toLowerCase() === needle);
  if (bySession.length === 1) return { ok: true, agent: bySession[0], matched: 'session_id' };

  // Метка вкладки — то, чем панель зовут вслух; точное совпадение важнее подстроки.
  const byLabel = listed.agents.filter((a) => String(a.label ?? '').toLowerCase() === needle);
  if (byLabel.length === 1) return { ok: true, agent: byLabel[0], matched: 'label' };

  const byTitle = listed.agents.filter((a) => has(a.label) || has(a.title));
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
