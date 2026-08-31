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

export function mapAgent(a) {
  return {
    alias: a?.name ?? null,
    pane_id: a?.pane_id ?? null,
    session_id: a?.agent_session?.value ?? null,
    status: a?.agent_status ?? null,
    title: a?.terminal_title_stripped ?? null,
    cwd: a?.cwd ?? null,
    focused: a?.focused === true,
  };
}

export function listAgents(run) {
  const parsed = parseEnvelope(run('herdr', ['agent', 'list']));
  if (!parsed.ok) return parsed;
  const agents = Array.isArray(parsed.value?.agents) ? parsed.value.agents : [];
  return { ok: true, agents: agents.map(mapAgent) };
}

export function getAgent(run, target) {
  const parsed = parseEnvelope(run('herdr', ['agent', 'get', target]));
  if (!parsed.ok) return parsed;
  if (!parsed.value?.agent) {
    return { ok: false, error: { code: 'agent_not_found', message: `агент ${target} не найден` } };
  }
  return { ok: true, agent: mapAgent(parsed.value.agent) };
}

export function readTail(run, target, lines) {
  const res = run('herdr', [
    'agent', 'read', target,
    '--source', 'recent',
    '--lines', String(lines),
    '--format', 'text',
  ]);
  if (!res.ok) {
    const msg = String(res.stderr ?? '').trim() || `herdr agent read завершился с кодом ${res.code}`;
    return { ok: false, error: { code: 'read_failed', message: msg.slice(0, 300) } };
  }
  return { ok: true, raw: res.stdout ?? '' };
}
