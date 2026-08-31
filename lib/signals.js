import { tailLines, cutText } from './normalize.js';

const MR = /(?:[A-Za-z0-9._-][A-Za-z0-9._/-]*)?![0-9]{2,}/g;
const TICKET = /\b(?:SNEW|SD|BF)-\d+\b/g;
const BAR = /[█░]/u;
const REPO = /^\s*(\S+)\s{2,}/u;
const RUNNING = /⏺\s+Running\b.*/u;

function collect(re, text, into) {
  for (const m of String(text ?? '').matchAll(re)) into.push(m[0]);
}

function dedupe(list, cap) {
  return [...new Set(list)].slice(0, cap);
}

export function extractSignals({ entries = [], rawTail = '', cap = 10 } = {}) {
  const lines = tailLines(rawTail);
  const mr = [];
  const tickets = [];

  for (const line of lines) {
    collect(MR, line, mr);
    collect(TICKET, line, tickets);
  }
  for (const entry of entries) {
    collect(MR, entry?.content, mr);
    collect(TICKET, entry?.content, tickets);
  }

  const barLine = lines.filter((l) => BAR.test(l)).pop() ?? '';
  const repoMatch = barLine.match(REPO);

  const runningLine = lines.filter((l) => RUNNING.test(l)).pop() ?? '';
  const runningMatch = runningLine.match(RUNNING);

  const lastUser = entries.find((e) => e?.kind === 'user');

  return {
    mr: dedupe(mr, cap),
    tickets: dedupe(tickets, cap),
    repo: repoMatch ? repoMatch[1].replace(/^…\/?/u, '') : null,
    last_user_prompt: lastUser ? cutText(lastUser.content, 200).text : null,
    current_tool_call: runningMatch ? cutText(runningMatch[0], 200).text : null,
  };
}
