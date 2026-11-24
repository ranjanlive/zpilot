// Workflows: list, visual graph builder and live run view.
import { h, api, act, S, loadCore, agentBy, avatar, page, ago, dur, isSet, confirmDialog, toast, md, can, emptyState, onEvent, modal } from './core.js';

const NODE_W = 220;

export async function viewWorkflows(args, ctx) {
  if (args[0]) return editor(args[0], args[1] === 'run' ? args[2] : null, ctx);
  const { go } = ctx;
  const [wfs, runs] = await Promise.all([api('GET', '/api/workflows'), api('GET', '/api/workflow-runs')]);
  await loadCore();
  const stateBadge = (s) => h('span', { class: 'badge ' + ({ running: 'accent', done: 'ok', failed: 'bad', canceled: 'warn' }[s] || '') }, s === 'running' ? h('span', { class: 'spin' }) : null, s);
  const cards = wfs.map((w) => h('div', { class: 'card hover pad stack', onclick: () => go('workflows/' + w.id) },
    h('div', { class: 'row' }, h('h3', {}, '⚙️ ' + w.name), h('span', { class: 'spacer' }), h('span', { class: 'badge' }, `${w.steps.length} steps`)),
    h('div', { class: 'sub' }, w.description || h('span', { class: 'hint' }, 'No description')),
    h('div', { class: 'row' }, [...new Set(w.steps.map((s) => s.agent))].map((u) => avatar(agentBy(u), 'xs')), h('span', { class: 'spacer' }),
      can('operator') ? h('button', { class: 'small primary', onclick: (e) => { e.stopPropagation(); runDialog(w, go); } }, '▶ Run') : null)));
  const runRows = runs.slice(0, 20).map((r) => h('tr', { style: 'cursor:pointer', onclick: () => go(`workflows/${r.workflow_id}/run/${r.id}`) },
    h('td', {}, h('b', {}, r.name)), h('td', {}, stateBadge(r.state)),
    h('td', { class: 'num' }, `${Object.values(r.steps).filter((s) => s.state === 'done').length}/${Object.keys(r.steps).length}`),
    h('td', {}, ago(r.started)), h('td', { class: 'num' }, isSet(r.ended) ? dur((new Date(r.ended) - new Date(r.started)) / 1000) : '…'), h('td', { class: 'hint' }, r.by)));
  const off = onEvent((ev) => { if (ev.type === 'workflow_run' || ev.type === 'workflows') ctx.rerender(); });
  return {
    el: page('Workflows', 'Chain agents into repeatable pipelines: steps run in parallel when they can, pass outputs forward with {{steps.<id>.output}}, and can branch on conditions.',
      can('admin') ? [h('button', { class: 'primary', onclick: () => go('workflows/new') }, '＋ New workflow')] : [],
      wfs.length ? h('div', { class: 'grid' }, cards) : emptyState('⚙️', 'No workflows yet', 'Build a pipeline such as research → draft → review, then run it on demand or on a schedule.',
        can('admin') ? h('button', { class: 'primary', onclick: () => go('workflows/new') }, 'Create a workflow') : null),
      runs.length ? [h('div', { class: 'section-title' }, 'Recent runs'), h('div', { class: 'card table-wrap' }, h('table', { class: 't' },
        h('thead', {}, h('tr', {}, ['Workflow', 'State', 'Steps', 'Started', 'Duration', 'By'].map((x) => h('th', {}, x)))), h('tbody', {}, runRows)))] : null),
    cleanup: off,
  };
}

function runDialog(w, go) {
  const input = h('textarea', { rows: 5, placeholder: 'Input for {{input}} (optional)' });
  modal('Run ' + w.name, h('label', { class: 'field' }, 'Input', input, h('span', { class: 'hint' }, 'Steps can use it as {{input}}.')), [
    (c) => h('button', { onclick: c }, 'Cancel'),
    (c) => h('button', { class: 'primary', onclick: async () => { const r = await act(() => api('POST', `/api/workflows/${w.id}/run`, { input: input.value }), 'Workflow started'); c(); go(`workflows/${w.id}/run/${r.id}`); } }, '▶ Run'),
  ]);
}

function autoLayout(steps) {
  const level = {};
  const lv = (s, seen = new Set()) => {
    if (level[s.id] != null) return level[s.id];
    if (seen.has(s.id)) return 0;
    seen.add(s.id);
    const deps = s.depends_on || [];
    level[s.id] = deps.length ? 1 + Math.max(...deps.map((d) => lv(steps.find((x) => x.id === d) || { id: d }, seen))) : 0;
    return level[s.id];
  };
  steps.forEach((s) => lv(s));
  const rows = {};
  for (const s of steps) { const l = level[s.id] || 0; rows[l] = (rows[l] || 0) + 1; s.x = 60 + l * 300; s.y = 60 + (rows[l] - 1) * 150; }
}

async function editor(id, runId, { go }) {
  await loadCore();
  const isNew = id === 'new';
  const wf = isNew ? { name: 'New workflow', description: '', steps: [] } : await api('GET', '/api/workflows/' + id);
  let run = runId ? await api('GET', '/api/workflow-runs/' + runId) : null;
  const ro = !can('admin') || !!run;
  if (run) wf.steps = run.graph;
  const view = { x: 0, y: 0, z: 1 };
  let sel = null; // {type:'node', id} | {type:'edge', from, to}
  let dirty = false;

  const wrap = h('div', { class: 'wf-canvas-wrap' });
  const canvas = h('div', { class: 'wf-canvas' });
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.classList.add('wf-edges');
  svg.setAttribute('width', '4000'); svg.setAttribute('height', '3000');
  canvas.append(svg);
  wrap.append(canvas);
  const side = h('div', { class: 'wf-side' });
  const applyView = () => { canvas.style.transform = `translate(${view.x}px,${view.y}px) scale(${view.z})`; };

  const step = (sid) => wf.steps.find((s) => s.id === sid);
  const nodeEls = {};
  function drawNodes() {
    for (const el of Object.values(nodeEls)) el.remove();
    for (const k of Object.keys(nodeEls)) delete nodeEls[k];
    for (const s of wf.steps) {
      const a = agentBy(s.agent);
      const sr = run?.steps?.[s.id];
      const el = h('div', { class: 'wf-node' + (sel?.type === 'node' && sel.id === s.id ? ' sel' : '') + (sr ? ' s-' + sr.state : ''), style: `left:${s.x}px;top:${s.y}px`, 'data-id': s.id },
        h('div', { class: 'port in', title: 'Drop a connection here' }),
        h('div', { class: 'hd' }, a ? avatar(a, 'xs') : h('span', {}, '❔'), h('b', { class: 'clip' }, s.id), h('span', { class: 'spacer' }),
          sr ? h('span', { class: 'badge ' + ({ running: 'accent', done: 'ok', failed: 'bad', skipped: '', canceled: 'warn' }[sr.state] || '') }, sr.state === 'running' ? h('span', { class: 'spin' }) : null, sr.state) : h('span', { class: 'hint' }, '@' + (s.agent || '?'))),
        h('div', { class: 'bd' }, s.prompt || h('i', {}, 'No prompt yet')),
        s.when ? h('span', { class: 'badge warn cond', title: `runs only if ${s.when.step} ${s.when.not ? 'does not contain' : 'contains'} "${s.when.contains}"` }, 'if') : null,
        h('div', { class: 'port out', title: 'Drag to another step to connect' }));
      nodeEls[s.id] = el;
      canvas.append(el);
      bindNode(el, s);
    }
    drawEdges();
  }
  function portPos(sid, side2) {
    const el = nodeEls[sid];
    const s = step(sid);
    if (!el || !s) return { x: 0, y: 0 };
    return { x: s.x + (side2 === 'out' ? NODE_W : 0), y: s.y + el.offsetHeight / 2 };
  }
  const curve = (p, q) => { const dx = Math.max(40, Math.abs(q.x - p.x) / 2); return `M${p.x},${p.y} C${p.x + dx},${p.y} ${q.x - dx},${q.y} ${q.x},${q.y}`; };
  function drawEdges(temp) {
    svg.replaceChildren();
    const ns = 'http://www.w3.org/2000/svg';
    for (const s of wf.steps) for (const d of s.depends_on || []) {
      const dpath = curve(portPos(d, 'out'), portPos(s.id, 'in'));
      const hit = document.createElementNS(ns, 'path'); hit.setAttribute('d', dpath); hit.setAttribute('class', 'hit');
      const p = document.createElementNS(ns, 'path'); p.setAttribute('d', dpath);
      const live = run && run.steps[s.id]?.state === 'running';
      p.setAttribute('class', (sel?.type === 'edge' && sel.from === d && sel.to === s.id ? 'sel ' : '') + (live ? 'live' : ''));
      if (!ro) hit.addEventListener('mousedown', (e) => { e.stopPropagation(); sel = { type: 'edge', from: d, to: s.id }; drawEdges(); drawSide(); });
      svg.append(hit, p);
    }
    if (temp) { const p = document.createElementNS(ns, 'path'); p.setAttribute('d', curve(temp.a, temp.b)); p.setAttribute('class', 'live'); svg.append(p); }
  }
  const toCanvas = (e) => { const r = wrap.getBoundingClientRect(); return { x: (e.clientX - r.left - view.x) / view.z, y: (e.clientY - r.top - view.y) / view.z }; };

  function bindNode(el, s) {
    el.addEventListener('pointerdown', (e) => {
      e.stopPropagation();
      if (e.target.classList.contains('port') && e.target.classList.contains('out') && !ro) {
        // connect: drag a temporary edge to another node's input port
        const a = portPos(s.id, 'out');
        const move = (ev) => drawEdges({ a, b: toCanvas(ev) });
        const up = (ev) => {
          window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', up);
          const target = document.elementFromPoint(ev.clientX, ev.clientY)?.closest('.wf-node');
          const tid = target?.dataset.id;
          if (tid && tid !== s.id) {
            const t = step(tid);
            t.depends_on ||= [];
            if (!t.depends_on.includes(s.id)) { t.depends_on.push(s.id); dirty = true; }
          }
          drawEdges();
        };
        window.addEventListener('pointermove', move); window.addEventListener('pointerup', up);
        return;
      }
      sel = { type: 'node', id: s.id };
      for (const [k, n] of Object.entries(nodeEls)) n.classList.toggle('sel', k === s.id);
      drawSide();
      if (ro) return;
      const start = { x: e.clientX, y: e.clientY, sx: s.x, sy: s.y };
      const move = (ev) => {
        s.x = Math.round(start.sx + (ev.clientX - start.x) / view.z); s.y = Math.round(start.sy + (ev.clientY - start.y) / view.z);
        el.style.left = s.x + 'px'; el.style.top = s.y + 'px'; dirty = true; drawEdges();
      };
      const up = () => { window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', up); };
      window.addEventListener('pointermove', move); window.addEventListener('pointerup', up);
    });
  }

  // pan & zoom
  wrap.addEventListener('pointerdown', (e) => {
    if (e.target !== wrap && e.target !== canvas && e.target !== svg) return;
    sel = null; for (const n of Object.values(nodeEls)) n.classList.remove('sel'); drawEdges(); drawSide();
    const start = { x: e.clientX, y: e.clientY, vx: view.x, vy: view.y };
    wrap.classList.add('panning');
    const move = (ev) => { view.x = start.vx + ev.clientX - start.x; view.y = start.vy + ev.clientY - start.y; applyView(); };
    const up = () => { wrap.classList.remove('panning'); window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', up); };
    window.addEventListener('pointermove', move); window.addEventListener('pointerup', up);
  });
  wrap.addEventListener('wheel', (e) => {
    e.preventDefault();
    const r = wrap.getBoundingClientRect();
    const mx = e.clientX - r.left, my = e.clientY - r.top;
    const nz = Math.min(2, Math.max(0.3, view.z * (e.deltaY < 0 ? 1.1 : 0.9)));
    view.x = mx - (mx - view.x) * (nz / view.z); view.y = my - (my - view.y) * (nz / view.z); view.z = nz; applyView();
  }, { passive: false });
  const zoom = (f) => { view.z = Math.min(2, Math.max(0.3, view.z * f)); applyView(); };
  const fit = () => {
    if (!wf.steps.length) return;
    const r = wrap.getBoundingClientRect();
    const minX = Math.min(...wf.steps.map((s) => s.x)), minY = Math.min(...wf.steps.map((s) => s.y));
    const maxX = Math.max(...wf.steps.map((s) => s.x + NODE_W)), maxY = Math.max(...wf.steps.map((s) => s.y + 100));
    view.z = Math.min(1.2, Math.max(0.3, Math.min((r.width - 80) / (maxX - minX), (r.height - 120) / (maxY - minY))));
    view.x = 40 - minX * view.z; view.y = 64 - minY * view.z; applyView();
  };

  function addStep() {
    let n = wf.steps.length + 1;
    while (step('step' + n)) n++;
    const last = wf.steps[wf.steps.length - 1];
    const s = { id: 'step' + n, agent: S.agents.find((a) => !a.builtin)?.username || S.agents[0]?.username || '', prompt: last ? `Continue from this:\n\n{{steps.${last.id}.output}}` : 'Work on this:\n\n{{input}}',
      depends_on: last ? [last.id] : [], x: last ? last.x + 300 : 60, y: last ? last.y : 60 };
    wf.steps.push(s); sel = { type: 'node', id: s.id }; dirty = true; drawNodes(); drawSide();
  }
  function removeSelected() {
    if (ro || !sel) return;
    if (sel.type === 'node') {
      wf.steps = wf.steps.filter((s) => s.id !== sel.id);
      for (const s of wf.steps) { s.depends_on = (s.depends_on || []).filter((d) => d !== sel.id); if (s.when?.step === sel.id) delete s.when; }
    } else {
      const t = step(sel.to); t.depends_on = t.depends_on.filter((d) => d !== sel.from);
    }
    sel = null; dirty = true; drawNodes(); drawSide();
  }

  // ---- side panel
  function drawSide() {
    if (run) return drawRunSide();
    const s = sel?.type === 'node' ? step(sel.id) : null;
    if (!s) {
      const nm = h('input', { value: wf.name, disabled: ro, oninput: (e) => { wf.name = e.target.value; dirty = true; } });
      const ds = h('textarea', { rows: 3, disabled: ro, oninput: (e) => { wf.description = e.target.value; dirty = true; } }); ds.value = wf.description || '';
      side.replaceChildren(h('h3', { style: 'margin:0' }, 'Workflow'), h('label', { class: 'field' }, 'Name', nm), h('label', { class: 'field' }, 'Description', ds),
        sel?.type === 'edge' ? h('div', { class: 'banner' }, `Connection ${sel.from} → ${sel.to}`, h('span', { class: 'spacer' }), h('button', { class: 'small danger', onclick: removeSelected }, 'Remove')) : null,
        h('div', { class: 'hint' }, ro ? 'Read-only.' : 'Add steps with ＋ Step. Drag a step to move it, drag from its right handle to another step to connect, click a step to edit it. Delete removes the selection.'),
        wf.id ? h('div', { class: 'stack' }, h('div', { class: 'section-title', style: 'margin:8px 0 0' }, 'Recent runs'), h('div', { id: 'wf-runs' }, h('span', { class: 'hint' }, 'Loading…'))) : null);
      if (wf.id) api('GET', '/api/workflow-runs?workflow=' + wf.id).then((runs) => {
        const box = document.getElementById('wf-runs');
        if (box) box.replaceChildren(...(runs.length ? runs.slice(0, 8).map((r) => h('div', { class: 'row', style: 'cursor:pointer;padding:4px 0', onclick: () => go(`workflows/${wf.id}/run/${r.id}`) },
          h('span', { class: 'badge ' + ({ running: 'accent', done: 'ok', failed: 'bad' }[r.state] || '') }, r.state), h('span', { class: 'hint' }, ago(r.started)))) : [h('span', { class: 'hint' }, 'Not run yet.')]));
      });
      return;
    }
    const upstream = wf.steps.filter((x) => x.id !== s.id && isUpstream(s.id, x.id)).map((x) => x.id);
    const idIn = h('input', { value: s.id, disabled: ro, onchange: (e) => {
      const nid = e.target.value.trim().toLowerCase().replace(/[^a-z0-9_]/g, '_');
      if (!nid || step(nid)) { e.target.value = s.id; toast('Step ids must be unique', 'bad'); return; }
      for (const x of wf.steps) { x.depends_on = (x.depends_on || []).map((d) => (d === s.id ? nid : d)); x.prompt = x.prompt.replaceAll(`{{steps.${s.id}.output}}`, `{{steps.${nid}.output}}`); if (x.when?.step === s.id) x.when.step = nid; }
      s.id = nid; sel.id = nid; dirty = true; drawNodes(); drawSide();
    } });
    const prompt = h('textarea', { rows: 9, disabled: ro, oninput: (e) => { s.prompt = e.target.value; dirty = true; const b = nodeEls[s.id]?.querySelector('.bd'); if (b) b.textContent = s.prompt; } });
    prompt.value = s.prompt;
    const insert = (t) => { const p = prompt.selectionStart ?? prompt.value.length; prompt.value = prompt.value.slice(0, p) + t + prompt.value.slice(p); s.prompt = prompt.value; dirty = true; prompt.focus(); };
    const condOn = !!s.when;
    const condBox = h('div', { class: 'stack' });
    const drawCond = () => condBox.replaceChildren(...(s.when ? [
      h('div', { class: 'row nowrap' }, h('span', { class: 'hint' }, 'Run only if'), h('select', { disabled: ro, onchange: (e) => { s.when.step = e.target.value; dirty = true; } }, upstream.map((u) => h('option', { value: u, selected: s.when.step === u }, u)))),
      h('div', { class: 'row nowrap' }, h('select', { disabled: ro, style: 'width:auto', onchange: (e) => { s.when.not = e.target.value === 'not'; dirty = true; } }, h('option', { value: 'yes', selected: !s.when.not }, 'contains'), h('option', { value: 'not', selected: s.when.not }, 'does not contain')),
        h('input', { value: s.when.contains || '', disabled: ro, placeholder: 'text, e.g. APPROVED', oninput: (e) => { s.when.contains = e.target.value; dirty = true; } }))] : []));
    drawCond();
    side.replaceChildren(
      h('div', { class: 'row' }, h('h3', { style: 'margin:0' }, 'Step'), h('span', { class: 'spacer' }), ro ? null : h('button', { class: 'small danger', onclick: removeSelected }, 'Delete step')),
      h('label', { class: 'field' }, 'Step id', idIn),
      h('label', { class: 'field' }, 'Agent', h('select', { disabled: ro, onchange: (e) => { s.agent = e.target.value; dirty = true; drawNodes(); } }, S.agents.map((a) => h('option', { value: a.username, selected: a.username === s.agent }, `${a.icon} @${a.username}`)))),
      h('label', { class: 'field' }, 'Prompt', prompt),
      ro ? null : h('div', { class: 'chips' }, h('span', { class: 'chip', onclick: () => insert('{{input}}') }, '{{input}}'), upstream.map((u) => h('span', { class: 'chip', onclick: () => insert(`{{steps.${u}.output}}`) }, `${u}.output`))),
      h('div', { class: 'field' }, h('span', {}, 'Depends on'), h('div', { class: 'hint' }, (s.depends_on || []).join(', ') || 'nothing (starts immediately)')),
      upstream.length ? h('div', { class: 'field' }, h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: condOn, disabled: ro, onchange: (e) => { s.when = e.target.checked ? { step: upstream[0], contains: '', not: false } : undefined; if (!s.when) delete s.when; dirty = true; drawCond(); drawNodes(); } }), 'Conditional step'), condBox) : null);
  }
  function isUpstream(id, target, seen = new Set()) {
    const s = step(id);
    for (const d of s?.depends_on || []) { if (d === target) return true; if (!seen.has(d)) { seen.add(d); if (isUpstream(d, target, seen)) return true; } }
    return false;
  }
  function drawRunSide() {
    const sid = sel?.type === 'node' ? sel.id : null;
    const head = h('div', { class: 'stack' },
      h('div', { class: 'row' }, h('h3', { style: 'margin:0' }, 'Run'), h('span', { class: 'badge ' + ({ running: 'accent', done: 'ok', failed: 'bad', canceled: 'warn' }[run.state] || '') }, run.state === 'running' ? h('span', { class: 'spin' }) : null, run.state),
        h('span', { class: 'spacer' }), run.state === 'running' && can('operator') ? h('button', { class: 'small danger', onclick: () => act(() => api('POST', `/api/workflow-runs/${run.id}/cancel`), 'Canceled') }, 'Cancel') : null),
      h('div', { class: 'hint' }, `Started ${ago(run.started)} by ${run.by}` + (isSet(run.ended) ? ` · took ${dur((new Date(run.ended) - new Date(run.started)) / 1000)}` : '')),
      h('div', { class: 'row' }, h('button', { class: 'small', onclick: () => go('chat/' + run.thread_id) }, '💬 Open conversation'), h('button', { class: 'small', onclick: () => go('workflows/' + run.workflow_id) }, '✎ Edit workflow')),
      run.input ? h('div', { class: 'field' }, h('span', {}, 'Input'), h('div', { class: 'card pad', style: 'max-height:120px;overflow:auto' }, md(run.input))) : null);
    if (!sid) { side.replaceChildren(head, h('div', { class: 'hint' }, 'Click a step to see its output.')); return; }
    const sr = run.steps[sid];
    side.replaceChildren(head, h('div', { class: 'section-title', style: 'margin:6px 0 0' }, `Step ${sid}`),
      h('div', { class: 'row' }, h('span', { class: 'badge' }, sr.state), isSet(sr.started) ? h('span', { class: 'hint' }, ago(sr.started)) : null),
      sr.error ? h('div', { class: 'banner bad' }, sr.error) : null,
      sr.output ? h('div', { class: 'card pad', style: 'max-height:50vh;overflow:auto' }, md(sr.output)) : h('div', { class: 'hint' }, sr.state === 'running' ? 'Working…' : 'No output.'));
  }

  // ---- actions
  async function save() {
    const body = { ...wf, steps: wf.steps.map((s) => ({ ...s, depends_on: s.depends_on || [] })) };
    const out = await act(() => (wf.id ? api('PUT', '/api/workflows/' + wf.id, body) : api('POST', '/api/workflows', body)), 'Workflow saved');
    dirty = false;
    if (!wf.id) go('workflows/' + out.id); else Object.assign(wf, out);
  }
  const toolbar = h('div', { class: 'wf-tools' },
    h('button', { class: 'small', onclick: () => go('workflows') }, '← All'),
    !ro ? [h('button', { class: 'small', onclick: addStep }, '＋ Step'), h('button', { class: 'small', onclick: () => { autoLayout(wf.steps); dirty = true; drawNodes(); fit(); } }, '⇶ Arrange'),
      h('button', { class: 'small primary', onclick: save, title: 'Ctrl+S' }, 'Save')] : null,
    wf.id && !run && can('operator') ? h('button', { class: 'small', onclick: async () => { if (dirty && can('admin')) await save(); runDialog(wf, go); } }, '▶ Run') : null,
    wf.id && !run && can('admin') ? h('button', { class: 'small ghost danger', onclick: async () => { if (await confirmDialog('Delete workflow?', wf.name, 'Delete', true)) { await act(() => api('DELETE', '/api/workflows/' + wf.id), 'Deleted'); go('workflows'); } } }, 'Delete') : null);
  const zoomBar = h('div', { class: 'wf-zoom' }, h('button', { class: 'small', onclick: () => zoom(1.15) }, '＋'), h('button', { class: 'small', onclick: () => zoom(1 / 1.15) }, '－'), h('button', { class: 'small', onclick: fit }, 'Fit'));
  wrap.append(toolbar, zoomBar);

  const onKey = (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 's' && !ro) { e.preventDefault(); save(); }
    if ((e.key === 'Delete' || e.key === 'Backspace') && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement?.tagName)) removeSelected();
  };
  document.addEventListener('keydown', onKey);
  if (isNew && !wf.steps.length) setTimeout(addStep, 0);

  const el = h('div', { class: 'wf' }, wrap, side);
  setTimeout(() => { drawNodes(); drawSide(); applyView(); fit(); }, 0);
  const off = onEvent((ev) => {
    if (run && ev.type === 'workflow_run' && ev.data.id === run.id) { run = ev.data; drawNodes(); drawSide(); }
  });
  return { el, fullHeight: true, cleanup: () => { off(); document.removeEventListener('keydown', onKey); } };
}
