import { cutText, cleanTail, toIso } from './normalize.js';
import { readTail } from './herdr.js';
import { resolveTarget } from './target.js';
import { readDigest } from './store.js';
import { extractSignals } from './signals.js';
import { readGauges } from './gauge.js';

export const DEFAULTS = { limit: 8, chars: 400, tailLines: 12 };

function shapeEntries(entries, chars) {
  return entries.map((e) => {
    const cut = cutText(e.content, chars);
    return {
      id: e.id ?? null,
      ts: toIso(e.timestamp),
      role: e.kind ?? null,
      text: cut.text,
      chars: e.contentFullSize ?? String(e.content ?? '').length,
      truncated: cut.truncated || e.contentTruncated === true,
    };
  });
}

export function shapeHistory(resolved, chars) {
  if (!resolved.ok) {
    return {
      history: {
        transcript_id: resolved.transcriptId ?? null,
        entry_count: null,
        unindexed_mb: resolved.unindexedMb,
        reason: resolved.reason,
      },
      entries: [],
    };
  }
  return {
    history: {
      transcript_id: resolved.transcriptId,
      provider: 'claude.code',
      entry_count: resolved.entryCount,
      last_activity: toIso(resolved.lastTs),
      entries: shapeEntries(resolved.entries, chars),
    },
    entries: resolved.entries,
  };
}

export function shapeLive(agent, gauges = {}) {
  return {
    label: agent.label ?? null,
    kind: agent.kind ?? null,
    pane_id: agent.pane_id,
    session_id: agent.session_id,
    status: agent.status,
    context_pct: gauges.context_pct ?? null,
    limits: gauges.limits ?? null,
    title: agent.title,
    cwd: agent.cwd,
    focused: agent.focused,
  };
}

export function buildDigest(run, dbPath, target, opts = {}) {
  const limit = opts.limit ?? DEFAULTS.limit;
  const chars = opts.chars ?? DEFAULTS.chars;
  const maxTail = opts.tailLines ?? DEFAULTS.tailLines;

  const live = resolveTarget(run, target);
  if (!live.ok) {
    // Живой панели нет — но цель могла прийти из find, где ключом служит
    // session_id закрытой сессии или transcript_id строки без него.
    const closed = readDigest(run, dbPath, target, { limit, chars });
    if (!closed.ok && !closed.transcriptId) return { ok: false, error: live.error };
    const shapedClosed = shapeHistory(closed, chars);
    return {
      ok: true,
      digest: {
        target,
        alias: null,
        live: null,
        history: shapedClosed.history,
        tail: null,
        signals: extractSignals({ entries: shapedClosed.entries, rawTail: '' }),
      },
    };
  }
  const agent = live.agent;

  // Хвост читаем при любом статусе: --source recent отдаёт скроллбэк, а не поле
  // ввода, и на завершённой панели там лежит как раз итоговый ответ.
  let rawTail = '';
  let tail = null;
  const read = readTail(run, agent.pane_id, Math.max(maxTail * 3, 30), { status: agent.status });
  if (read.ok) {
    rawTail = read.raw;
    const cleaned = cleanTail(rawTail, maxTail);
    tail = cleaned.length ? cleaned : null;
  }

  const shaped = shapeHistory(readDigest(run, dbPath, agent.session_id, { limit, chars }), chars);

  return {
    ok: true,
    digest: {
      target: agent.label ?? agent.alias ?? agent.pane_id,
      alias: agent.alias,
      live: shapeLive(agent, readGauges(rawTail)),
      history: shaped.history,
      tail,
      signals: extractSignals({ entries: shaped.entries, rawTail }),
    },
  };
}
