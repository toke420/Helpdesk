/* Help Desk chat widget. Embed with:
 *   <script src="https://help.example.com/widget.js" async></script>
 * Optional JS API: HelpDesk.open(), HelpDesk.close(), HelpDesk.identify({ name, email })
 */
(function () {
  'use strict';
  if (window.HelpDesk && window.HelpDesk.__loaded) return;

  var script = document.currentScript || document.querySelector('script[src$="/widget.js"]');
  var BASE = new URL(script ? script.src : '/widget.js', location.href).origin;
  var KEY = 'hd_visitor_token';

  var store = {
    get: function () { try { return localStorage.getItem(KEY) || ''; } catch (e) { return ''; } },
    set: function (v) { try { v ? localStorage.setItem(KEY, v) : localStorage.removeItem(KEY); } catch (e) { } },
  };

  var s = {
    token: store.get(),
    settings: { brand_name: 'Support', accent_color: '#2f6fed', greeting: '', offline_message: '', online: false },
    contact: { name: '', email: '' },
    cases: [],          // this visitor's cases, newest first
    view: 'chat',       // 'chat' | 'list'
    current: null,      // the case shown in chat view; null = a new, unsent conversation
    messages: [],
    unseen: {},         // case id -> true when an agent replied while it wasn't on screen
    open: false,
    badge: 0,
    picked: false,
    sending: false,
    pendingIdentify: null,
    typing: null,       // { case_id, name } while the AI agent is writing
  };

  // ---------- network ----------
  function req(method, path, body) {
    var headers = { 'Content-Type': 'application/json' };
    if (s.token) headers.Authorization = 'Bearer ' + s.token;
    return fetch(BASE + path, { method: method, headers: headers, body: body ? JSON.stringify(body) : undefined, mode: 'cors', credentials: 'omit' })
      .then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (d) {
          if (!r.ok) {
            var err = new Error(d.error || 'Something went wrong');
            err.status = r.status;
            throw err;
          }
          return d;
        });
      });
  }
  function page() { return location.origin + location.pathname; }

  function session(create) {
    return req('POST', '/api/widget/session', { token: s.token, page: page(), create: !!create }).then(function (d) {
      s.settings = d.settings;
      if (d.contact) s.contact = d.contact;
      if (d.token !== s.token) { s.token = d.token; store.set(d.token); }
      applyTheme();
      render();
      if (s.token) {
        startStream();
        if (s.pendingIdentify) { var p = s.pendingIdentify; s.pendingIdentify = null; identify(p); }
        return loadCases().then(pickInitial);
      }
    });
  }

  function loadCases() {
    if (!s.token) return Promise.resolve();
    return req('GET', '/api/widget/cases').then(function (d) {
      s.cases = d.cases || [];
      if (s.current) {
        var fresh = findCase(s.current.id);
        if (fresh) s.current = fresh;
      }
      render();
    });
  }

  // First open: continue the latest unfinished case, otherwise start fresh.
  function pickInitial() {
    if (s.picked) return s.current ? openCase(s.current.id, true, true) : null; // re-sync after a reconnect
    s.picked = true;
    var active = s.cases.filter(function (c) { return c.status !== 'closed'; })[0];
    if (active) return openCase(active.id, true);
    newConversation(true);
  }

  function openCase(id, quiet, keepView) {
    return req('GET', '/api/widget/cases/' + id).then(function (d) {
      s.current = d.case;
      s.messages = d.messages || [];
      if (!keepView) s.view = 'chat';
      if (s.view === 'chat' && s.open) delete s.unseen[id];
      render();
      if (!quiet) focusInput();
    }).catch(function (e) {
      if (e.status === 404) { s.cases = s.cases.filter(function (c) { return c.id !== id; }); newConversation(quiet); }
    });
  }

  function newConversation(quiet) {
    s.current = null;
    s.messages = [];
    s.view = 'chat';
    render();
    if (!quiet) focusInput();
  }

  function findCase(id) {
    for (var i = 0; i < s.cases.length; i++) if (s.cases[i].id === id) return s.cases[i];
    return null;
  }

  function upsertCase(c) {
    var i;
    for (i = 0; i < s.cases.length; i++) if (s.cases[i].id === c.id) break;
    if (i < s.cases.length) s.cases[i] = c; else s.cases.unshift(c);
    s.cases.sort(function (a, b) { return b.id - a.id; });
    if (s.current && s.current.id === c.id) s.current = c;
  }

  // Server-Sent Events over fetch (so the token rides in a header, not the URL).
  var streamCtl = null, retry = 1000;
  function startStream() {
    if (streamCtl || !s.token || !window.ReadableStream) return;
    var ctl = new AbortController();
    streamCtl = ctl;
    fetch(BASE + '/api/widget/stream', { headers: { Authorization: 'Bearer ' + s.token }, signal: ctl.signal, mode: 'cors', credentials: 'omit' })
      .then(function (r) {
        if (r.status === 401) { s.token = ''; store.set(''); throw new Error('expired'); }
        if (!r.ok || !r.body) throw new Error('stream ' + r.status);
        retry = 1000;
        var reader = r.body.getReader(), dec = new TextDecoder(), buf = '';
        function pump() {
          return reader.read().then(function (x) {
            if (x.done) throw new Error('closed');
            buf += dec.decode(x.value, { stream: true });
            var i;
            while ((i = buf.indexOf('\n\n')) >= 0) {
              var chunk = buf.slice(0, i); buf = buf.slice(i + 2);
              var data = chunk.split('\n').filter(function (l) { return l.indexOf('data: ') === 0; }).map(function (l) { return l.slice(6); }).join('\n');
              if (data) { try { onEvent(JSON.parse(data)); } catch (e) { } }
            }
            return pump();
          });
        }
        return pump();
      })
      .catch(function () { })
      .then(function () {
        if (streamCtl !== ctl) return;
        streamCtl = null;
        if (!s.token) return;
        setTimeout(function () { loadCases().then(pickInitial).catch(function () { }); startStream(); }, retry);
        retry = Math.min(retry * 2, 30000);
      });
  }

  var listTimer;
  function refreshListSoon() {
    clearTimeout(listTimer);
    listTimer = setTimeout(function () { loadCases().catch(function () { }); }, 300);
  }

  function onEvent(ev) {
    if (ev.type === 'presence') {
      s.settings.online = !!ev.data.online;
      render();
    } else if (ev.type === 'message') {
      var m = ev.data;
      var onScreen = s.open && s.view === 'chat' && s.current && s.current.id === m.conversation_id;
      if (s.current && s.current.id === m.conversation_id) {
        if (!s.messages.some(function (x) { return x.id === m.id; })) s.messages.push(m);
      }
      if (m.author_type === 'bot' && s.typing && s.typing.case_id === m.conversation_id) s.typing = null;
      if (m.author_type === 'agent' || m.author_type === 'bot') {
        if (!onScreen) s.unseen[m.conversation_id] = true;
        if (!s.open) s.badge++;
      }
      refreshListSoon();
      render();
    } else if (ev.type === 'case') {
      upsertCase(ev.data);
      render();
    } else if (ev.type === 'typing') {
      s.typing = ev.data.on ? ev.data : null;
      if (s.view === 'chat') render();
    }
  }

  function send(text) {
    if (s.sending) return;
    s.sending = true;
    ui.send.disabled = true;
    var ensure = s.token ? Promise.resolve() : session(true);
    ensure.then(function () {
      var body = { body: text, page: page() };
      if (s.current && s.current.status !== 'closed') body.case_id = s.current.id;
      else body.new = true;
      return req('POST', '/api/widget/messages', body);
    }).then(function (d) {
      if (!s.current || s.current.id !== d.case.id) s.messages = [];
      upsertCase(d.case);
      s.current = d.case;
      if (!s.messages.some(function (x) { return x.id === d.message.id; })) s.messages.push(d.message);
      ui.input.value = '';
      autosize();
      render();
    }).catch(function (e) {
      flash(e.message);
    }).then(function () {
      s.sending = false;
      ui.send.disabled = false;
    });
  }

  function identify(p) {
    if (!p) return Promise.resolve();
    if (!s.token) { s.pendingIdentify = p; return Promise.resolve(); }
    var body = {};
    if (typeof p.name === 'string') body.name = p.name;
    if (typeof p.email === 'string') body.email = p.email;
    return req('POST', '/api/widget/profile', body).then(function (d) {
      s.contact = d.contact;
      render();
    });
  }

  // ---------- UI ----------
  var css = [
    ':host{all:initial}',
    '*{box-sizing:border-box}',
    '.wrap{position:fixed;right:20px;bottom:20px;z-index:2147483000;font:14px/1.45 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;color:#1b1f27;-webkit-font-smoothing:antialiased}',
    'button{font:inherit}',
    '.bubble{width:56px;height:56px;border-radius:50%;border:0;background:var(--a);color:#fff;cursor:pointer;display:grid;place-items:center;box-shadow:0 6px 24px rgba(16,24,40,.22);transition:transform .15s ease;position:relative;margin-left:auto}',
    '.bubble:hover{transform:scale(1.05)}',
    '.bubble:focus-visible,.send:focus-visible,.hb:focus-visible,.case:focus-visible,.newbtn:focus-visible,.link:focus-visible{outline:3px solid var(--a);outline-offset:2px}',
    '.bubble svg{width:26px;height:26px}',
    '.badge{position:absolute;top:-2px;right:-2px;min-width:20px;height:20px;padding:0 5px;border-radius:10px;background:#d23b3b;color:#fff;font-size:12px;font-weight:700;display:grid;place-items:center;border:2px solid #fff}',
    '.badge[hidden]{display:none}',
    '.panel{position:absolute;right:0;bottom:72px;width:370px;height:min(600px,calc(100vh - 120px));background:#fff;border-radius:16px;box-shadow:0 12px 48px rgba(16,24,40,.24);display:flex;flex-direction:column;overflow:hidden;opacity:0;transform:translateY(12px) scale(.98);transform-origin:bottom right;pointer-events:none;transition:opacity .16s ease,transform .16s ease}',
    '.panel.on{opacity:1;transform:none;pointer-events:auto}',
    '.head{background:var(--a);color:#fff;padding:14px 12px 12px 18px;display:flex;align-items:center;gap:6px}',
    '.head .t{flex:1;min-width:0}',
    '.head .n{font-weight:700;font-size:16px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}',
    '.head .st{font-size:12.5px;opacity:.92;display:flex;align-items:center;gap:6px;margin-top:1px}',
    '.dot{width:8px;height:8px;border-radius:50%;background:#bfc6d1;flex:none}',
    '.dot.on{background:#4ade80}',
    '.hb{border:0;background:rgba(255,255,255,.16);color:#fff;height:32px;min-width:32px;padding:0 9px;border-radius:8px;cursor:pointer;display:inline-flex;align-items:center;justify-content:center;gap:5px;font-size:13px;font-weight:600;position:relative;flex:none}',
    '.hb:hover{background:rgba(255,255,255,.28)}',
    '.hb[hidden]{display:none}',
    '.hb svg{width:16px;height:16px;flex:none}',
    '.hb .pip{position:absolute;top:4px;right:4px;width:8px;height:8px;border-radius:50%;background:#ff6b6b;border:1.5px solid var(--a)}',
    '.casebar{display:flex;align-items:center;gap:8px;padding:7px 16px;border-bottom:1px solid #e3e6eb;background:#fff;font-size:12.5px;color:#5f6877}',
    '.casebar[hidden]{display:none}',
    '.casebar strong{color:#1b1f27;font-weight:600}',
    '.chip{display:inline-flex;align-items:center;height:19px;padding:0 7px;border-radius:999px;font-size:11px;font-weight:700;text-transform:capitalize;background:#eef0f3;color:#5f6877;flex:none}',
    '.chip.open{background:#e5f6ec;color:#17784a}',
    '.chip.pending{background:#fdf1dc;color:#9a620a}',
    '.body{flex:1;overflow-y:auto;padding:16px;display:flex;flex-direction:column;gap:8px;background:#f6f7f9}',
    '.m{max-width:82%;display:flex;flex-direction:column;gap:2px}',
    '.m .b{padding:9px 12px;border-radius:14px;white-space:pre-wrap;overflow-wrap:anywhere}',
    '.m .by{font-size:11.5px;color:#8b93a1;padding:0 4px}',
    '.m.them{align-self:flex-start}',
    '.m.them .b{background:#fff;border:1px solid #e3e6eb;border-bottom-left-radius:4px}',
    '.m.me{align-self:flex-end}',
    '.m.me .by{text-align:right}',
    '.m.me .b{background:var(--a);color:#fff;border-bottom-right-radius:4px}',
    '.sys{align-self:center;font-size:12px;color:#8b93a1;text-align:center;padding:4px 8px}',
    '.card{background:#fff;border:1px solid #e3e6eb;border-radius:12px;padding:12px;display:grid;gap:8px;align-self:stretch}',
    '.card p{margin:0;font-size:13px;color:#5f6877}',
    '.card input{height:36px;border:1px solid #cfd4dc;border-radius:8px;padding:0 10px;font:inherit;color:inherit;width:100%}',
    '.card input:focus,.foot textarea:focus{outline:2px solid var(--a);outline-offset:0;border-color:transparent}',
    '.card button{height:34px;border:0;border-radius:8px;background:var(--a);color:#fff;font-weight:600;cursor:pointer}',
    '.card .err{color:#d23b3b;font-size:12.5px}',
    '.link{align-self:stretch;display:flex;align-items:center;gap:10px;background:#fff;border:1px solid #e3e6eb;border-radius:12px;padding:10px 12px;cursor:pointer;text-align:left;color:inherit}',
    '.link:hover{border-color:#cfd4dc;background:#fbfbfc}',
    '.link .lt{flex:1;min-width:0}',
    '.link .lt div:first-child{font-weight:600}',
    '.link .lt div+div{font-size:12.5px;color:#5f6877}',
    '.link .arr{color:#8b93a1;font-size:18px}',
    '.foot{border-top:1px solid #e3e6eb;padding:10px;display:flex;gap:8px;align-items:flex-end;background:#fff}',
    '.foot[hidden]{display:none}',
    '.foot textarea{flex:1;border:1px solid #cfd4dc;border-radius:10px;padding:9px 11px;font:inherit;color:inherit;resize:none;max-height:120px;min-height:40px;background:#fff}',
    '.send{width:40px;height:40px;border-radius:10px;border:0;background:var(--a);color:#fff;cursor:pointer;display:grid;place-items:center;flex:none}',
    '.send:disabled{opacity:.5;cursor:default}',
    '.send svg{width:18px;height:18px}',
    '.closedfoot{border-top:1px solid #e3e6eb;padding:12px;background:#fff;display:grid;gap:6px;text-align:center}',
    '.closedfoot[hidden]{display:none}',
    '.closedfoot span{font-size:12.5px;color:#5f6877}',
    '.newbtn{height:40px;border:0;border-radius:10px;background:var(--a);color:#fff;font-weight:600;cursor:pointer;display:flex;align-items:center;justify-content:center;gap:6px;width:100%}',
    '.newbtn svg{width:16px;height:16px}',
    '.sect{font-size:11.5px;font-weight:700;text-transform:uppercase;letter-spacing:.05em;color:#8b93a1;margin:10px 2px 0}',
    '.case{display:grid;grid-template-columns:1fr auto;gap:2px 10px;width:100%;text-align:left;background:#fff;border:1px solid #e3e6eb;border-radius:12px;padding:10px 12px;cursor:pointer;color:inherit}',
    '.case:hover{border-color:#cfd4dc;background:#fbfbfc}',
    '.case .ct{font-weight:600;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;display:flex;align-items:center;gap:6px;min-width:0}',
    '.case .ct span{overflow:hidden;text-overflow:ellipsis}',
    '.case .new{width:8px;height:8px;border-radius:50%;background:var(--a);flex:none}',
    '.case .cm{grid-column:1/-1;font-size:12px;color:#8b93a1}',
    '.case .cp{grid-column:1/-1;font-size:12.5px;color:#5f6877;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}',
    '.case.closed .ct{color:#5f6877;font-weight:500}',
    '.empty{text-align:center;color:#8b93a1;font-size:13px;padding:24px 8px}',
    '.aitag{display:inline-block;margin-left:5px;padding:0 5px;border-radius:4px;background:#efe9fd;color:#6b3fd4;font-size:10px;font-weight:700;line-height:15px;vertical-align:1px}',
    '.typing{display:flex;gap:4px;align-items:center;height:36px}',
    '.typing i{width:6px;height:6px;border-radius:50%;background:#8b93a1;animation:hdblink 1.2s infinite both}',
    '.typing i:nth-child(2){animation-delay:.2s}.typing i:nth-child(3){animation-delay:.4s}',
    '@keyframes hdblink{0%,80%,100%{opacity:.25}40%{opacity:1}}',
    '.flash{position:absolute;left:12px;right:12px;bottom:70px;background:#1b1f27;color:#fff;padding:8px 12px;border-radius:8px;font-size:13px}',
    '@media (max-width:480px){.wrap{right:12px;bottom:12px}.panel{position:fixed;inset:0;width:auto;height:auto;border-radius:0;bottom:0}.wrap.open .bubble{display:none}}',
    '@media (prefers-reduced-motion:reduce){.panel,.bubble{transition:none}}',
  ].join('\n');

  var ICON_CHAT = '<svg viewBox="0 0 24 24" fill="none"><path d="M4 5h16v11H9l-5 4V5z" fill="currentColor"/></svg>';
  var ICON_DOWN = '<svg viewBox="0 0 24 24" fill="none"><path d="M6 9l6 6 6-6" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  var ICON_SEND = '<svg viewBox="0 0 24 24" fill="none"><path d="M4 12l16-8-6 16-2.5-6.5L4 12z" fill="currentColor"/></svg>';
  var ICON_LIST = '<svg viewBox="0 0 24 24" fill="none"><path d="M8 6h12M8 12h12M8 18h12M4 6h.01M4 12h.01M4 18h.01" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"/></svg>';
  var ICON_BACK = '<svg viewBox="0 0 24 24" fill="none"><path d="M15 6l-6 6 6 6" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  var ICON_PLUS = '<svg viewBox="0 0 24 24" fill="none"><path d="M12 5v14M5 12h14" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"/></svg>';
  var ICON_X = '<svg viewBox="0 0 24 24" fill="none"><path d="M6 6l12 12M18 6L6 18" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"/></svg>';

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function iconButton(cls, icon, label, text) {
    var b = el('button', cls);
    b.type = 'button';
    b.innerHTML = icon;
    b.setAttribute('aria-label', label);
    b.title = label;
    if (text) b.append(document.createTextNode(text));
    return b;
  }

  var host = el('div');
  host.id = 'helpdesk-widget';
  var root = host.attachShadow ? host.attachShadow({ mode: 'open' }) : host;
  var style = el('style');
  style.textContent = css;
  var ui = {};
  ui.wrap = el('div', 'wrap');
  ui.panel = el('div', 'panel');
  ui.panel.setAttribute('role', 'dialog');
  ui.panel.setAttribute('aria-label', 'Chat with us');
  ui.panel.setAttribute('aria-hidden', 'true');

  var head = el('div', 'head');
  ui.back = iconButton('hb', ICON_BACK, 'Back');
  ui.back.onclick = function () {
    if (s.current || s.messages.length) { s.view = 'chat'; render(); } else newConversation();
  };
  var t = el('div', 't');
  ui.name = el('div', 'n', 'Support');
  ui.status = el('div', 'st');
  t.append(ui.name, ui.status);
  ui.casesBtn = iconButton('hb', ICON_LIST, 'Your cases', 'Cases');
  ui.casesBtn.onclick = function () { s.view = 'list'; render(); loadCases().catch(function () { }); };
  var x = iconButton('hb', ICON_X, 'Close chat');
  x.onclick = function () { toggle(false); };
  head.append(ui.back, t, ui.casesBtn, x);

  ui.casebar = el('div', 'casebar');
  ui.body = el('div', 'body');
  ui.body.setAttribute('role', 'log');
  ui.body.setAttribute('aria-live', 'polite');

  ui.foot = el('div', 'foot');
  ui.input = el('textarea');
  ui.input.rows = 1;
  ui.input.placeholder = 'Write a message…';
  ui.input.setAttribute('aria-label', 'Message');
  ui.input.maxLength = 5000;
  ui.send = iconButton('send', ICON_SEND, 'Send');
  ui.foot.append(ui.input, ui.send);

  ui.closedFoot = el('div', 'closedfoot');
  ui.closedNote = el('span');
  var startNew = iconButton('newbtn', ICON_PLUS, 'Start a new conversation', 'Start a new conversation');
  startNew.onclick = function () { newConversation(); };
  ui.closedFoot.append(ui.closedNote, startNew);

  ui.panel.append(head, ui.casebar, ui.body, ui.foot, ui.closedFoot);
  ui.bubble = el('button', 'bubble');
  ui.bubble.setAttribute('aria-label', 'Open chat');
  ui.bubble.setAttribute('aria-expanded', 'false');
  ui.bubble.innerHTML = ICON_CHAT;
  ui.badge = el('span', 'badge');
  ui.badge.hidden = true;
  ui.wrap.append(ui.panel, ui.bubble);
  root.append(style, ui.wrap);

  function autosize() {
    ui.input.style.height = 'auto';
    ui.input.style.height = Math.min(ui.input.scrollHeight, 120) + 'px';
  }
  function focusInput() { setTimeout(function () { if (!ui.foot.hidden) ui.input.focus(); }, 60); }
  ui.input.addEventListener('input', autosize);
  ui.input.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); submit(); }
  });
  ui.send.onclick = submit;
  ui.bubble.onclick = function () { toggle(!s.open); };
  ui.wrap.addEventListener('keydown', function (e) { if (e.key === 'Escape' && s.open) toggle(false); });

  function submit() {
    var v = ui.input.value.trim();
    if (v) send(v);
  }

  function toggle(open) {
    s.open = open;
    ui.panel.classList.toggle('on', open);
    ui.wrap.classList.toggle('open', open);
    ui.panel.setAttribute('aria-hidden', open ? 'false' : 'true');
    ui.bubble.setAttribute('aria-expanded', open ? 'true' : 'false');
    ui.bubble.setAttribute('aria-label', open ? 'Close chat' : 'Open chat');
    ui.bubble.innerHTML = open ? ICON_DOWN : ICON_CHAT;
    if (open) {
      s.badge = 0;
      if (!s.token) session(true).catch(function (e) { flash(e.message); });
      else if (s.current && s.view === 'chat') delete s.unseen[s.current.id];
      render();
      focusInput();
    }
    renderBadge();
  }

  function renderBadge() {
    ui.badge.hidden = !s.badge || s.open;
    ui.badge.textContent = s.badge > 9 ? '9+' : String(s.badge);
    if (!ui.badge.parentNode) ui.bubble.append(ui.badge);
  }

  function applyTheme() {
    var c = /^#[0-9a-f]{6}$/i.test(s.settings.accent_color) ? s.settings.accent_color : '#2f6fed';
    ui.wrap.style.setProperty('--a', c);
  }

  function clock(ms) {
    return new Date(ms).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
  }
  function day(ms) {
    var d = new Date(ms), now = new Date();
    if (d.toDateString() === now.toDateString()) return 'today';
    var o = { day: 'numeric', month: 'short' };
    if (d.getFullYear() !== now.getFullYear()) o.year = 'numeric';
    return d.toLocaleDateString(undefined, o);
  }
  function stamp(ms) {
    return new Date(ms).toDateString() === new Date().toDateString() ? clock(ms) : day(ms) + ', ' + clock(ms);
  }

  function render() {
    renderBadge();
    var online = s.settings.online;
    var list = s.view === 'list';
    var unseenOther = Object.keys(s.unseen).some(function (id) { return !s.current || Number(id) !== s.current.id; });

    ui.name.textContent = list ? 'Your cases' : (s.settings.brand_name || 'Support');
    ui.status.textContent = '';
    if (list) {
      ui.status.append(document.createTextNode(s.cases.length + (s.cases.length === 1 ? ' case' : ' cases')));
    } else {
      ui.status.append(el('span', 'dot' + (online ? ' on' : '')),
        document.createTextNode(online ? 'We’re online' : 'Away — we’ll reply by email'));
    }
    ui.back.hidden = !list;
    ui.casesBtn.hidden = list || !s.cases.length;
    var pip = ui.casesBtn.querySelector('.pip');
    if (unseenOther && !pip) ui.casesBtn.append(el('span', 'pip'));
    if (!unseenOther && pip) pip.remove();

    var c = s.current;
    ui.casebar.hidden = list || !c;
    if (c && !list) {
      ui.casebar.textContent = '';
      ui.casebar.append(el('strong', null, 'Case #' + c.id), document.createTextNode('· opened ' + day(c.created_at)));
      var chip = el('span', 'chip ' + c.status, c.status);
      chip.style.marginLeft = 'auto';
      ui.casebar.append(chip);
    }
    var closed = !list && c && c.status === 'closed';
    ui.foot.hidden = list || closed;
    ui.closedFoot.hidden = !closed;
    if (closed) ui.closedNote.textContent = 'This case was closed' + (c.closed_at ? ' ' + day(c.closed_at) : '') + '. Need more help?';

    if (list) renderList(); else renderChat();
  }

  function renderChat() {
    var b = ui.body, c = s.current;
    b.textContent = '';
    var intro = el('div', 'm them');
    intro.append(el('div', 'by', s.settings.brand_name || 'Support'),
      el('div', 'b', s.settings.online ? s.settings.greeting : s.settings.offline_message));
    b.append(intro);

    s.messages.forEach(function (m) {
      var mine = m.author_type === 'contact';
      var row = el('div', 'm ' + (mine ? 'me' : 'them'));
      var by = el('div', 'by', (mine ? 'You' : (m.author_name || s.settings.brand_name)));
      if (m.author_type === 'bot') by.append(el('span', 'aitag', 'AI'));
      by.append(document.createTextNode(' · ' + stamp(m.created_at)));
      row.append(by, el('div', 'b', m.body));
      b.append(row);
    });
    if (s.typing && c && s.typing.case_id === c.id) {
      var t = el('div', 'm them');
      var dots = el('div', 'b typing');
      dots.setAttribute('aria-label', (s.typing.name || 'Assistant') + ' is typing');
      dots.append(el('i'), el('i'), el('i'));
      var tby = el('div', 'by', s.typing.name || 'Assistant');
      tby.append(el('span', 'aitag', 'AI'));
      t.append(tby, dots);
      b.append(t);
    }

    if (!c && s.cases.length) {
      // A fresh conversation: point to earlier cases.
      var link = el('button', 'link');
      link.type = 'button';
      var lt = el('div', 'lt');
      var openCount = s.cases.filter(function (x) { return x.status !== 'closed'; }).length;
      lt.append(el('div', null, 'Your previous cases'),
        el('div', null, s.cases.length + ' total' + (openCount ? ' · ' + openCount + ' still open' : '')));
      link.append(lt, el('span', 'arr', '›'));
      link.onclick = function () { s.view = 'list'; render(); };
      b.append(link);
    }
    var hasMine = s.messages.some(function (m) { return m.author_type === 'contact'; });
    if (c && c.status !== 'closed' && hasMine && !s.contact.email) b.append(emailCard());
    b.scrollTop = b.scrollHeight;
  }

  function caseItem(c) {
    var item = el('button', 'case' + (c.status === 'closed' ? ' closed' : ''));
    item.type = 'button';
    var title = el('div', 'ct');
    if (s.unseen[c.id]) title.append(el('span', 'new'));
    title.append(el('span', null, c.subject || 'Conversation'));
    item.append(title, el('span', 'chip ' + c.status, c.status));
    var meta = 'Case #' + c.id + ' · opened ' + day(c.created_at);
    if (c.status === 'closed' && c.closed_at) meta += ' · closed ' + day(c.closed_at);
    else meta += ' · last reply ' + day(c.last_message_at);
    item.append(el('div', 'cm', meta));
    if (c.preview && c.preview !== c.subject) item.append(el('div', 'cp', c.preview));
    item.setAttribute('aria-label', 'Case ' + c.id + ': ' + (c.subject || 'Conversation') + ', ' + c.status + (s.unseen[c.id] ? ', new reply' : ''));
    item.onclick = function () { openCase(c.id); };
    return item;
  }

  function renderList() {
    var b = ui.body;
    b.textContent = '';
    var nb = iconButton('newbtn', ICON_PLUS, 'Start a new conversation', 'Start a new conversation');
    nb.onclick = function () { newConversation(); };
    b.append(nb);
    var active = s.cases.filter(function (c) { return c.status !== 'closed'; });
    var done = s.cases.filter(function (c) { return c.status === 'closed'; });
    if (!s.cases.length) b.append(el('div', 'empty', 'No cases yet. Start a conversation and it’ll be kept here.'));
    if (active.length) {
      b.append(el('div', 'sect', 'Open · ' + active.length));
      active.sort(function (a, z) { return z.last_message_at - a.last_message_at; }).forEach(function (c) { b.append(caseItem(c)); });
    }
    if (done.length) {
      b.append(el('div', 'sect', 'Closed · ' + done.length));
      done.forEach(function (c) { b.append(caseItem(c)); });
    }
    b.scrollTop = 0;
  }

  function emailCard() {
    var card = el('div', 'card');
    card.append(el('p', null, s.settings.online
      ? 'Leave your email in case you have to go — we’ll send replies there.'
      : 'We’re away. Leave your email and we’ll reply there.'));
    var name = el('input');
    name.placeholder = 'Your name (optional)';
    name.value = s.contact.name || '';
    name.autocomplete = 'name';
    name.setAttribute('aria-label', 'Your name');
    var email = el('input');
    email.type = 'email';
    email.placeholder = 'you@example.com';
    email.autocomplete = 'email';
    email.setAttribute('aria-label', 'Your email');
    var err = el('div', 'err');
    var btn = el('button', null, 'Save');
    btn.type = 'button';
    btn.onclick = function () {
      err.textContent = '';
      var body = { email: email.value };
      if (name.value.trim()) body.name = name.value;
      req('POST', '/api/widget/profile', body).then(function (d) {
        s.contact = d.contact;
        render();
      }).catch(function (e) { err.textContent = e.message; });
    };
    email.addEventListener('keydown', function (e) { if (e.key === 'Enter') btn.onclick(); });
    card.append(name, email, err, btn);
    return card;
  }

  var flashTimer;
  function flash(text) {
    var f = root.querySelector('.flash');
    if (f) f.remove();
    f = el('div', 'flash', text);
    ui.panel.append(f);
    clearTimeout(flashTimer);
    flashTimer = setTimeout(function () { f.remove(); }, 3500);
  }

  function mount() {
    document.body.appendChild(host);
    applyTheme();
    render();
    session(false).catch(function () { });
  }
  if (document.body) mount(); else document.addEventListener('DOMContentLoaded', mount);

  window.HelpDesk = {
    __loaded: true,
    open: function () { toggle(true); },
    close: function () { toggle(false); },
    identify: function (p) { return identify(p); },
  };
})();
