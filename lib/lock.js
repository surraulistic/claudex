import { mkdirSync, writeFileSync, readFileSync, unlinkSync, openSync, closeSync, readdirSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';

export const LOCK_DIR = process.env.CLAUDEX_LOCK_DIR || join(homedir(), '.claudex', 'locks');

function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    return e.code === 'EPERM';
  }
}

// Два наблюдателя на одну панель разбудят ведущего дважды и второй отчитается о
// чужой задаче. Держим эксклюзивный файл; запись мёртвого процесса снимается.
export function acquire(target, { dir = LOCK_DIR, pid = process.pid } = {}) {
  mkdirSync(dir, { recursive: true });
  const path = join(dir, `${String(target).replace(/[^A-Za-z0-9._-]/g, '_')}.lock`);

  for (let attempt = 0; attempt < 2; attempt += 1) {
    try {
      closeSync(openSync(path, 'wx'));
      writeFileSync(path, JSON.stringify({ pid, target, at: new Date().toISOString() }));
      return { ok: true, path, release: () => { try { unlinkSync(path); } catch { /* уже снят */ } } };
    } catch (e) {
      if (e.code !== 'EEXIST') return { ok: false, reason: String(e.message).slice(0, 200) };
      let held = null;
      try { held = JSON.parse(readFileSync(path, 'utf8')); } catch { /* битый лок */ }
      if (held?.pid && alive(held.pid)) {
        return { ok: false, held, reason: `панель «${target}» уже под наблюдением, pid ${held.pid}` };
      }
      try { unlinkSync(path); } catch { /* гонка снятия */ }
    }
  }
  return { ok: false, reason: 'не удалось взять блокировку' };
}

// Прямой `herdr agent prompt` не оставляет следа, и задача уходит в работу
// незамеченной. Список живых наблюдателей позволяет показать это там, где
// ведущий и так смотрит: панель работает, а следит за ней никто.
export function listHeld({ dir = LOCK_DIR } = {}) {
  let names = [];
  try {
    names = readdirSync(dir).filter((n) => n.endsWith('.lock'));
  } catch {
    return new Map();
  }
  const held = new Map();
  for (const name of names) {
    const path = join(dir, name);
    let rec = null;
    try { rec = JSON.parse(readFileSync(path, 'utf8')); } catch { /* битый лок */ }
    if (!rec?.pid || !alive(rec.pid)) {
      try { unlinkSync(path); } catch { /* уже снят */ }
      continue;
    }
    held.set(rec.target, { pid: rec.pid, at: rec.at });
  }
  return held;
}
