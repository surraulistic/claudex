import { promptAgent } from './herdr.js';
import { resolveTarget } from './target.js';
import { buildWatch, WATCH_DEFAULTS } from './watch.js';

export const DELEGATE_DEFAULTS = { armMs: WATCH_DEFAULTS.armMs, timeoutMs: WATCH_DEFAULTS.timeoutMs };

// Herdr отвергает отправку только в blocked-панель; working молча ставит промпт
// в очередь, и две задачи перемешиваются. Отказ — на нашей стороне.
export function buildDelegate(run, dbPath, target, task, opts = {}) {
  const live = resolveTarget(run, target);
  if (!live.ok) return { ok: false, error: live.error };

  const agent = live.agent;
  const name = agent.label ?? agent.alias ?? agent.pane_id;
  if (agent.status !== 'idle') {
    return {
      ok: false,
      error: {
        code: 'target_busy',
        message: `панель «${name}» в состоянии ${agent.status}, промпт встанет в очередь ` +
          'и перемешает задачи. Дождитесь idle или заведите свежего агента: ' +
          'herdr tab create + herdr agent start',
      },
    };
  }

  const sent = promptAgent(run, agent.pane_id, task);
  if (!sent.ok) return { ok: false, error: sent.error };
  if (opts.noWait) {
    return { ok: true, delegate: { target: name, sent: true, waited: false } };
  }

  const watched = buildWatch(run, dbPath, agent.pane_id, {
    ...opts,
    armMs: opts.armMs ?? DELEGATE_DEFAULTS.armMs,
  });
  if (!watched.ok) return watched;

  return {
    ok: true,
    timedOut: watched.timedOut,
    delegate: { target: name, sent: true, ...watched.watch },
  };
}
