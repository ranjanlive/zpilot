// ZPilot dashboard entry point: shell, routing, command palette,
// shortcuts, sign-in and global live updates.
import { h, $, api, S, loadCore, loadApprovals, connectEvents, disconnectEvents, setGlobalEventHandler, setUnauthorizedHandler, clearListeners,
  agentBy, can, toast, browserNotify } from './core.js';
import { viewChat } from './chat.js';
import { viewAgents } from './agents.js';
import { viewWorkflows } from './workflows.js';
import { viewModels, viewTools, viewSkills, viewSchedules, viewChannels } from './config.js';
import { viewHome, viewInbox, viewActivity, viewAudit, viewUsers, viewSettings } from './admin.js';

const routes = [
  { path: 'home', label: 'Home', ico: '🏠', group: 'Work', view: viewHome, key: 'h' },
  { path: 'chat', label: 'Conversations', ico: '💬', group: 'Work', view: viewChat, key: 'c' },
  { path: 'inbox', label: 'Needs you', ico: '🔔', group: 'Work', view: viewInbox, key: 'i', count: () => S.approvals.length + (S.overview.pending_proposals || 0), hot: true },
  { path: 'agents', label: 'Agents', ico: '🤖', group: 'Work', view: viewAgents, key: 'a' },
  { path: 'workflows', label: 'Workflows', ico: '⚙️', group: 'Work', view: viewWorkflows, key: 'w' },
  { path: 'schedules', label: 'Schedules', ico: '⏰', group: 'Work', view: viewSchedules, key: 's' },
  { path: 'activity', label: 'Activity', ico: '📈', group: 'Work', view: viewActivity, count: () => S.overview.running },
  { path: 'tools', label: 'Tools', ico: '🧰', group: 'Build', view: viewTools, key: 't' },
  { path: 'skills', label: 'Skills', ico: '📚', group: 'Build', view: viewSkills },
  { path: 'models', label: 'Models', ico: '🧠', group: 'Build', view: viewModels, key: 'm' },
  { path: 'channels', label: 'Channels', ico: '📡', group: 'Build', view: viewChannels },
  { path: 'users', label: 'Users', ico: '👥', group: 'System', view: viewUsers, admin: true },
  { path: 'audit', label: 'Audit log', ico: '🛡️', group: 'System', view: viewAudit, admin: true },
  { path: 'settings', label: 'Settings', ico: '🔧', group: 'System', view: viewSettings },
];

function current() {
  const [path, ...rest] = location.hash.replace(/^#\/?/, '').split('/');
  return { route: routes.find((r) => r.path === path) || routes[0], args: rest.filter(Boolean).map(decodeURIComponent) };
}
const go = (p) => { if (location.hash !== '#/' + p) location.hash = '#/' + p; else render(); };

function applyTheme() {
  const t = localStorage.getItem('zpilot.theme') || 'system';
  if (t === 'system') document.documentElement.removeAttribute('data-theme'); else document.documentElement.setAttribute('data-theme', t);
}

// ---------------------------------------------------------------- shell
function renderNav() {
  const nav = $('#nav');
  if (!nav) return;
  const { route } = current();
  nav.replaceChildren();
  let group = '';
  for (const r of routes) {
    if (r.admin && !can('admin')) continue;
    if (r.group !== group) { group = r.group; nav.append(h('div', { class: 'group' }, group)); }
    const n = r.count ? r.count() : 0;
    nav.append(h('a', { href: '#/' + r.path, class: r === route ? 'active' : '', 'aria-current': r === route ? 'page' : null, onclick: () => document.body.classList.remove('nav-open') },
      h('span', { class: 'ico' }, r.ico), r.label, n ? h('span', { class: 'count' + (r.hot ? ' hot' : '') }, n) : null));
  }
  const t = $('#top-title');
  if (t) t.textContent = route.label;
  const need = S.approvals.length + (S.overview.pending_proposals || 0);
  document.title = (need ? `(${need}) ` : '') + 'ZPilot';
}
function updateLive() {
  const d = $('#live-dot'); if (d) d.className = 'dot' + (S.live ? ' live' : '');
  const t = $('#live-text'); if (t) t.textContent = S.live ? 'Live' : 'Reconnecting…';
}

let cleanup = null;
let renderSeq = 0;
async function render() {
  const seq = ++renderSeq;
  const { route, args } = current();
  renderNav();
  if (cleanup) { try { cleanup(); } catch { /* ignore */ } cleanup = null; }
  clearListeners();
  const main = $('#main');
  if (!main) return;
  if (route.admin && !can('admin')) { main.replaceChildren(h('div', { class: 'page' }, h('div', { class: 'banner bad' }, 'Only admins can open this page.'))); return; }
  try {
    const out = await route.view(args, { go, rerender: render, applyTheme });
    if (seq !== renderSeq) { out?.cleanup?.(); return; } // a newer navigation won
    main.style.overflow = out.fullHeight ? 'hidden' : 'auto';
    main.replaceChildren(out.el || out);
    if (out.cleanup) cleanup = out.cleanup;
  } catch (e) {
    if (e.status === 401 || seq !== renderSeq) return;
    main.replaceChildren(h('div', { class: 'page' }, h('div', { class: 'banner bad' }, 'Could not load this page: ' + e.message), h('button', { onclick: render }, 'Try again')));
  }
}

// ---------------------------------------------------------------- command palette
function openPalette() {
  if ($('.palette')) return;
  const input = h('input', { placeholder: 'Jump to a page, talk to an agent, open a workflow…', 'aria-label': 'Command' });
  const list = h('div', { class: 'list', role: 'listbox' });
  let items = [], sel = 0, workflows = null, threads = null;
  api('GET', '/api/workflows').then((w) => { workflows = w; draw(); }).catch(() => {});
  api('GET', '/api/threads').then((t) => { threads = t; draw(); }).catch(() => {});
  const close = () => { pal.remove(); document.removeEventListener('keydown', onKey, true); };
  const run = (it) => { close(); it.run(); };
  function draw() {
    const q = input.value.toLowerCase().trim();
    const all = [];
    for (const r of routes) if (!r.admin || can('admin')) all.push({ grp: 'Go to', label: r.label, ico: r.ico, k: r.key ? 'g ' + r.key : '', run: () => go(r.path) });
    if (can('operator')) for (const a of S.agents.filter((x) => !x.disabled)) all.push({ grp: 'Talk to', label: `${a.name}  @${a.username}`, ico: a.icon, run: () => { sessionStorage.setItem('zpilot.chat.agent', a.username); go('chat/new'); } });
    for (const a of S.agents) all.push({ grp: 'Agents', label: `Open ${a.name}`, ico: a.icon, run: () => go('agents/' + a.username) });
    for (const w of workflows || []) all.push({ grp: 'Workflows', label: w.name, ico: '⚙️', run: () => go('workflows/' + w.id) });
    for (const t of (threads || []).slice(0, 30)) all.push({ grp: 'Conversations', label: t.title, ico: '💬', run: () => go('chat/' + t.id) });
    if (can('admin')) {
      all.push({ grp: 'Create', label: 'New agent', ico: '＋', run: () => go('agents/new') });
      all.push({ grp: 'Create', label: 'New workflow', ico: '＋', run: () => go('workflows/new') });
    }
    all.push({ grp: 'Preferences', label: 'Toggle dark / light theme', ico: '🌓', run: () => {
      const dark = document.documentElement.getAttribute('data-theme') === 'dark' || (!document.documentElement.getAttribute('data-theme') && matchMedia('(prefers-color-scheme: dark)').matches);
      localStorage.setItem('zpilot.theme', dark ? 'light' : 'dark'); applyTheme();
    } });
    items = all.filter((it) => !q || it.label.toLowerCase().includes(q) || it.grp.toLowerCase().includes(q)).slice(0, 40);
    sel = Math.min(sel, Math.max(0, items.length - 1));
    const out = [];
    let g = '';
    items.forEach((it, i) => {
      if (it.grp !== g) { g = it.grp; out.push(h('div', { class: 'grp' }, g)); }
      out.push(h('div', { class: 'item' + (i === sel ? ' sel' : ''), role: 'option', onmousemove: () => { if (sel !== i) { sel = i; draw(); } }, onclick: () => run(it) },
        h('span', {}, it.ico), h('span', { class: 'clip' }, it.label), it.k ? h('span', { class: 'k' }, h('kbd', {}, it.k)) : null));
    });
    list.replaceChildren(...(out.length ? out : [h('div', { class: 'hint', style: 'padding:12px' }, 'No matches')]));
    list.querySelector('.sel')?.scrollIntoView({ block: 'nearest' });
  }
  const onKey = (e) => {
    if (e.key === 'Escape') { e.preventDefault(); close(); }
    if (e.key === 'ArrowDown') { e.preventDefault(); sel = Math.min(items.length - 1, sel + 1); draw(); }
    if (e.key === 'ArrowUp') { e.preventDefault(); sel = Math.max(0, sel - 1); draw(); }
    if (e.key === 'Enter' && items[sel]) { e.preventDefault(); run(items[sel]); }
  };
  input.addEventListener('input', () => { sel = 0; draw(); });
  const pal = h('div', { class: 'palette', onmousedown: (e) => e.target === pal && close() }, h('div', { class: 'box', role: 'dialog', 'aria-label': 'Command palette' }, input, list));
  document.body.append(pal);
  document.addEventListener('keydown', onKey, true);
  draw();
  input.focus();
}

// Shortcuts: Ctrl/Cmd+K palette, "g" then a letter to jump, "/" to search.
let gPending = 0;
document.addEventListener('keydown', (e) => {
  const typing = ['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement?.tagName) || document.activeElement?.isContentEditable;
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); openPalette(); return; }
  if (typing || e.ctrlKey || e.metaKey || e.altKey || !$('#main')) return;
  if (e.key === '/') { const s = $('#main input[type=search]'); if (s) { e.preventDefault(); s.focus(); } return; }
  if (e.key === 'g') { gPending = Date.now(); return; }
  if (Date.now() - gPending < 1200) { const r = routes.find((x) => x.key === e.key); if (r) { e.preventDefault(); go(r.path); } gPending = 0; }
});

// ---------------------------------------------------------------- global events
let navTimer = null;
setGlobalEventHandler(async (ev) => {
  if (ev.type === 'connection') { updateLive(); return; }
  if (ev.type === 'approval') {
    const a = ev.data;
    S.approvals = S.approvals.filter((x) => x.id !== a.id);
    if (a.state === 'pending') {
      S.approvals.push(a);
      toast(`🔐 @${a.username} needs approval to ${a.action}`);
      browserNotify(`Approval needed: @${a.username}`, a.detail, () => go('chat/' + a.thread_id));
    }
  }
  if (ev.type === 'proposal' && ev.data.state === 'pending') browserNotify(`Proposal from @${ev.data.by}`, ev.data.reason, () => go('inbox'));
  if (ev.type === 'message' && ev.data.to === '@user' && !ev.data.status && ev.data.from !== 'system') {
    const a = agentBy(ev.data.from);
    browserNotify(`${a ? a.icon + ' ' + a.name : ev.data.from} replied`, ev.data.text, () => go('chat/' + ev.data.thread_id));
  }
  if (['agents', 'models', 'settings'].includes(ev.type)) await loadCore().catch(() => {});
  if (['run', 'proposal', 'approval', 'agents'].includes(ev.type)) {
    clearTimeout(navTimer);
    navTimer = setTimeout(async () => { try { S.overview = await api('GET', '/api/overview'); } catch { /* ignore */ } renderNav(); }, 300);
  }
});

// ---------------------------------------------------------------- sign-in
function showLogin(msg) {
  disconnectEvents();
  const user = h('input', { placeholder: 'Username', autocomplete: 'username', value: localStorage.getItem('zpilot.lastUser') || '' });
  const pw = h('input', { type: 'password', placeholder: 'Password', autocomplete: 'current-password' });
  const err = h('div', { class: 'hint', style: 'color:var(--bad);min-height:18px' }, msg || '');
  const btn = h('button', { class: 'primary', type: 'submit' }, 'Sign in');
  const submit = async (e) => {
    e?.preventDefault();
    btn.disabled = true;
    try {
      await api('POST', '/api/login', { username: user.value.trim(), password: pw.value });
      localStorage.setItem('zpilot.lastUser', user.value.trim());
      boot();
    } catch (x) { err.textContent = x.message; pw.select(); } finally { btn.disabled = false; }
  };
  $('#app').className = '';
  $('#app').replaceChildren(h('div', { class: 'login' }, h('form', { class: 'card', onsubmit: submit },
    h('div', { class: 'brand', style: 'padding:0' }, h('div', { class: 'logo' }, 'Z'), h('div', {}, 'ZPilot')),
    h('h1', {}, 'Sign in'), h('p', { class: 'hint', style: 'margin:0' }, 'On the machine running ZPilot, "zpilot ui" or the desktop app signs you in automatically.'),
    h('div', { class: 'stack', style: 'margin-top:18px' }, user, pw, err, btn))));
  (user.value ? pw : user).focus();
}
setUnauthorizedHandler(() => showLogin('Your session ended. Please sign in again.'));

// ---------------------------------------------------------------- boot
let globalClick = false;
async function boot() {
  applyTheme();
  const m = location.hash.match(/(token|code)=([0-9a-f]+)/);
  if (m) {
    const next = (location.hash.match(/next=([A-Za-z0-9/_-]+)/) || [])[1] || 'home';
    history.replaceState(null, '', location.pathname + '#/' + next);
    try { await api('POST', '/api/login', { [m[1]]: m[2] }); } catch { showLogin('That sign-in link has expired. Run "zpilot ui" again, or sign in with a password.'); return; }
  }
  const sess = await fetch('/api/session', { credentials: 'same-origin' }).then((r) => r.json()).catch(() => ({}));
  if (!sess.authenticated) { showLogin(); return; }
  S.session = sess;
  await Promise.all([loadCore(), loadApprovals()]);
  const u = sess.user || {};
  $('#app').className = '';
  $('#app').replaceChildren(h('div', { class: 'shell' },
    h('header', { class: 'topbar' }, h('button', { class: 'ghost icon', 'aria-label': 'Menu', onclick: (e) => { e.stopPropagation(); document.body.classList.toggle('nav-open'); } }, '☰'),
      h('div', { class: 'title', id: 'top-title' }), h('button', { class: 'ghost icon', 'aria-label': 'Search', onclick: openPalette }, '🔍')),
    h('aside', { class: 'side', 'aria-label': 'Navigation' },
      h('div', { class: 'brand' }, h('div', { class: 'logo' }, 'Z'), h('div', {}, 'ZPilot', h('small', {}, 'v' + (sess.version || '')))),
      h('button', { class: 'search-btn', onclick: openPalette }, '🔍 Search or jump to…', h('kbd', {}, /Mac/.test(navigator.platform) ? '⌘K' : 'Ctrl K')),
      h('nav', { class: 'nav', id: 'nav' }),
      h('div', { class: 'side-foot' },
        h('div', { class: 'who' }, h('div', { class: 'avatar xs', style: 'background:var(--accent)' }, (u.name || 'O')[0].toUpperCase()),
          h('div', { style: 'min-width:0' }, h('b', {}, u.name || 'Owner'), h('span', {}, u.role))),
        h('span', { id: 'live-dot', class: 'dot', title: 'Live updates' }), h('span', { id: 'live-text', class: 'hide-sm' }, '…'))),
    h('main', { class: 'main', id: 'main' })));
  if (!globalClick) {
    globalClick = true;
    document.addEventListener('click', (e) => { if (document.body.classList.contains('nav-open') && !e.target.closest('.side')) document.body.classList.remove('nav-open'); });
  }
  if (!location.hash.replace(/^#\/?/, '')) history.replaceState(null, '', location.pathname + '#/home');
  connectEvents();
  window.onhashchange = render;
  render();
}

boot().catch((e) => { $('#app').textContent = 'Failed to start: ' + e.message; });
