// Conversations: thread list, messages with live streaming, inline command
// approvals, @mention routing and a mobile list/detail layout.
import { h, md, api, act, S, ago, agentBy, avatar, toast, confirmDialog, copyText, setupBanner, can, isSet, onEvent } from './core.js';

export async function viewChat(args, { go }) {
  const isNew = args[0] === 'new';
  const threadId = isNew ? '' : args[0];
  const st = { agent: sessionStorage.getItem('zpilot.chat.agent') || '', q: '', archived: false, threads: [], thread: null };
  const root = h('div', { class: 'chat' + (threadId || isNew ? '' : ' list-mode') });
  const list = h('div', { class: 'thread-list', role: 'list' });
  const convo = h('div', { class: 'convo' });

  // ---- thread list
  const search = h('input', { type: 'search', placeholder: 'Search conversations…', 'aria-label': 'Search conversations', oninput: (e) => { st.q = e.target.value.toLowerCase(); drawList(); } });
  const agentSelect = h('select', { 'aria-label': 'Filter by agent', onchange: (e) => { st.agent = e.target.value; sessionStorage.setItem('zpilot.chat.agent', st.agent); loadList(); } },
    h('option', { value: '' }, 'All agents'), S.agents.map((a) => h('option', { value: a.username, selected: a.username === st.agent }, `${a.icon} @${a.username}`)));
  const archivedBtn = h('button', { class: 'small ghost', title: 'Show archived', onclick: () => { st.archived = !st.archived; archivedBtn.classList.toggle('primary', st.archived); loadList(); } }, '🗄');

  async function loadList() {
    st.threads = await api('GET', '/api/threads?agent=' + encodeURIComponent(st.agent) + (st.archived ? '&archived=1' : ''));
    drawList();
  }
  function drawList() {
    const items = st.threads.filter((t) => !st.q || (t.title + ' ' + (t.last_text || '') + ' ' + (t.agents || []).join(' ')).toLowerCase().includes(st.q));
    const running = new Set(Object.values(S.liveRuns).map((r) => r.thread_id));
    const waiting = new Set(S.approvals.map((a) => a.thread_id));
    list.replaceChildren(...(items.length ? items.map((t) => {
      const first = agentBy((t.agents || [])[0]);
      return h('div', { class: 'thread-item' + (t.id === threadId ? ' active' : ''), role: 'listitem', tabindex: 0, onclick: () => go('chat/' + t.id), onkeydown: (e) => e.key === 'Enter' && go('chat/' + t.id) },
        first ? avatar(first, 'sm') : h('div', { class: 'avatar sm', style: 'background:#64748b' }, '💬'),
        h('div', { class: 'body' },
          h('div', { class: 'row nowrap' }, h('div', { class: 't clip' }, t.title), h('span', { class: 'spacer' }), h('span', { class: 'hint', style: 'flex:none' }, ago(t.updated))),
          h('div', { class: 'l clip' }, t.last_text || ''),
          h('div', { class: 'm' }, (t.agents || []).slice(0, 5).map((u) => agentBy(u)?.icon || '🤖').join(' '),
            t.origin !== 'user' ? h('span', { class: 'badge' }, t.origin.split(':')[0]) : null,
            waiting.has(t.id) ? h('span', { class: 'badge warn' }, '🔐 needs you') : running.has(t.id) ? h('span', { class: 'badge accent' }, h('span', { class: 'spin' }), 'working') : null)));
    }) : [h('div', { class: 'empty' }, st.q ? 'No matches.' : 'No conversations yet.')]));
  }

  async function loadThread() {
    if (!threadId) { st.thread = null; return; }
    try { st.thread = await api('GET', '/api/threads/' + encodeURIComponent(threadId)); } catch { st.thread = null; }
  }

  // ---- messages
  const msgBox = h('div', { class: 'messages', role: 'log', 'aria-live': 'polite' });
  const nearBottom = () => msgBox.scrollHeight - msgBox.scrollTop - msgBox.clientHeight < 120;

  function drawMessages(forceBottom) {
    if (!st.thread) return;
    const stick = forceBottom || nearBottom();
    const out = [];
    let lastDay = '';
    for (const m of st.thread.messages) {
      const day = new Date(m.created).toDateString();
      if (day !== lastDay) { out.push(h('div', { class: 'day' }, new Date(m.created).toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' }))); lastDay = day; }
      out.push(renderMessage(m));
      if (m.status === 'running' && m.run_id) out.push(renderLive(m));
    }
    for (const a of S.approvals.filter((x) => x.thread_id === st.thread.id)) out.push(renderApproval(a));
    msgBox.replaceChildren(...out);
    if (stick) msgBox.scrollTop = msgBox.scrollHeight;
  }

  function fromAvatar(m) {
    if (m.from === '@user') return h('div', { class: 'avatar sm', style: 'background:var(--accent)' }, (m.from_name || 'Y')[0].toUpperCase());
    const a = agentBy(m.from);
    if (a) return avatar(a, 'sm');
    const icon = m.from.startsWith('channel:') ? '📡' : m.from.startsWith('schedule:') ? '⏰' : m.from.startsWith('workflow:') ? '⚙️' : '🛈';
    return h('div', { class: 'avatar sm', style: 'background:#64748b' }, icon);
  }

  function renderMessage(m) {
    const mine = m.from === '@user';
    const system = m.from === 'system';
    const a2a = m.from.startsWith('@') && m.to.startsWith('@') && !mine && m.to !== '@user';
    const cls = ['msg', mine ? 'mine' : '', a2a ? 'a2a' : '', system ? 'system' : '', m.kind === 'error' ? 'error' : '', m.kind === 'notice' && !system ? 'notice' : ''].join(' ');
    const statusBadge = m.status && m.status !== 'done' && m.status !== 'running'
      ? h('span', { class: 'badge ' + ({ queued: 'info', failed: 'bad', rejected: 'bad', canceled: '' }[m.status] || '') }, m.status) : null;
    const tools = h('div', { class: 'msg-tools' }, h('button', { class: 'small ghost', title: 'Copy', onclick: () => copyText(m.text) }, '⧉'));
    if (m.status === 'queued' && can('operator')) tools.append(h('button', { class: 'small ghost', title: 'Cancel', onclick: () => act(() => api('POST', `/api/threads/${m.thread_id}/messages/${m.id}/cancel`), 'Canceled') }, '✕'));
    const name = mine ? (m.from_name || 'You') : (m.from_name || m.from);
    return h('div', { class: cls, id: 'm-' + m.id },
      system ? null : fromAvatar(m),
      h('div', { class: 'bubble' }, tools,
        h('div', { class: 'msg-meta' }, h('b', {}, name), h('span', {}, '→'), h('span', { class: 'mono' }, m.to === '@user' ? 'you' : m.to),
          m.mode ? h('span', { class: 'badge' }, m.mode) : null, statusBadge, h('span', { title: new Date(m.created).toLocaleString() }, ago(m.created))),
        md(m.text)));
  }

  function renderLive(m) {
    const live = S.liveRuns[m.run_id] || { text: '', steps: [] };
    const a = agentBy(m.to);
    const body = h('div', { class: 'live-box', id: 'live-' + m.run_id });
    fillLive(body, live);
    return h('div', { class: 'msg' }, a ? avatar(a, 'sm') : null,
      h('div', { class: 'bubble' },
        h('div', { class: 'msg-meta' }, h('b', {}, a ? a.name : m.to), h('span', { class: 'badge accent' }, h('span', { class: 'spin' }), 'working'),
          can('operator') ? h('button', { class: 'small ghost danger', onclick: () => act(() => api('POST', `/api/runs/${m.run_id}/stop`), 'Stopping…') }, 'Stop') : null),
        body));
  }
  function fillLive(box, live) {
    const kids = [];
    if (live.steps?.length) kids.push(h('div', { class: 'steps' }, live.steps.slice(-8).map((s) => h('div', {}, '▸ ' + s))));
    if (live.text) { const t = md(live.text); t.lastElementChild?.classList.add('cursor'); kids.push(t); }
    else kids.push(h('div', { class: 'typing' }, h('i'), h('i'), h('i')));
    box.replaceChildren(...kids);
  }

  function renderApproval(a) {
    const reason = h('input', { placeholder: 'Reason (optional, the agent sees it)' });
    const agent = agentBy(a.username);
    return h('div', { class: 'approval-card', id: 'apr-' + a.id },
      h('div', { class: 'row' }, agent ? avatar(agent, 'xs') : null, h('b', {}, `@${a.username} wants to ${a.action}`), h('span', { class: 'badge warn' }, 'needs approval'), h('span', { class: 'spacer' }), h('span', { class: 'hint' }, ago(a.created))),
      h('pre', { class: 'mono' }, a.detail),
      can('admin') ? h('div', { class: 'row' }, reason,
        h('button', { class: 'danger', onclick: () => act(() => api('POST', `/api/approvals/${a.id}/deny`, { reason: reason.value }), 'Denied') }, 'Deny'),
        h('button', { class: 'ok', onclick: () => act(() => api('POST', `/api/approvals/${a.id}/allow`, { reason: reason.value }), 'Approved') }, 'Allow once'))
        : h('div', { class: 'hint' }, 'An admin must approve this.'));
  }

  // ---- composer with @mentions
  const input = h('textarea', { placeholder: 'Message an agent…  Type @ to choose who answers', rows: 2, 'aria-label': 'Message' });
  const pop = h('div', { class: 'pop', hidden: true, role: 'listbox' });
  const toSelect = h('select', { 'aria-label': 'Send to' }, S.agents.filter((a) => !a.disabled).map((a) => h('option', { value: a.username }, `${a.icon} @${a.username}`)));
  if (st.agent && agentBy(st.agent)) toSelect.value = st.agent;
  const sendBtn = h('button', { class: 'primary small', onclick: send, title: 'Send (Enter)' }, 'Send ↵');
  let popSel = 0, popItems = [];
  const autosize = () => { input.style.height = 'auto'; input.style.height = Math.min(input.scrollHeight, 240) + 'px'; };
  function updatePop() {
    const mm = input.value.slice(0, input.selectionStart).match(/(?:^|\s)@([a-z0-9_]*)$/);
    if (!mm) { pop.hidden = true; return; }
    popItems = S.agents.filter((a) => !a.disabled && (a.username.startsWith(mm[1]) || a.name.toLowerCase().startsWith(mm[1]))).slice(0, 8);
    if (!popItems.length) { pop.hidden = true; return; }
    popSel = Math.min(popSel, popItems.length - 1);
    pop.replaceChildren(...popItems.map((a, i) => h('div', { class: i === popSel ? 'sel' : '', role: 'option', onmousedown: (e) => { e.preventDefault(); pick(a); } },
      avatar(a, 'xs'), h('b', {}, '@' + a.username), h('span', { class: 'hint clip' }, a.description || a.name))));
    pop.hidden = false;
  }
  function pick(a) {
    const pos = input.selectionStart;
    const before = input.value.slice(0, pos).replace(/@([a-z0-9_]*)$/, '@' + a.username + ' ');
    input.value = before + input.value.slice(pos);
    toSelect.value = a.username;
    pop.hidden = true;
    input.focus();
  }
  input.addEventListener('input', () => { updatePop(); autosize(); });
  input.addEventListener('keydown', (e) => {
    if (!pop.hidden) {
      if (e.key === 'ArrowDown') { popSel = (popSel + 1) % popItems.length; updatePop(); e.preventDefault(); return; }
      if (e.key === 'ArrowUp') { popSel = (popSel - 1 + popItems.length) % popItems.length; updatePop(); e.preventDefault(); return; }
      if (e.key === 'Enter' || e.key === 'Tab') { pick(popItems[popSel]); e.preventDefault(); return; }
      if (e.key === 'Escape') { pop.hidden = true; e.stopPropagation(); return; }
    }
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
  });

  async function send() {
    let text = input.value.trim();
    if (!text) return;
    let to = toSelect.value;
    const mm = text.match(/^@([a-z0-9_]+)\s+([\s\S]+)/);
    if (mm && agentBy(mm[1])) { to = mm[1]; text = mm[2]; }
    if (!to) { toast('Create an agent first', 'bad'); return; }
    sendBtn.disabled = true;
    try {
      const msg = await api('POST', '/api/messages', { thread_id: threadId || '', to, text });
      input.value = ''; autosize();
      if (!threadId) go('chat/' + msg.thread_id);
      else { st.thread.messages.push(msg); drawMessages(true); }
    } catch (e) { toast(e.message, 'bad'); } finally { sendBtn.disabled = false; input.focus(); }
  }

  function drawConvo() {
    if (!S.agents.length) {
      convo.replaceChildren(h('div', { class: 'page' }, setupBanner(go), h('div', { class: 'empty' }, h('div', { class: 'big' }, '🤖'), h('b', {}, 'No agents yet'), h('p', {}, 'Create an agent, give it instructions and tools, then talk to it here.'),
        can('admin') ? h('button', { class: 'primary', onclick: () => go('agents/new') }, 'Create your first agent') : null)));
      return;
    }
    const t = st.thread;
    const back = h('button', { class: 'ghost icon show-sm', onclick: () => go('chat'), 'aria-label': 'Back to conversations' }, '←');
    const head = h('div', { class: 'convo-head' }, back,
      t ? [h('div', { style: 'min-width:0' }, h('div', { class: 'title clip' }, t.title), h('div', { class: 'hint' }, (t.agents || []).map((u) => '@' + u).join(' · '))), h('span', { class: 'spacer' }),
        h('span', { class: 'badge hide-sm', title: 'Agent runs used in this conversation' }, `${t.runs}/${t.max_runs} runs`),
        can('operator') ? h('button', { class: 'small ghost', title: t.archived ? 'Unarchive' : 'Archive', onclick: () => act(() => api('POST', `/api/threads/${t.id}/archive`, { archived: !t.archived }), t.archived ? 'Restored' : 'Archived').then(() => go('chat')) }, t.archived ? '📤' : '🗄') : null,
        can('admin') ? h('button', { class: 'small ghost danger', title: 'Delete', onclick: async () => { if (await confirmDialog('Delete this conversation?', 'Its messages are removed. Agent memories and files are not affected.', 'Delete', true)) { await act(() => api('DELETE', '/api/threads/' + t.id), 'Deleted'); go('chat'); } } }, '🗑') : null]
        : [h('div', { class: 'title' }, 'New conversation'), h('span', { class: 'hint hide-sm' }, 'Pick an agent and say what you need.')]);
    if (t) {
      const lastAgent = [...t.messages].reverse().find((m) => m.from === '@user')?.to?.replace('@', '');
      if (lastAgent && agentBy(lastAgent)) toSelect.value = lastAgent;
    }
    const composer = can('operator') ? h('div', { class: 'composer' },
      h('div', { class: 'composer-box' }, pop, input,
        h('div', { class: 'composer-bar' }, h('span', { class: 'hint' }, 'To'), toSelect, h('span', { class: 'hint hide-sm' }, 'Shift+Enter for a new line'), h('span', { class: 'spacer' }), sendBtn)))
      : h('div', { class: 'composer' }, h('div', { class: 'hint' }, 'You have read-only access.'));
    const body = t ? msgBox : h('div', { class: 'messages' }, setupBanner(go), h('div', { class: 'empty' }, h('div', { class: 'big' }, '💬'), h('b', {}, 'Start a conversation'),
      h('p', {}, 'Ask any agent. Agents can bring in colleagues with ask_agent and tell_agent; you will see their exchange here.'),
      h('div', { class: 'chips', style: 'justify-content:center' }, S.agents.filter((a) => !a.disabled).slice(0, 6).map((a) => h('span', { class: 'chip', onclick: () => { toSelect.value = a.username; input.focus(); } }, `${a.icon} @${a.username}`)))));
    convo.replaceChildren(head, body, composer);
    drawMessages(true);
    setTimeout(() => { if (window.innerWidth > 900 || threadId) input.focus(); }, 0);
  }

  await Promise.all([loadList(), loadThread()]);
  drawConvo();
  root.append(h('div', { class: 'threads' },
    h('div', { class: 'threads-head' }, h('div', { class: 'row nowrap' }, h('b', {}, 'Conversations'), h('span', { class: 'spacer' }), archivedBtn,
      can('operator') ? h('button', { class: 'small primary', onclick: () => go('chat/new') }, '＋ New') : null),
      search, agentSelect), list), convo);

  let reload = null;
  const off = onEvent((ev) => {
    const t = st.thread;
    if (ev.type === 'message' && t && ev.data.thread_id === t.id) {
      const i = t.messages.findIndex((m) => m.id === ev.data.id);
      if (i >= 0) t.messages[i] = ev.data; else t.messages.push(ev.data);
      drawMessages();
    }
    if (ev.type === 'live' && t && ev.data.thread_id === t.id) {
      const box = document.getElementById('live-' + ev.data.run_id);
      if (box) { const stick = nearBottom(); fillLive(box, S.liveRuns[ev.data.run_id]); if (stick) msgBox.scrollTop = msgBox.scrollHeight; }
      else drawMessages();
    }
    if (ev.type === 'approval' && t && ev.data.thread_id === t.id) drawMessages();
    if (ev.type === 'thread' || ev.type === 'run' || ev.type === 'approval') { clearTimeout(reload); reload = setTimeout(loadList, 250); }
  });
  return { el: root, cleanup: off, fullHeight: true };
}
