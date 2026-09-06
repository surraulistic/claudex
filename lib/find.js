import { cutText, toIso } from './normalize.js';

// Плоский список попаданий не отвечает на первый вопрос читателя — «что это за
// сессия». Группируем по сессии и подписываем каждую тем, что проверяемо:
// проект, каталог, ветка, окно активности. Ключ группы всегда адресуем командой
// `claudex <target>` — если provider_session_id пуст, целью служит transcript_id.
export function groupBySession(results, chars) {
  const groups = new Map();

  for (const r of results) {
    const target = r.session_id || r.transcript_id;
    if (!target) continue;
    if (!groups.has(target)) {
      groups.set(target, {
        target,
        session_id: r.session_id ?? null,
        transcript_id: r.transcript_id ?? null,
        project: r.project ?? null,
        cwd: r.cwd ?? null,
        branch: r.branch ?? null,
        first_hit: r.ts ?? null,
        last_hit: r.ts ?? null,
        hits: 0,
        entries: [],
      });
    }
    const g = groups.get(target);
    g.hits += 1;
    if (r.ts != null) {
      if (g.first_hit == null || r.ts < g.first_hit) g.first_hit = r.ts;
      if (g.last_hit == null || r.ts > g.last_hit) g.last_hit = r.ts;
    }
    if (g.cwd == null) g.cwd = r.cwd ?? null;
    if (g.branch == null) g.branch = r.branch ?? null;
    g.entries.push({
      id: r.id ?? null,
      ts: toIso(r.ts),
      role: r.kind ?? null,
      text: cutText(r.text, chars).text,
    });
  }

  return [...groups.values()]
    .sort((a, b) => (b.last_hit ?? 0) - (a.last_hit ?? 0))
    .map((g) => ({ ...g, first_hit: toIso(g.first_hit), last_hit: toIso(g.last_hit) }));
}
