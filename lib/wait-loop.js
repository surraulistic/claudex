import { waitAgent, classifyWaitError } from './herdr.js';

export const RETRY = { attempts: 4, pauseMs: 2000 };

function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

// herdr перезапускается, сокет моргает, вызов возвращает мусор — это не значит,
// что панель не дошла. Такие ответы пересматриваются, пока не выйдет отведённое
// время; исход «не знаем» отделён от исхода «не дождались».
export function waitSettled(run, target, { until, timeoutMs, retry = RETRY, sleep = sleepSync, now = Date.now } = {}) {
  const deadline = now() + timeoutMs;
  const log = [];
  let transient = 0;

  for (;;) {
    const left = deadline - now();
    if (left <= 0) return { ok: false, outcome: 'timeout', log };

    const got = waitAgent(run, target, { until, timeoutMs: left });
    if (got.ok) return { ok: true, outcome: 'settled', agent: got.agent, log };

    const kind = got.kind ?? classifyWaitError(got.error);
    log.push({ kind, message: String(got.error?.message ?? '').slice(0, 200) });

    if (kind === 'timeout') return { ok: false, outcome: 'timeout', log };
    if (kind === 'gone') return { ok: false, outcome: 'gone', log };

    transient += 1;
    if (transient >= retry.attempts) return { ok: false, outcome: 'herdr_error', log };
    sleep(Math.min(retry.pauseMs, Math.max(0, deadline - now())));
  }
}
