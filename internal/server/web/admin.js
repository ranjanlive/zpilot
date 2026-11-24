// Home, Inbox (approvals & proposals), Activity, Audit, Users, Settings.
import { h, api, act, S, loadCore, loadApprovals, agentBy, avatar, page, ago, when, dur, isSet, confirmDialog, toast, modal, can, emptyState, onEvent, toggle, setupBanner } from './core.js';

// ================================================================ HOME
export async function viewHome(args, { go, rerender }) {
  await Promise.all([loadCore(), loadApprovals()]);
  const [runs, proposals, threads] = await Promise.all([api('GET', '/api/runs?limit=8'), api('GET', '/api/proposals'), api('GET', '/api/threads')]);
  const pendingProps = proposals.filter((p) => p.state === 'pending');
  const ov = S.overview;
  const stat = (n, l, onclick, tone = '') => h('div', { class: 'card stat ' + (onclick ? 'hover' : ''), onclick }, h('div', { class: 'n', style: tone ? `color:var(--${tone})` : '' }, n), h('div', { class: 'l' }, l));
  const needs = S.approvals.length + pendingProps.length;
  const hello = new Date().getHours() < 12 ? 'Good morning' : new Date().getHours() < 18 ? 'Good afternoon' : 'Good evening';
  const off = onEvent((ev) => { if (['run', 'approval', 'proposal', 'agents'].includes(ev.type)) { clearTimeout(off.t); off.t = setTimeout(rerender, 600); } });
  return {
    el: page(`${hello}${S.session?.user?.name && S.session.user.name !== 'Owner' ? ', ' + S.session.user.name : ''}`, 'Here is what your agents are doing.',
      can('operator') ? [h('button', { class: 'primary', onclick: () => go('chat/new') }, '💬 New conversation')] : [],
      setupBanner(go),
      needs ? h('div', { class: 'banner warn' }, h('span', {}, '🔔'), h('div', { class: 'spacer' }, h('b', {}, `${needs} item${needs > 1 ? 's' : ''} need${needs > 1 ? '' : 's'} your decision.`)), h('button', { class: 'primary', onclick: () => go('inbox') }, 'Review')) : null,
      h('div', { class: 'grid', style: 'grid-template-columns:repeat(auto-fill,minmax(170px,1fr))' },
        stat(ov.agents, 'agents', () => go('agents')), stat(ov.running, 'running now', () => go('activity'), ov.running ? 'accent' : ''),
        stat(ov.queued, 'queued'), stat(needs, 'need you', () => go('inbox'), needs ? 'warn' : ''), stat(ov.models, 'models', () => go('models'))),
      h('div', { class: 'grid', style: 'margin-top:14px;grid-template-columns:repeat(auto-fit,minmax(340px,1fr))' },
        h('div', { class: 'card pad stack' }, h('div', { class: 'row' }, h('h3', {}, 'Recent conversations'), h('span', { class: 'spacer' }), h('a', { href: '#/chat' }, 'All →')),
          threads.length ? threads.slice(0, 6).map((t) => h('div', { class: 'row nowrap', style: 'cursor:pointer', onclick: () => go('chat/' + t.id) },
            avatar(agentBy((t.agents || [])[0]), 'xs'), h('div', { class: 'clip', style: 'flex:1' }, h('b', {}, t.title), h('div', { class: 'hint clip' }, t.last_text || '')), h('span', { class: 'hint' }, ago(t.updated))))
            : h('div', { class: 'hint' }, 'No conversations yet.')),
        h('div', { class: 'card pad stack' }, h('div', { class: 'row' }, h('h3', {}, 'Latest runs'), h('span', { class: 'spacer' }), h('a', { href: '#/activity' }, 'All →')),
          runs.length ? runs.map((r) => h('div', { class: 'row nowrap', style: 'cursor:pointer', onclick: () => go('chat/' + r.thread_id) },
            avatar(agentBy(r.username), 'xs'), h('span', { class: 'clip', style: 'flex:1' }, '@' + r.username), runBadge(r.state), h('span', { class: 'hint' }, ago(r.started))))
            : h('div', { class: 'hint' }, 'Nothing has run yet.')),
        h('div', { class: 'card pad stack' }, h('h3', {}, 'Your team'),
          h('div', { class: 'row' }, S.agents.map((a) => h('div', { style: 'cursor:pointer', title: `${a.name} (@${a.username})`, onclick: () => go('agents/' + a.username) }, avatar(a)))),
          can('admin') ? h('div', { class: 'row' }, h('button', { class: 'small', onclick: () => go('agents/new') }, '＋ Agent'), h('button', { class: 'small', onclick: () => go('workflows/new') }, '＋ Workflow'), h('button', { class: 'small', onclick: () => go('channels') }, '＋ Channel')) : null))),
    cleanup: off,
  };
}

export const runBadge = (s) => h('span', { class: 'badge ' + ({ running: 'accent', done: 'ok', failed: 'bad', timeout: 'bad', stopped: 'warn', interrupted: 'warn' }[s] || '') }, s === 'running' ? h('span', { class: 'spin' }) : null, s);

// ================================================================ INBOX
export async function viewInbox(args, { go, rerender }) {
  const [approvals, proposals] = await Promise.all([api('GET', '/api/approvals'), api('GET', '/api/proposals')]);
  const admin = can('admin');
  const pendingA = approvals.filter((a) => a.state === 'pending');
  const pendingP = proposals.filter((p) => p.state === 'pending');
  const approvalCard = (a) => {
    const reason = h('input', { placeholder: 'Reason (optional; the agent sees it)' });
    return h('div', { class: 'approval-card', style: 'max-width:none' },
      h('div', { class: 'row' }, avatar(agentBy(a.username), 'xs'), h('b', {}, `@${a.username} wants to ${a.action}`), h('span', { class: 'spacer' }), h('span', { class: 'hint' }, ago(a.created))),
      h('pre', { class: 'mono' }, a.detail),
      h('div', { class: 'row' }, h('button', { class: 'small ghost', onclick: () => go('chat/' + a.thread_id) }, 'Open conversation'), h('span', { class: 'spacer' }),
        admin ? [reason, h('button', { class: 'danger', onclick: () => act(() => api('POST', `/api/approvals/${a.id}/deny`, { reason: reason.value }), 'Denied').then(rerender) }, 'Deny'),
          h('button', { class: 'ok', onclick: () => act(() => api('POST', `/api/approvals/${a.id}/allow`, { reason: reason.value }), 'Approved').then(rerender) }, 'Allow once')] : h('span', { class: 'hint' }, 'Only admins can decide.')));
  };
  const proposalCard = (p) => h('div', { class: 'card pad stack' },
    h('div', { class: 'row' }, h('h3', {}, '🧭 ' + p.action.replaceAll('_', ' ') + (p.target ? ' ' + p.target : '')), h('span', { class: 'spacer' }),
      h('span', { class: 'badge ' + ({ pending: 'warn', approved: 'ok', rejected: '', failed: 'bad' }[p.state]) }, p.state)),
    h('div', { class: 'sub' }, `Proposed by @${p.by} · ${ago(p.created)}`), h('div', {}, h('b', {}, 'Why: '), p.reason),
    h('pre', { class: 'log' }, JSON.stringify(p.payload, null, 2)),
    p.result ? h('div', { class: 'hint' }, 'Result: ' + p.result) : null,
    p.state === 'pending' && admin ? h('div', { class: 'row' }, h('span', { class: 'spacer' }),
      h('button', { class: 'danger', onclick: () => act(() => api('POST', `/api/proposals/${p.id}/reject`), 'Rejected').then(rerender) }, 'Reject'),
      h('button', { class: 'primary', onclick: () => act(() => api('POST', `/api/proposals/${p.id}/approve`)).then((r) => { toast(r.state === 'approved' ? 'Applied: ' + r.result : 'Failed: ' + r.result, r.state === 'approved' ? 'ok' : 'bad'); rerender(); }) }, 'Approve & apply')) : null);
  const history = [...approvals.filter((a) => a.state !== 'pending').slice(0, 15).map((a) => ({ t: a.decided || a.created, el: h('div', { class: 'row nowrap' },
    h('span', { class: 'badge ' + (a.state === 'allowed' ? 'ok' : a.state === 'denied' ? 'bad' : '') }, a.state), h('span', { class: 'mono clip', style: 'flex:1' }, `@${a.username}: ${a.detail}`), h('span', { class: 'hint' }, `${a.by || ''} ${ago(a.decided)}`)) })),
  ...proposals.filter((p) => p.state !== 'pending').slice(0, 15).map((p) => ({ t: p.decided || p.created, el: h('div', { class: 'row nowrap' },
    h('span', { class: 'badge ' + (p.state === 'approved' ? 'ok' : p.state === 'failed' ? 'bad' : '') }, p.state), h('span', { class: 'clip', style: 'flex:1' }, `${p.action} ${p.target} — ${p.reason}`), h('span', { class: 'hint' }, ago(p.decided))) }))]
    .sort((x, y) => new Date(y.t) - new Date(x.t));
  const off = onEvent((ev) => { if (ev.type === 'approval' || ev.type === 'proposal') { clearTimeout(off.t); off.t = setTimeout(rerender, 200); } });
  return {
    el: page('Needs you', 'Commands waiting for approval and changes proposed by @manager. Nothing happens until you decide.',
      [h('button', { onclick: () => { sessionStorage.setItem('zpilot.chat.agent', 'manager'); go('chat/new'); } }, '🧭 Talk to @manager')],
      pendingA.length || pendingP.length ? h('div', { class: 'stack' }, pendingA.map(approvalCard), pendingP.map(proposalCard))
        : emptyState('✅', "You're all caught up", 'When an agent wants to run a command that needs approval, or @manager proposes a change, it appears here and as a notification.'),
      history.length ? [h('div', { class: 'section-title' }, 'History'), h('div', { class: 'card pad stack' }, history.map((x) => x.el))] : null),
    cleanup: off,
  };
}

// ================================================================ ACTIVITY
export async function showRunLog(id) {
  const box = h('pre', { class: 'log' }, 'Loading…');
  modal('Run log', box, [(c) => h('button', { onclick: c }, 'Close')], 'wide');
  try { const r = await api('GET', `/api/runs/${id}/log`); box.textContent = r.log || '(no output yet)'; box.scrollTop = box.scrollHeight; } catch (e) { box.textContent = e.message; }
}

export async function viewActivity(args, { go, rerender }) {
  const runs = await api('GET', '/api/runs?limit=200');
  const rows = runs.map((r) => h('tr', {},
    h('td', {}, h('div', { class: 'row nowrap' }, avatar(agentBy(r.username), 'xs'), '@' + r.username)),
    h('td', {}, runBadge(r.state), r.error ? h('div', { class: 'hint', style: 'max-width:340px' }, r.error.slice(0, 200)) : null),
    h('td', {}, when(r.started)), h('td', { class: 'num' }, isSet(r.ended) ? dur((new Date(r.ended) - new Date(r.started)) / 1000) : '…'),
    h('td', { class: 'num' }, `${r.prompt_tokens.toLocaleString()} / ${r.answer_tokens.toLocaleString()}`),
    h('td', {}, h('div', { class: 'row nowrap' }, h('button', { class: 'small', onclick: () => go('chat/' + r.thread_id) }, 'Thread'), h('button', { class: 'small', onclick: () => showRunLog(r.id) }, 'Log'),
      r.state === 'running' && can('operator') ? h('button', { class: 'small danger', onclick: () => act(() => api('POST', `/api/runs/${r.id}/stop`), 'Stopping…') }, 'Stop') : null))));
  const totals = runs.reduce((t, r) => ({ p: t.p + r.prompt_tokens, a: t.a + r.answer_tokens }), { p: 0, a: 0 });
  let t = null;
  const off = onEvent((ev) => { if (ev.type === 'run') { clearTimeout(t); t = setTimeout(rerender, 500); } });
  return {
    el: page('Activity', `${runs.length} recent runs · ${totals.p.toLocaleString()} prompt / ${totals.a.toLocaleString()} answer tokens`, [h('button', { onclick: rerender }, '↻ Refresh')],
      runs.length ? h('div', { class: 'card table-wrap' }, h('table', { class: 't' }, h('thead', {}, h('tr', {}, ['Agent', 'State', 'Started', 'Duration', 'Tokens in / out', ''].map((x) => h('th', {}, x)))), h('tbody', {}, rows)))
        : emptyState('📈', 'No runs yet', 'Every time an agent works on a message, the run appears here with its log and token use.')),
    cleanup: off,
  };
}

// ================================================================ AUDIT
export async function viewAudit() {
  const list = await api('GET', '/api/audit?limit=400');
  let q = '';
  const tbody = h('tbody');
  const draw = () => tbody.replaceChildren(...list.filter((e) => !q || JSON.stringify(e).toLowerCase().includes(q)).map((e) =>
    h('tr', {}, h('td', { class: 'num' }, when(e.time)), h('td', {}, e.actor), h('td', { class: 'mono' }, e.action), h('td', {}, e.target || ''), h('td', { class: 'hint' }, e.detail || ''))));
  draw();
  return page('Audit log', 'Every configuration change and security-relevant event, newest first.',
    [h('input', { type: 'search', placeholder: 'Filter…', style: 'width:220px', oninput: (e) => { q = e.target.value.toLowerCase(); draw(); } })],
    h('div', { class: 'card table-wrap' }, h('table', { class: 't' }, h('thead', {}, h('tr', {}, ['Time', 'Actor', 'Action', 'Target', 'Detail'].map((x) => h('th', {}, x)))), tbody)));
}

// ================================================================ USERS
export async function viewUsers(args, { rerender }) {
  const users = await api('GET', '/api/users');
  const roleInfo = { viewer: 'Read conversations, agents and activity', operator: 'Also talk to agents, run schedules and workflows, stop runs', admin: 'Everything: configuration, approvals, users' };
  const rows = users.map((u) => h('tr', {},
    h('td', {}, h('b', {}, u.name || u.username), h('div', { class: 'hint mono' }, u.username)),
    h('td', {}, h('span', { class: 'badge ' + (u.role === 'admin' ? 'accent' : u.role === 'operator' ? 'info' : '') }, u.role)),
    h('td', {}, u.disabled ? h('span', { class: 'badge' }, 'disabled') : h('span', { class: 'badge ok' }, 'active')),
    h('td', {}, ago(u.last_login)),
    h('td', {}, h('div', { class: 'row nowrap' }, h('button', { class: 'small', onclick: () => userForm(u, roleInfo, rerender) }, 'Edit'),
      h('button', { class: 'small ghost danger', onclick: async () => { if (await confirmDialog('Delete user?', u.username, 'Delete', true)) { await act(() => api('DELETE', '/api/users/' + u.id), 'Deleted'); rerender(); } } }, '🗑')))));
  return page('Users', 'People who can sign in with a password. On this machine you can always sign in with "zpilot ui".',
    [h('button', { class: 'primary', onclick: () => userForm(null, roleInfo, rerender) }, '＋ Add user')],
    h('div', { class: 'grid', style: 'grid-template-columns:repeat(auto-fit,minmax(220px,1fr));margin-bottom:14px' }, Object.entries(roleInfo).map(([r, d]) => h('div', { class: 'card pad' }, h('b', { style: 'text-transform:capitalize' }, r), h('div', { class: 'hint' }, d)))),
    users.length ? h('div', { class: 'card table-wrap' }, h('table', { class: 't' }, h('thead', {}, h('tr', {}, ['User', 'Role', 'Status', 'Last sign-in', ''].map((x) => h('th', {}, x)))), h('tbody', {}, rows)))
      : emptyState('👥', 'No password accounts yet', 'Add accounts to share ZPilot with teammates or to sign in from other devices.'));
}

function userForm(u, roleInfo, rerender) {
  const v = structuredClone(u || { username: '', name: '', role: 'operator', disabled: false });
  let pw = '';
  const roles = h('div', { class: 'choice' });
  const drawRoles = () => roles.replaceChildren(...Object.entries(roleInfo).map(([r, d]) => h('div', { class: v.role === r ? 'on' : '', onclick: () => { v.role = r; drawRoles(); } }, h('b', {}, r), h('span', {}, d))));
  drawRoles();
  modal(u ? 'Edit ' + u.username : 'Add user', h('div', { class: 'form' },
    h('div', { class: 'form two' },
      h('label', { class: 'field' }, 'Username', h('input', { value: v.username, disabled: !!u, placeholder: 'alex', oninput: (e) => { v.username = e.target.value; } })),
      h('label', { class: 'field' }, 'Display name', h('input', { value: v.name, placeholder: 'Alex Kim', oninput: (e) => { v.name = e.target.value; } }))),
    h('div', { class: 'field' }, h('span', {}, 'Role'), roles),
    h('label', { class: 'field' }, u ? 'New password (leave empty to keep)' : 'Password (min. 10 characters)', h('input', { type: 'password', autocomplete: 'new-password', oninput: (e) => { pw = e.target.value; } })),
    u ? toggle(v.disabled, (x) => { v.disabled = x; }, 'Disabled (signs the user out everywhere)') : null), [
    (c) => h('button', { onclick: c }, 'Cancel'),
    (c) => h('button', { class: 'primary', onclick: async () => { await act(() => (u ? api('PUT', '/api/users/' + u.id, { ...v, password: pw }) : api('POST', '/api/users', { ...v, password: pw })), 'User saved'); c(); rerender(); } }, 'Save'),
  ]);
}

// ================================================================ SETTINGS
export async function viewSettings(args, { rerender, applyTheme }) {
  const ov = await api('GET', '/api/overview');
  const v = structuredClone(ov.settings);
  const admin = can('admin');
  const num = (label, k, hint) => h('label', { class: 'field' }, label, h('input', { type: 'number', min: 1, value: v[k], disabled: !admin, oninput: (e) => { v[k] = Number(e.target.value); } }), h('span', { class: 'hint' }, hint));
  const theme = localStorage.getItem('zpilot.theme') || 'system';
  const notifState = !('Notification' in window) ? 'unsupported' : Notification.permission;
  return page('Settings', '', admin ? [h('button', { class: 'primary', onclick: async () => { await act(() => api('PUT', '/api/settings', v), 'Settings saved'); await loadCore(); } }, 'Save')] : [],
    h('div', { class: 'stack' },
      h('div', { class: 'card pad' }, h('h3', {}, 'Limits & safety'), h('div', { class: 'form two', style: 'margin-top:12px' },
        num('Parallel runs (whole installation)', 'global_parallel', 'How many agents may work at the same time.'),
        num('Max agent hops', 'max_hops', 'How deep agent-to-agent delegation may go.'),
        num('Runs per conversation', 'thread_max_runs', 'A budget that stops runaway agent loops.'),
        num('ask_agent timeout (minutes)', 'ask_timeout_minutes', 'How long an agent waits for a colleague.'),
        num('Command approval timeout (minutes)', 'approval_timeout_minutes', 'Pending approvals are denied after this long.'),
        h('label', { class: 'field' }, 'Default model', h('select', { disabled: !admin, onchange: (e) => { v.default_model_id = e.target.value; } }, h('option', { value: '' }, '—'), S.models.map((m) => h('option', { value: m.id, selected: m.id === v.default_model_id }, m.name)))))),
      h('div', { class: 'card pad stack' }, h('h3', {}, 'This browser'),
        h('div', { class: 'field' }, h('span', {}, 'Theme'), h('div', { class: 'seg' }, ['system', 'light', 'dark'].map((t) => h('button', { class: theme === t ? 'on' : '', onclick: () => { localStorage.setItem('zpilot.theme', t); applyTheme(); rerender(); } }, t)))),
        h('div', { class: 'field' }, h('span', {}, 'Desktop notifications'),
          notifState === 'granted' ? toggle(localStorage.getItem('zpilot.notify') !== 'off', (x) => localStorage.setItem('zpilot.notify', x ? 'on' : 'off'), 'Notify me about approvals, proposals and replies while this tab is in the background')
            : notifState === 'unsupported' ? h('span', { class: 'hint' }, 'Not supported by this browser.')
              : h('div', {}, h('button', { onclick: async () => { await Notification.requestPermission(); rerender(); } }, 'Enable notifications'), notifState === 'denied' ? h('span', { class: 'hint' }, ' (blocked in the browser settings)') : null)),
        h('div', { class: 'hint' }, 'Tip: run "zpilot desktop" for an app window with a tray icon and native notifications.')),
      h('div', { class: 'card pad' }, h('h3', {}, 'About'), h('table', { class: 't' }, h('tbody', {},
        [['Version', ov.version], ['Signed in as', `${S.session?.user?.name} (${S.session?.user?.role})`], ['Data directory', ov.data_dir], ['Platform', ov.platform],
          ['Remote access', 'Create users, then serve with --listen 0.0.0.0:7700 and TLS (or behind an HTTPS reverse proxy).']].map(([k, val]) => h('tr', {}, h('td', { class: 'hint' }, k), h('td', { class: 'mono' }, val))))),
        h('div', { class: 'row', style: 'margin-top:12px' }, h('button', { onclick: async () => { await api('POST', '/api/logout'); location.hash = ''; location.reload(); } }, 'Sign out')))));
}
