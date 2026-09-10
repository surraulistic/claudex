function parseEnvelope(res) {
  const out = String(res.stdout ?? '').trim() || String(res.stderr ?? '').trim();
  if (!out) {
    return { ok: false, error: { code: 'no_output', message: 'herdr вернул пустой ответ' } };
  }
  let parsed;
  try {
    parsed = JSON.parse(out);
  } catch {
    return { ok: false, error: { code: 'bad_json', message: out.slice(0, 300) } };
  }
  if (parsed && parsed.error) {
    return {
      ok: false,
      error: {
        code: String(parsed.error.code ?? 'error'),
        message: String(parsed.error.message ?? ''),
      },
    };
  }
  return { ok: true, value: parsed.result };
}

export function mapAgent(a, labels = null) {
  const tabId = a?.tab_id ?? null;
  return {
    alias: a?.name ?? null,
    label: labels?.get(tabId) ?? null,
    kind: a?.agent ?? null,
    pane_id: a?.pane_id ?? null,
    tab_id: tabId,
    session_id: a?.agent_session?.value ?? null,
    status: a?.agent_status ?? null,
    title: a?.terminal_title_stripped ?? null,
    cwd: a?.cwd ?? null,
    focused: a?.focused === true,
  };
}

// Имя, которое пользователь видит и произносит, — это метка вкладки, а не имя
// агента: `herdr agent rename` почти никто не зовёт, а вкладки подписаны всегда.
export function listTabs(run) {
  const parsed = parseEnvelope(run('herdr', ['tab', 'list']));
  if (!parsed.ok) return new Map();
  const tabs = Array.isArray(parsed.value?.tabs) ? parsed.value.tabs : [];
  return new Map(tabs.map((t) => [t?.tab_id ?? null, t?.label ?? null]));
}

export function listAgents(run, { withLabels = false } = {}) {
  const parsed = parseEnvelope(run('herdr', ['agent', 'list']));
  if (!parsed.ok) return parsed;
  const agents = Array.isArray(parsed.value?.agents) ? parsed.value.agents : [];
  const labels = withLabels ? listTabs(run) : null;
  return { ok: true, agents: agents.map((a) => mapAgent(a, labels)) };
}

export function getAgent(run, target) {
  const parsed = parseEnvelope(run('herdr', ['agent', 'get', target]));
  if (!parsed.ok) return parsed;
  if (!parsed.value?.agent) {
    return { ok: false, error: { code: 'agent_not_found', message: `агент ${target} не найден` } };
  }
  return { ok: true, agent: mapAgent(parsed.value.agent) };
}

function readSource(run, target, lines, source) {
  return run('herdr', [
    'agent', 'read', target,
    '--source', source,
    '--lines', String(lines),
    '--format', 'text',
  ]);
}

// На работающей панели --source recent отдаёт пусто — измерено на всех
// working-панелях, тогда как visible исправно даёт экран. На покое наоборот:
// recent приносит вчетверо больше строк. Поэтому источник выбирается по
// состоянию, а пустой ответ добирается вторым.
export function promptAgent(run, target, text) {
  const res = run('herdr', ['agent', 'prompt', target, text], { timeoutMs: 30000 });
  const parsed = parseEnvelope(res);
  if (!parsed.ok) return parsed;
  return { ok: true };
}

// Таймаут и сбой herdr — разные исходы: первый значит «панель не дошла», второй
// «мы не знаем». Смешивать их нельзя, иначе перезапуск демона читается как
// незавершённая задача.
const TIMEOUT_CODES = new Set(['timeout', 'agent_prompt_stalled']);

export function classifyWaitError(error) {
  if (!error) return 'herdr_error';
  if (TIMEOUT_CODES.has(String(error.code))) return 'timeout';
  if (error.code === 'agent_not_found') return 'gone';
  return 'herdr_error';
}

// Возвращает ту же обёртку, что и agent get, поэтому разбор общий.
export function waitAgent(run, target, { until = ['idle', 'done', 'blocked'], timeoutMs } = {}) {
  const args = ['agent', 'wait', target];
  for (const state of until) args.push('--until', state);
  if (timeoutMs) args.push('--timeout', String(timeoutMs));
  const res = run('herdr', args, { timeoutMs: timeoutMs ? timeoutMs + 15000 : 3600000 });
  const parsed = parseEnvelope(res);
  if (!parsed.ok) return { ...parsed, kind: classifyWaitError(parsed.error) };
  if (!parsed.value?.agent) {
    return { ok: false, error: { code: 'wait_failed', message: 'herdr не вернул агента' } };
  }
  return { ok: true, agent: mapAgent(parsed.value.agent) };
}

export function readTail(run, target, lines, { status = null } = {}) {
  const order = status === 'working' ? ['visible', 'recent'] : ['recent', 'visible'];
  let last = null;
  for (const source of order) {
    const res = readSource(run, target, lines, source);
    last = res;
    if (res.ok && String(res.stdout ?? '').trim()) {
      return { ok: true, raw: res.stdout, source };
    }
  }
  if (last?.ok) return { ok: true, raw: last.stdout ?? '', source: order[order.length - 1] };
  const msg = String(last?.stderr ?? '').trim() || `herdr agent read завершился с кодом ${last?.code}`;
  return { ok: false, error: { code: 'read_failed', message: msg.slice(0, 300) } };
}
