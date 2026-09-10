import { spawn } from 'node:child_process';
import { openSync, mkdirSync } from 'node:fs';
import { homedir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

export const RUN_DIR = process.env.CLAUDEX_RUN_DIR || join(homedir(), '.claudex', 'runs');
const BIN = fileURLToPath(new URL('../bin/claudex', import.meta.url));

// Фоновый запуск через `&` не освобождает вызывающего: дочерний процесс
// наследует stdout, пайп остаётся открытым, и оболочка ждёт EOF — замерено,
// двухсекундный фоновый sleep задержал вызов на 2021 мс. Освобождает только
// полный отрыв: своя сессия и стандартные потоки, уведённые в файл.
export function detachDelegate(argv, { runDir = RUN_DIR, now = Date.now } = {}) {
  mkdirSync(runDir, { recursive: true });
  const log = join(runDir, `delegate-${now()}.json`);
  const fd = openSync(log, 'a');

  const child = spawn(process.execPath, [BIN, ...argv], {
    detached: true,
    stdio: ['ignore', fd, fd],
    env: { ...process.env, CLAUDEX_DETACHED: '1' },
  });
  child.unref();
  return { pid: child.pid, log };
}

export function ensureRunDir(dir = RUN_DIR) {
  mkdirSync(dirname(join(dir, 'x')), { recursive: true });
  return dir;
}
