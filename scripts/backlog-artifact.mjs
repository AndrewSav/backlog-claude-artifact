#!/usr/bin/env node
// backlog-artifact: render a Backlog.md backlog into a single HTML page and
// ask Claude to publish it as an Artifact when it has changed.
//
// Subcommands:
//   stop          Stop hook. Renders; if the backlog changed and the throttle allows,
//                 blocks the stop with an instruction to publish.
//   record        PostToolUse hook for the Artifact tool. Saves the artifact URL.
//   instructions  Renders and prints the publish instruction (used by /backlog-artifact:publish).
//   render [dir]  Renders and prints a summary. For testing.

import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const TEMPLATE_PATH = path.join(HERE, 'template.html');
const OUT_DIR_REL = path.join('.claude', 'backlog-artifact');

// ---------- options ----------

export function readOptions(env = process.env) {
  const get = (key) => {
    for (const k of [`CLAUDE_PLUGIN_OPTION_${key}`, `CLAUDE_PLUGIN_OPTION_${key.toUpperCase()}`]) {
      if (env[k] !== undefined && env[k] !== '') return env[k];
    }
    return undefined;
  };
  const bool = (key, def) => {
    const v = get(key);
    if (v === undefined) return def;
    return /^(1|true|yes|on)$/i.test(String(v).trim());
  };
  const num = (key, def) => {
    const v = Number(get(key));
    return Number.isFinite(v) && v >= 0 ? v : def;
  };
  return {
    includeDrafts: bool('include_drafts', false),
    includeArchived: bool('include_archived', false),
    autoPublish: bool('auto_publish', true),
    intervalMinutes: num('publish_interval_minutes', 15),
  };
}

// ---------- minimal YAML (the subset Backlog.md writes) ----------

function unquote(s) {
  s = s.trim();
  if (s.length >= 2 && s[0] === "'" && s.at(-1) === "'") return s.slice(1, -1).replace(/''/g, "'");
  if (s.length >= 2 && s[0] === '"' && s.at(-1) === '"') {
    try { return JSON.parse(s); } catch { return s.slice(1, -1); }
  }
  return s;
}

function splitInline(inner) {
  const out = [];
  let cur = '', q = null;
  for (const ch of inner) {
    if (q) { cur += ch; if (ch === q) q = null; continue; }
    if (ch === "'" || ch === '"') { q = ch; cur += ch; continue; }
    if (ch === ',') { out.push(cur); cur = ''; continue; }
    cur += ch;
  }
  if (cur.trim() !== '') out.push(cur);
  return out.map(unquote).filter((x) => x !== '');
}

function scalar(raw) {
  const v = raw.trim();
  if (v === '') return '';
  if (v[0] === '[' && v.at(-1) === ']') return splitInline(v.slice(1, -1));
  return unquote(v);
}

export function parseYaml(text) {
  const obj = {};
  let listKey = null;
  for (const line of text.split(/\r?\n/)) {
    if (!line.trim() || line.trim().startsWith('#')) continue;
    const item = line.match(/^\s*-\s+(.*)$/);
    if (item && listKey) {
      if (!Array.isArray(obj[listKey])) obj[listKey] = [];
      obj[listKey].push(unquote(item[1]));
      continue;
    }
    const kv = line.match(/^([A-Za-z_][\w-]*):(?:\s+(.*))?$/);
    if (kv) {
      const [, key, rest = ''] = kv;
      if (rest.trim() === '') { obj[key] = ''; listKey = key; }
      else { obj[key] = scalar(rest); listKey = null; }
    } else {
      listKey = null; // nested or unsupported structure: ignore
    }
  }
  return obj;
}

const asList = (v) => (Array.isArray(v) ? v : v ? [String(v)] : []);

// ---------- tasks ----------

export function splitFrontmatter(text) {
  const m = text.match(/^﻿?---\r?\n([\s\S]*?)\r?\n---\r?\n?([\s\S]*)$/);
  if (!m) return { fm: {}, body: text };
  return { fm: parseYaml(m[1]), body: m[2] };
}

export function idKey(id) {
  const m = String(id).match(/(\d+(?:\.\d+)*)\s*$/);
  return m ? m[1].split('.').map(Number) : [Number.MAX_SAFE_INTEGER];
}

function parseBody(body) {
  const clean = body.replace(/<!--[\s\S]*?-->/g, '');
  const sections = [];
  let cur = null;
  for (const line of clean.split(/\r?\n/)) {
    const h = line.match(/^##\s+(.+?)\s*$/);
    if (h) { cur = { h: h[1], md: [] }; sections.push(cur); continue; }
    if (!cur) { if (!line.trim()) continue; cur = { h: '', md: [] }; sections.push(cur); }
    cur.md.push(line);
  }
  let done = 0, total = 0;
  const out = [];
  for (const s of sections) {
    const md = s.md.join('\n').trim();
    if (!md) continue;
    if (/acceptance criteria/i.test(s.h)) {
      for (const l of md.split('\n')) {
        const c = l.match(/^\s*-\s+\[([ xX])\]/);
        if (c) { total++; if (c[1] !== ' ') done++; }
      }
    }
    out.push({ h: s.h, md });
  }
  return { sections: out, ac: total ? [done, total] : null };
}

export function parseTask(text, set) {
  const { fm, body } = splitFrontmatter(text);
  if (!fm.id && !fm.title) return null;
  const { sections, ac } = parseBody(body);
  return {
    id: String(fm.id || ''),
    k: idKey(fm.id || ''),
    title: String(fm.title || '(untitled)'),
    status: String(fm.status || ''),
    set,
    priority: fm.priority ? String(fm.priority).toLowerCase() : '',
    assignee: asList(fm.assignee),
    labels: asList(fm.labels),
    deps: asList(fm.dependencies),
    parent: fm.parent_task_id ? String(fm.parent_task_id) : '',
    milestone: fm.milestone ? String(fm.milestone) : '',
    created: String(fm.created_date || ''),
    updated: String(fm.updated_date || fm.created_date || ''),
    ordinal: fm.ordinal !== undefined && fm.ordinal !== '' ? Number(fm.ordinal) : null,
    ac,
    sections,
  };
}

// ---------- project discovery ----------

export function findBacklog(start) {
  for (const name of ['config.yml', 'config.yaml']) {
    const p = path.join(start, 'backlog', name);
    if (fs.existsSync(p)) return { root: start, backlog: path.join(start, 'backlog'), config: p };
  }
  return null;
}

function listMd(dir) {
  if (!fs.existsSync(dir)) return [];
  return fs.readdirSync(dir, { withFileTypes: true })
    .filter((d) => d.isFile() && d.name.toLowerCase().endsWith('.md'))
    .map((d) => path.join(dir, d.name))
    .sort();
}

export function sourceFiles(bl, opts) {
  const files = [{ file: bl.config, set: 'config' }];
  for (const f of listMd(path.join(bl.backlog, 'tasks'))) files.push({ file: f, set: 'task' });
  if (opts.includeDrafts) for (const f of listMd(path.join(bl.backlog, 'drafts'))) files.push({ file: f, set: 'draft' });
  if (opts.includeArchived) for (const f of listMd(path.join(bl.backlog, 'archive', 'tasks'))) files.push({ file: f, set: 'archived' });
  return files;
}

export const DRAFT_STATE = 'Draft';
export const ARCHIVED_STATE = 'Archived';

export function buildModel(bl, opts) {
  const files = sourceFiles(bl, opts);
  const hash = crypto.createHash('sha256');
  hash.update(fs.readFileSync(TEMPLATE_PATH));
  hash.update(JSON.stringify({ d: opts.includeDrafts, a: opts.includeArchived }));
  let config = {};
  const tasks = [];
  for (const { file, set } of files) {
    const text = fs.readFileSync(file, 'utf8');
    hash.update(path.relative(bl.backlog, file).replace(/\\/g, '/') + '\0' + text + '\0');
    if (set === 'config') { config = parseYaml(text); continue; }
    const t = parseTask(text, set);
    if (!t) continue;
    if (set === 'draft') t.state = DRAFT_STATE;
    else if (set === 'archived') t.state = ARCHIVED_STATE;
    else t.state = t.status || String(config.default_status || 'To Do');
    tasks.push(t);
  }
  const configured = asList(config.statuses);
  const states = configured.length ? [...configured] : ['To Do', 'In Progress', 'Done'];
  const seen = new Set(states.map((s) => s.toLowerCase()));
  const extras = [];
  for (const t of tasks) {
    if (t.set !== 'task') continue;
    const s = t.state;
    if (!seen.has(s.toLowerCase())) { seen.add(s.toLowerCase()); extras.push(s); }
    else t.state = states.find((x) => x.toLowerCase() === s.toLowerCase()) || s;
  }
  const finalState = states.at(-1);
  states.push(...extras);
  if (opts.includeDrafts) states.push(DRAFT_STATE);
  if (opts.includeArchived) states.push(ARCHIVED_STATE);
  const defaultOn = states.filter((s) => s !== finalState && s !== ARCHIVED_STATE);
  return {
    project: String(config.project_name || path.basename(bl.root)),
    states,
    finalState,
    defaultOn,
    tasks,
    generated: new Date().toISOString(),
    hash: hash.digest('hex'),
  };
}

// ---------- rendering ----------

function escHtml(s) {
  return String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
}

export function renderHtml(model) {
  const tpl = fs.readFileSync(TEMPLATE_PATH, 'utf8');
  const { hash, ...data } = model;
  const json = JSON.stringify(data)
    .replace(/</g, '\\u003c')
    .split(String.fromCharCode(0x2028)).join('\\u2028')
    .split(String.fromCharCode(0x2029)).join('\\u2029');
  return tpl
    .replace('__TITLE__', () => escHtml(`${model.project} Backlog`))
    .replace('/*__DATA__*/null', () => json);
}

export function outPaths(root) {
  // BACKLOG_ARTIFACT_OUT_DIR redirects output (tests, dry runs) away from the repo.
  const override = process.env.BACKLOG_ARTIFACT_OUT_DIR;
  const dir = override ? path.join(override, path.basename(root)) : path.join(root, OUT_DIR_REL);
  return { dir, html: path.join(dir, 'backlog.html'), state: path.join(dir, 'state.json') };
}

function ensureExcluded(root) {
  try {
    const gitDir = path.join(root, '.git');
    if (!fs.statSync(gitDir).isDirectory()) return;
    const ex = path.join(gitDir, 'info', 'exclude');
    const line = '/.claude/backlog-artifact/';
    const cur = fs.existsSync(ex) ? fs.readFileSync(ex, 'utf8') : '';
    if (cur.split(/\r?\n/).includes(line)) return;
    fs.mkdirSync(path.dirname(ex), { recursive: true });
    fs.appendFileSync(ex, (cur && !cur.endsWith('\n') ? '\n' : '') + line + '\n');
  } catch { /* not a plain git repo: nothing to do */ }
}

export function renderProject(root, opts) {
  const bl = findBacklog(root);
  if (!bl) return null;
  const model = buildModel(bl, opts);
  const html = renderHtml(model);
  const p = outPaths(root);
  fs.mkdirSync(p.dir, { recursive: true });
  if (!process.env.BACKLOG_ARTIFACT_OUT_DIR) ensureExcluded(root);
  const prev = fs.existsSync(p.html) ? fs.readFileSync(p.html, 'utf8') : null;
  // Keep the file byte-stable when only the timestamp would differ.
  const prevGen = prev && prev.match(/"generated":"([^"]+)"/);
  const stable = prevGen && loadState(p.state).renderedHash === model.hash
    ? html.replace(/"generated":"[^"]+"/, `"generated":"${prevGen[1]}"`)
    : html;
  if (stable !== prev) fs.writeFileSync(p.html, stable);
  const st = loadState(p.state);
  st.renderedHash = model.hash;
  saveState(p.state, st);
  return { bl, model, paths: p, bytes: Buffer.byteLength(stable) };
}

// ---------- state ----------

export function loadState(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch { return {}; }
}
export function saveState(file, st) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, JSON.stringify(st, null, 2) + '\n');
}

// ---------- publish instruction ----------

export function instruction(htmlPath, st, sessionId) {
  const parts = [`Backlog changed. Publish it now with the Artifact tool: file_path "${htmlPath}"`];
  if (st.url) {
    parts.push(`, url "${st.url}"`);
    if (st.lastSessionId !== sessionId) {
      parts.push(`. This conversation has not touched that artifact yet, so first call Artifact with action "read" and that url, then publish. The page is generated, so the new file replaces the live version as is`);
    }
  } else {
    parts.push(`, icon "list", description "Backlog.md tasks, filterable by state and sortable"`);
  }
  parts.push('. Do not edit or read the HTML file. After the publish succeeds, end the turn with no further text. If this session has no Artifact tool, end the turn without comment.');
  return parts.join('');
}

// ---------- hook entry points ----------

function readStdin() {
  try { return JSON.parse(fs.readFileSync(0, 'utf8') || '{}'); } catch { return {}; }
}

function projectDir(input) {
  return process.env.CLAUDE_PROJECT_DIR || input.cwd || process.cwd();
}

export function decideStop({ input, root, opts, now = Date.now(), entrypoint = process.env.CLAUDE_CODE_ENTRYPOINT || '' }) {
  if (input.stop_hook_active) return { action: 'none', why: 'stop_hook_active' };
  if (!findBacklog(root)) return { action: 'none', why: 'no backlog' };
  const r = renderProject(root, opts);
  if (!opts.autoPublish) return { action: 'none', why: 'auto_publish off', r };
  // Headless runs (claude -p, the Agent SDK) have no Artifact tool, so asking would only waste a turn.
  if (/^sdk/.test(entrypoint)) return { action: 'none', why: 'headless', r };
  const st = loadState(r.paths.state);
  if (st.lastPublishedHash === r.model.hash) return { action: 'none', why: 'unchanged', r };
  const last = Math.max(st.lastPublishedAt || 0, st.lastAskedAt || 0);
  if (now - last < opts.intervalMinutes * 60_000) return { action: 'none', why: 'throttled', r };
  st.pendingHash = r.model.hash;
  st.lastAskedAt = now;
  saveState(r.paths.state, st);
  return { action: 'block', reason: instruction(r.paths.html, st, input.session_id), r };
}

const samePath = (a, b) => {
  const n = (p) => path.resolve(p).replace(/\\/g, '/').toLowerCase();
  return n(a) === n(b);
};

export function recordPublish({ input, root, now = Date.now() }) {
  const fp = input.tool_input && input.tool_input.file_path;
  if (!fp) return false;
  const p = outPaths(root);
  if (!samePath(path.isAbsolute(fp) ? fp : path.join(input.cwd || root, fp), p.html)) return false;
  const blob = JSON.stringify(input.tool_response ?? '');
  const m = blob.match(/https:\/\/claude\.ai\/(?:code\/)?artifact\/[A-Za-z0-9_-]+/);
  const st = loadState(p.state);
  if (m) st.url = m[0];
  else if (input.tool_input.url) st.url = input.tool_input.url;
  st.lastPublishedHash = st.pendingHash || st.renderedHash;
  st.lastPublishedAt = now;
  st.lastSessionId = input.session_id;
  delete st.pendingHash;
  saveState(p.state, st);
  return true;
}

function main() {
  const [cmd, arg] = process.argv.slice(2);
  const opts = readOptions();
  if (cmd === 'stop') {
    const input = readStdin();
    const d = decideStop({ input, root: projectDir(input), opts });
    if (d.action === 'block') process.stdout.write(JSON.stringify({ decision: 'block', reason: d.reason }));
    return;
  }
  if (cmd === 'record') {
    const input = readStdin();
    recordPublish({ input, root: projectDir(input) });
    return;
  }
  if (cmd === 'instructions') {
    const root = projectDir({});
    const r = renderProject(root, opts);
    if (!r) { console.log(`No Backlog.md backlog found: ${path.join(root, 'backlog', 'config.yml')} does not exist.`); return; }
    const st = loadState(r.paths.state);
    st.pendingHash = r.model.hash;
    saveState(r.paths.state, st);
    console.log(instruction(r.paths.html, st, process.env.CLAUDE_SESSION_ID || ''));
    return;
  }
  if (cmd === 'render') {
    const root = path.resolve(arg || projectDir({}));
    const r = renderProject(root, opts);
    if (!r) { console.error(`No backlog/config.yml under ${root}`); process.exitCode = 1; return; }
    const counts = {};
    for (const t of r.model.tasks) counts[t.state] = (counts[t.state] || 0) + 1;
    console.log(JSON.stringify({ project: r.model.project, states: r.model.states, defaultOn: r.model.defaultOn, tasks: r.model.tasks.length, counts, bytes: r.bytes, html: r.paths.html }, null, 2));
    return;
  }
  console.error('usage: backlog-artifact.mjs stop|record|instructions|render [dir]');
  process.exitCode = 2;
}

if (process.argv[1] && samePath(process.argv[1], fileURLToPath(import.meta.url))) {
  try { main(); } catch (e) {
    // A hook must never break the session: log and exit cleanly.
    console.error(`backlog-artifact: ${e && e.stack || e}`);
  }
}
