import { spawnSync, execFile } from 'node:child_process';

const LIMITS = { maxBuffer: 32 * 1024 * 1024 };

export function run(cmd, args, { timeoutMs = 15000 } = {}) {
  const r = spawnSync(cmd, args, { encoding: 'utf8', timeout: timeoutMs, ...LIMITS });
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

export function runAll(jobs, { timeoutMs = 15000 } = {}) {
  return Promise.all(jobs.map(([cmd, args]) => new Promise((resolve) => {
    execFile(cmd, args, { encoding: 'utf8', timeout: timeoutMs, ...LIMITS }, (error, stdout, stderr) => {
      resolve({
        ok: !error,
        code: typeof error?.code === 'number' ? error.code : error ? -1 : 0,
        stdout: stdout ?? '',
        stderr: stderr ?? String(error?.message ?? ''),
      });
    });
  })));
}
