import { promptAgent } from './herdr.js';
import { resolveTarget } from './target.js';
import { buildWatch, WATCH_DEFAULTS } from './watch.js';
import { acquire } from './lock.js';
import { notify, summarize } from './notify.js';

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

  // Наблюдение начинается в том же процессе сразу после отправки, поэтому
  // корреляция задачи и завершения не нуждается в отдельном идентификаторе:
  // окна между «послали» и «смотрим» просто нет.
  const lock = opts.noWait ? { ok: true, release: () => {} } : acquire(name, opts.lock ?? {});
  if (!lock.ok) return { ok: false, error: { code: 'already_watched', message: lock.reason } };

  try {
    const sent = promptAgent(run, agent.pane_id, task);
    if (!sent.ok) return { ok: false, error: sent.error };
    if (opts.noWait) {
      return { ok: true, outcome: 'sent', delegate: { target: name, sent: true, waited: false, outcome: 'sent' } };
    }

    const watched = buildWatch(run, dbPath, agent.pane_id, {
      ...opts,
      holdsLock: true,
      armMs: opts.armMs ?? DELEGATE_DEFAULTS.armMs,
    });
    if (!watched.ok) return watched;

    const delegate = { target: name, sent: true, ...watched.watch };
    if (opts.notifyTarget) {
      const said = notify(run, opts.notifyTarget, summarize(delegate), { retry: opts.notify, sleep: opts.sleep });
      delegate.notified = { target: opts.notifyTarget, ok: said.ok, attempts: said.attempts, log: said.log.length ? said.log : undefined };
    }
    return { ok: true, outcome: watched.outcome, delegate };
  } finally {
    lock.release();
  }
}
