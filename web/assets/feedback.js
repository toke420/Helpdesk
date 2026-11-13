// Public feedback board: list, vote, post and comment, through the widget API.
(function () {
  'use strict';
  var KEY = 'hd_visitor_token';
  var THEME_KEY = 'hd_theme';
  var app = document.getElementById('app');
  var s = {
    token: get(KEY), contact: { name: '', email: '' }, settings: {},
    ideas: [], loaded: false, status: 'active', sort: 'top',
  };
  function get(k) { try { return localStorage.getItem(k) || ''; } catch (e) { return ''; } }
  function put(k, v) { try { v ? localStorage.setItem(k, v) : localStorage.removeItem(k); } catch (e) { } }

  function req(method, path, body) {
    var headers = { 'Content-Type': 'application/json' };
    if (s.token) headers.Authorization = 'Bearer ' + s.token;
    return fetch(path, { method: method, headers: headers, body: body ? JSON.stringify(body) : undefined, credentials: 'omit' })
      .then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (d) {
          if (!r.ok) { var e = new Error(d.error || 'Something went wrong'); e.status = r.status; throw e; }
          return d;
        });
      });
  }
  function session(create) {
    return req('POST', '/api/widget/session', { token: s.token, page: location.origin + '/feedback', create: !!create }).then(function (d) {
      s.settings = d.settings || {};
      if (d.contact) s.contact = d.contact;
      if (d.token !== s.token) { s.token = d.token; put(KEY, d.token); }
      brand();
    });
  }
  function ensureToken() { return s.token ? Promise.resolve() : session(true); }

  // ---------- tiny DOM helper ----------
  function h(tag, attrs) {
    var e = document.createElement(tag);
    for (var k in attrs || {}) {
      var v = attrs[k];
      if (v == null || v === false) continue;
      if (k === 'class') e.className = v;
      else if (k === 'html') e.innerHTML = v;
      else if (k.slice(0, 2) === 'on') e[k] = v;
      else if (k === 'value') e.value = v;
      else e.setAttribute(k, v === true ? '' : v);
    }
    for (var i = 2; i < arguments.length; i++) {
      var c = arguments[i];
      [].concat(c).forEach(function (x) { if (x != null && x !== false) e.append(x.nodeType ? x : String(x)); });
    }
    return e;
  }
  var UP = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M6 15l6-6 6 6" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  var THEME = '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true"><circle cx="12" cy="12" r="8.5" stroke="currentColor" stroke-width="1.8"/><path d="M12 3.5a8.5 8.5 0 0 0 0 17z" fill="currentColor"/></svg>';
  // show replaces the page, skipping null/false (replaceChildren would print "null").
  function show() { app.replaceChildren.apply(app, [].slice.call(arguments).filter(function (x) { return x != null && x !== false; })); }
  var toastTimer;
  function toast(text) {
    var t = document.getElementById('toast');
    t.textContent = text; t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.hidden = true; }, 3500);
  }
  function day(ms) { return new Date(ms).toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: new Date(ms).getFullYear() === new Date().getFullYear() ? undefined : 'numeric' }); }

  function brand() {
    var title = s.settings.feedback_title || 'Feedback & ideas';
    document.title = title + ' · ' + (s.settings.brand_name || 'Support');
    document.getElementById('brand').textContent = s.settings.brand_name || 'Support';
    if (/^#[0-9a-f]{6}$/i.test(s.settings.accent_color || '')) document.documentElement.style.setProperty('--a', s.settings.accent_color);
  }
  var themeBtn = document.getElementById('theme');
  themeBtn.innerHTML = THEME;
  themeBtn.onclick = function () {
    var cur = document.documentElement.getAttribute('data-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    var next = cur === 'dark' ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', next);
    put(THEME_KEY, next);
  };

  // ---------- data ----------
  function loadIdeas() {
    return req('GET', '/api/widget/ideas?status=' + s.status + '&sort=' + s.sort).then(function (d) { s.ideas = d.ideas; s.loaded = true; });
  }
  function voteBtn(it, after) {
    var b = h('button', { class: 'vote' + (it.voted ? ' on' : ''), type: 'button', 'aria-pressed': String(!!it.voted),
      'aria-label': (it.voted ? 'Remove your vote for ' : 'Vote for ') + it.title + ', ' + it.votes + (it.votes === 1 ? ' vote' : ' votes'),
      html: UP, disabled: it.status === 'closed' && !it.voted }, h('b', {}, it.votes));
    b.onclick = function () {
      var on = !it.voted;
      b.disabled = true;
      ensureToken().then(function () { return req('POST', '/api/widget/ideas/' + it.id + '/vote', { vote: on }); })
        .then(function (d) { it.votes = d.votes; it.voted = d.voted; after(); })
        .catch(function (e) { toast(e.message); b.disabled = false; });
    };
    return b;
  }
  function chip(it) { return h('span', { class: 'chip ' + (it.approved ? it.status : 'pending') }, it.approved ? it.status_label : 'Awaiting approval'); }
  function needWho() { return !s.contact.email || !(s.contact.name || '').trim(); }
  function whoCard(then) {
    var name = h('input', { type: 'text', autocomplete: 'name', maxlength: 100, value: s.contact.name || '' });
    var email = h('input', { type: 'email', autocomplete: 'email', maxlength: 254, value: s.contact.email || '' });
    var err = h('div', { class: 'err', role: 'alert' });
    var go = h('button', { class: 'btn pri', type: 'submit' }, 'Continue');
    var form = h('form', { class: 'card', onsubmit: function (e) {
      e.preventDefault();
      if (!name.value.trim() || !email.value.trim()) { err.textContent = 'Please add your name and email.'; return; }
      go.disabled = true;
      ensureToken().then(function () { return req('POST', '/api/widget/profile', { name: name.value.trim(), email: email.value.trim() }); })
        .then(function (d) { s.contact = d.contact; then(); })
        .catch(function (ex) { err.textContent = ex.message; go.disabled = false; });
    } },
      h('h2', {}, 'Who’s posting?'),
      h('p', {}, 'So we can let you know when things move forward. Your email is never shown on the board.'),
      h('label', {}, 'Your name', name), h('label', {}, 'Email', email), err, h('div', {}, go));
    setTimeout(function () { (name.value ? email : name).focus(); }, 30);
    return form;
  }

  // ---------- pages ----------
  function nav(on) {
    return h('nav', { class: 'pnav', 'aria-label': 'Feedback' },
      [['ideas', 'Ideas', '#/'], ['roadmap', 'Roadmap', '#/roadmap'], ['changelog', 'What’s new', '#/changelog']].map(function (o) {
        return h('a', { class: on === o[0] ? 'on' : null, href: o[2], 'aria-current': on === o[0] ? 'page' : null }, o[1]);
      }));
  }
  function ideaCard(it) {
    return h('a', { class: 'rcard', href: '#/idea/' + it.id },
      h('span', { class: 'rv' }, h('span', { html: UP }), h('b', {}, it.votes)), h('span', { class: 't' }, it.title));
  }
  function renderRoadmap() {
    show(nav('roadmap'), h('p', { class: 'muted' }, 'Loading…'));
    req('GET', '/api/widget/ideas?status=roadmap&sort=top').then(function (d) {
      var cols = [['planned', 'Planned', 'What we’re going to do next.'], ['in_progress', 'In progress', 'Being worked on now.'], ['complete', 'Done', 'Shipped — thanks for the ideas.']];
      show(nav('roadmap'),
        h('h1', {}, 'Roadmap'),
        h('p', { class: 'lead' }, 'Where the most-wanted ideas stand. Vote on the board to move things up.'),
        h('div', { class: 'roadmap' }, cols.map(function (c) {
          var items = d.ideas.filter(function (x) { return x.status === c[0]; });
          if (c[0] === 'complete') items = items.sort(function (a, b) { return b.status_at - a.status_at; }).slice(0, 20);
          return h('section', { class: 'rcol ' + c[0] },
            h('h2', {}, h('span', { class: 'chip ' + c[0] }, c[1]), h('span', { class: 'n' }, items.length)),
            h('p', { class: 'muted small' }, c[2]),
            items.length ? items.map(ideaCard) : h('p', { class: 'muted small' }, 'Nothing here yet.'));
        })));
    }).catch(fail);
  }
  function renderChangelog() {
    show(nav('changelog'), h('p', { class: 'muted' }, 'Loading…'));
    req('GET', '/api/widget/changelog').then(function (d) {
      show(nav('changelog'),
        h('h1', {}, 'What’s new'),
        h('p', { class: 'lead' }, 'Recent improvements — many of them your ideas.'),
        d.entries.length ? h('div', { class: 'log' }, d.entries.map(function (e) {
          return h('article', { class: 'card entry' },
            h('div', { class: 'muted small' }, new Date(e.published_at).toLocaleDateString(undefined, { day: 'numeric', month: 'long', year: 'numeric' })),
            h('h2', {}, e.title),
            e.body ? h('p', { class: 'body' }, e.body) : null,
            e.ideas.length ? h('div', { class: 'delivers' }, h('span', { class: 'muted small' }, 'Ideas this delivers:'),
              e.ideas.map(function (it) { return h('a', { class: 'dchip', href: '#/idea/' + it.id }, '✓ ' + it.title + ' · ' + it.votes + (it.votes === 1 ? ' vote' : ' votes')); })) : null);
        })) : h('div', { class: 'empty' }, 'Nothing announced yet — check back soon.'));
    }).catch(fail);
  }
  function renderBoard() {
    var title = s.settings.feedback_title || 'Feedback & ideas';
    var tabs = h('div', { class: 'tabs', role: 'group', 'aria-label': 'Filter by status' },
      [['active', 'All'], ['under_review', 'Under review'], ['planned', 'Planned'], ['in_progress', 'In progress'], ['complete', 'Done']].map(function (o) {
        return h('button', { class: 'tab' + (s.status === o[0] ? ' on' : ''), type: 'button', 'aria-pressed': String(s.status === o[0]),
          onclick: function () { s.status = o[0]; s.loaded = false; render(); loadIdeas().then(render).catch(fail); } }, o[1]);
      }));
    var sort = h('select', { 'aria-label': 'Sort', onchange: function () { s.sort = sort.value; loadIdeas().then(render).catch(fail); } },
      h('option', { value: 'top', selected: s.sort === 'top' }, 'Most votes'), h('option', { value: 'new', selected: s.sort === 'new' }, 'Newest'));
    var list = h('div', { class: 'list' });
    if (!s.loaded) list.append(h('p', { class: 'muted' }, 'Loading…'));
    else if (!s.ideas.length) list.append(h('div', { class: 'empty' }, s.status === 'active' ? 'No ideas yet — be the first to post one.' : 'Nothing here yet.'));
    s.ideas.forEach(function (it) {
      list.append(h('div', { class: 'idea' }, voteBtn(it, render),
        h('a', { class: 'main', href: '#/idea/' + it.id },
          h('span', { class: 't' }, it.title),
          h('span', { class: 'meta' }, chip(it), [it.comments ? it.comments + (it.comments === 1 ? ' comment' : ' comments') : '', it.author_name, day(it.created_at)].filter(Boolean).join(' · ')))));
    });
    show(nav('ideas'),
      h('h1', {}, title),
      h('p', { class: 'lead' }, 'Tell us what would make things better, and vote for the ideas you want most.'),
      h('div', { class: 'bar' }, tabs, sort, h('a', { class: 'btn pri', href: '#/new' }, '+ Post an idea')),
      list);
  }
  function renderNew() {
    if (needWho()) { show(h('a', { class: 'back', href: '#/' }, '← All ideas'), whoCard(renderNew)); return; }
    var title = h('input', { type: 'text', maxlength: 120, placeholder: 'Short and specific', required: true });
    var body = h('textarea', { rows: 6, maxlength: 5000, placeholder: 'What would it do for you?' });
    var err = h('div', { class: 'err', role: 'alert' });
    var go = h('button', { class: 'btn pri', type: 'submit' }, 'Post idea');
    show(h('a', { class: 'back', href: '#/' }, '← All ideas'),
      h('form', { class: 'card', onsubmit: function (e) {
        e.preventDefault();
        go.disabled = true; err.textContent = '';
        req('POST', '/api/widget/ideas', { title: title.value, body: body.value }).then(function (d) {
          s.loaded = false;
          toast(d.idea.approved ? 'Posted — thanks!' : 'Thanks! We’ll add it to the board once we’ve had a look.');
          location.hash = '#/idea/' + d.idea.id;
        }).catch(function (ex) {
          if (ex.status === 428) { s.contact.email = ''; renderNew(); return; }
          err.textContent = ex.message; go.disabled = false;
        });
      } },
        h('h2', {}, 'Share an idea'),
        h('p', {}, 'Others can vote on it, and you’ll see when it’s planned or done.'),
        h('label', {}, 'Your idea', title), h('label', {}, 'Details (optional)', body), err, h('div', {}, go)));
    title.focus();
  }
  function renderIdea(id) {
    show(h('a', { class: 'back', href: '#/' }, '← All ideas'), h('p', { class: 'muted' }, 'Loading…'));
    req('GET', '/api/widget/ideas/' + id).then(function (d) {
      if (d.merged_into) { location.replace('#/idea/' + d.merged_into); return; }
      var it = d.idea, draft = '';
      function draw(askWho) {
        var comments = h('div', { class: 'comments' });
        if (!d.comments.length) comments.append(h('p', { class: 'muted' }, 'No comments yet.'));
        d.comments.forEach(function (c) {
          if (c.author_type === 'system') { comments.append(h('div', { class: 'sys' }, c.body + ' · ' + day(c.created_at))); return; }
          comments.append(h('div', { class: 'c' + (c.author_type === 'agent' ? ' team' : '') },
            h('div', { class: 'who' }, h('b', {}, c.mine ? 'You' : c.author_name), ' · ' + day(c.created_at)), h('div', { class: 'txt' }, c.body)));
        });
        var box = null;
        if (askWho) box = whoCard(function () { draw(false); send(); });
        else if (it.approved && it.status !== 'closed') {
          var ta = h('textarea', { rows: 3, maxlength: 3000, placeholder: 'Add a comment…', 'aria-label': 'Comment', value: draft });
          ta.oninput = function () { draft = ta.value; };
          var go = h('button', { class: 'btn pri', type: 'submit' }, 'Post comment');
          box = h('form', { class: 'card', onsubmit: function (e) { e.preventDefault(); send(); } }, ta, h('div', {}, go));
        }
        show(h('a', { class: 'back', href: '#/' }, '← All ideas'),
          h('div', { class: 'detail' }, voteBtn(it, function () { draw(false); }),
            h('div', { style: 'flex:1;min-width:0' }, h('h1', {}, it.title),
              h('div', { class: 'meta' }, chip(it), it.author_name + ' · ' + day(it.created_at)))),
          it.body ? h('p', { class: 'body' }, it.body) : null,
          !it.approved ? h('div', { class: 'note' }, 'Only you can see this until our team approves it.') : null,
          h('div', { class: 'sect' }, 'Activity'), comments, box);
      }
      function send() {
        if (!draft.trim()) return;
        if (needWho()) { draw(true); return; }
        req('POST', '/api/widget/ideas/' + it.id + '/comments', { body: draft })
          .then(function () { renderIdea(it.id); })
          .catch(function (ex) { if (ex.status === 428) { s.contact.email = ''; draw(true); } else toast(ex.message); });
      }
      draw(false);
    }).catch(function (e) { toast(e.message); location.hash = '#/'; });
  }
  function fail(e) {
    show(h('div', { class: 'empty' }, e.status === 404 ? 'The feedback board isn’t open right now.' : e.message));
  }
  function render() {
    var m = location.hash.match(/^#\/idea\/(\d+)$/);
    if (m) return renderIdea(Number(m[1]));
    if (location.hash === '#/new') return renderNew();
    if (location.hash === '#/roadmap') return renderRoadmap();
    if (location.hash === '#/changelog') return renderChangelog();
    renderBoard();
  }
  window.addEventListener('hashchange', function () {
    if (location.hash === '#/' || !location.hash) { if (!s.loaded) loadIdeas().then(render).catch(fail); else render(); }
    else render();
    window.scrollTo(0, 0);
  });
  session(false).then(function () {
    if (!s.settings.feedback) { fail({ status: 404 }); return; }
    render();
    return loadIdeas().then(function () { if (!location.hash || location.hash === '#/') render(); });
  }).catch(fail);
})();
