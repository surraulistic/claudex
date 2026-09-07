import { tailLines } from './normalize.js';

// Статусная строка Claude Code: <репо> <ветка> <модель> ██░░░49% 5h░░░░░7% 12:00 7d███░░57%
// Первый столбик без префикса — контекст, дальше именованные лимиты. При высоком
// заполнении Claude Code показывает вместо столбика текст «N% context used».
const BAR = /(5h|7d)?\s*[█░]{2,}\s*(\d+)%/g;
const SPELLED = /(\d+)%\s*context used/;

export function readGauges(rawTail) {
  const lines = tailLines(rawTail);
  const out = { context_pct: null, limits: null };

  for (const line of lines) {
    const spelled = line.match(SPELLED);
    if (spelled) out.context_pct = Number(spelled[1]);
    for (const m of line.matchAll(BAR)) {
      const [, name, value] = m;
      if (name) {
        out.limits = { ...(out.limits ?? {}), [name]: Number(value) };
      } else if (out.context_pct == null) {
        out.context_pct = Number(value);
      }
    }
  }
  return out;
}
