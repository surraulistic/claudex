import { cutText, cleanTail, toIso } from './normalize.js';
import { getAgent, readTail } from './herdr.js';
import { resolveTranscript, activity } from './contextify.js';
import { extractSignals } from './signals.js';

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

export function buildDigest(run, dbPath, alias, opts = {}) {
  const limit = opts.limit ?? DEFAULTS.limit;
  const chars = opts.chars ?? DEFAULTS.chars;
  const maxTail = opts.tailLines ?? DEFAULTS.tailLines;

  const live = getAgent(run, alias);
  if (!live.ok) return { ok: false, error: live.error };

  const agent = live.agent;
  let rawTail = '';
  let tail = null;
  if (agent.status === 'working') {
    const read = readTail(run, alias, Math.max(maxTail * 3, 30));
    if (read.ok) {
      rawTail = read.raw;
      const cleaned = cleanTail(rawTail, maxTail);
      tail = cleaned.length ? cleaned : null;
    }
  }

  let history = null;
  let entries = [];
  const resolved = resolveTranscript(run, dbPath, agent.session_id);
  if (!resolved.ok) {
    history = {
      transcript_id: resolved.transcriptId ?? null,
      entry_count: 0,
      reason: resolved.reason,
    };
  } else {
    const act = activity(run, dbPath, resolved.transcriptId, limit);
    if (!act.ok) {
      history = { transcript_id: resolved.transcriptId, reason: act.reason };
    } else {
      entries = act.entries;
      history = {
        transcript_id: resolved.transcriptId,
        provider: 'claude.code',
        entry_count: resolved.entryCount,
        last_activity: toIso(resolved.lastTs),
        entries: shapeEntries(entries, chars),
      };
    }
  }

  return {
    ok: true,
    digest: {
      alias: agent.alias ?? alias,
      live: {
        pane_id: agent.pane_id,
        session_id: agent.session_id,
        status: agent.status,
        title: agent.title,
        cwd: agent.cwd,
        focused: agent.focused,
      },
      history,
      tail,
      signals: extractSignals({ entries, rawTail }),
    },
  };
}
