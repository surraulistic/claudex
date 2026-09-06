import { test } from 'node:test';
import assert from 'node:assert/strict';
import { groupBySession } from '../lib/find.js';

const HITS = [
  { id: 'a', kind: 'user', ts: 300, session_id: 's1', transcript_id: 'T1', project: 'GolandProjects', cwd: '/g', branch: 'main', text: 'лицензии раз' },
  { id: 'b', kind: 'assistant', ts: 100, session_id: 's1', transcript_id: 'T1', project: 'GolandProjects', cwd: '/g', branch: 'main', text: 'лицензии два' },
  { id: 'c', kind: 'user', ts: 500, session_id: null, transcript_id: 'T2', project: 'GolandProjects', cwd: '/g/wt', branch: 'feat/x', text: 'лицензии три' },
];

test('попадания сворачиваются в сессии, а не сыплются плоским списком', () => {
  const got = groupBySession(HITS, 400);
  assert.equal(got.length, 2);
  assert.equal(got.find((g) => g.target === 's1').hits, 2);
});

test('строка без provider_session_id адресуется своим transcript_id', () => {
  const g = groupBySession(HITS, 400).find((x) => x.transcript_id === 'T2');
  assert.equal(g.session_id, null);
  assert.equal(g.target, 'T2', 'иначе попадание — тупик: адресовать нечем');
});

test('сессии идут от самой свежей', () => {
  assert.deepEqual(groupBySession(HITS, 400).map((g) => g.target), ['T2', 's1']);
});

test('окно активности считается по всем попаданиям сессии', () => {
  const g = groupBySession(HITS, 400).find((x) => x.target === 's1');
  assert.match(g.first_hit, /^1970-01-01T/);
  assert.notEqual(g.first_hit, g.last_hit);
});

test('сессия подписана каталогом и веткой — по ним видно, куда смотреть', () => {
  const g = groupBySession(HITS, 400).find((x) => x.target === 'T2');
  assert.equal(g.cwd, '/g/wt');
  assert.equal(g.branch, 'feat/x');
  assert.equal(g.project, 'GolandProjects');
});

test('текст попадания режется по chars', () => {
  const g = groupBySession(HITS, 5).find((x) => x.target === 'T2');
  assert.equal(g.entries[0].text, 'лицен…');
});

test('попадание, у которого нет ни session_id, ни transcript_id, отбрасывается', () => {
  assert.deepEqual(groupBySession([{ id: 'x', session_id: null, transcript_id: null }], 400), []);
});
