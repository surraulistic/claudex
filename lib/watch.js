import { resolveTarget } from './target.js';
import { buildDigest, DEFAULTS } from './digest.js';
import { waitSettled } from './wait-loop.js';

export const WATCH_DEFAULTS = { timeoutMs: 1800000, armMs: 120000 };

// Наблюдатель ничего не будит: он ждёт и выходит. Пробуждение — дело харнесса,
// который запустил его фоном. Исходы разделены намеренно: settled — панель
// дошла, timeout — не дошла, herdr_error — мы не знаем, и это не то же самое.
export function buildWatch(run, dbPath, target, opts = {}) {
  const timeoutMs = opts.timeoutMs ?? WATCH_DEFAULTS.timeoutMs;
  const started = Date.now();
  const deps = { sleep: opts.sleep, now: opts.now };

  const live = resolveTarget(run, target);
  if (!live.ok) return { ok: false, error: live.error };

  let agent = live.agent;
  let armed = null;
  const log = [];

  // Панель может ещё не начать: промпт долетает не мгновенно, и без этой фазы
  // ожидание завершится сразу же, отчитавшись о чужом — предыдущем — ходе.
  if (opts.armMs && agent.status !== 'working') {
    const start = waitSettled(run, agent.pane_id, { until: ['working'], timeoutMs: opts.armMs, ...deps });
    armed = start.ok;
    log.push(...start.log.map((l) => ({ phase: 'arm', ...l })));
    if (start.ok) agent = { ...start.agent, label: start.agent.label ?? agent.label };
  } else if (agent.status === 'working') {
    armed = true;
  }

  let outcome = 'settled';
  let waited = false;
  if (agent.status === 'working') {
    waited = true;
    const done = waitSettled(run, agent.pane_id, { timeoutMs, ...deps });
    outcome = done.outcome;
    log.push(...done.log.map((l) => ({ phase: 'settle', ...l })));
    if (done.ok) agent = { ...done.agent, label: done.agent.label ?? agent.label };
  } else if (armed === false) {
    outcome = 'never_started';
  }

  const digest = outcome === 'settled' || outcome === 'never_started'
    ? buildDigest(run, dbPath, agent.pane_id, {
      limit: opts.limit ?? DEFAULTS.limit,
      chars: opts.chars ?? DEFAULTS.chars,
      tailLines: opts.tailLines ?? DEFAULTS.tailLines,
    })
    : { ok: false, error: { message: `дайджест не собирался: исход ${outcome}` } };

  return {
    ok: true,
    outcome,
    watch: {
      target: agent.label ?? agent.alias ?? agent.pane_id,
      armed,
      waited,
      outcome,
      waited_ms: Date.now() - started,
      final_status: outcome === 'settled' ? agent.status : 'unknown',
      log: log.length ? log : undefined,
      digest: digest.ok ? digest.digest : null,
      error: digest.ok ? undefined : digest.error?.message,
    },
  };
}
