import { test } from 'node:test';
import assert from 'node:assert/strict';
import { resolveTarget } from '../lib/target.js';

const PANES = [
  { name: 'media', pane_id: 'wE:pX', agent_session: { value: 's1' }, agent_status: 'idle', terminal_title_stripped: 'MEDIA-UNIFICATION', cwd: '/Users/r/GolandProjects' },
  { name: null, pane_id: 'wE:p13', agent_session: { value: 's2' }, agent_status: 'idle', terminal_title_stripped: 'config-service кеш инвалидация', cwd: '/Users/r/GolandProjects' },
  { name: null, pane_id: 'w1:p2', agent_session: { value: 's3' }, agent_status: 'idle', terminal_title_stripped: 'Master+', cwd: '/Users/r/projects/masterplus-site' },
];

function fakeRun({ known = ['media', 'wE:pX'] } = {}) {
  return (cmd, args) => {
    if (args[1] === 'get') {
      const target = args[2];
      const hit = known.includes(target) ? PANES.find((p) => p.name === target || p.pane_id === target) : null;
      const body = hit
        ? { result: { agent: hit } }
        : { error: { code: 'agent_not_found', message: `agent target ${target} not found` } };
      return { ok: true, code: 0, stdout: JSON.stringify(body), stderr: '' };
    }
    return { ok: true, code: 0, stdout: JSON.stringify({ result: { agents: PANES } }), stderr: '' };
  };
}

test('алиас и pane_id уходят прямо в herdr, без листинга', () => {
  const calls = [];
  const run = (cmd, args) => {
    calls.push(args[1]);
    return fakeRun()(cmd, args);
  };
  assert.equal(resolveTarget(run, 'media').agent.pane_id, 'wE:pX');
  assert.deepEqual(calls, ['get']);
});

test('кусок заголовка находит безымянную панель', () => {
  const got = resolveTarget(fakeRun(), 'кеш инвалидация');
  assert.equal(got.ok, true);
  assert.equal(got.agent.pane_id, 'wE:p13');
  assert.equal(got.matched, 'title');
});

test('поиск по заголовку не зависит от регистра', () => {
  assert.equal(resolveTarget(fakeRun(), 'master+').agent.pane_id, 'w1:p2');
});

test('cwd находит панель, когда по заголовку не совпало', () => {
  const got = resolveTarget(fakeRun(), 'masterplus-site');
  assert.equal(got.agent.pane_id, 'w1:p2');
  assert.equal(got.matched, 'cwd');
});

test('несколько совпадений — ошибка со списком, а не молчаливый первый', () => {
  const got = resolveTarget(fakeRun(), 'GolandProjects');
  assert.equal(got.ok, false);
  assert.equal(got.error.code, 'ambiguous_target');
  assert.match(got.error.message, /media/);
  assert.match(got.error.message, /wE:p13/);
});

test('промах перечисляет живые цели', () => {
  const got = resolveTarget(fakeRun(), 'нетакой');
  assert.equal(got.ok, false);
  assert.equal(got.error.code, 'agent_not_found');
  assert.match(got.error.message, /media, wE:p13, w1:p2/);
});

test('недоступный herdr не превращается в «цель не найдена»', () => {
  const run = () => ({ ok: false, code: 1, stdout: '', stderr: 'connection refused' });
  const got = resolveTarget(run, 'media');
  assert.equal(got.ok, false);
  assert.notEqual(got.error.code, 'agent_not_found');
});

test('session_id живой панели ведёт на саму панель, а не на её историю', () => {
  const got = resolveTarget(fakeRun(), 's2');
  assert.equal(got.ok, true);
  assert.equal(got.agent.pane_id, 'wE:p13');
  assert.equal(got.matched, 'session_id');
});

test('session_id проверяется раньше подстроки заголовка', () => {
  const got = resolveTarget(fakeRun(), 's1');
  assert.equal(got.agent.pane_id, 'wE:pX');
  assert.equal(got.matched, 'session_id');
});
