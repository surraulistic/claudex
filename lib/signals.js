import { tailLines, cutText } from './normalize.js';

const MR = /(?:[A-Za-z0-9._-][A-Za-z0-9._/-]*)?![0-9]{2,}/g;
const TICKET = /\b(?:SNEW|SD|BF)-\d+\b/g;
const BAR = /[█░]/u;
const REPO = /^\s*(\S+)\s{2,}/u;
const RUNNING = /⏺\s+Running\b.*/u;
const FOOTER = /⏵⏵/u;
const FOOTER_INTACT = /\(shift\+tab to cycle\)/u;

// Узкая панель ужимает две служебные строки, срезая id и в конце, и в середине
// списка: SD-6613 приезжает как SD-66, а такого тикета не существует. Обычный
// текст переносится по словам и такого не даёт, поэтому строгие правила — только
// для служебных строк: ужатый футер не читаем вовсе, у статус-бара отбрасываем
// совпадение, упёршееся в край или в многоточие.
function lineKind(line) {
  if (FOOTER.test(line)) return FOOTER_INTACT.test(line) ? 'footer' : 'squeezed';
  return BAR.test(line) ? 'bar' : 'text';
}

function collect(re, text, into, kind = 'entry') {
  if (kind === 'squeezed') return;
  const line = String(text ?? '');
  for (const m of line.matchAll(re)) {
    const after = line.slice(m.index + m[0].length);
    if (after.startsWith('…')) continue;
    if (kind === 'bar' && after === '') continue;
    into.push(m[0]);
  }
}

function dedupe(list, cap) {
  return [...new Set(list)].slice(0, cap);
}

export function extractSignals({ entries = [], rawTail = '', cap = 10 } = {}) {
  const lines = tailLines(rawTail);
  const mr = [];
  const tickets = [];

  for (const line of lines) {
    const kind = lineKind(line);
    collect(MR, line, mr, kind);
    collect(TICKET, line, tickets, kind);
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
