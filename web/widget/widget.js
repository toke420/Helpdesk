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
    view: 'chat',       // 'chat' | 'list' | 'ideas' | 'idea' | 'newidea' | 'news'
    // Feedback board (when s.settings.feedback is on).
    fb: { ideas: [], loaded: false, status: 'active', sort: 'top', idea: null, comments: [], err: '', busy: false, title: '', body: '', comment: '', askWho: false, news: null },
    current: null,      // the case shown in chat view; null = a new, unsent conversation
    messages: [],
    unseen: {},         // case id -> true when an agent replied while it wasn't on screen
    open: false,
    badge: 0,
    picked: false,
    sending: false,
    pendingIdentify: null,
    typing: null,       // { case_id, name } while the AI agent is writing
    // "Is this about a previous case?" — asked before a message starts a new case.
    caseAns: null,     // null | { via: 'new' } | { via: 'number' | 'unsure', related: 'HD-1035' }
    caseStep: 'ask',   // 'ask' | 'number' | 'unsure'
    lastCase: null,    // the visitor's own last case, for "I'm not sure"
    caseErr: '',
  };

  // A message only starts a new case when no open case is on screen.
  function statusText(c) { return c.status_label || c.status; }
  function startsNewCase() { return !s.current || s.current.status === 'closed'; }
  function resetCase() { s.caseAns = null; s.caseStep = 'ask'; s.lastCase = null; s.caseErr = ''; }

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
    resetCase();
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
    if (startsNewCase() && !s.caseAns) { render(); return; }
    s.sending = true;
    ui.send.disabled = true;
    var ensure = s.token ? Promise.resolve() : session(true);
    ensure.then(function () {
      var body = { body: text, page: page() };
      if (!startsNewCase()) body.case_id = s.current.id;
      else {
        body.new = true;
        if (s.caseAns) {
          body.via = s.caseAns.via;
          if (s.caseAns.related) body.related = s.caseAns.related;
        }
      }
      return req('POST', '/api/widget/messages', body);
    }).then(function (d) {
      if (!s.current || s.current.id !== d.case.id) s.messages = [];
      upsertCase(d.case);
      s.current = d.case;
      if (d.new) resetCase();
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

  // ---------- feedback board ----------
  function isBoard() { return s.view === 'ideas' || s.view === 'idea' || s.view === 'newidea' || s.view === 'news'; }
  function openNews() {
    s.view = 'news';
    render();
    req('GET', '/api/widget/changelog').then(function (d) { s.fb.news = d.entries; if (s.view === 'news') render(); })
      .catch(function (e) { flash(e.message); });
  }
  function boardSwitch(on) {
    var sw = el('div', 'bsw');
    [['ideas', 'Ideas', openBoard], ['news', 'What’s new', openNews]].forEach(function (o) {
      var b = btn(on === o[0] ? 'on' : '', o[1], o[2]);
      b.setAttribute('aria-pressed', on === o[0] ? 'true' : 'false');
      sw.append(b);
    });
    return sw;
  }
  function renderNews() {
    var b = ui.body;
    b.textContent = '';
    b.append(boardSwitch('news'));
    if (!s.fb.news) { b.append(el('div', 'empty', 'Loading…')); return; }
    if (!s.fb.news.length) b.append(el('div', 'empty', 'Nothing announced yet — check back soon.'));
    s.fb.news.forEach(function (e) {
      var card = el('div', 'news');
      card.append(el('div', 'nd', day(e.published_at)), el('div', 'nt', e.title));
      if (e.body) card.append(el('div', 'ibody', e.body));
      e.ideas.forEach(function (it) {
        var l = btn('nidea', '✓ ' + it.title, function () { openIdea(it.id); });
        card.append(l);
      });
      b.append(card);
    });
    b.scrollTop = 0;
  }
  function ensureToken() { return s.token ? Promise.resolve() : session(true); }
  function loadIdeas() {
    var q = '?status=' + encodeURIComponent(s.fb.status) + '&sort=' + encodeURIComponent(s.fb.sort);
    return req('GET', '/api/widget/ideas' + q).then(function (d) {
      s.fb.ideas = d.ideas; s.fb.loaded = true;
      if (s.view === 'ideas') render();
    });
  }
  function openBoard() {
    s.view = 'ideas'; s.fb.err = '';
    render();
    loadIdeas().catch(function (e) { flash(e.message); });
  }
  function openIdea(id) {
    s.view = 'idea'; s.fb.err = ''; s.fb.askWho = false;
    if (!s.fb.idea || s.fb.idea.id !== id) { s.fb.idea = null; s.fb.comments = []; s.fb.comment = ''; }
    render();
    return req('GET', '/api/widget/ideas/' + id).then(function (d) {
      if (d.merged_into) return openIdea(d.merged_into);
      s.fb.idea = d.idea; s.fb.comments = d.comments;
      if (s.view === 'idea') render();
    }).catch(function (e) { flash(e.message); openBoard(); });
  }
  function vote(it) {
    var on = !it.voted;
    it.voted = on; it.votes += on ? 1 : -1; // show it straight away
    render();
    ensureToken().then(function () {
      return req('POST', '/api/widget/ideas/' + it.id + '/vote', { vote: on });
    }).then(function (d) {
      it.votes = d.votes; it.voted = d.voted;
      s.fb.ideas.forEach(function (x) { if (x.id === it.id) { x.votes = d.votes; x.voted = d.voted; } });
      render();
    }).catch(function (e) {
      it.voted = !on; it.votes += on ? -1 : 1;
      render(); flash(e.message);
    });
  }
  function needWho() { return !s.contact.email || !(s.contact.name || '').trim(); }
  // whoCard: name + email, asked once before the first post or comment.
  function whoCard(then) {
    var card = el('div', 'card');
    var name = el('input'); name.type = 'text'; name.autocomplete = 'name'; name.value = s.contact.name || ''; name.maxLength = 100;
    var email = el('input'); email.type = 'email'; email.autocomplete = 'email'; email.spellcheck = false; email.setAttribute('autocapitalize', 'off'); email.value = s.contact.email || ''; email.maxLength = 254;
    var ln = el('label', null, 'Your name'); ln.append(name);
    var le = el('label', null, 'Email'); le.append(email);
    var err = el('div', 'err', s.fb.err);
    var go = btn('pri', 'Continue', function () {
      if (!name.value.trim() || !email.value.trim()) { err.textContent = 'Please add your name and email.'; return; }
      go.disabled = true;
      ensureToken().then(function () {
        return req('POST', '/api/widget/profile', { name: name.value.trim(), email: email.value.trim() });
      }).then(function (d) { s.contact = d.contact; s.fb.err = ''; s.fb.askWho = false; then(); })
        .catch(function (e) { err.textContent = e.message; go.disabled = false; });
    });
    card.append(el('div', 'h', 'Who’s posting?'),
      el('p', null, 'So we can let you know when your idea moves forward. Your email is never shown on the board.'),
      ln, le, err, go);
    setTimeout(function () { (name.value ? email : name).focus(); }, 30);
    return card;
  }
  function postIdea() {
    if (needWho()) { s.fb.askWho = true; render(); return; }
    s.fb.busy = true; s.fb.err = ''; render();
    ensureToken().then(function () {
      return req('POST', '/api/widget/ideas', { title: s.fb.title, body: s.fb.body });
    }).then(function (d) {
      s.fb.title = s.fb.body = ''; s.fb.busy = false;
      s.fb.idea = d.idea; s.fb.comments = [];
      s.fb.loaded = false;
      openIdea(d.idea.id);
      flash(d.idea.approved ? 'Posted — thanks!' : 'Thanks! We’ll put it on the board once we’ve had a look.');
    }).catch(function (e) {
      s.fb.busy = false;
      if (e.status === 428) s.fb.askWho = true; else s.fb.err = e.message;
      render();
    });
  }
  function postComment() {
    var it = s.fb.idea;
    if (!it || !s.fb.comment.trim()) return;
    if (needWho()) { s.fb.askWho = true; render(); return; }
    s.fb.busy = true; render();
    req('POST', '/api/widget/ideas/' + it.id + '/comments', { body: s.fb.comment }).then(function () {
      s.fb.comment = ''; s.fb.busy = false;
      return openIdea(it.id);
    }).catch(function (e) {
      s.fb.busy = false;
      if (e.status === 428) s.fb.askWho = true; else flash(e.message);
      render();
    });
  }
  function voteButton(it) {
    var b = el('button', 'vote' + (it.voted ? ' on' : ''));
    b.type = 'button';
    b.setAttribute('aria-pressed', it.voted ? 'true' : 'false');
    b.setAttribute('aria-label', (it.voted ? 'Remove your vote for ' : 'Vote for ') + it.title + ', ' + it.votes + (it.votes === 1 ? ' vote' : ' votes'));
    b.innerHTML = ICON_UP;
    b.append(el('b', null, String(it.votes)));
    b.disabled = it.status === 'closed' && !it.voted;
    b.onclick = function (e) { e.stopPropagation(); vote(it); };
    return b;
  }
  function renderIdeas() {
    var b = ui.body;
    b.textContent = '';
    b.append(boardSwitch('ideas'));
    var add = iconButton('newbtn', ICON_PLUS, 'Post an idea', 'Post an idea');
    add.onclick = function () { s.view = 'newidea'; s.fb.err = ''; s.fb.askWho = false; render(); };
    var bar = el('div', 'fbbar');
    [['active', 'All'], ['planned', 'Planned'], ['in_progress', 'In progress'], ['complete', 'Done']].forEach(function (o) {
      var t = btn('tab' + (s.fb.status === o[0] ? ' on' : ''), o[1], function () { s.fb.status = o[0]; loadIdeas().catch(function (e) { flash(e.message); }); render(); });
      t.setAttribute('aria-pressed', s.fb.status === o[0] ? 'true' : 'false');
      bar.append(t);
    });
    var sort = el('select', 'sort');
    sort.setAttribute('aria-label', 'Sort ideas');
    [['top', 'Top'], ['new', 'New']].forEach(function (o) { var op = el('option', null, o[1]); op.value = o[0]; op.selected = s.fb.sort === o[0]; sort.append(op); });
    sort.onchange = function () { s.fb.sort = sort.value; loadIdeas().catch(function (e) { flash(e.message); }); };
    bar.append(sort);
    b.append(add, bar);
    if (!s.fb.loaded) { b.append(el('div', 'empty', 'Loading…')); return; }
    if (!s.fb.ideas.length) b.append(el('div', 'empty', s.fb.status === 'active' ? 'No ideas yet — be the first to post one.' : 'Nothing here yet.'));
    s.fb.ideas.forEach(function (it) {
      var row = el('div', 'idea');
      var main = el('button', 'im');
      main.type = 'button';
      main.append(el('div', 'it', it.title));
      var meta = el('div', 'imeta');
      meta.append(el('span', 'chip i-' + it.status, it.approved ? it.status_label : 'Awaiting approval'));
      if (it.comments) meta.append(document.createTextNode(it.comments + (it.comments === 1 ? ' comment' : ' comments')));
      main.append(meta);
      main.onclick = function () { openIdea(it.id); };
      row.append(voteButton(it), main);
      b.append(row);
    });
    b.scrollTop = 0;
  }
  function renderIdea() {
    var b = ui.body, it = s.fb.idea;
    b.textContent = '';
    if (!it) { b.append(el('div', 'empty', 'Loading…')); return; }
    var head = el('div', 'idea big');
    var hm = el('div', 'im');
    hm.append(el('div', 'it', it.title));
    var meta = el('div', 'imeta');
    meta.append(el('span', 'chip i-' + it.status, it.approved ? it.status_label : 'Awaiting approval'),
      document.createTextNode(it.author_name + ' · ' + day(it.created_at)));
    hm.append(meta);
    head.append(voteButton(it), hm);
    b.append(head);
    if (it.body) b.append(el('div', 'ibody', it.body));
    if (!it.approved) b.append(el('div', 'note', 'Only you can see this until our team approves it.'));
    b.append(el('div', 'sect', 'Activity'));
    if (!s.fb.comments.length) b.append(el('div', 'empty small', 'No comments yet.'));
    s.fb.comments.forEach(function (c) {
      if (c.author_type === 'system') { b.append(el('div', 'sys', c.body + ' · ' + day(c.created_at))); return; }
      var row = el('div', 'm ' + (c.mine ? 'me' : 'them'));
      row.append(el('div', 'by', (c.mine ? 'You' : c.author_name) + ' · ' + day(c.created_at)), el('div', 'b', c.body));
      b.append(row);
    });
    if (s.fb.askWho) { b.append(whoCard(postComment)); return; }
    if (it.approved && it.status !== 'closed') {
      var box = el('div', 'cbox');
      var ta = el('textarea');
      ta.rows = 2; ta.maxLength = 3000; ta.placeholder = 'Add a comment…'; ta.value = s.fb.comment;
      ta.setAttribute('aria-label', 'Comment');
      ta.oninput = function () { s.fb.comment = ta.value; };
      var go = iconButton('send', ICON_SEND, 'Post comment');
      go.disabled = s.fb.busy;
      go.onclick = postComment;
      box.append(ta, go);
      b.append(box);
    }
  }
  function renderNewIdea() {
    var b = ui.body;
    b.textContent = '';
    if (s.fb.askWho) { b.append(whoCard(postIdea)); return; }
    var card = el('div', 'card');
    var title = el('input'); title.type = 'text'; title.maxLength = 120; title.value = s.fb.title; title.placeholder = 'Short and specific';
    title.oninput = function () { s.fb.title = title.value; };
    var body = el('textarea'); body.rows = 5; body.maxLength = 5000; body.value = s.fb.body; body.placeholder = 'What would it do for you?';
    body.oninput = function () { s.fb.body = body.value; };
    var lt = el('label', null, 'Your idea'); lt.append(title);
    var lb = el('label', null, 'Details (optional)'); lb.append(body);
    var go = btn('pri', s.fb.busy ? 'Posting…' : 'Post idea', postIdea);
    go.disabled = s.fb.busy;
    card.append(el('div', 'h', 'Share an idea'), el('p', null, 'Others can vote on it, and you’ll see when it’s planned or done.'), lt, lb, el('div', 'err', s.fb.err), go);
    b.append(card);
    setTimeout(function () { if (!title.value) title.focus(); }, 30);
  }

  // ---------- UI ----------
  var css = [
    ':host{all:initial}',
    '*{box-sizing:border-box}',
    '.wrap{--bg:#fff;--sunken:#f2f4f7;--line:#e4e7ec;--line2:#cdd2da;--text:#161a22;--muted:#596273;--field:#fff;--ok:#177a45;--ok-bg:#e3f4ea;--bad:#b42323;--warn:#9a5b00;--warn-bg:#fdf0d9;--ai:#6b3fd4;--ai-bg:#efe9fd;--shadow:0 24px 60px rgba(16,24,40,.18),0 2px 6px rgba(16,24,40,.08);color-scheme:light}',
    '@media (prefers-color-scheme:dark){.wrap:not([data-theme=light]){--bg:#16191f;--sunken:#1f232b;--line:#2b3039;--line2:#3a404b;--text:#e9ecf1;--muted:#a1a9b6;--field:#1d2128;--ok:#5fd394;--ok-bg:#16301f;--bad:#ff8a8a;--warn:#f0b65a;--warn-bg:#33270f;--ai:#b9a2f5;--ai-bg:#2c2147;--shadow:0 24px 60px rgba(0,0,0,.55),0 2px 6px rgba(0,0,0,.4);color-scheme:dark}}',
    '.wrap[data-theme=dark]{--bg:#16191f;--sunken:#1f232b;--line:#2b3039;--line2:#3a404b;--text:#e9ecf1;--muted:#a1a9b6;--field:#1d2128;--ok:#5fd394;--ok-bg:#16301f;--bad:#ff8a8a;--warn:#f0b65a;--warn-bg:#33270f;--ai:#b9a2f5;--ai-bg:#2c2147;--shadow:0 24px 60px rgba(0,0,0,.55),0 2px 6px rgba(0,0,0,.4);color-scheme:dark}',
    '.wrap{position:fixed;right:20px;bottom:20px;z-index:2147483000;font:14px/1.45 Inter,ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;color:var(--text);-webkit-font-smoothing:antialiased}',
    'button{font:inherit;color:inherit}',
    'a,button,input,select,textarea,label{touch-action:manipulation;-webkit-tap-highlight-color:transparent}',
    '.bubble{width:60px;height:60px;border-radius:50%;border:0;background:var(--a);color:#fff;cursor:pointer;display:grid;place-items:center;box-shadow:var(--shadow);transition:transform .15s ease;position:relative;margin-left:auto}',
    '.bubble:hover{transform:scale(1.05)}',
    'button:focus-visible,input:focus-visible,textarea:focus-visible{outline:2px solid var(--a);outline-offset:2px}',
    '.bubble svg{width:28px;height:28px}',
    '.badge{position:absolute;top:-2px;right:-2px;min-width:22px;height:22px;padding:0 6px;border-radius:11px;background:#c42b2b;color:#fff;font-size:12px;font-weight:700;display:grid;place-items:center;border:2px solid var(--bg)}',
    '.badge[hidden]{display:none}',
    '.panel{position:absolute;right:0;bottom:76px;width:370px;height:min(600px,calc(100vh - 120px));height:min(600px,calc(100dvh - 120px));background:var(--bg);border-radius:20px;box-shadow:var(--shadow);display:flex;flex-direction:column;overflow:hidden;opacity:0;transform:translateY(12px) scale(.98);transform-origin:bottom right;pointer-events:none;transition:opacity .16s ease,transform .16s ease}',
    '.panel.on{opacity:1;transform:none;pointer-events:auto}',
    '.head{background:var(--a);color:#fff;padding:14px 12px 14px 16px;display:flex;align-items:center;gap:8px}',
    '.logo{width:40px;height:40px;border-radius:12px;background:rgba(255,255,255,.18);display:grid;place-items:center;flex:none}',
    '.logo svg{width:22px;height:22px}',
    '.head .t{flex:1;min-width:0}',
    '.head .n{font-weight:650;font-size:16px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}',
    '.head .st{font-size:13px;display:flex;align-items:center;gap:6px}',
    '.dot{width:8px;height:8px;border-radius:50%;border:2px solid #fff;flex:none}',
    '.dot.on{background:#7ef0b0;border:0}',
    '.hb{border:0;background:rgba(255,255,255,.16);color:#fff;height:36px;min-width:36px;padding:0 9px;border-radius:10px;cursor:pointer;display:inline-flex;align-items:center;justify-content:center;gap:5px;font-size:13px;font-weight:600;position:relative;flex:none}',
    '.hb:hover{background:rgba(255,255,255,.28)}',
    '.hb svg{width:18px;height:18px}',
    '.hb:focus-visible{outline-color:#fff}',
    '.hb[hidden],.logo[hidden]{display:none}',
    '.hb .pip{position:absolute;top:4px;right:4px;width:8px;height:8px;border-radius:50%;background:#ff6b6b;border:1.5px solid var(--a)}',
    '.casebar{display:flex;align-items:center;gap:8px;padding:8px 16px;border-bottom:1px solid var(--line);background:var(--bg);font-size:12.5px;color:var(--muted);flex-wrap:wrap}',
    '.casebar[hidden]{display:none}',
    '.casebar strong{color:var(--text);font-weight:650;font-variant-numeric:tabular-nums}',
    '.chip{display:inline-flex;align-items:center;height:20px;padding:0 8px;border-radius:999px;font-size:11px;font-weight:700;text-transform:capitalize;background:var(--sunken);color:var(--muted);flex:none}',
    '.chip.open{background:var(--ok-bg);color:var(--ok)}',
    '.chip.pending{background:var(--warn-bg);color:var(--warn)}',
    '.body{flex:1;overflow-y:auto;padding:18px 16px;display:flex;flex-direction:column;gap:12px;background:var(--bg)}',
    '.m{max-width:84%;display:flex;flex-direction:column;gap:4px}',
    '.m .b{padding:10px 14px;border-radius:16px;white-space:pre-wrap;overflow-wrap:anywhere}',
    '.m .by{font-size:12px;color:var(--muted);padding:0 4px}',
    '.m.them{align-self:flex-start}',
    '.m.them .b{background:var(--sunken);border-bottom-left-radius:4px}',
    '.m.me{align-self:flex-end}',
    '.m.me .by{text-align:right}',
    '.m.me .b{background:var(--a);color:#fff;border-bottom-right-radius:4px}',
    '.sys{align-self:center;font-size:12px;color:var(--muted);text-align:center;padding:2px 8px}',
    '.card{background:var(--sunken);border-radius:16px;padding:14px;display:grid;gap:10px;align-self:stretch}',
    '.card p{margin:0;font-size:13px;color:var(--muted)}',
    '.card .h{font-weight:600;color:var(--text);font-size:14px}',
    '.card label{display:grid;gap:4px;font-size:12px;color:var(--muted)}',
    '.card input{height:44px;border:1px solid var(--line2);border-radius:10px;padding:0 12px;font:inherit;color:var(--text);background:var(--field);width:100%}',
    '.card .pri{height:44px;border:0;border-radius:10px;background:var(--a);color:#fff;font-weight:600;cursor:pointer}',
    '.card .sec{height:44px;border:1px solid var(--line2);border-radius:10px;background:var(--bg);color:var(--text);font-weight:600;cursor:pointer}',
    '.card .lnk{height:36px;border:0;background:none;color:var(--text);text-decoration:underline;font-size:13px;cursor:pointer}',
    '.card .err{color:var(--bad);font-size:13px}',
    '.card .err:empty{display:none}',
    '.opt{display:grid;gap:2px;text-align:left;width:100%;min-height:56px;padding:10px 14px;border-radius:12px;border:1px solid var(--line2);background:var(--bg);cursor:pointer}',
    '.opt:hover{border-color:var(--a)}',
    '.opt b{font-weight:600}',
    '.opt span{font-size:13px;color:var(--muted)}',
    '.num{display:flex;align-items:center;height:48px;border:2px solid var(--line2);border-radius:12px;background:var(--field);overflow:hidden}',
    '.num:focus-within{border-color:var(--a)}',
    '.num span{padding:0 2px 0 14px;color:var(--muted);font-weight:600}',
    '.card .num input{border:0;height:100%;padding:0 12px 0 2px;font-size:16px;font-weight:600;letter-spacing:.04em;background:transparent;outline:none}',
    '.found{border:1px solid var(--line2);border-radius:12px;padding:12px 14px;background:var(--bg);display:grid;gap:2px}',
    '.found .r{display:flex;justify-content:space-between;font-size:12px;color:var(--muted)}',
    '.found .r b{font-variant-numeric:tabular-nums}',
    '.linked{align-self:stretch;display:flex;align-items:center;gap:8px;font-size:13px;color:var(--ok);background:var(--ok-bg);border-radius:12px;padding:10px 12px}',
    '.linked span{flex:1}',
    '.linked button{border:0;background:none;color:var(--text);text-decoration:underline;font-size:13px;cursor:pointer;padding:4px}',
    '.foot{border-top:1px solid var(--line);padding:12px;display:flex;gap:8px;align-items:flex-end;background:var(--bg)}',
    '.foot textarea{flex:1;border:1px solid var(--line2);border-radius:12px;padding:11px 14px;font:inherit;color:var(--text);resize:none;max-height:120px;min-height:44px;background:var(--field)}',
    '.foot textarea:disabled{opacity:.6}',
    '.send{width:44px;height:44px;border-radius:12px;border:0;background:var(--a);color:#fff;cursor:pointer;display:grid;place-items:center;flex:none}',
    '.send:disabled{opacity:.5;cursor:default}',
    '.send svg{width:18px;height:18px}',
    '.foot[hidden]{display:none}',
    '.link{align-self:stretch;display:flex;align-items:center;gap:10px;background:var(--bg);border:1px solid var(--line2);border-radius:12px;padding:10px 14px;cursor:pointer;text-align:left;color:var(--text)}',
    '.link:hover{border-color:var(--a)}',
    '.link .lt{flex:1;min-width:0}',
    '.link .lt div:first-child{font-weight:600}',
    '.link .lt div+div{font-size:12.5px;color:var(--muted)}',
    '.link .arr{color:var(--muted);font-size:18px}',
    '.closedfoot{border-top:1px solid var(--line);padding:12px;background:var(--bg);display:grid;gap:8px;text-align:center}',
    '.closedfoot[hidden]{display:none}',
    '.closedfoot span{font-size:13px;color:var(--muted)}',
    '.newbtn{height:44px;border:0;border-radius:12px;background:var(--a);color:#fff;font-weight:600;cursor:pointer;display:flex;align-items:center;justify-content:center;gap:6px;width:100%}',
    '.newbtn svg{width:16px;height:16px}',
    '.sect{font-size:11.5px;font-weight:700;text-transform:uppercase;letter-spacing:.05em;color:var(--muted);margin:10px 2px 0}',
    '.case{display:grid;grid-template-columns:1fr auto;gap:2px 10px;width:100%;text-align:left;background:var(--bg);border:1px solid var(--line2);border-radius:12px;padding:10px 14px;cursor:pointer;color:var(--text)}',
    '.case:hover{border-color:var(--a)}',
    '.case .ct{font-weight:600;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;display:flex;align-items:center;gap:6px;min-width:0}',
    '.case .ct span{overflow:hidden;text-overflow:ellipsis}',
    '.case .new{width:8px;height:8px;border-radius:50%;background:var(--a);flex:none}',
    '.case .cm{grid-column:1/-1;font-size:12px;color:var(--muted);font-variant-numeric:tabular-nums}',
    '.case .cp{grid-column:1/-1;font-size:12.5px;color:var(--muted);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}',
    '.case.closed .ct{color:var(--muted);font-weight:500}',
    '.empty{text-align:center;color:var(--muted);font-size:13px;padding:24px 8px}',
    '.aitag{display:inline-block;margin-left:5px;padding:0 5px;border-radius:4px;background:var(--ai-bg);color:var(--ai);font-size:10px;font-weight:700;line-height:15px;vertical-align:1px}',
    '.typing{display:flex;gap:4px;align-items:center;height:36px}',
    '.typing i{width:6px;height:6px;border-radius:50%;background:var(--muted);animation:hdblink 1.2s infinite both}',
    '.typing i:nth-child(2){animation-delay:.2s}.typing i:nth-child(3){animation-delay:.4s}',
    '@keyframes hdblink{0%,80%,100%{opacity:.25}40%{opacity:1}}',
    '.flash{position:absolute;left:12px;right:12px;bottom:76px;background:var(--text);color:var(--bg);padding:8px 12px;border-radius:10px;font-size:13px}',
    '.fbbar{display:flex;gap:4px;align-items:center;flex-wrap:wrap}',
    '.bsw{display:grid;grid-template-columns:1fr 1fr;gap:4px;padding:4px;background:var(--sunken);border-radius:12px}',
    '.bsw button{height:34px;border:0;border-radius:9px;background:transparent;color:var(--muted);font-weight:650;cursor:pointer}',
    '.bsw button.on{background:var(--bg);color:var(--text);box-shadow:0 1px 2px rgba(16,24,40,.12)}',
    '.news{border:1px solid var(--line2);border-radius:12px;padding:12px 14px;display:grid;gap:6px}',
    '.news .nd{font-size:12px;color:var(--muted)}',
    '.news .nt{font-weight:650;font-size:15px}',
    '.nidea{justify-self:start;border:0;background:var(--ok-bg);color:var(--ok);border-radius:999px;padding:3px 10px;font-size:12.5px;font-weight:650;cursor:pointer;text-align:left}',
    '.fbbar .tab{height:30px;padding:0 10px;border:0;border-radius:8px;background:var(--sunken);color:var(--muted);font-size:12.5px;font-weight:600;cursor:pointer}',
    '.fbbar .tab.on{background:var(--a);color:#fff}',
    '.fbbar .sort{margin-left:auto;height:30px;border:1px solid var(--line2);border-radius:8px;background:var(--field);color:var(--text);font:inherit;font-size:12.5px;padding:0 6px}',
    '.idea{display:flex;gap:10px;align-items:stretch}',
    '.idea .im{flex:1;min-width:0;text-align:left;background:var(--bg);border:1px solid var(--line2);border-radius:12px;padding:10px 12px;cursor:pointer;color:var(--text);display:grid;gap:4px}',
    '.idea .im:hover{border-color:var(--a)}',
    '.idea.big .im{border:0;padding:0 2px;cursor:default}',
    '.idea.big .it{font-size:16px}',
    '.it{font-weight:600;overflow-wrap:anywhere}',
    '.imeta{display:flex;gap:8px;align-items:center;flex-wrap:wrap;font-size:12px;color:var(--muted)}',
    '.vote{width:52px;flex:none;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:2px;border:1px solid var(--line2);border-radius:12px;background:var(--bg);color:var(--text);cursor:pointer;min-height:56px;font-variant-numeric:tabular-nums}',
    '.vote svg{width:16px;height:16px}',
    '.vote b{font-size:14px}',
    '.vote:hover{border-color:var(--a)}',
    '.vote.on{background:var(--a);border-color:var(--a);color:#fff}',
    '.vote:disabled{opacity:.5;cursor:default}',
    '.ibody{white-space:pre-wrap;overflow-wrap:anywhere;font-size:14px}',
    '.note{font-size:13px;color:var(--warn);background:var(--warn-bg);border-radius:10px;padding:8px 12px}',
    '.empty.small{padding:8px}',
    '.chip.i-under_review{background:var(--warn-bg);color:var(--warn)}',
    '.chip.i-planned,.chip.i-in_progress{background:var(--ai-bg);color:var(--ai)}',
    '.chip.i-complete{background:var(--ok-bg);color:var(--ok)}',
    '.card textarea{border:1px solid var(--line2);border-radius:10px;padding:10px 12px;font:inherit;color:var(--text);background:var(--field);width:100%;resize:vertical}',
    '.cbox{display:flex;gap:8px;align-items:flex-end;margin-top:4px}',
    '.cbox textarea{flex:1;border:1px solid var(--line2);border-radius:12px;padding:10px 12px;font:inherit;color:var(--text);background:var(--field);resize:vertical;min-height:44px}',
    // Full screen on phones: keep the header and the message box clear of the notch and home bar.
    '@media (max-width:480px){.wrap{right:12px;bottom:12px}.panel{position:fixed;inset:0;width:auto;height:auto;border-radius:0;bottom:0;padding:0 env(safe-area-inset-right) env(safe-area-inset-bottom) env(safe-area-inset-left);overscroll-behavior:contain}.head{padding-top:calc(14px + env(safe-area-inset-top))}.wrap.open .bubble{display:none}}',
    '@media (prefers-reduced-motion:reduce){.panel,.bubble{transition:none}.bubble:hover{transform:none}.typing i{animation:none;opacity:.6}}',
    // Last so it beats the font:inherit on each field: iOS zooms into any field under 16px.
    '@media (pointer:coarse){input,textarea,select{font-size:16px!important}}',
  ].join('\n');

  var ICON_CHAT = '<svg viewBox="0 0 32 32" aria-hidden="true"><path d="M8 10h16v11h-9l-5 4v-4H8z" fill="currentColor"/></svg>';
  var ICON_LOGO = '<svg viewBox="0 0 32 32" aria-hidden="true"><path d="M9 11h14v9h-8l-4 3v-3H9z" fill="#fff"/></svg>';
  var ICON_THEME = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><circle cx="12" cy="12" r="8.5" stroke="currentColor" stroke-width="1.8"/><path d="M12 3.5a8.5 8.5 0 0 0 0 17z" fill="currentColor"/></svg>';
  var ICON_DOWN = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M6 9l6 6 6-6" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  var ICON_SEND = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M5 12h14M13 6l6 6-6 6" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  var ICON_LIST = '<svg viewBox="0 0 24 24" fill="none"><path d="M8 6h12M8 12h12M8 18h12M4 6h.01M4 12h.01M4 18h.01" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"/></svg>';
  var ICON_BACK = '<svg viewBox="0 0 24 24" fill="none"><path d="M15 6l-6 6 6 6" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  var ICON_PLUS = '<svg viewBox="0 0 24 24" fill="none"><path d="M12 5v14M5 12h14" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"/></svg>';
  var ICON_UP = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M6 15l6-6 6 6" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  var ICON_IDEA = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M9 18h6M10 21h4M12 3a6 6 0 0 0-3.5 10.9c.6.4 1 1.1 1 1.8V16h5v-.3c0-.7.4-1.4 1-1.8A6 6 0 0 0 12 3z" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  var ICON_X = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M6 6l12 12M18 6 6 18" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>';

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function btn(cls, text, onclick) {
    var b = el('button', cls, text);
    b.type = 'button';
    if (onclick) b.onclick = onclick;
    return b;
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
  // A constructed stylesheet needs no CSP exception on the host page; older
  // browsers fall back to a <style> element.
  var sheet = null;
  try {
    if (root !== host && root.adoptedStyleSheets !== undefined && window.CSSStyleSheet) {
      sheet = new CSSStyleSheet();
      sheet.replaceSync(css);
      root.adoptedStyleSheets = [sheet];
    }
  } catch (e) { sheet = null; }
  var style = null;
  if (!sheet) { style = el('style'); style.textContent = css; }
  var ui = {};
  ui.wrap = el('div', 'wrap');
  ui.panel = el('div', 'panel');
  ui.panel.setAttribute('role', 'dialog');
  ui.panel.setAttribute('aria-label', 'Chat with us');
  ui.panel.setAttribute('aria-hidden', 'true');

  var head = el('div', 'head');
  ui.back = iconButton('hb', ICON_BACK, 'Back');
  ui.back.onclick = function () {
    if (s.view === 'idea' || s.view === 'newidea' || s.view === 'news') { s.fb.askWho = false; openBoard(); return; }
    if (s.current || s.messages.length) { s.view = 'chat'; render(); } else newConversation();
  };
  ui.logo = el('div', 'logo');
  ui.logo.innerHTML = ICON_LOGO;
  var t = el('div', 't');
  ui.name = el('div', 'n', 'Support');
  ui.status = el('div', 'st');
  t.append(ui.name, ui.status);
  ui.casesBtn = iconButton('hb', ICON_LIST, 'Your cases', 'Cases');
  ui.casesBtn.onclick = function () { s.view = 'list'; render(); loadCases().catch(function () { }); };
  ui.ideasBtn = iconButton('hb', ICON_IDEA, 'Feedback and ideas', 'Ideas');
  ui.ideasBtn.onclick = openBoard;
  var x = iconButton('hb', ICON_X, 'Close chat');
  x.onclick = function () { toggle(false); };
  ui.themeBtn = iconButton('hb', ICON_THEME, 'Switch theme');
  ui.themeBtn.onclick = function () { setTheme(isDark() ? 'light' : 'dark'); };
  head.append(ui.back, ui.logo, t, ui.casesBtn, ui.ideasBtn, ui.themeBtn, x);

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
  ui.bubble.type = 'button';
  ui.bubble.setAttribute('aria-label', 'Open chat');
  ui.bubble.setAttribute('aria-expanded', 'false');
  ui.bubble.innerHTML = ICON_CHAT;
  ui.badge = el('span', 'badge');
  ui.badge.hidden = true;
  ui.wrap.append(ui.panel, ui.bubble);
  if (style) root.append(style);
  root.append(ui.wrap);

  // ---------- light / dark ----------
  var THEME_KEY = 'hd_widget_theme';
  var darkMQ = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  function savedTheme() { try { return localStorage.getItem(THEME_KEY) || ''; } catch (e) { return ''; } }
  function isDark() {
    var t = ui.wrap.getAttribute('data-theme');
    return t ? t === 'dark' : !!(darkMQ && darkMQ.matches);
  }
  function setTheme(t) {
    if (t) ui.wrap.setAttribute('data-theme', t); else ui.wrap.removeAttribute('data-theme');
    try { t ? localStorage.setItem(THEME_KEY, t) : localStorage.removeItem(THEME_KEY); } catch (e) { }
    var lbl = isDark() ? 'Switch to light theme' : 'Switch to dark theme';
    ui.themeBtn.setAttribute('aria-label', lbl);
    ui.themeBtn.title = lbl;
  }
  setTheme(savedTheme());
  if (darkMQ && darkMQ.addEventListener) darkMQ.addEventListener('change', function () { setTheme(savedTheme()); });

  function autosize() {
    ui.input.style.height = 'auto';
    ui.input.style.height = Math.min(ui.input.scrollHeight, 120) + 'px';
  }
  function focusInput() { setTimeout(function () { if (!ui.foot.hidden && !ui.input.disabled) ui.input.focus(); }, 60); }
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
    var board = isBoard();
    if (board && !s.settings.feedback) { s.view = 'chat'; board = false; }
    var list = s.view === 'list' || board;
    var unseenOther = Object.keys(s.unseen).some(function (id) { return !s.current || Number(id) !== s.current.id; });

    ui.name.textContent = board ? (s.settings.feedback_title || 'Feedback & ideas') : list ? 'Your cases' : (s.settings.brand_name || 'Support');
    ui.status.textContent = '';
    if (board) {
      ui.status.append(document.createTextNode('Vote on what we do next'));
    } else if (list) {
      ui.status.append(document.createTextNode(s.cases.length + (s.cases.length === 1 ? ' case' : ' cases')));
    } else {
      ui.status.append(el('span', 'dot' + (online ? ' on' : '')),
        document.createTextNode(online ? 'We’re online' : 'Away — we’ll reply by email'));
    }
    ui.back.hidden = !list;
    ui.logo.hidden = true; // set below, once we know whether the Cases button needs the room
    ui.casesBtn.hidden = list || !s.cases.length;
    ui.ideasBtn.hidden = list || !s.settings.feedback;
    ui.logo.hidden = list || !ui.casesBtn.hidden || !ui.ideasBtn.hidden;
    var pip = ui.casesBtn.querySelector('.pip');
    if (unseenOther && !pip) ui.casesBtn.append(el('span', 'pip'));
    if (!unseenOther && pip) pip.remove();

    var c = s.current;
    ui.casebar.hidden = list || !c;
    if (c && !list) {
      ui.casebar.textContent = '';
      ui.casebar.append(el('strong', null, 'Case ' + c.number),
        document.createTextNode('· opened ' + day(c.created_at) + (c.related_number ? ' · follows ' + c.related_number : '')));
      var chip = el('span', 'chip ' + c.status, statusText(c));
      chip.style.marginLeft = 'auto';
      ui.casebar.append(chip);
    }
    var closed = !list && c && c.status === 'closed';
    ui.foot.hidden = list || closed;
    ui.closedFoot.hidden = !closed;
    if (closed) ui.closedNote.textContent = 'This case was closed' + (c.closed_at ? ' ' + day(c.closed_at) : '') + '. Need more help?';

    if (s.view === 'ideas') renderIdeas();
    else if (s.view === 'idea') renderIdea();
    else if (s.view === 'newidea') renderNewIdea();
    else if (s.view === 'news') renderNews();
    else if (list) renderList(); else renderChat();
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
    if (!c) b.append(s.caseAns ? caseLinked() : caseCard());
    else if (c.status !== 'closed' && hasMine && !s.contact.email) b.append(emailCard());
    var waiting = !c && !s.caseAns;
    ui.input.disabled = waiting;
    ui.send.disabled = waiting || s.sending;
    ui.input.placeholder = waiting ? 'Choose an option above first' : 'Write a message…';
    b.scrollTop = b.scrollHeight;
  }

  function caseItem(c) {
    var item = el('button', 'case' + (c.status === 'closed' ? ' closed' : ''));
    item.type = 'button';
    var title = el('div', 'ct');
    if (s.unseen[c.id]) title.append(el('span', 'new'));
    title.append(el('span', null, c.subject || 'Conversation'));
    item.append(title, el('span', 'chip ' + c.status, statusText(c)));
    var meta = 'Case ' + c.number + ' · opened ' + day(c.created_at);
    if (c.status === 'closed' && c.closed_at) meta += ' · closed ' + day(c.closed_at);
    else meta += ' · last reply ' + day(c.last_message_at);
    item.append(el('div', 'cm', meta));
    if (c.preview && c.preview !== c.subject) item.append(el('div', 'cp', c.preview));
    item.setAttribute('aria-label', 'Case ' + c.number + ': ' + (c.subject || 'Conversation') + ', ' + statusText(c) + (s.unseen[c.id] ? ', new reply' : ''));
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

  function chooseNew() { s.caseAns = { via: 'new' }; s.caseErr = ''; render(); focusInput(); }

  function caseCard() {
    var card = el('div', 'card');
    card.setAttribute('role', 'group');
    if (s.caseStep === 'number') {
      card.setAttribute('aria-label', 'Enter your case number');
      card.append(el('div', 'h', 'What’s your case number?'),
        el('p', null, 'It’s in our emails and at the top of the chat, and looks like HD-1234.'));
      var lab = el('label', null, 'Case number');
      var box = el('div', 'num');
      var input = el('input');
      input.inputMode = 'numeric';
      input.autocomplete = 'off';
      input.placeholder = '1234';
      input.setAttribute('aria-describedby', 'hd-case-err');
      box.append(el('span', null, 'HD-'), input);
      lab.append(box);
      var err = el('div', 'err', s.caseErr);
      err.id = 'hd-case-err';
      err.setAttribute('role', 'alert');
      var go = btn('pri', 'Continue', function () {
        var v = input.value.trim();
        if (!v) { input.focus(); return; }
        if (/^\d+$/.test(v)) v = 'HD-' + v;
        go.disabled = true;
        var ensure = s.token ? Promise.resolve() : session(true);
        ensure.then(function () { return req('POST', '/api/widget/case-check', { number: v }); })
          .then(function (d) { s.caseAns = { via: 'number', related: d.number }; s.caseErr = ''; render(); focusInput(); })
          .catch(function (e) { s.caseErr = e.message; render(); });
      });
      input.addEventListener('keydown', function (e) { if (e.key === 'Enter') { e.preventDefault(); go.onclick(); } });
      card.append(lab, err, go,
        btn('lnk', 'I can’t find my case number', function () { s.caseErr = ''; askUnsure(); }),
        btn('lnk', 'Back', function () { s.caseStep = 'ask'; s.caseErr = ''; render(); }));
      setTimeout(function () { input.focus(); }, 30);
      return card;
    }
    if (s.caseStep === 'unsure' && s.lastCase) {
      var c = s.lastCase;
      card.setAttribute('aria-label', 'Your last case');
      var found = el('div', 'found');
      var r = el('div', 'r');
      r.append(el('b', null, c.number), document.createTextNode(new Date(c.created_at).toLocaleDateString(undefined, { day: 'numeric', month: 'short' }) + ' · ' + statusText(c)));
      found.append(r, el('div', null, c.subject || 'Your last conversation'));
      card.append(el('div', 'h', 'Is it about your last case?'), found,
        btn('pri', 'Yes, continue this case', function () { s.caseAns = { via: 'unsure', related: c.number }; render(); focusInput(); }),
        btn('sec', 'No, start a new case', chooseNew));
      return card;
    }
    card.setAttribute('aria-label', 'Is this about a previous case?');
    card.append(el('div', 'h', 'Is this about a case you’ve already contacted us about?'));
    [['Yes, I have a case number', 'Enter it on the next step', function () { s.caseStep = 'number'; render(); }],
     ['No, this is something new', 'We’ll open a new case', chooseNew],
     ['I’m not sure', 'We’ll look up your last case', askUnsure]].forEach(function (o) {
      var b = btn('opt', null, o[2]);
      b.append(el('b', null, o[0]), el('span', null, o[1]));
      card.append(b);
    });
    var err2 = el('div', 'err', s.caseErr);
    err2.setAttribute('role', 'alert');
    card.append(err2);
    return card;
  }

  function askUnsure() {
    if (!s.token) { s.caseAns = { via: 'new', note: 'We couldn’t find an earlier case from this browser, so we’ll open a new one.' }; render(); focusInput(); return; }
    req('GET', '/api/widget/last-case').then(function (d) {
      if (!d.case) {
        s.caseAns = { via: 'new', note: 'We couldn’t find an earlier case from this browser, so we’ll open a new one.' };
      } else {
        s.lastCase = d.case;
        s.caseStep = 'unsure';
      }
      render();
      focusInput();
    }).catch(function (e) { s.caseErr = e.message; render(); });
  }

  function caseLinked() {
    var a = s.caseAns;
    var row = el('div', 'linked');
    row.append(el('span', null, a.related ? 'Your message will be linked to case ' + a.related + '.' : (a.note || 'We’ll open a new case when you send your message.')),
      btn(null, 'Change', function () { resetCase(); render(); }));
    return row;
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
    email.spellcheck = false;
    email.setAttribute('autocapitalize', 'off');
    email.setAttribute('aria-label', 'Your email');
    var err = el('div', 'err');
    var save = btn('pri', 'Save');
    save.onclick = function () {
      err.textContent = '';
      var body = { email: email.value };
      if (name.value.trim()) body.name = name.value;
      req('POST', '/api/widget/profile', body).then(function (d) {
        s.contact = d.contact;
        render();
      }).catch(function (e) { err.textContent = e.message; });
    };
    email.addEventListener('keydown', function (e) { if (e.key === 'Enter') save.onclick(); });
    card.append(name, email, err, save);
    return card;
  }

  var flashTimer;
  function flash(text) {
    var f = root.querySelector('.flash');
    if (f) f.remove();
    f = el('div', 'flash', text);
    f.setAttribute('role', 'status'); // read out by screen readers
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
