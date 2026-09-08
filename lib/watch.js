import { waitAgent } from './herdr.js';
import { resolveTarget } from './target.js';
import { buildDigest, DEFAULTS } from './digest.js';

export const WATCH_DEFAULTS = { timeoutMs: 1800000 };

// Наблюдатель ничего не будит: он ждёт и выходит. Пробуждение — дело харнесса,
// который запустил его фоном (Claude Code и Codex будят агента на завершении
// процесса). Поэтому здесь нет ни уведомителей, ни сокетов.
export function buildWatch(run, dbPath, target, opts = {}) {
  const timeoutMs = opts.timeoutMs ?? WATCH_DEFAULTS.timeoutMs;
  const started = Date.now();

  const live = resolveTarget(run, target);
  if (!live.ok) return { ok: false, error: live.error };

  let waited = false;
  let timedOut = false;
  let agent = live.agent;

  if (agent.status === 'working') {
    waited = true;
    const done = waitAgent(run, agent.pane_id, { timeoutMs });
    if (done.ok) {
      agent = done.agent;
    } else {
      timedOut = true;
    }
  }

  const digest = buildDigest(run, dbPath, agent.pane_id, {
    limit: opts.limit ?? DEFAULTS.limit,
    chars: opts.chars ?? DEFAULTS.chars,
    tailLines: opts.tailLines ?? DEFAULTS.tailLines,
  });

  return {
    ok: true,
    timedOut,
    watch: {
      target: agent.label ?? agent.alias ?? agent.pane_id,
      waited,
      timed_out: timedOut,
      waited_ms: Date.now() - started,
      final_status: timedOut ? 'unknown' : agent.status,
      digest: digest.ok ? digest.digest : null,
      error: digest.ok ? undefined : digest.error?.message,
    },
  };
}
