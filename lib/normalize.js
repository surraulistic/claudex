const ANSI = /\u001B\[[0-9;?]*[ -/]*[@-~]/g;
const RULE_ONLY = /^[\s─-╿]*$/u;
const FOOTER = /[█░]|⏵⏵|auto mode on/u;
const PROMPT_ONLY = /^\s*❯\s*$/u;

export function stripAnsi(text) {
  return String(text ?? '').replace(ANSI, '');
}

export function toIso(unixSeconds) {
  if (typeof unixSeconds !== 'number' || !Number.isFinite(unixSeconds)) return null;
  const d = new Date(unixSeconds * 1000);
  const pad = (n) => String(Math.abs(n)).padStart(2, '0');
  const off = -d.getTimezoneOffset();
  const sign = off >= 0 ? '+' : '-';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}` +
    `T${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}` +
    `${sign}${pad(Math.floor(Math.abs(off) / 60))}:${pad(Math.abs(off) % 60)}`;
}

export function cutText(text, maxChars) {
  const flat = stripAnsi(text)
    .replace(/\s*\n+\s*/g, ' ')
    .replace(/[ \t]{2,}/g, ' ')
    .trim();
  if (flat.length <= maxChars) return { text: flat, truncated: false };
  return { text: flat.slice(0, maxChars).trimEnd() + '…', truncated: true };
}

export function tailLines(raw) {
  return stripAnsi(raw)
    .split('\n')
    .map((l) => l.replace(/\s+$/u, ''))
    .filter((l) => l.trim() !== '');
}

export function cleanTail(raw, maxLines) {
  return tailLines(raw)
    .filter((l) => !RULE_ONLY.test(l))
    .filter((l) => !FOOTER.test(l))
    .filter((l) => !PROMPT_ONLY.test(l))
    .map((l) => l.replace(/^\s{1,2}/, ''))
    .slice(-maxLines);
}
