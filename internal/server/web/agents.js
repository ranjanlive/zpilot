// Agents: card grid and the profile editor.
import { h, api, act, S, loadCore, agentBy, avatar, levelBadge, levelInfo, page, setupBanner, confirmDialog, chipSet, toggle, can, emptyState, onEvent } from './core.js';

export async function viewAgents(args, ctx) {
  if (args[0]) return agentEditor(args[0] === 'new' ? null : args[0], ctx);
  const { go } = ctx;
  await loadCore();
  let q = '';
  const grid = h('div', { class: 'grid' });
  const draw = () => {
    const items = S.agents.filter((a) => !q || (a.username + ' ' + a.name + ' ' + (a.description || '')).toLowerCase().includes(q));
    grid.replaceChildren(...items.map((a) => h('div', { class: 'card hover agent-card', tabindex: 0, onclick: () => go('agents/' + a.username), onkeydown: (e) => e.key === 'Enter' && go('agents/' + a.username) },
      h('div', { class: 'agent-top' }, avatar(a), h('div', { class: 'who' }, h('b', { class: 'clip' }, a.name), h('span', {}, '@' + a.username)), h('span', { class: 'spacer' }),
        a.builtin ? h('span', { class: 'badge accent' }, 'built-in') : null, a.disabled ? h('span', { class: 'badge' }, 'disabled') : null),
      h('div', { class: 'desc' }, a.description || h('span', { class: 'hint' }, 'No description yet')),
      h('div', { class: 'row' }, levelBadge(a.level), a.approve_commands ? h('span', { class: 'badge warn', title: 'Every command needs approval' }, '🔐 approve') : null,
        h('span', { class: 'badge', title: 'Model' }, '🧠 ' + (a.model_name || 'no model')),
        a.tools.length ? h('span', { class: 'badge', title: a.tools.join(', ') }, `🧰 ${a.tools.length}`) : null,
        a.contacts.length ? h('span', { class: 'badge', title: 'Contacts' }, `👥 ${a.contacts.includes('*') ? 'all' : a.contacts.length}`) : null,
        a.channels ? h('span', { class: 'badge info' }, `📡 ${a.channels}`) : null,
        a.running ? h('span', { class: 'badge accent' }, h('span', { class: 'spin' }), `${a.running} running`) : null, a.queued ? h('span', { class: 'badge info' }, `${a.queued} queued`) : null),
      h('div', { class: 'row' }, can('operator') ? h('button', { class: 'small', onclick: (e) => { e.stopPropagation(); sessionStorage.setItem('zpilot.chat.agent', a.username); go('chat/new'); } }, '💬 Chat') : null,
        h('span', { class: 'spacer' }), h('span', { class: 'hint' }, 'Open →')))));
  };
  draw();
  const off = onEvent((ev) => { if (ev.type === 'agents' || ev.type === 'run') loadCore().then(draw); });
  return {
    el: page('Agents', 'Each agent has its own identity, instructions, tools, security level and workspace. Agents reach each other by @username.',
      [h('input', { type: 'search', placeholder: 'Filter agents…', style: 'width:200px', oninput: (e) => { q = e.target.value.toLowerCase(); draw(); } }),
        can('admin') ? h('button', { class: 'primary', onclick: () => go('agents/new') }, '＋ New agent') : null],
      setupBanner(go), S.agents.length ? grid : emptyState('🤖', 'No agents yet', 'Create your first agent.')),
    cleanup: off,
  };
}

const icons = ['🤖', '🧭', '🧠', '🛠️', '📊', '✍️', '🔎', '🧪', '📣', '💼', '🎨', '📚', '🛡️', '⚡', '🌐', '📱', '🧾', '🗂️', '💡', '🎧'];
const colors = ['#6366f1', '#0ea5e9', '#10b981', '#f59e0b', '#ef4444', '#ec4899', '#8b5cf6', '#14b8a6', '#64748b', '#0f172a'];

async function agentEditor(username, { go }) {
  const [tools, skills] = await Promise.all([api('GET', '/api/tools'), api('GET', '/api/skills')]);
  await loadCore();
  const isNew = !username;
  const cur = isNew ? null : agentBy(username);
  if (!isNew && !cur) return page('Agent not found', '', [h('button', { onclick: () => go('agents') }, 'Back')], emptyState('🤷', 'No such agent', ''));
  const a = structuredClone(cur || { username: '', name: '', icon: '🤖', color: '#6366f1', description: '', instruction: '', model_id: S.settings.default_model_id || '',
    level: 'standard', workspace: '', tools: [], skills: [], contacts: [], accept_from: ['*'], max_parallel: 2, timeout_minutes: 30, max_tool_rounds: 200,
    disabled: false, external_input_with_commands: false, approve_commands: false, memory_mb: 0, max_processes: 0, isolate: false });
  const ro = !can('admin');
  const dirty = { v: false };
  const bind = (key, props = {}) => {
    const tag = props.tag || 'input';
    const el = h(tag, { ...props, tag: null, disabled: ro || props.disabled, oninput: (e) => { a[key] = props.type === 'number' ? Number(e.target.value) : e.target.value; dirty.v = true; } });
    el.value = a[key] ?? '';
    return el;
  };
  const preview = h('div', {});
  const drawPreview = () => preview.replaceChildren(avatar(a));
  drawPreview();
  const others = S.agents.filter((x) => x.username !== a.username).map((x) => x.username);
  const levelBox = h('div', { class: 'choice' });
  const approveRow = h('div');
  const drawLevels = () => {
    levelBox.replaceChildren(...Object.entries(levelInfo).map(([l, d]) => h('div', { class: a.level === l ? 'on' : '', tabindex: 0, role: 'radio', 'aria-checked': a.level === l ? 'true' : 'false',
      onclick: () => { if (ro) return; a.level = l; dirty.v = true; drawLevels(); }, onkeydown: (e) => e.key === 'Enter' && e.target.click() }, h('b', {}, l), h('span', {}, d))));
    approveRow.replaceChildren(['trusted', 'full'].includes(a.level)
      ? toggle(a.approve_commands, (v) => { a.approve_commands = v; dirty.v = true; }, 'Ask me before every shell command (recommended). Requests appear in the conversation and as notifications.')
      : h('span', { class: 'hint' }, 'This level cannot run shell commands.'));
  };
  drawLevels();

  const section = (title, sub, ...kids) => h('div', { class: 'card pad' }, h('h3', {}, title), sub ? h('p', { class: 'sub', style: 'margin:2px 0 12px' }, sub) : h('div', { style: 'height:10px' }), kids);
  const form = h('div', { class: 'stack' },
    section('Identity', null, h('div', { class: 'form two' },
      h('label', { class: 'field' }, 'Name', bind('name', { placeholder: 'Research Assistant', maxlength: 64 })),
      h('label', { class: 'field' }, 'Username', h('div', { class: 'row nowrap' }, h('span', { class: 'mono' }, '@'),
        bind('username', { placeholder: 'researcher', disabled: cur?.builtin, oninput: (e) => { a.username = e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, ''); e.target.value = a.username; dirty.v = true; } })),
        h('span', { class: 'hint' }, 'How you and other agents address it.')),
      h('label', { class: 'field full' }, 'Description', bind('description', { placeholder: 'Finds and summarises sources on any topic.' }), h('span', { class: 'hint' }, 'Shown to other agents when they look for help.')),
      h('div', { class: 'field full' }, h('span', {}, 'Icon & colour'), h('div', { class: 'row' }, preview,
        h('div', { class: 'chips' }, icons.map((i) => h('span', { class: 'chip', onclick: () => { if (ro) return; a.icon = i; dirty.v = true; drawPreview(); } }, i))),
        h('div', { class: 'chips' }, colors.map((c) => h('span', { class: 'chip', title: c, style: `background:${c};width:24px;height:24px;padding:0`, onclick: () => { if (ro) return; a.color = c; dirty.v = true; drawPreview(); } }))))))),
    section('Instructions', 'Its role, how it should work and what it must never do. ZPilot adds context about collaboration and security automatically.',
      bind('instruction', { tag: 'textarea', rows: 10, placeholder: 'You are a meticulous research assistant. Always cite sources…' })),
    section('Model & security', 'Security is enforced by the engine configuration, not the prompt. Pick the lowest level that can do the job.',
      h('div', { class: 'form' },
        h('label', { class: 'field' }, 'Model', h('select', { disabled: ro, onchange: (e) => { a.model_id = e.target.value; dirty.v = true; } }, h('option', { value: '' }, '— choose —'),
          S.models.map((m) => h('option', { value: m.id, selected: m.id === a.model_id }, `${m.name} (${m.model})`)))),
        h('div', { class: 'field' }, h('span', {}, 'Security level'), levelBox, approveRow),
        h('label', { class: 'field' }, 'Workspace folder', bind('workspace', { placeholder: 'Empty = a private folder in the data directory' }), h('span', { class: 'hint' }, cur ? 'Currently: ' + cur.workspace_path : 'An absolute path the agent works in.')),
        toggle(a.external_input_with_commands, (v) => { a.external_input_with_commands = v; dirty.v = true; },
          'Allow commands with external input (needed only for trusted/full agents connected to a channel without approval; strangers could try to make it run harmful commands)'))),
    section('Tools & skills', 'Built-in file tools, memory and sub-agents are always available within the security level.',
      h('div', { class: 'field' }, h('span', {}, 'Tools'), tools.length ? chipSet(a.tools, tools.map((t) => t.id)) : h('span', { class: 'hint' }, 'No tools installed yet. ', h('a', { href: '#/tools/registry' }, 'Browse ZTOOLS →'))),
      h('div', { class: 'field', style: 'margin-top:12px' }, h('span', {}, 'Skills'), skills.length ? chipSet(a.skills, skills.map((s) => s.name)) : h('span', { class: 'hint' }, 'No skills yet. ', h('a', { href: '#/skills' }, 'Create one →')))),
    section('Collaboration', 'Whom it may message with ask_agent / tell_agent. Permissions never pass between agents: each keeps its own level.',
      h('div', { class: 'field' }, h('span', {}, 'May contact'), chipSet(a.contacts, ['*', ...others], (x) => (x === '*' ? 'everyone' : '@' + x))),
      h('div', { class: 'field', style: 'margin-top:12px' }, h('span', {}, 'Accepts requests from'), chipSet(a.accept_from, ['*', ...others], (x) => (x === '*' ? 'any agent' : '@' + x)))),
    section('Limits & sandbox', 'Applied to the agent and every process it starts. 0 means unlimited.',
      h('div', { class: 'form three' },
        h('label', { class: 'field' }, 'Parallel conversations', bind('max_parallel', { type: 'number', min: 1, max: 16 })),
        h('label', { class: 'field' }, 'Timeout per run (min)', bind('timeout_minutes', { type: 'number', min: 1, max: 1440 })),
        h('label', { class: 'field' }, 'Max tool rounds per run', bind('max_tool_rounds', { type: 'number', min: 1, max: 1000 })),
        h('label', { class: 'field' }, 'Memory limit (MB)', bind('memory_mb', { type: 'number', min: 0, step: 128 }), h('span', { class: 'hint' }, 'Windows and Linux')),
        h('label', { class: 'field' }, 'Max processes', bind('max_processes', { type: 'number', min: 0 }), h('span', { class: 'hint' }, 'Windows')),
        h('div', { class: 'field' }, h('span', {}, 'Isolation'), toggle(a.isolate, (v) => { a.isolate = v; dirty.v = true; }, 'Read-only file system except workspace (Linux + bubblewrap)'))),
      h('div', { style: 'margin-top:12px' }, toggle(a.disabled, (v) => { a.disabled = v; dirty.v = true; }, 'Disabled: queued messages wait until it is enabled again'))));

  const save = async () => {
    const body = { ...a, max_parallel: +a.max_parallel, timeout_minutes: +a.timeout_minutes, max_tool_rounds: +a.max_tool_rounds, memory_mb: +a.memory_mb || 0, max_processes: +a.max_processes || 0 };
    const out = await act(() => (isNew ? api('POST', '/api/agents', body) : api('PUT', '/api/agents/' + cur.username, body)), isNew ? 'Agent created' : 'Saved');
    dirty.v = false;
    await loadCore();
    go('agents/' + out.username);
  };
  const del = async () => {
    if (await confirmDialog(`Delete @${cur.username}?`, 'Conversations stay; its sessions remain on disk; its workspace is untouched.', 'Delete', true)) {
      await act(() => api('DELETE', '/api/agents/' + cur.username), 'Deleted'); await loadCore(); go('agents');
    }
  };
  const onKey = (e) => { if ((e.ctrlKey || e.metaKey) && e.key === 's' && !ro) { e.preventDefault(); save(); } };
  document.addEventListener('keydown', onKey);
  const leave = (e) => { if (dirty.v) { e.preventDefault(); e.returnValue = ''; } };
  window.addEventListener('beforeunload', leave);
  return {
    el: page(isNew ? 'New agent' : `${a.icon} ${cur.name}`, isNew ? 'Design an agent. You can change everything later.' : '@' + cur.username + (cur.builtin ? ' · built-in' : ''),
      [h('button', { onclick: () => go('agents') }, '← Back'),
        !isNew && can('operator') ? h('button', { onclick: () => { sessionStorage.setItem('zpilot.chat.agent', cur.username); go('chat/new'); } }, '💬 Chat') : null,
        !isNew && !cur.builtin && !ro ? h('button', { class: 'danger', onclick: del }, 'Delete') : null,
        ro ? null : h('button', { class: 'primary', onclick: save, title: 'Ctrl+S' }, isNew ? 'Create agent' : 'Save changes')],
      form),
    cleanup: () => { document.removeEventListener('keydown', onKey); window.removeEventListener('beforeunload', leave); },
  };
}
