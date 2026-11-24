/* zagent web interface. A thin view over the engine's control API: all state
 * lives in the engine; this page renders its event log and live stream. */
(function () {
  'use strict';

  const $ = (id) => document.getElementById(id);
  const el = {
    log: $('log'), input: $('input'), send: $('send'), note: $('note'), stop: $('stop'), resume: $('resume'),
    status: $('status'), statusText: $('status-text'), banner: $('banner'), jump: $('jump'), hint: $('hint'),
    title: $('session-title'), sub: $('session-sub'), ctxNow: $('ctx-now'), ctxLimit: $('ctx-limit'),
    ctxBar: $('ctx-bar'), totals: $('totals'), chips: $('chips'), file: $('file'),
  };

  // ---- token ----
  let token = '';
  (function readToken() {
    const m = /(?:^|[#&])token=([0-9a-f]+)/.exec(location.hash);
    if (m) {
      token = m[1];
      try { sessionStorage.setItem('zagent-token', token); } catch (e) { /* private mode */ }
      history.replaceState(null, '', location.pathname + location.search); // keep the secret out of history
    } else {
      try { token = sessionStorage.getItem('zagent-token') || ''; } catch (e) { token = ''; }
    }
  })();

  const state = {
    seq: 0, status: 'idle', compactAt: 0, ctx: 0, counters: null,
    attachments: [], streamEl: null, streamText: '', thinking: false, tools: new Map(),
    stuck: true, source: null, retry: 0, activity: '',
  };

  // ---- api ----
  async function api(path, method, body) {
    const opts = { method: method || 'GET', headers: { Authorization: 'Bearer ' + token } };
    if (body !== undefined) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
    const resp = await fetch(path, opts);
    if (!resp.ok) {
      let msg = resp.statusText;
      try { msg = (await resp.json()).error || msg; } catch (e) { /* not json */ }
      throw new Error(msg);
    }
    const text = await resp.text();
    return text ? JSON.parse(text) : null;
  }

  // ---- small helpers ----
  function h(tag, cls, text) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text;
    return n;
  }
  function fmtTokens(n) {
    if (n >= 1e6) return (n / 1e6).toFixed(n >= 1e7 ? 0 : 1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(n >= 1e4 ? 0 : 1) + 'k';
    return String(n);
  }
  function fmtBytes(n) {
    if (n >= 1048576) return (n / 1048576).toFixed(1) + ' MB';
    if (n >= 1024) return Math.round(n / 1024) + ' KB';
    return n + ' B';
  }
  function showBanner(msg, kind) {
    if (!msg) { el.banner.hidden = true; return; }
    el.banner.textContent = msg;
    el.banner.className = 'banner' + (kind ? ' ' + kind : '');
    el.banner.hidden = false;
  }
  function append(node) {
    el.log.appendChild(node);
    if (state.stuck) el.log.scrollTop = el.log.scrollHeight;
  }

  // ---- markdown (DOM-built: model output can never inject markup) ----
  function inline(text, parent) {
    const re = /(`[^`\n]+`)|(\*\*[^*\n]+\*\*)|(\*[^*\n]+\*)|(\[[^\]\n]+\]\((https?:\/\/[^)\s]+|mailto:[^)\s]+)\))/g;
    let last = 0, m;
    while ((m = re.exec(text))) {
      if (m.index > last) parent.appendChild(document.createTextNode(text.slice(last, m.index)));
      if (m[1]) parent.appendChild(h('code', '', m[1].slice(1, -1)));
      else if (m[2]) { const s = h('strong'); inline(m[2].slice(2, -2), s); parent.appendChild(s); } // allow `code` inside bold
      else if (m[3]) { const s = h('em'); inline(m[3].slice(1, -1), s); parent.appendChild(s); }
      else {
        const a = h('a', '', m[4].slice(1, m[4].indexOf(']')));
        a.href = m[5]; a.target = '_blank'; a.rel = 'noopener noreferrer';
        parent.appendChild(a);
      }
      last = re.lastIndex;
    }
    if (last < text.length) parent.appendChild(document.createTextNode(text.slice(last)));
  }

  function markdown(src) {
    const frag = document.createDocumentFragment();
    const lines = src.replace(/\r\n/g, '\n').split('\n');
    let i = 0, para = [];
    const flush = () => {
      if (!para.length) return;
      const p = h('p');
      inline(para.join('\n'), p);
      frag.appendChild(p);
      para = [];
    };
    while (i < lines.length) {
      const line = lines[i];
      const fence = /^```\s*([\w+-]*)\s*$/.exec(line);
      if (fence) {
        flush();
        const body = [];
        i++;
        while (i < lines.length && !/^```\s*$/.test(lines[i])) body.push(lines[i++]);
        i++;
        const wrap = h('div', 'codeblock');
        if (fence[1]) wrap.appendChild(h('span', 'lang', fence[1]));
        wrap.appendChild(h('pre', '', body.join('\n')));
        frag.appendChild(wrap);
        continue;
      }
      const head = /^(#{1,3})\s+(.*)$/.exec(line);
      if (head) { flush(); const e = h('h' + head[1].length); inline(head[2], e); frag.appendChild(e); i++; continue; }
      if (/^\s*([-*])\s+/.test(line) || /^\s*\d+[.)]\s+/.test(line)) {
        flush();
        const ordered = /^\s*\d+[.)]\s+/.test(line);
        const list = h(ordered ? 'ol' : 'ul');
        while (i < lines.length && (ordered ? /^\s*\d+[.)]\s+/ : /^\s*[-*]\s+/).test(lines[i])) {
          const li = h('li');
          inline(lines[i].replace(ordered ? /^\s*\d+[.)]\s+/ : /^\s*[-*]\s+/, ''), li);
          list.appendChild(li);
          i++;
        }
        frag.appendChild(list);
        continue;
      }
      if (/^>\s?/.test(line)) {
        flush();
        const q = h('blockquote'), parts = [];
        while (i < lines.length && /^>\s?/.test(lines[i])) parts.push(lines[i++].replace(/^>\s?/, ''));
        inline(parts.join('\n'), q);
        frag.appendChild(q);
        continue;
      }
      if (line.trim() === '') { flush(); i++; continue; }
      para.push(line);
      i++;
    }
    flush();
    return frag;
  }

  // ---- message rendering ----
  function mediaNode(m, withThumb) {
    if (withThumb && m.kind === 'image' && m.blob) {
      const img = h('img');
      img.alt = m.name || 'image';
      img.src = '/api/blob/' + m.blob + '?token=' + token;
      img.onerror = () => img.replaceWith(h('span', 'file', '🖼 ' + (m.name || 'image') + ' (removed from context)'));
      return img;
    }
    const icon = { image: '🖼', audio: '🔊', video: '🎞', pdf: '📄' }[m.kind] || '📎';
    return h('span', 'file', icon + ' ' + (m.name || m.mime) + (m.size ? ' · ' + fmtBytes(m.size) : ''));
  }

  function bubble(role, label) {
    const wrap = h('div', 'msg ' + role);
    wrap.appendChild(h('div', 'who', label));
    const b = h('div', 'bubble');
    wrap.appendChild(b);
    return { wrap, body: b };
  }

  function addToolCall(call) {
    const wrap = h('div', 'tool running');
    const d = h('details');
    const sum = h('summary');
    sum.appendChild(h('span', 'tname', call.name));
    sum.appendChild(h('span', 'targs', briefArgs(call.args)));
    const st = h('span', 'tstate', 'running…');
    sum.appendChild(st);
    d.appendChild(sum);
    d.appendChild(h('div', 'label', 'arguments'));
    d.appendChild(h('pre', '', prettyJSON(call.args)));
    wrap.appendChild(d);
    state.tools.set(call.id, { wrap, details: d, stateEl: st });
    append(wrap);
  }

  function briefArgs(args) {
    try { return JSON.stringify(typeof args === 'string' ? JSON.parse(args) : args); } catch (e) { return String(args || ''); }
  }
  function prettyJSON(args) {
    try { return JSON.stringify(typeof args === 'string' ? JSON.parse(args) : args, null, 2); } catch (e) { return String(args || ''); }
  }

  function addToolResult(r) {
    const t = state.tools.get(r.call_id);
    const text = (r.output || '').length > 20000 ? r.output.slice(0, 20000) + '\n… (truncated in this view)' : (r.output || '');
    if (!t) { const s = h('div', 'sys'); const b = h('div', 'box'); b.appendChild(h('pre', '', r.name + ': ' + text)); s.appendChild(b); append(s); return; }
    t.wrap.classList.remove('running');
    t.wrap.classList.add(r.is_error ? 'err' : 'ok');
    t.stateEl.textContent = r.is_error ? 'error' : 'done';
    t.details.appendChild(h('div', 'label', r.is_error ? 'error' : 'result'));
    t.details.appendChild(h('pre', '', text));
    if (r.media && r.media.length) {
      const box = h('div', 'media');
      r.media.forEach((m) => box.appendChild(mediaNode(m, true)));
      t.details.appendChild(box);
    }
  }

  function renderMessage(m) {
    const info = [];
    if (m.role === 'user') {
      const texts = [], media = [];
      m.parts.forEach((p) => {
        if (p.type === 'text') texts.push(p.text);
        else if (p.type === 'media') media.push(p.media);
        else if (p.type === 'tool_result') addToolResult(p.result);
        else if (p.type === 'info') info.push(p.text);
      });
      if (texts.length || media.length) {
        const isResume = texts.length && texts[0].startsWith('[info: context compacted]');
        if (!isResume) {
          const b = bubble('user', 'you');
          texts.forEach((t) => b.body.appendChild(markdown(t)));
          if (media.length) { const box = h('div', 'media'); media.forEach((x) => box.appendChild(mediaNode(x, true))); b.body.appendChild(box); }
          append(b.wrap);
        }
      }
      info.forEach((t) => append(h('div', 'note', t.replace(/^\[info:\s*/, '').replace(/\]$/, ''))));
      return;
    }
    // assistant
    let textEl = null;
    m.parts.forEach((p) => {
      if (p.type === 'text' && p.text.trim()) {
        if (!textEl) { textEl = bubble('agent', 'agent'); }
        textEl.body.appendChild(markdown(p.text));
      }
    });
    if (state.streamEl) { state.streamEl.remove(); state.streamEl = null; state.streamText = ''; }
    if (textEl) append(textEl.wrap);
    m.parts.forEach((p) => { if (p.type === 'tool_call') addToolCall(p.call); });
  }

  function renderEvent(ev) {
    if (ev.seq && ev.seq <= state.seq) return;
    if (ev.seq) state.seq = ev.seq;
    switch (ev.kind) {
      case 'message': if (ev.message) renderMessage(ev.message); break;
      case 'note': break; // shown once, when delivered inside the next message
      case 'compaction': {
        const c = ev.compaction || {};
        const d = h('div', 'divider');
        const det = h('details');
        det.appendChild(h('summary', '', 'context compacted — ' + (c.reason || '')));
        det.appendChild(h('pre', '', c.summary || ''));
        d.appendChild(det);
        append(d);
        break;
      }
      case 'prune': append(h('div', 'divider', (ev.blobs ? ev.blobs.length : 0) + ' attachment(s) removed from context')); break;
      case 'status': renderStatus(ev); break;
      case 'usage': if (ev.usage) { state.ctx = ev.usage.prompt_tokens; renderMeter(); } break;
      default: break;
    }
  }

  function renderStatus(ev) {
    const kinds = { error: 'error', warning: 'warn', paused: 'warn', stopped: 'warn' };
    const kind = kinds[ev.state];
    if (!kind) return;
    const s = h('div', 'sys ' + kind), b = h('div', 'box');
    b.appendChild(h('pre', '', ev.state + (ev.detail ? ': ' + ev.detail : '')));
    s.appendChild(b);
    append(s);
  }

  // ---- header / composer state ----
  function renderMeter() {
    el.ctxNow.textContent = fmtTokens(state.ctx || 0);
    if (state.compactAt > 0) {
      el.ctxLimit.textContent = ' / ' + fmtTokens(state.compactAt);
      const pct = Math.min(100, (state.ctx / state.compactAt) * 100);
      el.ctxBar.style.width = pct + '%';
      el.ctxBar.className = pct > 90 ? 'hot' : pct > 70 ? 'warn' : '';
    } else { el.ctxLimit.textContent = ''; el.ctxBar.style.width = '0'; }
    const c = state.counters;
    if (c) {
      const est = c.estimated ? '~' : '';
      el.totals.textContent = '↑ ' + est + fmtTokens(c.prompt) + '  ↓ ' + est + fmtTokens(c.completion + c.thought) + (c.cached ? '  cached ' + fmtTokens(c.cached) : '') + '  · ' + c.requests + ' req';
    }
  }

  const stateText = {
    idle: 'idle', working: 'working', compacting: 'compacting context', paused: 'paused — network trouble',
    waiting_bg: 'waiting for background tasks', closed: 'engine stopped',
  };
  function setStatus(s, detail) {
    state.status = s;
    el.status.dataset.state = s;
    let text = stateText[s] || s;
    if (s === 'working') text = state.activity || 'working';
    el.statusText.textContent = text;
    el.statusText.classList.toggle('dots', s === 'working' || s === 'compacting');
    const busy = s === 'working' || s === 'compacting' || s === 'paused';
    el.send.hidden = busy;               // while the agent works you can only add a note
    el.note.hidden = !busy;
    el.stop.hidden = !busy;
    el.resume.hidden = s !== 'paused';
    el.hint.textContent = busy
      ? 'The agent is working. A note is attached to the end of its next request · /stop interrupts'
      : 'Enter sends · Shift+Enter for a new line · /help for commands';
    el.input.placeholder = busy ? 'Send a note to the agent…' : 'Message the agent…  (/help for commands)';
    if (s === 'paused' && detail) showBanner(detail, '');
    else if (s !== 'paused') showBanner('');
    if (s === 'closed') showBanner('The engine for this session has stopped. Start it again with: zagent --session <name> --ui web', 'error');
  }

  async function refreshState() {
    const s = await api('/api/state');
    el.title.textContent = s.title || s.session;
    el.sub.textContent = s.session + ' · ' + s.model + ' · ' + s.workspace;
    document.title = (s.title || s.session) + ' — zagent';
    state.compactAt = s.compact_at; state.counters = s.counters; state.ctx = s.counters.context;
    renderMeter();
    setStatus(s.state, s.detail);
    return s;
  }

  // ---- live events ----
  function ensureStream() {
    if (state.streamEl) return state.streamEl;
    const b = bubble('agent', 'agent');
    state.streamEl = b.wrap; state.streamBody = b.body; state.streamText = '';
    append(b.wrap);
    return b.wrap;
  }
  let renderPending = false;
  function scheduleStreamRender() {
    if (renderPending) return;
    renderPending = true;
    requestAnimationFrame(() => {
      renderPending = false;
      if (!state.streamBody) return;
      state.streamBody.replaceChildren(state.streamText ? markdown(state.streamText) : h('span', 'thinking dots', 'thinking'));
      if (state.stuck) el.log.scrollTop = el.log.scrollHeight;
    });
  }

  function onLive(l) {
    switch (l.kind) {
      case 'text': ensureStream(); state.streamText += l.text; state.activity = 'writing'; setStatus('working'); scheduleStreamRender(); break;
      case 'thought': ensureStream(); state.activity = 'thinking'; setStatus('working'); scheduleStreamRender(); break;
      case 'tool_call': state.activity = 'preparing ' + l.name; setStatus('working'); break;
      case 'tool_run': state.activity = 'running ' + l.name; setStatus('working'); break;
      case 'tool_done': state.activity = 'thinking'; setStatus('working'); break;
      case 'state': state.activity = ''; setStatus(l.state, l.detail); if (l.state === 'idle' || l.state === 'waiting_bg') refreshState().catch(() => {}); break;
      case 'usage': if (l.tokens) { state.counters = l.tokens; renderMeter(); } break;
      case 'retry':
        if (l.text === 'reset_stream' && state.streamEl) { state.streamEl.remove(); state.streamEl = null; state.streamText = ''; }
        if (l.detail) showBanner(l.detail, '');
        break;
      default: break;
    }
  }

  // ---- connection ----
  function connect() {
    if (state.source) state.source.close();
    const es = new EventSource('/api/stream?since=' + state.seq + '&token=' + token);
    state.source = es;
    es.addEventListener('event', (e) => { try { renderEvent(JSON.parse(e.data)); } catch (err) { console.error(err); } });
    es.addEventListener('live', (e) => { try { onLive(JSON.parse(e.data)); } catch (err) { console.error(err); } });
    es.onopen = () => { state.retry = 0; if (el.banner.classList.contains('error')) showBanner(''); };
    es.onerror = async () => {
      es.close();
      state.retry++;
      try { await api('/api/state'); } catch (err) {
        setStatus('closed');
        return;
      }
      setTimeout(connect, Math.min(5000, 400 * state.retry));
    };
  }

  // ---- composer ----
  function autosize() { el.input.style.height = 'auto'; el.input.style.height = Math.min(220, el.input.scrollHeight) + 'px'; }
  el.input.addEventListener('input', autosize);
  el.input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); submit(); }
  });

  async function submit() {
    const text = el.input.value.trim();
    if (!text && !state.attachments.length) return;
    const busy = el.send.hidden;
    try {
      if (text.startsWith('/')) {
        const r = await api('/api/command', 'POST', { line: text });
        if (r.reply) append(sysLine(r.reply));
        if (r.exit) append(sysLine('This page is detached. The agent keeps running; reopen it with zagent --session ' + (el.sub.textContent.split(' · ')[0]) + ' --ui web'));
        if (r.new_session) append(sysLine('Start a new session with: zagent --new --ui web'));
      } else if (busy) {
        await api('/api/note', 'POST', { text });
        append(h('div', 'note', 'note sent: ' + text));
      } else {
        const files = state.attachments.map((a) => a.path);
        await api('/api/prompt', 'POST', { text, attachments: files });
        state.attachments = []; renderChips();
      }
      el.input.value = ''; autosize();
    } catch (err) {
      append(sysLine('⚠ ' + err.message, 'error'));
    }
  }
  function sysLine(text, kind) {
    const s = h('div', 'sys' + (kind ? ' ' + kind : '')), b = h('div', 'box');
    b.appendChild(h('pre', '', text)); s.appendChild(b);
    return s;
  }
  el.send.addEventListener('click', submit);
  el.note.addEventListener('click', submit);
  el.stop.addEventListener('click', () => api('/api/stop', 'POST').catch((e) => append(sysLine('⚠ ' + e.message, 'error'))));
  el.resume.addEventListener('click', () => api('/api/resume', 'POST').catch(() => {}));

  function renderChips() {
    el.chips.replaceChildren();
    state.attachments.forEach((a, i) => {
      const c = h('span', 'chip', a.name + ' · ' + fmtBytes(a.size));
      const x = h('button', '', '×');
      x.title = 'Remove';
      x.addEventListener('click', () => { state.attachments.splice(i, 1); renderChips(); });
      c.appendChild(x);
      el.chips.appendChild(c);
    });
  }
  async function upload(files) {
    for (const f of files) {
      try {
        const resp = await fetch('/api/upload?name=' + encodeURIComponent(f.name), { method: 'POST', headers: { Authorization: 'Bearer ' + token }, body: f });
        const j = await resp.json();
        if (!resp.ok) throw new Error(j.error || resp.statusText);
        state.attachments.push({ path: j.path, name: f.name, size: j.size });
        renderChips();
      } catch (err) { append(sysLine('⚠ could not attach ' + f.name + ': ' + err.message, 'error')); }
    }
  }
  el.file.addEventListener('change', () => { upload(Array.from(el.file.files)); el.file.value = ''; });
  document.addEventListener('dragover', (e) => e.preventDefault());
  document.addEventListener('drop', (e) => { e.preventDefault(); if (e.dataTransfer.files.length) upload(Array.from(e.dataTransfer.files)); });
  el.input.addEventListener('paste', (e) => {
    const files = Array.from(e.clipboardData.files || []);
    if (files.length) { e.preventDefault(); upload(files); }
  });

  // ---- scrolling ----
  el.log.addEventListener('scroll', () => {
    state.stuck = el.log.scrollHeight - el.log.scrollTop - el.log.clientHeight < 80;
    el.jump.hidden = state.stuck;
  });
  el.jump.addEventListener('click', () => { el.log.scrollTop = el.log.scrollHeight; });

  // ---- start ----
  async function start() {
    if (!token) {
      el.title.textContent = 'Not authorised';
      showBanner('Open this page with the full URL that zagent printed: it carries the access token after "#token=".', 'error');
      return;
    }
    try {
      await refreshState();
      const events = await api('/api/events?since=0');
      state.stuck = true;
      events.forEach(renderEvent);
      // Replaying history ends with the state the engine is in right now.
      await refreshState();
      connect();
      setInterval(() => refreshState().catch(() => {}), 4000);
    } catch (err) {
      el.title.textContent = 'Cannot reach the engine';
      showBanner(err.message, 'error');
    }
  }
  start();
})();
