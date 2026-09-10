import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, readFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';
import { detachDelegate } from '../lib/detach.js';

// Фоновый запуск через `&` не освобождает вызывающего: дочерний наследует
// stdout, пайп остаётся открытым, оболочка ждёт EOF. Измерено: двухсекундный
// фоновый sleep задержал вызов на 2021 мс. Проверяем, что отрыв — настоящий.
function waitFor(check, ms = 15000, step = 100) {
  const until = Date.now() + ms;
  while (Date.now() < until) {
    if (check()) return true;
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, step);
  }
  return false;
}

test('вызывающий освобождается сразу, работа продолжается отдельно', () => {
  const dir = mkdtempSync(join(tmpdir(), 'claudex-run-'));
  try {
    const t0 = Date.now();
    const child = detachDelegate(['--help'], { runDir: dir });
    const elapsed = Date.now() - t0;

    assert.ok(child.pid > 0, 'процесс запущен');
    assert.ok(elapsed < 3000, `управление вернулось за ${elapsed} мс, а не после завершения работы`);
    assert.ok(waitFor(() => existsSync(child.log) && readFileSync(child.log, 'utf8').includes('claudex')),
      'вывод ушёл в файл, а не в пайп вызывающего');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// Пока родитель жив, завершившийся ребёнок остаётся зомби в его таблице, и
// проверять «исчез ли pid» бессмысленно. Наблюдаемое свойство отрыва — своя
// группа процессов: только она переживает уход вызывающего.
test('оторванный процесс ведёт собственную группу', () => {
  const dir = mkdtempSync(join(tmpdir(), 'claudex-run-'));
  try {
    const child = detachDelegate(['--help'], { runDir: dir });
    const pgid = Number(execFileSync('ps', ['-o', 'pgid=', '-p', String(child.pid)], { encoding: 'utf8' }).trim());
    assert.equal(pgid, child.pid, 'иначе процесс уйдёт вместе с группой вызывающего');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('каждый запуск пишет в свой файл', () => {
  const dir = mkdtempSync(join(tmpdir(), 'claudex-run-'));
  try {
    let n = 1000;
    const a = detachDelegate(['--help'], { runDir: dir, now: () => (n += 1) });
    const b = detachDelegate(['--help'], { runDir: dir, now: () => (n += 1) });
    assert.notEqual(a.log, b.log, 'иначе два наблюдателя затрут отчёты друг друга');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
