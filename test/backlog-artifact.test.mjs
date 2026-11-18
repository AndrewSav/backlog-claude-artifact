import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  parseYaml, parseTask, idKey, buildModel, findBacklog, decideStop, recordPublish,
  loadState, outPaths, readOptions,
} from '../scripts/backlog-artifact.mjs';

const OPTS = { includeDrafts: false, includeArchived: false, autoPublish: true, intervalMinutes: 15 };

function task(id, status, extra = '') {
  return `---\nid: ${id}\ntitle: 'Task ${id}: it''s quoted'\nstatus: ${status}\nassignee:\n  - '@me'\ncreated_date: '2026-01-0${id.slice(-1)} 10:00'\nlabels: [ui, 'back end']\ndependencies: []\n${extra}---\n\n## Description\n\n<!-- SECTION:DESCRIPTION:BEGIN -->\nBody of ${id}\n<!-- SECTION:DESCRIPTION:END -->\n\n## Acceptance Criteria\n<!-- AC:BEGIN -->\n- [x] #1 one\n- [ ] #2 two\n<!-- AC:END -->\n`;
}

function makeRepo(statuses = '["To Do", "In Progress", "In Review", "Done"]') {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'bla-'));
  const b = path.join(root, 'backlog');
  fs.mkdirSync(path.join(b, 'tasks'), { recursive: true });
  fs.mkdirSync(path.join(b, 'drafts'), { recursive: true });
  fs.mkdirSync(path.join(b, 'archive', 'tasks'), { recursive: true });
  fs.writeFileSync(path.join(b, 'config.yml'), `project_name: "Demo"\ndefault_status: "To Do"\nstatuses: ${statuses}\n`);
  fs.writeFileSync(path.join(b, 'tasks', 'task-1 - a.md'), task('TASK-1', 'Done'));
  fs.writeFileSync(path.join(b, 'tasks', 'task-2 - b.md'), task('TASK-2', 'In Review', 'parent_task_id: TASK-1\npriority: high\n'));
  fs.writeFileSync(path.join(b, 'tasks', 'task-3 - c.md'), task('TASK-3', 'Blocked'));
  fs.writeFileSync(path.join(b, 'drafts', 'draft-1 - d.md'), task('DRAFT-1', 'Draft'));
  fs.writeFileSync(path.join(b, 'archive', 'tasks', 'task-4 - e.md'), task('TASK-4', 'To Do'));
  return root;
}

test('yaml subset: quotes, inline and block lists', () => {
  const y = parseYaml(`a: "x y"\nb: 'it''s'\nc: [one, 'two, three', "four"]\nd:\n  - '@me'\n  - you\ne: []\nf: 12`);
  assert.equal(y.a, 'x y');
  assert.equal(y.b, "it's");
  assert.deepEqual(y.c, ['one', 'two, three', 'four']);
  assert.deepEqual(y.d, ['@me', 'you']);
  assert.deepEqual(y.e, []);
  assert.equal(y.f, '12');
});

test('task numbers sort numerically, subtasks after their parent', () => {
  const ids = ['TASK-10', 'TASK-2', 'TASK-1.2', 'TASK-1', 'TASK-1.10', 'ues-3'];
  const cmp = (a, b) => { const x = idKey(a), y = idKey(b); for (let i = 0; i < Math.max(x.length, y.length); i++) { const d = (x[i] ?? -1) - (y[i] ?? -1); if (d) return d; } return 0; };
  assert.deepEqual([...ids].sort(cmp), ['TASK-1', 'TASK-1.2', 'TASK-1.10', 'TASK-2', 'ues-3', 'TASK-10']);
});

test('task parsing strips section markers and counts criteria', () => {
  const t = parseTask(task('TASK-7', 'To Do'), 'task');
  assert.equal(t.title, "Task TASK-7: it's quoted");
  assert.deepEqual(t.ac, [1, 2]);
  assert.deepEqual(t.labels, ['ui', 'back end']);
  assert.ok(!t.sections.some((s) => s.md.includes('<!--')));
});

test('states come from config, unknown statuses are appended, extras off by default', () => {
  const m = buildModel(findBacklog(makeRepo()), OPTS);
  assert.deepEqual(m.states, ['To Do', 'In Progress', 'In Review', 'Done', 'Blocked']);
  assert.deepEqual(m.defaultOn, ['To Do', 'In Progress', 'In Review', 'Blocked']);
  assert.equal(m.tasks.length, 3);
});

test('drafts and archived appear only when enabled', () => {
  const m = buildModel(findBacklog(makeRepo('["To Do", "In Progress", "Done"]')), { ...OPTS, includeDrafts: true, includeArchived: true });
  assert.deepEqual(m.states.slice(-2), ['Draft', 'Archived']);
  assert.equal(m.tasks.find((t) => t.id === 'TASK-4').state, 'Archived');
  assert.ok(!m.defaultOn.includes('Archived'));
});

test('hash is stable and changes with content', () => {
  const root = makeRepo();
  const bl = findBacklog(root);
  const h1 = buildModel(bl, OPTS).hash;
  assert.equal(buildModel(bl, OPTS).hash, h1);
  fs.appendFileSync(path.join(root, 'backlog', 'tasks', 'task-1 - a.md'), '\nmore');
  assert.notEqual(buildModel(bl, OPTS).hash, h1);
});

test('options read from CLAUDE_PLUGIN_OPTION_* in either case', () => {
  const o = readOptions({ CLAUDE_PLUGIN_OPTION_include_drafts: 'true', CLAUDE_PLUGIN_OPTION_PUBLISH_INTERVAL_MINUTES: '0' });
  assert.equal(o.includeDrafts, true);
  assert.equal(o.intervalMinutes, 0);
  assert.equal(o.autoPublish, true);
});

test('stop hook: publish, record, unchanged, throttled, no loop', () => {
  process.env.BACKLOG_ARTIFACT_OUT_DIR = fs.mkdtempSync(path.join(os.tmpdir(), 'bla-out-'));
  const root = makeRepo();
  const t0 = 1_000_000_000_000;

  assert.equal(decideStop({ input: { stop_hook_active: true }, root, opts: OPTS, now: t0 }).action, 'none');

  const d1 = decideStop({ input: { session_id: 's1' }, root, opts: OPTS, now: t0 });
  assert.equal(d1.action, 'block');
  assert.match(d1.reason, /icon "list"/);
  const html = fs.readFileSync(outPaths(root).html, 'utf8');
  assert.match(html, /<title>Demo Backlog<\/title>/);
  assert.ok(!html.includes('/*__DATA__*/'));

  // Asked but not published yet: no nagging inside the interval.
  assert.equal(decideStop({ input: { session_id: 's1' }, root, opts: OPTS, now: t0 + 60_000 }).why, 'throttled');

  assert.ok(recordPublish({
    input: { session_id: 's1', cwd: root, tool_input: { file_path: outPaths(root).html }, tool_response: 'Published https://claude.ai/code/artifact/abc-123 (watching)' },
    root, now: t0 + 61_000,
  }));
  assert.equal(loadState(outPaths(root).state).url, 'https://claude.ai/code/artifact/abc-123');
  assert.equal(decideStop({ input: { session_id: 's1' }, root, opts: OPTS, now: t0 + 9e9 }).why, 'unchanged');

  fs.appendFileSync(path.join(root, 'backlog', 'tasks', 'task-2 - b.md'), '\nchanged');
  assert.equal(decideStop({ input: { session_id: 's1' }, root, opts: OPTS, now: t0 + 120_000 }).why, 'throttled');
  const d2 = decideStop({ input: { session_id: 's2' }, root, opts: OPTS, now: t0 + 61_000 + 15 * 60_000 });
  assert.equal(d2.action, 'block');
  assert.match(d2.reason, /url "https:\/\/claude\.ai\/code\/artifact\/abc-123"/);
  assert.match(d2.reason, /action "read"/); // new session must read before updating
  assert.doesNotMatch(d2.reason, /icon/);

  assert.equal(recordPublish({ input: { tool_input: { file_path: path.join(root, 'other.html') } }, root }), false);
});

test('headless sessions render but never ask', () => {
  process.env.BACKLOG_ARTIFACT_OUT_DIR = fs.mkdtempSync(path.join(os.tmpdir(), 'bla-out-'));
  const root = makeRepo();
  const d = decideStop({ input: {}, root, opts: OPTS, entrypoint: 'sdk-cli' });
  assert.equal(d.why, 'headless');
  assert.ok(fs.existsSync(outPaths(root).html));
  assert.equal(decideStop({ input: {}, root, opts: OPTS, entrypoint: 'cli' }).action, 'block');
});

test('no backlog: nothing happens', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'bla-none-'));
  assert.equal(decideStop({ input: {}, root, opts: OPTS }).why, 'no backlog');
});
