// Configuration pages: models, tools, skills, schedules, channels.
import { h, api, act, S, loadCore, avatar, page, ago, when, dur, isSet, confirmDialog, toast, modal, md, can, emptyState, chipSet, toggle, onEvent, copyText } from './core.js';

// ================================================================ MODELS
const presets = {
  gemini: { name: 'Gemini 2.5 Flash', provider: 'gemini', base_url: '', model: 'gemini-2.5-flash', vision: true, pdf: true, context_limit: 1000000 },
  openai: { name: 'OpenAI', provider: 'openai_compatible', base_url: 'https://api.openai.com/v1', model: 'gpt-4.1-mini', vision: true },
  anthropic: { name: 'Claude', provider: 'anthropic', base_url: '', model: 'claude-sonnet-5-5', vision: true, pdf: true },
  zyreca: { name: 'Zyreca Gateway', provider: 'openai_compatible', base_url: 'https://api.zyreca.com/v1', model: '' },
  openrouter: { name: 'OpenRouter', provider: 'openai_compatible', base_url: 'https://openrouter.ai/api/v1', model: '' },
  ollama: { name: 'Ollama (local)', provider: 'openai_compatible', base_url: 'http://127.0.0.1:11434/v1', model: 'llama3.1', auth_style: 'none' },
};

export async function viewModels(args, { rerender }) {
  const models = await api('GET', '/api/models');
  S.models = models;
  const admin = can('admin');
  const cards = models.map((m) => h('div', { class: 'card pad stack' },
    h('div', { class: 'row' }, h('h3', {}, '🧠 ' + m.name), m.id === S.settings.default_model_id ? h('span', { class: 'badge accent' }, 'default') : null, h('span', { class: 'spacer' }),
      m.has_key || m.auth_style === 'none' ? h('span', { class: 'badge ok' }, 'key set') : h('span', { class: 'badge bad' }, 'no key')),
    h('div', { class: 'mono hint clip' }, `${m.provider} · ${m.model}`), h('div', { class: 'hint clip' }, m.base_url || 'provider default endpoint'),
    h('div', { class: 'row' }, ['vision', 'pdf', 'audio', 'video'].filter((k) => m[k]).map((k) => h('span', { class: 'badge' }, k))),
    admin ? h('div', { class: 'row' }, h('button', { class: 'small', onclick: () => testModel(m) }, '⚡ Test'), h('button', { class: 'small', onclick: () => modelForm(m, rerender) }, 'Edit'),
      m.id !== S.settings.default_model_id ? h('button', { class: 'small ghost', onclick: () => act(() => api('PUT', '/api/settings', { ...S.settings, default_model_id: m.id }), 'Default updated').then(loadCore).then(rerender) }, 'Make default') : null,
      h('span', { class: 'spacer' }), h('button', { class: 'small ghost danger', onclick: async () => { if (await confirmDialog('Delete model?', `Remove ${m.name} and its stored key?`, 'Delete', true)) { await act(() => api('DELETE', '/api/models/' + m.id), 'Deleted'); rerender(); } } }, 'Delete')) : null));
  return page('Models', 'Provider connections your agents think with. Keys are encrypted at rest and only ever handed to the engine process.',
    admin ? [h('button', { class: 'primary', onclick: () => modelForm(null, rerender) }, '＋ Add model')] : [],
    models.length ? h('div', { class: 'grid' }, cards) : emptyState('🧠', 'No models yet', 'Add Gemini, an OpenAI-compatible endpoint, Anthropic or a local Ollama.', admin ? h('button', { class: 'primary', onclick: () => modelForm(null, rerender) }, 'Add your first model') : null));
}

async function testModel(m) {
  const box = h('pre', { class: 'log' }, h('span', { class: 'spin' }), ' Sending one tiny request to the provider…');
  modal('Test ' + m.name, box, [(c) => h('button', { onclick: c }, 'Close')]);
  try { const r = await api('POST', `/api/models/${m.id}/test`); box.textContent = (r.ok ? '✅ The connection works.\n\n' : '❌ The test failed.\n\n') + r.report; }
  catch (e) { box.textContent = '❌ ' + e.message; }
}

function modelForm(m, rerender) {
  const v = structuredClone(m || { name: '', provider: 'gemini', base_url: '', model: '', api_format: '', context_limit: 0, compact_at: '', vision: true, audio: false, pdf: false, video: false, auth_style: '' });
  let key = null;
  const body = h('div', {});
  const draw = () => {
    const field = (label, k, props = {}) => h('label', { class: 'field' + (props.full ? ' full' : '') }, label,
      h('input', { value: v[k] ?? '', type: props.type || 'text', placeholder: props.placeholder || '', oninput: (e) => { v[k] = props.type === 'number' ? Number(e.target.value) : e.target.value; } }),
      props.hint ? h('span', { class: 'hint' }, props.hint) : null);
    body.replaceChildren(
      !m ? h('div', { class: 'field', style: 'margin-bottom:14px' }, h('span', {}, 'Start from a preset'), h('div', { class: 'chips' }, Object.values(presets).map((p) => h('span', { class: 'chip', onclick: () => { Object.assign(v, structuredClone(p)); draw(); } }, p.name)))) : null,
      h('div', { class: 'form two' },
        field('Display name', 'name', { placeholder: 'Gemini Flash' }),
        h('label', { class: 'field' }, 'Provider', h('select', { onchange: (e) => { v.provider = e.target.value; draw(); } }, ['gemini', 'openai_compatible', 'anthropic', 'custom'].map((p) => h('option', { value: p, selected: v.provider === p }, p)))),
        field('Model id', 'model', { placeholder: 'gemini-2.5-flash' }),
        field('Base URL', 'base_url', { placeholder: v.provider === 'openai_compatible' ? 'https://…/v1' : v.provider === 'custom' ? 'exact endpoint URL' : '(optional)' }),
        v.provider === 'custom' ? h('label', { class: 'field' }, 'Wire format', h('select', { onchange: (e) => { v.api_format = e.target.value; } }, ['', 'openai', 'gemini', 'anthropic'].map((f) => h('option', { value: f, selected: v.api_format === f }, f || '— choose —')))) : null,
        h('label', { class: 'field' }, m ? 'API key (leave empty to keep the stored key)' : 'API key', h('input', { type: 'password', autocomplete: 'off', placeholder: m?.has_key ? '•••••••• stored' : 'paste key', oninput: (e) => { key = e.target.value; } })),
        field('Context window (tokens)', 'context_limit', { type: 'number', placeholder: '0 = engine default' }),
        field('Compact at', 'compact_at', { placeholder: '80%' }),
        h('label', { class: 'field' }, 'Auth style', h('select', { onchange: (e) => { v.auth_style = e.target.value; } }, ['', 'bearer', 'x-api-key', 'x-goog-api-key', 'none'].map((s) => h('option', { value: s, selected: v.auth_style === s }, s || 'provider default')))),
        h('div', { class: 'field full' }, h('span', {}, 'Accepts'), h('div', { class: 'row' }, ['vision', 'audio', 'pdf', 'video'].map((k) => h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: v[k], onchange: (e) => { v[k] = e.target.checked; } }), k))))));
  };
  draw();
  modal(m ? 'Edit model' : 'Add model', body, [
    (c) => h('button', { onclick: c }, 'Cancel'),
    (c) => h('button', { class: 'primary', onclick: async () => {
      const payload = { ...v, context_limit: Number(v.context_limit) || 0 };
      if (key !== null && (key !== '' || !m)) payload.api_key = key;
      await act(() => (m ? api('PUT', '/api/models/' + m.id, payload) : api('POST', '/api/models', payload)), 'Model saved');
      c(); await loadCore(); rerender();
    } }, 'Save'),
  ]);
}

// ================================================================ TOOLS
export async function viewTools(args, { go, rerender }) {
  const tab = args[0] || 'installed';
  const installed = await api('GET', '/api/tools');
  const admin = can('admin');
  const tabs = h('div', { class: 'tabs' }, [['installed', `Installed (${installed.length})`], ['registry', 'ZTOOLS registry'], admin ? ['local', 'Install from folder'] : null].filter(Boolean)
    .map(([k, l]) => h('button', { class: tab === k ? 'on' : '', onclick: () => go('tools/' + k) }, l)));
  let content;
  if (tab === 'installed') {
    content = installed.length ? h('div', { class: 'grid' }, installed.map((t) => h('div', { class: 'card pad stack' },
      h('div', { class: 'row' }, h('h3', {}, '🧰 ' + (t.name || t.id)), h('span', { class: 'spacer' }), t.verified ? h('span', { class: 'badge ok' }, '✓ verified') : h('span', { class: 'badge warn' }, t.source)),
      h('div', { class: 'mono hint' }, `${t.id} ${t.version || ''}`), h('div', { class: 'sub' }, t.description),
      admin ? h('div', { class: 'row' }, t.has_config ? h('button', { class: 'small', onclick: () => toolConfig(t) }, '⚙ Configure') : null, h('span', { class: 'spacer' }),
        h('button', { class: 'small ghost danger', onclick: async () => { if (await confirmDialog('Remove tool?', `Uninstall ${t.id}?`, 'Remove', true)) { await act(() => api('DELETE', '/api/tools/' + t.id), 'Removed'); rerender(); } } }, 'Remove')) : null)))
      : emptyState('🧰', 'No tools installed', 'Give agents new abilities from the ZTOOLS registry.', h('button', { class: 'primary', onclick: () => go('tools/registry') }, 'Browse the registry'));
  } else if (tab === 'registry') {
    content = h('div', { class: 'grid' }, Array.from({ length: 6 }, () => h('div', { class: 'skeleton', style: 'height:170px' })));
    api('GET', '/api/registry').then((r) => {
      const cats = Object.fromEntries((r.registry.categories || []).map((c) => [c.id, c.name]));
      content.replaceWith(h('div', { class: 'grid' }, r.registry.tools.map((t) => {
        const avail = t.downloads && t.downloads[r.platform];
        const btn = h('button', { class: t.installed_version ? 'small' : 'small primary', disabled: !avail || !admin, onclick: async () => {
          btn.disabled = true; btn.replaceChildren(h('span', { class: 'spin' }), ' Installing…');
          try { await act(() => api('POST', '/api/tools/install', { id: t.id }), `Installed ${t.id}`); rerender(); }
          catch (e) {
            if (/no checksum/.test(e.message) && await confirmDialog('Install an unverified tool?', e.message, 'Install anyway', true)) { await act(() => api('POST', '/api/tools/install', { id: t.id, allow_unverified: true }), `Installed ${t.id}`); rerender(); }
            btn.disabled = false; btn.textContent = 'Install';
          }
        } }, !avail ? `Not available for ${r.platform}` : t.installed_version ? (t.installed_version === t.version ? 'Reinstall' : `Update to ${t.version}`) : 'Install');
        return h('div', { class: 'card pad stack' },
          h('div', { class: 'row' }, h('h3', {}, t.name), h('span', { class: 'spacer' }), h('span', { class: 'badge' }, cats[t.category] || t.category)),
          h('div', { class: 'mono hint' }, `${t.id} · v${t.version} · ${t.license || ''}`), h('div', { class: 'sub', style: 'flex:1' }, t.description),
          h('div', { class: 'row' }, t.installed_version ? h('span', { class: 'badge ok' }, 'installed ' + t.installed_version) : null, h('span', { class: 'spacer' }),
            t.source_url ? h('a', { href: t.source_url, target: '_blank', rel: 'noopener' }, 'Source') : null, btn));
      })));
    }).catch((e) => content.replaceWith(h('div', { class: 'banner bad' }, 'Could not load the registry: ' + e.message)));
  } else {
    const p = h('input', { placeholder: 'C:\\tools\\my_tool   or   /opt/tools/my_tool' });
    content = h('div', { class: 'card pad stack' }, h('p', { class: 'sub', style: 'margin:0' }, 'A tool folder contains a manifest named after the folder (my_tool/my_tool.json) and the program it runs. See the ZTOOLS tool specification.'),
      h('label', { class: 'field' }, 'Folder on the machine running ZPilot', p),
      h('div', {}, h('button', { class: 'primary', onclick: async () => { await act(() => api('POST', '/api/tools/install', { path: p.value.trim() }), 'Installed'); go('tools/installed'); } }, 'Install')));
  }
  return page('Tools', 'Abilities beyond the built-in file, memory and sub-agent tools. Registry downloads are checked against published SHA-256 sums.', [], tabs, content);
}

async function toolConfig(t) {
  const r = await api('GET', `/api/tools/${t.id}/config`);
  const ta = h('textarea', { rows: 14, class: 'mono' });
  ta.value = JSON.stringify(r.config, null, 2);
  modal('Configure ' + t.id, h('div', { class: 'stack' }, h('p', { class: 'sub', style: 'margin:0' }, "The tool's config.json. Changes reach agents on their next run."), ta), [
    (c) => h('button', { onclick: c }, 'Cancel'),
    (c) => h('button', { class: 'primary', onclick: async () => { let cfg; try { cfg = JSON.parse(ta.value); } catch (e) { toast('Invalid JSON: ' + e.message, 'bad'); return; } await act(() => api('PUT', `/api/tools/${t.id}/config`, { config: cfg }), 'Saved'); c(); } }, 'Save'),
  ]);
}

// ================================================================ SKILLS
export async function viewSkills(args, { rerender }) {
  const skills = await api('GET', '/api/skills');
  const admin = can('admin');
  return page('Skills', 'Reusable instruction packs an agent loads when a task matches the description (a style guide, a release checklist, a support playbook).',
    admin ? [h('button', { class: 'primary', onclick: () => skillForm(null, rerender) }, '＋ New skill')] : [],
    skills.length ? h('div', { class: 'grid' }, skills.map((s) => h('div', { class: 'card hover pad stack', onclick: () => skillForm(s.name, rerender) }, h('h3', {}, '📚 ' + s.name), h('div', { class: 'sub' }, s.description), h('span', { class: 'hint' }, `${s.files} file(s)`))))
      : emptyState('📚', 'No skills yet', 'Write a skill once and attach it to any agent.'));
}

async function skillForm(name, rerender) {
  const s = name ? await api('GET', '/api/skills/' + name) : { name: '', description: '', body: '' };
  const ro = !can('admin');
  const n = h('input', { value: s.name, disabled: !!name || ro, placeholder: 'release-checklist' });
  const d = h('input', { value: s.description, disabled: ro, placeholder: 'Use when preparing a release: version bump, changelog, tags.' });
  const b = h('textarea', { rows: 14, disabled: ro }); b.value = s.body;
  modal(name ? 'Skill · ' + name : 'New skill', h('div', { class: 'form' }, h('label', { class: 'field' }, 'Name', n, h('span', { class: 'hint' }, 'lowercase letters, digits, - and _')),
    h('label', { class: 'field' }, 'When to use it', d), h('label', { class: 'field' }, 'Instructions (Markdown)', b)), ro ? [] : [
    name ? (c) => h('button', { class: 'danger', onclick: async () => { if (await confirmDialog('Delete skill?', name, 'Delete', true)) { await act(() => api('DELETE', '/api/skills/' + name), 'Deleted'); c(); rerender(); } } }, 'Delete') : null,
    h('span', { class: 'spacer' }),
    (c) => h('button', { onclick: c }, 'Cancel'),
    (c) => h('button', { class: 'primary', onclick: async () => { await act(() => api('PUT', '/api/skills/' + encodeURIComponent(n.value.trim()), { description: d.value, body: b.value }), 'Skill saved'); c(); rerender(); } }, 'Save'),
  ], 'wide');
}

// ================================================================ SCHEDULES
const days = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
export function describeTrigger(t) {
  switch (t.kind) {
    case 'interval': return 'every ' + dur(t.every_seconds);
    case 'random_interval': return `randomly every ${dur(t.min_seconds)}–${dur(t.max_seconds)}`;
    case 'daily': return 'daily at ' + t.time;
    case 'weekly': return (t.days || []).map((d) => days[d]).join(', ') + ' at ' + t.time;
    case 'random_weekly': return (t.days || []).map((d) => days[d]).join(', ') + ` between ${t.window_start}–${t.window_end}`;
    case 'once': return 'once at ' + when(t.at);
  }
  return t.kind;
}

export async function viewSchedules(args, { go, rerender }) {
  const [scheds, wfs] = await Promise.all([api('GET', '/api/schedules'), api('GET', '/api/workflows')]);
  await loadCore();
  const byId = Object.fromEntries(S.agents.map((a) => [a.id, a]));
  const wfById = Object.fromEntries(wfs.map((w) => [w.id, w]));
  const admin = can('admin');
  const rows = scheds.map((s) => h('tr', {},
    h('td', {}, h('b', {}, s.name), h('div', { class: 'hint clip', style: 'max-width:320px' }, s.task)),
    h('td', {}, s.workflow_id ? '⚙️ ' + (wfById[s.workflow_id]?.name || '?') : byId[s.agent_id] ? `${byId[s.agent_id].icon} @${byId[s.agent_id].username}` : '?'),
    h('td', {}, describeTrigger(s.trigger), h('div', { class: 'hint' }, s.trigger.timezone)),
    h('td', {}, s.enabled ? ago(s.next_run) : h('span', { class: 'badge' }, 'paused')), h('td', {}, ago(s.last_run)),
    h('td', {}, h('div', { class: 'row nowrap' },
      can('operator') ? h('button', { class: 'small', onclick: () => act(() => api('POST', `/api/schedules/${s.id}/run`), 'Started').then((m) => go('chat/' + m.thread_id)) }, '▶ Run now') : null,
      admin ? [h('button', { class: 'small', onclick: () => scheduleForm(s, wfs, rerender) }, 'Edit'),
        h('button', { class: 'small ghost', onclick: () => act(() => api('PUT', '/api/schedules/' + s.id, { ...s, enabled: !s.enabled }), s.enabled ? 'Paused' : 'Resumed').then(rerender) }, s.enabled ? 'Pause' : 'Resume'),
        h('button', { class: 'small ghost danger', onclick: async () => { if (await confirmDialog('Delete schedule?', s.name, 'Delete', true)) { await act(() => api('DELETE', '/api/schedules/' + s.id), 'Deleted'); rerender(); } } }, '🗑')] : null))));
  return page('Schedules', 'Send a task to an agent, or run a workflow, automatically. A run missed while ZPilot was off fires once when it starts again.',
    admin ? [h('button', { class: 'primary', onclick: () => scheduleForm(null, wfs, rerender) }, '＋ New schedule')] : [],
    scheds.length ? h('div', { class: 'card table-wrap' }, h('table', { class: 't' }, h('thead', {}, h('tr', {}, ['Schedule', 'Runs', 'When', 'Next', 'Last', ''].map((x) => h('th', {}, x)))), h('tbody', {}, rows)))
      : emptyState('⏰', 'No schedules yet', 'A morning briefing, a nightly test run, a weekly report…'));
}

function scheduleForm(s, wfs, rerender) {
  const v = structuredClone(s || { name: '', agent_id: S.agents.find((a) => !a.builtin)?.id || S.agents[0]?.id || '', workflow_id: '', task: '', enabled: true, fresh_thread: false,
    trigger: { kind: 'daily', time: '09:00', timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'Local', days: [1], every_seconds: 3600 } });
  let target = v.workflow_id ? 'workflow' : 'agent';
  const body = h('div', {});
  const draw = () => {
    const t = v.trigger;
    const num = (label, k, mult = 1) => h('label', { class: 'field' }, label, h('input', { type: 'number', min: 1, value: t[k] ? t[k] / mult : '', oninput: (e) => { t[k] = Number(e.target.value) * mult; } }));
    const txt = (label, k, ph) => h('label', { class: 'field' }, label, h('input', { value: t[k] || '', placeholder: ph, oninput: (e) => { t[k] = e.target.value; } }));
    const dayChips = h('div', { class: 'chips' }, days.map((d, i) => h('span', { class: 'chip' + ((t.days || []).includes(i) ? ' on' : ''), onclick: () => { t.days = (t.days || []).includes(i) ? t.days.filter((x) => x !== i) : [...(t.days || []), i]; draw(); } }, d)));
    let extra = [];
    if (t.kind === 'interval') extra = [num('Every (minutes)', 'every_seconds', 60)];
    if (t.kind === 'random_interval') extra = [num('At least (minutes)', 'min_seconds', 60), num('At most (minutes)', 'max_seconds', 60)];
    if (t.kind === 'daily') extra = [txt('Time (HH:MM)', 'time', '09:00')];
    if (t.kind === 'weekly') extra = [h('div', { class: 'field full' }, h('span', {}, 'Days'), dayChips), txt('Time (HH:MM)', 'time', '09:00')];
    if (t.kind === 'random_weekly') extra = [h('div', { class: 'field full' }, h('span', {}, 'Days'), dayChips), txt('Window start', 'window_start', '10:00'), txt('Window end', 'window_end', '17:00')];
    if (t.kind === 'once') extra = [h('label', { class: 'field' }, 'Date and time', h('input', { type: 'datetime-local', value: isSet(t.at) ? new Date(new Date(t.at).getTime() - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 16) : '', oninput: (e) => { t.at = new Date(e.target.value).toISOString(); } }))];
    const ta = h('textarea', { rows: 4, placeholder: target === 'workflow' ? 'Input for the workflow ({{input}})' : "Summarise yesterday's commits and open issues…", oninput: (e) => { v.task = e.target.value; } }); ta.value = v.task;
    body.replaceChildren(h('div', { class: 'form two' },
      h('label', { class: 'field' }, 'Name', h('input', { value: v.name, placeholder: 'Morning briefing', oninput: (e) => { v.name = e.target.value; } })),
      h('div', { class: 'field' }, h('span', {}, 'Runs'), h('div', { class: 'seg' }, h('button', { class: target === 'agent' ? 'on' : '', onclick: () => { target = 'agent'; draw(); } }, 'An agent'), h('button', { class: target === 'workflow' ? 'on' : '', onclick: () => { target = 'workflow'; draw(); } }, 'A workflow'))),
      target === 'agent'
        ? h('label', { class: 'field full' }, 'Agent', h('select', { onchange: (e) => { v.agent_id = e.target.value; } }, S.agents.map((a) => h('option', { value: a.id, selected: a.id === v.agent_id }, `${a.icon} @${a.username}`))))
        : h('label', { class: 'field full' }, 'Workflow', h('select', { onchange: (e) => { v.workflow_id = e.target.value; } }, h('option', { value: '' }, '— choose —'), wfs.map((w) => h('option', { value: w.id, selected: w.id === v.workflow_id }, w.name)))),
      h('label', { class: 'field full' }, target === 'workflow' ? 'Input' : 'Task', ta),
      h('label', { class: 'field' }, 'Repeat', h('select', { onchange: (e) => { t.kind = e.target.value; draw(); } }, [['daily', 'Every day'], ['weekly', 'Weekly'], ['interval', 'Fixed interval'], ['random_interval', 'Random interval'], ['random_weekly', 'Random time in a weekly window'], ['once', 'Once']].map(([k, l]) => h('option', { value: k, selected: t.kind === k }, l)))),
      txt('Timezone', 'timezone', 'Asia/Kolkata'), ...extra,
      target === 'agent' ? h('div', { class: 'full' }, toggle(v.fresh_thread, (x) => { v.fresh_thread = x; }, 'Start a fresh conversation each run (otherwise the agent remembers earlier runs)')) : null,
      h('div', { class: 'full' }, toggle(v.enabled, (x) => { v.enabled = x; }, 'Enabled'))));
  };
  draw();
  modal(s ? 'Edit schedule' : 'New schedule', body, [(c) => h('button', { onclick: c }, 'Cancel'),
    (c) => h('button', { class: 'primary', onclick: async () => {
      const payload = { ...v };
      if (target === 'agent') payload.workflow_id = ''; else payload.agent_id = '';
      await act(() => (s ? api('PUT', '/api/schedules/' + s.id, payload) : api('POST', '/api/schedules', payload)), 'Schedule saved'); c(); rerender();
    } }, 'Save')]);
}

// ================================================================ CHANNELS
const kinds = {
  telegram: { icon: '✈️', label: 'Telegram', idLabel: 'Telegram user or chat ids', secret: 'Bot token', help: 'Message @BotFather on Telegram, send /newbot, and paste the token here. No public URL needed.' },
  discord: { icon: '🎮', label: 'Discord', idLabel: 'Discord user or channel ids', secret: 'Bot token', help: 'In the Discord developer portal create an application → Bot, enable the MESSAGE CONTENT intent, copy the token, and invite the bot to your server. It answers DMs and messages that mention it.' },
  email: { icon: '✉️', label: 'Email', idLabel: 'Allowed senders (addresses, or @domain.com)', secret: 'Mailbox password', help: 'Works with Gmail (use an App Password with 2-step verification), Outlook/Microsoft 365 and any IMAP/SMTP provider. Mail that fails SPF/DKIM/DMARC is ignored.' },
  whatsapp: { icon: '🟢', label: 'WhatsApp', idLabel: 'Allowed WhatsApp numbers (digits only, with country code)', secret: 'Access token', help: "Uses Meta's official WhatsApp Cloud API. ZPilot must be reachable over HTTPS: point the app's webhook to the URL below and use the same verify token." },
  webhook: { icon: '🔗', label: 'Webhook', idLabel: '', secret: 'Shared secret', help: 'Any system can POST JSON to the URL below with the shared secret as a Bearer token, and optionally wait for the reply.' },
};
const mailPresets = { gmail: { imap_host: 'imap.gmail.com', smtp_host: 'smtp.gmail.com', smtp_tls: 'starttls' }, outlook: { imap_host: 'outlook.office365.com', smtp_host: 'smtp.office365.com', smtp_tls: 'starttls' }, custom: {} };

export async function viewChannels(args, { rerender }) {
  const chans = await api('GET', '/api/channels');
  await loadCore();
  const admin = can('admin');
  const cards = chans.map((c) => {
    const k = kinds[c.kind] || {};
    return h('div', { class: 'card pad stack' },
      h('div', { class: 'row' }, h('h3', {}, `${k.icon} ${c.name}`), h('span', { class: 'spacer' }), c.enabled ? h('span', { class: 'badge ok' }, 'enabled') : h('span', { class: 'badge' }, 'disabled')),
      h('div', { class: 'sub' }, `${k.label} → @${c.agent}`), c.status ? h('div', { class: 'hint mono clip', title: c.status }, c.status) : null,
      c.kind === 'webhook' ? h('div', { class: 'row nowrap' }, h('code', { class: 'clip' }, `${location.origin}/hooks/${c.id}`), h('button', { class: 'small ghost', onclick: () => copyText(`${location.origin}/hooks/${c.id}`) }, '⧉'))
        : c.kind === 'whatsapp' ? h('div', { class: 'row nowrap' }, h('code', { class: 'clip' }, `${location.origin}/hooks/whatsapp/${c.id}`), h('button', { class: 'small ghost', onclick: () => copyText(`${location.origin}/hooks/whatsapp/${c.id}`) }, '⧉'))
          : h('div', { class: 'hint' }, 'Allowed: ', c.allowed.length ? c.allowed.join(', ') : 'nobody yet'),
      (c.rejected || []).length ? h('div', { class: 'banner warn', style: 'margin:0;flex-direction:column;align-items:stretch;gap:6px' }, h('b', {}, 'Waiting for access'),
        c.rejected.map((r) => h('div', { class: 'row nowrap' }, h('span', { class: 'clip' }, `${r.name || r.id}`), h('span', { class: 'hint clip' }, `“${r.text || ''}” · ${ago(r.last)}`), h('span', { class: 'spacer' }),
          admin ? h('button', { class: 'small primary', onclick: () => act(() => api('POST', `/api/channels/${c.id}/allow`, { sender: r.id, allow: true }), 'Allowed').then(rerender) }, 'Allow') : null))) : null,
      admin ? h('div', { class: 'row' }, h('button', { class: 'small', onclick: () => channelForm(c, c.kind, rerender) }, 'Edit'), h('span', { class: 'spacer' }),
        h('button', { class: 'small ghost danger', onclick: async () => { if (await confirmDialog('Delete channel?', c.name, 'Delete', true)) { await act(() => api('DELETE', '/api/channels/' + c.id), 'Deleted'); rerender(); } } }, 'Delete')) : null);
  });
  const off = onEvent((ev) => { if (ev.type === 'channels') rerender(); });
  return {
    el: page('Channels', 'Let people reach an agent from outside. Everything arriving here is treated as untrusted input.',
      admin ? [h('div', { class: 'row' }, Object.entries(kinds).map(([k, v]) => h('button', { class: k === 'telegram' ? 'primary' : '', onclick: () => channelForm(null, k, rerender) }, `＋ ${v.icon} ${v.label}`)))] : [],
      h('div', { class: 'banner' }, h('span', {}, '🛡️'), h('div', {}, 'Agents reachable from a channel cannot run commands unless every command needs your approval, or you explicitly allow it on the agent. Unknown senders are refused and listed here for one-click approval.')),
      chans.length ? h('div', { class: 'grid' }, cards) : emptyState('📡', 'No channels yet', 'Connect Telegram, Discord, Email, WhatsApp or a webhook.')),
    cleanup: off,
  };
}

function channelForm(c, kind, rerender) {
  const info = kinds[kind];
  const v = structuredClone(c || { kind, name: '', agent_id: S.agents.find((a) => !a.builtin)?.id || S.agents[0]?.id, enabled: true, allowed: [], settings: {} });
  v.settings ||= {};
  let secret = null, secret2 = null;
  if (!c && kind === 'webhook') secret = Array.from(crypto.getRandomValues(new Uint8Array(24)), (b) => b.toString(16).padStart(2, '0')).join('');
  if (!c && kind === 'whatsapp' && !v.settings.verify_token) v.settings.verify_token = Array.from(crypto.getRandomValues(new Uint8Array(12)), (b) => b.toString(16).padStart(2, '0')).join('');
  const allowed = h('input', { value: (v.allowed || []).join(', '), placeholder: kind === 'email' ? 'alice@example.com, @mycompany.com' : 'e.g. 123456789' });
  const inputs = {};
  const set = (k, label, ph, hint) => h('label', { class: 'field' }, label,
    (inputs[k] = h('input', { value: v.settings[k] || '', placeholder: ph || '', oninput: (e) => { v.settings[k] = e.target.value.trim(); } })), hint ? h('span', { class: 'hint' }, hint) : null);
  const applyPreset = (p) => { for (const [k, val] of Object.entries(mailPresets[p])) { v.settings[k] = val; if (inputs[k]) inputs[k].value = val; } };
  const secretField = (label, setter, stored) => h('label', { class: 'field' }, label, h('input', { type: 'password', autocomplete: 'off', placeholder: stored ? '•••••••• stored (leave empty to keep)' : '', oninput: (e) => setter(e.target.value) }));
  const body = h('div', { class: 'form' },
    h('div', { class: 'banner', style: 'margin:0' }, h('span', {}, info.icon), h('div', { class: 'hint', style: 'color:var(--text)' }, info.help)),
    h('div', { class: 'form two' },
      h('label', { class: 'field' }, 'Name', h('input', { value: v.name, placeholder: info.label + ' assistant', oninput: (e) => { v.name = e.target.value; } })),
      h('label', { class: 'field' }, 'Answered by', h('select', { onchange: (e) => { v.agent_id = e.target.value; } }, S.agents.map((a) => h('option', { value: a.id, selected: a.id === v.agent_id }, `${a.icon} @${a.username} (${a.level})`))))),
    kind === 'email' ? [
      h('div', { class: 'field' }, h('span', {}, 'Provider'), h('div', { class: 'chips' }, Object.keys(mailPresets).map((p) => h('span', { class: 'chip', onclick: () => applyPreset(p) }, p)))),
      h('div', { class: 'form two' }, set('address', 'Mailbox address', 'assistant@gmail.com'), set('username', 'Login (if different)', ''),
        set('imap_host', 'IMAP host', 'imap.gmail.com'), set('imap_port', 'IMAP port', '993'), set('smtp_host', 'SMTP host', 'smtp.gmail.com'), set('smtp_port', 'SMTP port', '587'),
        set('smtp_tls', 'SMTP security', 'starttls | tls'), set('poll_seconds', 'Check every (seconds)', '60'))] : null,
    kind === 'whatsapp' ? h('div', { class: 'form two' }, set('phone_number_id', 'Phone number id', 'from the WhatsApp > API setup page'), set('verify_token', 'Webhook verify token', '', 'Enter the same value in Meta\'s webhook settings.'),
      c ? h('div', { class: 'field full' }, h('span', {}, 'Callback URL'), h('code', {}, `${location.origin}/hooks/whatsapp/${c.id}`)) : h('div', { class: 'hint full' }, 'The callback URL is shown after saving.')) : null,
    secretField(info.secret, (x) => { secret = x; }, c?.has_secret),
    kind === 'whatsapp' ? secretField('App secret (verifies webhook signatures)', (x) => { secret2 = x; }, c?.has_secret2) : null,
    kind === 'webhook' && !c ? h('div', { class: 'field' }, h('span', {}, 'Shared secret (shown only now)'), h('div', { class: 'row nowrap' }, h('code', { class: 'clip' }, secret), h('button', { class: 'small', onclick: () => copyText(secret) }, 'Copy'))) : null,
    info.idLabel ? h('label', { class: 'field' }, info.idLabel, allowed, h('span', { class: 'hint' }, 'Leave empty, write to the bot once, then allow yourself from the “Waiting for access” list.')) : null,
    toggle(v.enabled, (x) => { v.enabled = x; }, 'Enabled'));
  modal(c ? 'Edit ' + info.label + ' channel' : 'Connect ' + info.label, body, [(close) => h('button', { onclick: close }, 'Cancel'),
    (close) => h('button', { class: 'primary', onclick: async () => {
      v.allowed = allowed.value.split(',').map((x) => x.trim()).filter(Boolean);
      const payload = { ...v };
      if (secret) payload.secret = secret;
      if (secret2) payload.secret2 = secret2;
      await act(() => (c ? api('PUT', '/api/channels/' + c.id, payload) : api('POST', '/api/channels', payload)), 'Channel saved');
      close(); rerender();
    } }, 'Save')], 'wide');
}
