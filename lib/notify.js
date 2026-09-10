import { promptAgent } from './herdr.js';

export const NOTIFY_RETRY = { attempts: 3, pauseMs: 3000 };

function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

export function summarize(delegate) {
  const d = delegate.digest ?? {};
  const bits = [`панель ${delegate.target}: ${delegate.outcome}`];
  if (delegate.final_status) bits.push(`статус ${delegate.final_status}`);
  const sig = d.signals ?? {};
  if (sig.mr?.length) bits.push(`MR ${sig.mr.slice(0, 4).join(' ')}`);
  if (sig.tickets?.length) bits.push(`тикеты ${sig.tickets.slice(0, 4).join(' ')}`);
  const last = (d.history?.entries ?? []).find((e) => e.role === 'assistant');
  if (last) bits.push(`последнее: ${last.text.slice(0, 220)}`);
  bits.push(`Проверьте: claudex ${delegate.target}. Не считайте задачу успешной без проверки.`);
  return bits.join('. ');
}

// Уведомление — последний шаг, и молчаливый провал здесь стоит дороже всего:
// ведущий не проснётся и не узнает почему. Поэтому ретраи и полный отчёт.
export function notify(run, target, text, { retry, sleep = sleepSync } = {}) {
  const plan = { ...NOTIFY_RETRY, ...(retry ?? {}) };
  const attempts = [];
  for (let i = 1; i <= plan.attempts; i += 1) {
    const sent = promptAgent(run, target, text);
    if (sent.ok) return { ok: true, attempts: i, log: attempts };
    attempts.push({ attempt: i, code: sent.error?.code ?? 'unknown', message: String(sent.error?.message ?? '').slice(0, 200) });
    if (i < plan.attempts) sleep(plan.pauseMs);
  }
  return { ok: false, attempts: plan.attempts, log: attempts };
}
