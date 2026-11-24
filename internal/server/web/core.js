// Shared helpers, state and API client for the ZPilot dashboard.
// Untrusted text is always inserted with textContent; the markdown renderer
// builds DOM nodes and never assigns HTML strings.

export const $ = (sel, root = document) => root.querySelector(sel);

export function h(tag, props = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'style') el.style.cssText = v;
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k in el && typeof v !== 'string') el[k] = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

// ---------------------------------------------------------------- markdown
// A small, safe renderer: headings, lists, quotes, fenced code, tables,
// inline code, bold, italics and http(s) links. Everything becomes DOM nodes.
export function md(text) {
  const root = h('div', { class: 'md' });
  const lines = String(text ?? '').replace(/\r\n/g, '\n').split('\n');
  let i = 0;
  const para = [];
  const flush = () => { if (para.length) { root.append(h('p', {}, inline(para.join('\n')))); para.length = 0; } };
  while (i < lines.length) {
    const line = lines[i];
    const fence = line.match(/^```\s*([\w+-]*)\s*$/);
    if (fence) {
      flush();
      const code = [];
      i++;
      while (i < lines.length && !/^```\s*$/.test(lines[i])) code.push(lines[i++]);
      i++;
      const pre = h('pre', {}, h('code', {}, code.join('\n')));
      pre.append(h('button', { class: 'small copy', onclick: (e) => { e.stopPropagation(); copyText(code.join('\n')); } }, 'Copy'));
      root.append(pre);
      continue;
    }
    const hd = line.match(/^(#{1,4})\s+(.*)$/);
    if (hd) { flush(); root.append(h('h' + hd[1].length, {}, inline(hd[2]))); i++; continue; }
    if (/^\s*([-*_])\s*\1\s*\1[\s\1]*$/.test(line)) { flush(); root.append(h('hr')); i++; continue; }
    if (/^>\s?/.test(line)) {
      flush();
      const q = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) q.push(lines[i++].replace(/^>\s?/, ''));
      root.append(h('blockquote', {}, inline(q.join('\n'))));
      continue;
    }
    if (/^\s*([-*+]|\d+[.)])\s+/.test(line)) {
      flush();
      const ordered = /^\s*\d+[.)]/.test(line);
      const list = h(ordered ? 'ol' : 'ul');
      while (i < lines.length && /^\s*([-*+]|\d+[.)])\s+/.test(lines[i])) {
        let item = lines[i++].replace(/^\s*([-*+]|\d+[.)])\s+/, '');
        while (i < lines.length && /^\s{2,}\S/.test(lines[i]) && !/^\s*([-*+]|\d+[.)])\s+/.test(lines[i])) item += '\n' + lines[i++].trim();
        list.append(h('li', {}, inline(item)));
      }
      root.append(list);
      continue;
    }
    if (/^\s*\|.*\|\s*$/.test(line) && i + 1 < lines.length && /^\s*\|?\s*:?-{2,}/.test(lines[i + 1])) {
      flush();
      const cells = (l) => l.trim().replace(/^\||\|$/g, '').split('|').map((c) => c.trim());
      const head = cells(line);
      i += 2;
      const tbody = h('tbody');
      while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) tbody.append(h('tr', {}, cells(lines[i++]).map((c) => h('td', {}, inline(c)))));
      root.append(h('table', {}, h('thead', {}, h('tr', {}, head.map((c) => h('th', {}, inline(c))))), tbody));
      continue;
    }
    if (line.trim() === '') { flush(); i++; continue; }
    para.push(line);
    i++;
  }
  flush();
  return root;
}

function inline(text) {
  const out = [];
  const re = /(`[^`\n]+`)|(\*\*[^*\n]+\*\*)|(\*[^*\s][^*\n]*\*)|(\[[^\]\n]+\]\(https?:\/\/[^\s)]+\))|(https?:\/\/[^\s<>()]+[^\s<>().,;:!?'"])/g;
  let last = 0, m;
  while ((m = re.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index));
    const t = m[0];
    if (m[1]) out.push(h('code', {}, t.slice(1, -1)));
    else if (m[2]) out.push(h('b', {}, t.slice(2, -2)));
    else if (m[3]) out.push(h('i', {}, t.slice(1, -1)));
    else if (m[4]) { const [, label, href] = t.match(/^\[([^\]]+)\]\((.+)\)$/); out.push(h('a', { href, target: '_blank', rel: 'noopener noreferrer' }, label)); }
    else out.push(h('a', { href: t, target: '_blank', rel: 'noopener noreferrer' }, t));
    last = m.index + t.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

export function copyText(t) {
  navigator.clipboard?.writeText(t).then(() => toast('Copied', 'ok'), () => toast('Copy failed', 'bad'));
}

// ---------------------------------------------------------------- time
export function ago(iso) {
  if (!iso || String(iso).startsWith('0001')) return '—';
  const d = (Date.now() - new Date(iso).getTime()) / 1000;
  if (d < -60) return 'in ' + dur(-d);
  if (d < 45) return 'just now';
  if (d < 3600) return Math.round(d / 60) + 'm ago';
  if (d < 86400) return Math.round(d / 3600) + 'h ago';
  if (d < 7 * 86400) return Math.round(d / 86400) + 'd ago';
  return new Date(iso).toLocaleDateString();
}
export function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return sec + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm ' + (sec % 60) + 's';
  return Math.floor(sec / 3600) + 'h ' + Math.floor((sec % 3600) / 60) + 'm';
}
export const when = (iso) => (iso && !String(iso).startsWith('0001') ? new Date(iso).toLocaleString() : '—');
export const isSet = (iso) => iso && !String(iso).startsWith('0001');

// ---------------------------------------------------------------- feedback
export function toast(msg, kind = '') {
  let box = $('.toasts');
  if (!box) document.body.append((box = h('div', { class: 'toasts', role: 'status', 'aria-live': 'polite' })));
  const t = h('div', { class: 'toast ' + kind }, h('span', {}, kind === 'bad' ? '⚠️' : kind === 'ok' ? '✅' : 'ℹ️'), h('div', {}, msg));
  box.append(t);
  setTimeout(() => t.remove(), kind === 'bad' ? 7000 : 3200);
}

export function modal(title, body, actions = [], size = '') {
  const prev = document.activeElement;
  const close = () => { ov.remove(); document.removeEventListener('keydown', onKey); prev?.focus?.(); };
  const ov = h('div', { class: 'overlay', onmousedown: (e) => e.target === ov && close() },
    h('div', { class: 'modal ' + size, role: 'dialog', 'aria-modal': 'true', 'aria-label': title },
      h('div', { class: 'modal-head' }, h('h2', {}, title), h('span', { class: 'spacer' }), h('button', { class: 'ghost icon', onclick: close, 'aria-label': 'Close' }, '✕')),
      h('div', { class: 'modal-body' }, body),
      actions.length ? h('div', { class: 'modal-foot' }, actions.filter(Boolean).map((a) => (typeof a === 'function' ? a(close) : a))) : null));
  const onKey = (e) => { if (e.key === 'Escape') close(); };
  document.addEventListener('keydown', onKey);
  document.body.append(ov);
  setTimeout(() => ov.querySelector('input:not([disabled]),textarea,select')?.focus(), 30);
  return close;
}

export function confirmDialog(title, text, okLabel = 'Confirm', danger = false) {
  return new Promise((resolve) => {
    modal(title, h('p', { style: 'margin:0' }, text), [
      (c) => h('button', { onclick: () => { c(); resolve(false); } }, 'Cancel'),
      (c) => h('button', { class: danger ? 'danger solid' : 'primary', onclick: () => { c(); resolve(true); } }, okLabel),
    ], 'narrow');
  });
}

export function promptDialog(title, label, placeholder = '', multiline = false) {
  return new Promise((resolve) => {
    const input = multiline ? h('textarea', { rows: 4, placeholder }) : h('input', { placeholder });
    modal(title, h('label', { class: 'field' }, label, input), [
      (c) => h('button', { onclick: () => { c(); resolve(null); } }, 'Cancel'),
      (c) => h('button', { class: 'primary', onclick: () => { c(); resolve(input.value); } }, 'OK'),
    ], 'narrow');
  });
}

// ---------------------------------------------------------------- API
export class ApiError extends Error { constructor(status, msg) { super(msg); this.status = status; } }
let onUnauthorized = () => {};
export const setUnauthorizedHandler = (fn) => { onUnauthorized = fn; };

export async function api(method, path, body) {
  const opt = { method, headers: { 'X-Requested-With': 'zpilot' }, credentials: 'same-origin' };
  if (body !== undefined) { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  let r;
  try { r = await fetch(path, opt); } catch { throw new ApiError(0, 'ZPilot is not reachable. Is the daemon running?'); }
  let data = null;
  try { data = await r.json(); } catch { /* empty */ }
  if (r.status === 401 && path !== '/api/login') { onUnauthorized(); throw new ApiError(401, 'Please sign in'); }
  if (!r.ok) throw new ApiError(r.status, (data && data.error) || `HTTP ${r.status}`);
  return data;
}

export async function act(fn, okMsg) {
  try { const v = await fn(); if (okMsg) toast(okMsg, 'ok'); return v; } catch (e) { toast(e.message, 'bad'); throw e; }
}

// ---------------------------------------------------------------- state
export const S = {
  agents: [], models: [], settings: {}, overview: {}, session: {}, live: false,
  liveRuns: {},  // run_id -> {text, steps[], thread_id, username, state}
  approvals: [], // pending command approvals
};
export const role = () => S.session?.user?.role || 'viewer';
export const can = (need) => ['viewer', 'operator', 'admin'].indexOf(role()) >= ['viewer', 'operator', 'admin'].indexOf(need);

export async function loadCore() {
  const [agents, models, overview] = await Promise.all([api('GET', '/api/agents'), api('GET', '/api/models'), api('GET', '/api/overview')]);
  S.agents = agents; S.models = models; S.overview = overview; S.settings = overview.settings;
}
export async function loadApprovals() {
  try { S.approvals = await api('GET', '/api/approvals?pending=1'); } catch { S.approvals = []; }
}
export const agentBy = (u) => S.agents.find((a) => a.username === String(u || '').replace(/^@/, ''));
export const levelInfo = {
  restricted: 'Reads files in its workspace. No changes, no commands.',
  standard: 'Reads and writes files in its workspace. No commands.',
  trusted: 'Workspace files plus shell commands with your OS permissions.',
  full: 'Commands and files anywhere your user can reach.',
};
export function avatar(a, size = '') {
  const busy = a && (a.running > 0 || Object.values(S.liveRuns).some((r) => r.username === a.username));
  return h('div', { class: 'avatar ' + size, style: `background:${a?.color || '#6366f1'}`, title: a ? '@' + a.username : '' }, a?.icon || '🤖', busy ? h('span', { class: 'busy' }) : null);
}
export const levelBadge = (l) => h('span', { class: 'badge lvl-' + l }, l);

export function page(title, subtitle, actions, ...body) {
  return h('div', { class: 'page' },
    h('div', { class: 'page-head' }, h('div', {}, h('h1', {}, title), subtitle ? h('p', {}, subtitle) : null), h('div', { class: 'row' }, actions)),
    body);
}

export function setupBanner(go) {
  if (S.models.length || !can('admin')) return null;
  return h('div', { class: 'banner warn' }, h('span', {}, '👋'), h('div', { class: 'spacer' }, h('b', {}, 'Welcome to ZPilot. '), 'Add a model (Gemini, OpenAI-compatible, Anthropic, Ollama…) so your agents can think.'),
    h('button', { class: 'primary', onclick: () => go('models') }, 'Add a model'));
}

export function emptyState(icon, title, text, action) {
  return h('div', { class: 'card empty' }, h('div', { class: 'big' }, icon), h('b', {}, title), text ? h('p', {}, text) : null, action);
}

export function skeleton(n = 3) {
  return h('div', { class: 'stack' }, Array.from({ length: n }, () => h('div', { class: 'skeleton', style: 'height:64px' })));
}

export function toggle(checked, onchange, label) {
  const input = h('input', { type: 'checkbox', checked, onchange: (e) => onchange(e.target.checked) });
  const sw = h('label', { class: 'switch' }, input, h('span'));
  return label ? h('label', { class: 'check', style: 'align-items:center' }, sw, h('span', {}, label)) : sw;
}

export function chipSet(values, options, labelFn = (x) => x, { exclusiveStar = true } = {}) {
  const box = h('div', { class: 'chips' });
  const draw = () => box.replaceChildren(...options.map((o) => h('span', { class: 'chip' + (values.includes(o) ? ' on' : ''), role: 'checkbox', 'aria-checked': values.includes(o) ? 'true' : 'false', tabindex: 0,
    onkeydown: (e) => { if (e.key === ' ' || e.key === 'Enter') { e.preventDefault(); e.target.click(); } },
    onclick: () => {
      const i = values.indexOf(o);
      if (i >= 0) values.splice(i, 1); else values.push(o);
      if (exclusiveStar) {
        if (o === '*' && values.includes('*')) values.splice(0, values.length, '*');
        else if (o !== '*') { const s = values.indexOf('*'); if (s >= 0) values.splice(s, 1); }
      }
      draw();
    } }, labelFn(o))));
  draw();
  return box;
}

// ---------------------------------------------------------------- live events
const listeners = new Set();
export const onEvent = (fn) => { listeners.add(fn); return () => listeners.delete(fn); };
export const clearListeners = () => listeners.clear();
let es = null;
let globalHandler = () => {};
export const setGlobalEventHandler = (fn) => { globalHandler = fn; };

export function connectEvents() {
  if (es) es.close();
  es = new EventSource('/api/events');
  es.onopen = () => { S.live = true; globalHandler({ type: 'connection' }); refreshLive(); };
  es.onerror = () => { S.live = false; globalHandler({ type: 'connection' }); };
  es.onmessage = (e) => {
    const ev = JSON.parse(e.data);
    if (ev.type === 'live') {
      const d = ev.data;
      const r = (S.liveRuns[d.run_id] ||= { text: '', steps: [], thread_id: d.thread_id, username: d.username, state: '' });
      if (d.kind === 'text') r.text += d.text || '';
      if (d.kind === 'tool_run') { r.steps.push(((d.name || '') + ' ' + (d.detail || '')).trim()); r.steps = r.steps.slice(-30); r.text = ''; }
      if (d.kind === 'state') r.state = d.state;
    }
    if (ev.type === 'run' && ev.data.state !== 'running') delete S.liveRuns[ev.data.id];
    globalHandler(ev);
    for (const fn of listeners) { try { fn(ev); } catch (err) { console.error(err); } }
  };
}
export function disconnectEvents() { if (es) { es.close(); es = null; } }
export async function refreshLive() {
  try {
    const live = await api('GET', '/api/live');
    S.liveRuns = Object.fromEntries(live.map((r) => [r.run_id, { text: r.text, steps: r.steps || [], thread_id: r.thread_id, username: r.username, state: r.state }]));
  } catch { /* ignore */ }
}

// ---------------------------------------------------------------- notifications
export function browserNotify(title, body, onclick) {
  if (!('Notification' in window) || Notification.permission !== 'granted' || document.visibilityState === 'visible') return;
  if (localStorage.getItem('zpilot.notify') === 'off') return;
  try {
    const n = new Notification(title, { body: String(body || '').slice(0, 200), icon: 'favicon.svg', tag: title });
    n.onclick = () => { window.focus(); onclick?.(); n.close(); };
  } catch { /* ignore */ }
}
