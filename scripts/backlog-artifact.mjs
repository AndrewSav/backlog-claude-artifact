#!/usr/bin/env node
// backlog-artifact: render a Backlog.md backlog into a single HTML page and
// ask Claude to publish it as an Artifact when it has changed.
//
// Subcommands:
//   stop          Stop hook. Renders; if the backlog changed: in server mode uploads it
//                 directly (no model involvement); in artifact mode, when the throttle
//                 allows, blocks the stop with an instruction to publish.
//   record        PostToolUse hook for the Artifact tool. Saves the artifact URL.
//   instructions  Renders and prints the publish instruction (used by /backlog-artifact:publish).
//   render [dir]  Renders and prints a summary. For testing.

import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';
import os from 'node:os';
import { spawnSync } from 'node:child_process';

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
  const str = (key) => String(get(key) ?? '').trim();
  return {
    includeDrafts: bool('include_drafts', false),
    includeArchived: bool('include_archived', false),
    autoPublish: bool('auto_publish', true),
    intervalMinutes: num('publish_interval_minutes', 15),
    publishTarget: str('publish_target').toLowerCase() === 'server' ? 'server' : 'artifact',
    serverUrl: str('server_url').replace(/\/+$/, ''),
    serverKey: str('server_key'),
    sshKey: str('ssh_key'),
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

// Block scalars: `key: >-` (folded) or `key: |` (literal), then indented lines.
// Backlog.md writes long titles as `title: >-`. Trailing newlines are dropped:
// every value here is displayed, never written back.
function blockScalar(indicator, lines) {
  const indents = lines.filter((l) => l.trim()).map((l) => l.match(/^ */)[0].length);
  const body = lines.map((l) => l.slice(indents.length ? Math.min(...indents) : 0));
  if (indicator[0] === '|') return body.join('\n').trimEnd();
  return body.reduce((acc, l) => (l === '' ? acc + '\n' : acc === '' || acc.endsWith('\n') ? acc + l : `${acc} ${l}`), '').trimEnd();
}

export function parseYaml(text) {
  const obj = {};
  let listKey = null;
  const all = text.split(/\r?\n/);
  for (let i = 0; i < all.length; i++) {
    const line = all[i];
    const block = line.match(/^([A-Za-z_][\w-]*):\s+([>|][-+]?)\s*$/);
    if (block) {
      const body = [];
      while (i + 1 < all.length && (/^\s+\S/.test(all[i + 1]) || all[i + 1].trim() === '')) body.push(all[++i]);
      obj[block[1]] = blockScalar(block[2], body);
      listKey = null;
      continue;
    }
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
  const m = text.match(/^\uFEFF?---\r?\n([\s\S]*?)\r?\n---\r?\n?([\s\S]*)$/);
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
  // The renderer and template are part of the output, so a plugin update republishes.
  hash.update(fs.readFileSync(TEMPLATE_PATH));
  hash.update(fs.readFileSync(fileURLToPath(import.meta.url)));
  const home = opts.publishTarget === 'server' && opts.serverUrl ? opts.serverUrl + '/' : '';
  hash.update(JSON.stringify({ d: opts.includeDrafts, a: opts.includeArchived, t: opts.publishTarget, home }));
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
    home,
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
  parts.push('. Do not edit the HTML file. After the publish succeeds, end the turn with no further text. If this session has no Artifact tool, end the turn without comment.');
  return parts.join('');
}

// ---------- server publishing ----------

export function slugify(name) {
  const s = String(name).toLowerCase().normalize('NFKD').replace(/[\u0300-\u036f]/g, '').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 64).replace(/-+$/, '');
  return s || 'backlog';
}

// Must match signedMessage in server/auth_publish.go.
export function signedMessage(method, slug, ts, body) {
  const sum = crypto.createHash('sha256').update(body).digest('hex');
  return `backlog-publish\n${method}\n${slug}\n${ts}\n${sum}`;
}

// Windows ships its OpenSSH (whose ssh-agent listens on a named pipe) in
// Program Files or System32; Git Bash's copies on PATH can't reach that agent.
function sshTool(name) {
  if (process.platform === 'win32') {
    for (const dir of [
      path.join(process.env.ProgramFiles || 'C:\\Program Files', 'OpenSSH'),
      path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'OpenSSH'),
    ]) {
      const p = path.join(dir, `${name}.exe`);
      if (fs.existsSync(p)) return p;
    }
  }
  return name;
}

// Signs with `ssh-keygen -Y sign`. Given a public key, ssh-keygen asks
// ssh-agent to sign with the matching private key, so the private key is
// never read here and no passphrase is needed. Returns the armored signature.
export function sshSign(message, opts) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'backlog-sign-'));
  try {
    let keyFile = opts.sshKey;
    if (keyFile) {
      if (!keyFile.endsWith('.pub') && fs.existsSync(keyFile + '.pub')) keyFile += '.pub';
      if (!fs.existsSync(keyFile)) throw new Error(`the ssh_key option points to ${keyFile}, which does not exist`);
    } else {
      const r = spawnSync(sshTool('ssh-add'), ['-L'], { encoding: 'utf8', timeout: 10_000, windowsHide: true });
      const line = (r.stdout || '').split(/\r?\n/).find((l) => /^(ssh-|ecdsa-|sk-)/.test(l));
      if (!line) throw new Error('ssh-agent has no keys (ssh-add -L listed none). Load a key with ssh-add, or set the ssh_key or server_key option');
      keyFile = path.join(tmp, 'agent-key.pub');
      fs.writeFileSync(keyFile, line + '\n');
    }
    const msgFile = path.join(tmp, 'message');
    fs.writeFileSync(msgFile, message);
    const r = spawnSync(sshTool('ssh-keygen'), ['-Y', 'sign', '-n', 'backlog-artifact', '-f', keyFile, msgFile], {
      encoding: 'utf8', timeout: 15_000, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'],
    });
    if (r.status !== 0 || !fs.existsSync(msgFile + '.sig')) {
      const why = (r.stderr || (r.error && r.error.message) || 'no output').trim().split(/\r?\n/).pop();
      throw new Error(`ssh-keygen could not sign (${why})`);
    }
    return fs.readFileSync(msgFile + '.sig', 'utf8');
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
}

// Sends an authorized request for one project: a Bearer token if server_key
// is set, otherwise an SSH signature over the method, slug, time and body.
async function serverRequest(method, slug, body, opts, { fetchImpl = fetch, sign = sshSign, now = Date.now() } = {}) {
  if (!opts.serverUrl) throw new Error('the server_url option is empty');
  const headers = body ? { 'Content-Type': 'application/json' } : {};
  if (opts.serverKey) {
    headers.Authorization = `Bearer ${opts.serverKey}`;
  } else {
    const ts = String(Math.floor(now / 1000));
    headers['X-Backlog-Timestamp'] = ts;
    headers['X-Backlog-Signature'] = Buffer.from(sign(signedMessage(method, slug, ts, body), opts)).toString('base64');
  }
  let res;
  try {
    res = await fetchImpl(`${opts.serverUrl}/api/projects/${slug}`, { method, headers, body: body || undefined, signal: AbortSignal.timeout(10_000) });
  } catch (e) {
    throw new Error(`could not reach ${opts.serverUrl} (${(e && e.cause && e.cause.code) || (e && e.message) || e})`);
  }
  const text = await res.text();
  let json = {};
  try { json = JSON.parse(text); } catch { /* not JSON */ }
  const fail = () => new Error(`the server answered ${res.status}: ${json.error || text.slice(0, 200)}`);
  return { res, json, fail };
}

export async function uploadToServer(r, opts, deps = {}) {
  const slug = slugify(r.model.project);
  const { hash, ...data } = r.model;
  const body = JSON.stringify({ project: r.model.project, html: fs.readFileSync(r.paths.html, 'utf8'), data });
  const { res, json, fail } = await serverRequest('PUT', slug, body, opts, deps);
  if (!res.ok) throw fail();
  return json.url || `${opts.serverUrl}/p/${slug}`;
}

// Removes a project's page. Returns false if the server had no such project.
export async function deleteFromServer(slug, opts, deps = {}) {
  const { res, fail } = await serverRequest('DELETE', slug, '', opts, deps);
  if (res.status === 404) return false;
  if (!res.ok) throw fail();
  return true;
}

const RETRY_AFTER_FAILURE_MS = 60_000;

// Uploads if the backlog changed, records the outcome, and returns a message
// for the user when there is something new to say (first publish, or a new
// error). Never asks Claude for anything.
export async function publishToServer(r, opts, { now = Date.now(), force = false, upload = uploadToServer } = {}) {
  const st = loadState(r.paths.state);
  if (!force) {
    if (st.lastPublishedHash === r.model.hash) return { why: 'unchanged' };
    if (st.lastError && now - (st.lastAttemptAt || 0) < RETRY_AFTER_FAILURE_MS) return { why: 'backoff' };
  }
  st.lastAttemptAt = now;
  try {
    const url = await upload(r, opts, { now });
    const first = st.serverPageUrl !== url;
    Object.assign(st, { serverPageUrl: url, lastPublishedHash: r.model.hash, lastPublishedAt: now });
    delete st.lastError;
    saveState(r.paths.state, st);
    return { why: 'published', url, message: first ? `Backlog published: ${url}` : undefined };
  } catch (e) {
    const msg = String((e && e.message) || e);
    try { fs.appendFileSync(path.join(r.paths.dir, 'publish.log'), `${new Date(now).toISOString()} ${msg}\n`); } catch { /* ignore */ }
    const repeat = st.lastError === msg;
    st.lastError = msg;
    saveState(r.paths.state, st);
    return { why: 'failed', error: msg, message: repeat ? undefined : `backlog-artifact: could not publish the backlog: ${msg}` };
  }
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
  // Opted out with /backlog-artifact:unpublish: leave the project alone entirely.
  if (loadState(outPaths(root).state).disabled) return { action: 'none', why: 'disabled' };
  const r = renderProject(root, opts);
  if (!opts.autoPublish) return { action: 'none', why: 'auto_publish off', r };
  // Server uploads cost no model tokens, so they need no throttle and work headless too.
  if (opts.publishTarget === 'server') return { action: 'upload', r };
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

// Turns publishing off for a project and removes what it published. The
// server page is deleted here; only Claude can delete an artifact, so for
// that the returned text tells Claude what to call.
export async function unpublishProject(root, opts, { remove = deleteFromServer } = {}) {
  const bl = findBacklog(root);
  const p = outPaths(root);
  const st = loadState(p.state);
  st.disabled = true;
  delete st.lastPublishedHash;
  delete st.pendingHash;
  const lines = [];
  if (bl && (opts.publishTarget === 'server' || st.serverPageUrl)) {
    const slug = slugify(buildModel(bl, opts).project);
    try {
      lines.push(await remove(slug, opts)
        ? `Removed /p/${slug} from ${opts.serverUrl}.`
        : `${opts.serverUrl} has no /p/${slug}, so there was nothing to remove.`);
      delete st.serverPageUrl;
    } catch (e) {
      lines.push(`Could not remove /p/${slug} from the server: ${(e && e.message) || e}.`);
    }
  }
  if (st.url) {
    lines.push(`This project was also published as the artifact ${st.url}. Delete it with the Artifact tool: action "delete", url "${st.url}".`);
    delete st.url;
  }
  saveState(p.state, st);
  lines.push('Publishing is now off for this project. /backlog-artifact:publish turns it back on.');
  return lines.join('\n');
}

async function main() {
  const [cmd, arg] = process.argv.slice(2);
  const opts = readOptions();
  if (cmd === 'stop') {
    const input = readStdin();
    const d = decideStop({ input, root: projectDir(input), opts });
    if (d.action === 'block') process.stdout.write(JSON.stringify({ decision: 'block', reason: d.reason }));
    if (d.action === 'upload') {
      const res = await publishToServer(d.r, opts);
      if (res.message) process.stdout.write(JSON.stringify({ systemMessage: res.message }));
    }
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
    const was = loadState(r.paths.state);
    if (was.disabled) { delete was.disabled; saveState(r.paths.state, was); }
    if (opts.publishTarget === 'server') {
      const res = await publishToServer(r, opts, { force: true });
      console.log(res.url
        ? `The backlog-artifact plugin already published the backlog to ${res.url}. Tell the user that URL in one line; do not call the Artifact tool.`
        : `Publishing the backlog to the server failed: ${res.error}. Tell the user that in one line; do not call the Artifact tool.`);
      return;
    }
    const st = loadState(r.paths.state);
    st.pendingHash = r.model.hash;
    saveState(r.paths.state, st);
    console.log(instruction(r.paths.html, st, process.env.CLAUDE_SESSION_ID || ''));
    return;
  }
  if (cmd === 'unpublish') {
    if (arg === '--slug') {
      // Removes a page by its address, for a project that isn't on this machine.
      const slug = process.argv[4];
      if (!slug) { console.error('usage: backlog-artifact.mjs unpublish --slug <slug>'); process.exitCode = 2; return; }
      console.log(await deleteFromServer(slug, opts) ? `Removed /p/${slug} from ${opts.serverUrl}.` : `${opts.serverUrl} has no /p/${slug}.`);
      return;
    }
    console.log(await unpublishProject(path.resolve(arg || projectDir({})), opts));
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
  console.error('usage: backlog-artifact.mjs stop|record|instructions|unpublish|render [dir]');
  process.exitCode = 2;
}

if (process.argv[1] && samePath(process.argv[1], fileURLToPath(import.meta.url))) {
  main().catch((e) => {
    // A hook must never break the session: log and exit cleanly.
    console.error(`backlog-artifact: ${e && e.stack || e}`);
  });
}
