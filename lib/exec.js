import { spawnSync } from 'node:child_process';

export function run(cmd, args, { timeoutMs = 15000 } = {}) {
  const r = spawnSync(cmd, args, {
    encoding: 'utf8',
    timeout: timeoutMs,
    maxBuffer: 32 * 1024 * 1024,
  });
  if (r.error) {
    return { ok: false, code: -1, stdout: '', stderr: String(r.error.message ?? r.error) };
  }
  return {
    ok: r.status === 0,
    code: typeof r.status === 'number' ? r.status : -1,
    stdout: r.stdout ?? '',
    stderr: r.stderr ?? '',
  };
}
