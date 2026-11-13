// Help Desk agent inbox — no build step, no dependencies.

const $app = document.getElementById('app');

const state = {
  me: null,
  emailEnabled: false,
  ai: null,            // { enabled, provider, model } from the server
  summaries: {},       // conversation id -> Copilot summary
  pendingArticle: null, // a Copilot-drafted article waiting to be edited
  baseUrl: location.origin,
  view: 'open',
  convs: [],
  counts: {},
  agents: [],
  conv: null,
  messages: [],
  history: [],
  settings: {},
  mode: 'reply',
  drafts: {},
  live: false,
  ntfyEnabled: false,
  notifyEmail: '',
  q: '',
  myTasks: [],         // my open tasks (plus today's finished ones) — drives the Activities badge
  caseTasks: [],       // tasks linked to the open conversation
};

// ---------- this device: presence + sound ----------
// Each open inbox tells the server whether it's on screen, so it knows whether
// anyone will hear the chime or whether to send ntfy / email instead.
const CLIENT_ID = (crypto.randomUUID?.() || String(Math.random()).slice(2)) ;
const IS_MOBILE = /Android|iPhone|iPad|iPod|Mobile/i.test(navigator.userAgent) ||
  (navigator.maxTouchPoints > 1 && /Macintosh/.test(navigator.userAgent));
const visibleNow = () => document.visibilityState === 'visible';

function reportPresence() {
  if (!state.me) return;
  fetch('/api/me/presence', {
    method: 'POST', keepalive: true, credentials: 'same-origin',
    headers: { 'X-Helpdesk': '1', 'Content-Type': 'application/json' },
    body: JSON.stringify({ client: CLIENT_ID, visible: visibleNow() }),
  }).catch(() => {});
}

const sound = {
  get on() { try { return localStorage.getItem('hd_sound') !== 'off'; } catch { return true; } },
  set on(v) { try { localStorage.setItem('hd_sound', v ? 'on' : 'off'); } catch { } },
  ctx: null,
};
// Browsers only allow audio after the user has interacted with the page.
function unlockAudio() {
  const AC = window.AudioContext || window.webkitAudioContext;
  if (!AC) return;
  if (!sound.ctx) sound.ctx = new AC();
  if (sound.ctx.state === 'suspended') sound.ctx.resume().catch(() => {});
}
['pointerdown', 'keydown', 'touchstart'].forEach(t => window.addEventListener(t, unlockAudio, { passive: true }));

function chime(force) {
  if ((!sound.on && !force) || !sound.ctx || sound.ctx.state !== 'running') return false;
  const ctx = sound.ctx, t = ctx.currentTime;
  for (const [i, f] of [[0, 880], [1, 1318.5]]) {
    const o = ctx.createOscillator(), g = ctx.createGain();
    o.type = 'sine';
    o.frequency.value = f;
    const s = t + i * 0.16;
    g.gain.setValueAtTime(0.0001, s);
    g.gain.exponentialRampToValueAtTime(0.35, s + 0.02);
    g.gain.exponentialRampToValueAtTime(0.0001, s + 0.45);
    o.connect(g).connect(ctx.destination);
    o.start(s);
    o.stop(s + 0.5);
  }
  return true;
}

async function setAway(away) {
  try {
    const r = await api('PUT', '/api/me/away', { away });
    state.me.away = r.agent.away;
    renderSidebar();
    toast(away ? 'You\'re Away' : 'You\'re available');
  } catch (ex) { toast(ex.message); }
}
const els = {};

const VIEWS = [
  ['open', 'Open'],
  ['mine', 'Assigned to me'],
  ['unassigned', 'Unassigned'],
  ['pending', 'Pending'],
  ['closed', 'Closed'],
];

// ---------- tiny DOM helper ----------
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'html') el.innerHTML = v; // only ever used with static strings
    else if (k === 'value') el.value = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

// fill replaces an element's children, skipping null/false the way h() does
// (replaceChildren itself would print them as "null").
function fill(el, ...kids) {
  el.replaceChildren(...kids.flat(Infinity).filter(k => k != null && k !== false));
}

const ICON_LOGO = '<svg viewBox="0 0 32 32" aria-hidden="true"><path d="M9 11h14v9h-8l-4 3v-3H9z" fill="white"/></svg>';
const svg = (paths) => `<svg class="i" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${paths}</svg>`;
const ICON = {
  bell: svg('<path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/>'),
  bellOff: svg('<path d="M8.7 3A6 6 0 0 1 18 8c0 2.8.6 4.7 1.3 6"/><path d="M17 17H3s3-2 3-9a4.7 4.7 0 0 1 .3-1.7"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/><path d="m2 2 20 20"/>'),
  moon: svg('<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/>'),
  sun: svg('<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>'),
  search: svg('<circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/>'),
  link: svg('<path d="M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7-7l-1.7 1.7"/><path d="M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7 7l1.7-1.7"/>'),
  check: svg('<path d="M20 6 9 17l-5-5"/>'),
  menu: svg('<path d="M4 6h16M4 12h16M4 18h16"/>'),
  back: svg('<path d="M19 12H5M11 6l-6 6 6 6"/>'),
};

// ---------- light / dark ----------
const darkMQ = window.matchMedia('(prefers-color-scheme: dark)');
function isDark() {
  const t = document.documentElement.getAttribute('data-theme');
  return t ? t === 'dark' : darkMQ.matches;
}
function setTheme(t) {
  document.documentElement.setAttribute('data-theme', t);
  try { localStorage.setItem('hd_theme', t); } catch { }
  renderSidebar();
}
darkMQ.addEventListener?.('change', () => renderSidebar());

const RELATED_HOW = { number: 'customer entered it', unsure: 'customer wasn\'t sure, matched last case' };

// ---------- API ----------
class ApiError extends Error {}
async function api(method, path, body) {
  const opts = { method, headers: { 'X-Helpdesk': '1' }, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401 && path !== '/api/auth/login') {
    state.me = null;
    stopStream();
    renderLogin();
    throw new ApiError('Signed out');
  }
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(data.error || `Request failed (${res.status})`);
  return data;
}

function toast(text) {
  const t = h('div', { class: 'toast', role: 'status' }, text);
  document.body.append(t);
  setTimeout(() => t.remove(), 2600);
}

// ---------- formatting ----------
function hue(s) {
  if (typeof s === 'number') return Math.round((s * 137.508) % 360); // golden angle: neighbouring ids get distinct colours
  let x = 0;
  for (const ch of String(s)) x = (x * 31 + ch.charCodeAt(0)) >>> 0;
  return x % 360;
}
function initials(name) {
  const parts = String(name).replace(/[^\p{L}\p{N} ]/gu, ' ').trim().split(/\s+/);
  return ((parts[0]?.[0] || '?') + (parts[1]?.[0] || '')).toUpperCase();
}
function avatar(name, seed, opts = {}) {
  return h('div', {
    class: 'avatar' + (opts.sm ? ' sm' : '') + (opts.lg ? ' lg' : ''),
    style: `background:hsl(${hue(seed ?? name)} 55% 48%)`,
    title: name,
  }, initials(name), opts.online ? h('span', { class: 'dot', title: 'On the site now' }) : null);
}
// Case numbers as customers see them (store.go caseNumber: HD-<id+1000>).
const caseNo = (id) => 'HD-' + (Number(id) + 1000);

function contactName(c) {
  return c?.name || c?.email || `Visitor #${c?.id}`;
}
function relTime(ms) {
  const d = Date.now() - ms;
  if (d < 60e3) return 'now';
  if (d < 3600e3) return `${Math.floor(d / 60e3)}m`;
  if (d < 86400e3) return `${Math.floor(d / 3600e3)}h`;
  const dt = new Date(ms);
  if (d < 6 * 86400e3) return dt.toLocaleDateString(undefined, { weekday: 'short' });
  return dt.toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}
function clock(ms) {
  return new Date(ms).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
}
function dayLabel(ms) {
  const d = new Date(ms), today = new Date();
  const y = new Date(); y.setDate(today.getDate() - 1);
  if (d.toDateString() === today.toDateString()) return 'Today';
  if (d.toDateString() === y.toDateString()) return 'Yesterday';
  return d.toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long' });
}
function browserOf(ua = '') {
  const b = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome'
    : /Safari\//.test(ua) ? 'Safari' : '';
  const o = /iPhone|iPad/.test(ua) ? 'iOS' : /Android/.test(ua) ? 'Android' : /Windows/.test(ua) ? 'Windows'
    : /Mac OS X/.test(ua) ? 'macOS' : /Linux/.test(ua) ? 'Linux' : '';
  return [b, o].filter(Boolean).join(' on ') || (ua ? ua.slice(0, 60) : '—');
}
function agentName(id) {
  return state.agents.find(a => a.id === id)?.name;
}
function clipText(s, n) {
  s = String(s || '');
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}
// Case states an agent can pick: [key, label, short pill text]. The server
// stores status (open/pending/closed) plus sub_status; customers only see status.
const STATUS_OPTIONS = [
  ['open', 'Open', 'open'],
  ['in_progress', 'In Progress', 'in progress'],
  ['pending', 'Awaiting Customer', 'awaiting customer'],
  ['pending_vendor', 'Awaiting Manufacturer', 'awaiting manufacturer'],
  ['awaiting_approval', 'Awaiting Approval', 'awaiting approval'],
  ['scheduled', 'Scheduled', 'scheduled'],
  ['on_hold', 'On Hold', 'on hold'],
  ['closed', 'Closed', 'closed'],
];
const statusKey = (c) => c.sub_status || c.status;
const statusInfo = (c) => STATUS_OPTIONS.find(o => o[0] === statusKey(c)) || [c.status, c.status, c.status];
const statusPill = (c) => h('span', { class: 'pill ' + c.status }, statusInfo(c)[2]);

function categories() {
  return String(state.settings.categories || '').split('\n').map(s => s.trim()).filter(Boolean);
}
function duration(ms) {
  const m = Math.max(0, Math.round(ms / 60e3));
  if (m < 60) return `${m || 1}m`;
  const hrs = Math.round(m / 60);
  if (hrs < 48) return `${hrs}h`;
  return `${Math.round(hrs / 24)}d`;
}
function shortDate(ms) {
  const d = new Date(ms), now = new Date();
  return d.toLocaleDateString(undefined, d.getFullYear() === now.getFullYear()
    ? { day: 'numeric', month: 'short' } : { day: 'numeric', month: 'short', year: 'numeric' });
}

// ---------- routing ----------
function parseRoute() {
  const [path, query = ''] = location.hash.replace(/^#\/?/, '').split('?');
  const p = path.split('/');
  if (p[0] === 'c' && /^\d+$/.test(p[1] || '')) return { kind: 'conv', id: Number(p[1]) };
  if (p[0] === 'settings') return { kind: 'settings' };
  if (p[0] === 'cases') return { kind: 'cases', params: new URLSearchParams(query) };
  if (p[0] === 'activities') return { kind: 'activities', params: new URLSearchParams(query) };
  if (p[0] === 'knowledge') return { kind: 'knowledge', id: p[1] === 'new' ? 'new' : /^\d+$/.test(p[1] || '') ? Number(p[1]) : null };
  if (p[0] === 'view' && VIEWS.some(v => v[0] === p[1])) return { kind: 'view', view: p[1] };
  return { kind: 'view', view: 'open' };
}

async function route() {
  if (!state.me) return;
  const r = parseRoute();
  setDrawer(false);
  if (r.kind === 'settings') {
    renderSettings();
    return;
  }
  if (r.kind === 'cases') {
    renderCases(r.params);
    return;
  }
  if (r.kind === 'activities') {
    renderActivities(r.params);
    return;
  }
  if (r.kind === 'knowledge') {
    if (r.id) renderArticleEditor(r.id); else renderKnowledge();
    return;
  }
  if (!els.shell || !document.body.contains(els.shell) || inPage()) renderShell();
  if (r.kind === 'view') {
    state.view = r.view;
    state.conv = null;
    if (state.q) { state.q = ''; if (els.search) els.search.value = ''; }
    renderSidebar();
    renderThread();
    await loadList();
  } else {
    if (!state.convs.length) loadList();
    await openConversation(r.id);
  }
}

// ---------- login ----------
function renderLogin() {
  const err = h('div', { class: 'error', role: 'alert' });
  const email = h('input', { class: 'input', type: 'email', name: 'email', autocomplete: 'username', required: true, autofocus: true });
  const pw = h('input', { class: 'input', type: 'password', name: 'password', autocomplete: 'current-password', required: true });
  const btn = h('button', { class: 'btn primary', type: 'submit' }, 'Sign in');
  const form = h('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      btn.disabled = true;
      err.textContent = '';
      try {
        await api('POST', '/api/auth/login', { email: email.value, password: pw.value });
        await boot();
      } catch (ex) {
        err.textContent = ex.message;
        btn.disabled = false;
      }
    },
  },
    h('div', { class: 'logo', html: ICON_LOGO }),
    h('h1', {}, 'Sign in'),
    h('p', {}, 'Agent inbox'),
    h('label', { class: 'field' }, 'Email', email),
    h('label', { class: 'field' }, 'Password', pw),
    err, btn,
  );
  fill($app, h('div', { class: 'login' }, form));
  email.focus();
}

// ---------- shell ----------
function renderShell() {
  els.sidebar = h('nav', { class: 'sidebar', id: 'sidebar', 'aria-label': 'Menu' });
  // Swipe the drawer left to close it.
  let sx = null, sy = null;
  els.sidebar.addEventListener('touchstart', (e) => { sx = e.touches[0].clientX; sy = e.touches[0].clientY; }, { passive: true });
  els.sidebar.addEventListener('touchmove', (e) => {
    if (sx == null || !drawerOpen()) return;
    const dx = e.touches[0].clientX - sx, dy = e.touches[0].clientY - sy;
    if (dx < -60 && Math.abs(dx) > Math.abs(dy) * 1.5) { sx = null; setDrawer(false); }
  }, { passive: true });
  els.sidebar.addEventListener('touchend', () => { sx = null; }, { passive: true });
  els.scrim = h('div', { class: 'scrim', 'aria-hidden': 'true', onclick: () => setDrawer(false, { restoreFocus: true }) });
  els.list = h('section', { class: 'list', 'aria-label': 'Conversations' });
  els.thread = h('section', { class: 'thread', 'aria-label': 'Conversation' });
  els.details = h('aside', { class: 'details', 'aria-label': 'Customer details' });
  els.shell = h('div', { class: 'shell' }, els.sidebar, els.list, els.thread, els.details, els.scrim);
  fill($app, els.shell);
  renderSidebar();
  renderList();
  renderThread();
}

// ----- phone menu: the sidebar slides in over the page -----
const ICON_MENU = '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M3.5 5.5h13M3.5 10h13M3.5 14.5h13"/></svg>';
const drawerOpen = () => !!els.shell?.classList.contains('show-sidebar');
function setDrawer(open, { restoreFocus = false } = {}) {
  if (!els.shell) return;
  const was = drawerOpen();
  els.shell.classList.toggle('show-sidebar', open);
  document.querySelectorAll('.menu-btn').forEach(b => b.setAttribute('aria-expanded', String(open)));
  if (open && !was) {
    (els.sidebar.querySelector('.nav-item.active') || els.sidebar.querySelector('.nav-item'))?.focus({ preventScroll: true });
  } else if (!open && was && restoreFocus) {
    document.querySelector('.menu-btn')?.focus({ preventScroll: true });
  }
}
function attention() {
  return (state.counts.unread || 0) + myDue().n;
}
function menuButton() {
  const n = attention();
  return h('button', {
    class: 'btn ghost menu-btn' + (n ? ' dot' : ''), html: ICON_MENU, type: 'button',
    'aria-label': n ? `Menu — ${n} need${n === 1 ? 's' : ''} attention` : 'Menu',
    'aria-expanded': String(drawerOpen()), 'aria-controls': 'sidebar',
    onclick: () => setDrawer(!drawerOpen()),
  });
}
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && drawerOpen() && !document.querySelector('dialog[open]')) setDrawer(false, { restoreFocus: true });
});

function renderSidebar() {
  if (!els.sidebar) return;
  const route = parseRoute();
  const items = VIEWS.map(([key, label]) => {
    const n = state.counts[key];
    const active = (route.kind === 'view' || route.kind === 'conv') && state.view === key;
    return h('a', { class: 'nav-item' + (active ? ' active' : ''), href: `#/view/${key}` },
      label,
      key !== 'closed' && n ? h('span', { class: 'count' + (key === 'unassigned' && n ? ' hot' : '') }, n) : null);
  });
  const away = !!state.me.away;
  const dark = isDark();
  // A live update can redraw the menu while someone is using it: keep their place.
  const focused = els.sidebar.contains(document.activeElement)
    ? (document.activeElement.getAttribute('href') || '.' + [...document.activeElement.classList].join('.')) : null;
  els.sidebar.replaceChildren(
    h('div', { class: 'brand' }, h('span', { class: 'logo', html: ICON_LOGO }),
      h('div', { class: 'brand-name' }, h('div', { class: 'bn' }, state.settings.brand_name || 'Help Desk'), h('div', { class: 'bs' }, 'Help desk')),
      h('button', { type: 'button', class: 'btn ghost drawer-close', 'aria-label': 'Close menu', onclick: () => setDrawer(false, { restoreFocus: true }) }, '✕')),
    ...items,
    h('div', { class: 'nav-sep' }),
    activitiesNav(route.kind === 'activities'),
    h('a', { class: 'nav-item' + (route.kind === 'cases' ? ' active' : ''), href: '#/cases' }, 'All cases'),
    h('a', { class: 'nav-item' + (route.kind === 'knowledge' ? ' active' : ''), href: '#/knowledge' }, 'Knowledge'),
    h('a', { class: 'nav-item' + (route.kind === 'settings' ? ' active' : ''), href: '#/settings' }, 'Settings'),
    h('div', { class: 'spacer' }),
    h('div', { class: 'away-toggle', role: 'group', 'aria-label': 'Your status' },
      h('button', { type: 'button', class: !away ? 'on' : '', 'aria-pressed': String(!away), onclick: () => away && setAway(false) }, h('span', { class: 'd' }), 'Available'),
      h('button', { type: 'button', class: away ? 'on' : '', 'aria-pressed': String(away), onclick: () => !away && setAway(true) }, h('span', { class: 'd away' }), 'Away')),
    h('p', { class: 'status-hint' }, away
      ? 'New chats go to ntfy, then email if that fails.'
      : 'You\'ll hear a chime for new messages.'),
    h('div', { class: 'me' },
      avatar(state.me.name, state.me.id, { sm: true }),
      h('div', { class: 'who' },
        h('div', {}, state.me.name),
        h('div', { class: 'small muted' },
          h('span', { class: 'conn' + (state.live ? (away ? ' away' : ' live') : '') }),
          !state.live ? 'Reconnecting…' : away ? 'Away' : 'Available')),
      h('button', {
        class: 'icon-btn', type: 'button', html: sound.on ? ICON.bell : ICON.bellOff,
        title: sound.on ? 'Sound on' : 'Sound off',
        'aria-label': sound.on ? 'Mute new-message sound' : 'Turn on new-message sound',
        'aria-pressed': String(sound.on),
        onclick: () => { sound.on = !sound.on; unlockAudio(); if (sound.on) chime(true); renderSidebar(); },
      }),
      h('button', {
        class: 'icon-btn', type: 'button', html: dark ? ICON.sun : ICON.moon,
        title: dark ? 'Light theme' : 'Dark theme',
        'aria-label': dark ? 'Switch to light theme' : 'Switch to dark theme',
        onclick: () => setTheme(dark ? 'light' : 'dark'),
      })),
  );
  if (focused && focused !== '.') {
    try {
      const el = focused.startsWith('#') ? els.sidebar.querySelector(`a[href="${CSS.escape(focused)}"]`) : els.sidebar.querySelector(focused);
      el?.focus({ preventScroll: true });
    } catch { /* not a usable selector: leave focus alone */ }
  }
  const n = attention();
  document.querySelectorAll('.menu-btn').forEach(b => {
    b.classList.toggle('dot', n > 0);
    b.setAttribute('aria-label', n ? `Menu — ${n} need${n === 1 ? 's' : ''} attention` : 'Menu');
  });
  const unread = state.counts.unread || 0;
  document.title = (unread ? `(${unread}) ` : '') + (state.settings.brand_name || 'Help Desk') + ' inbox';
}

async function loadList() {
  try {
    const [list, counts] = await Promise.all([
      api('GET', `/api/conversations?view=${state.view}` + (state.q ? `&q=${encodeURIComponent(state.q)}` : '')),
      api('GET', '/api/conversations/counts'),
    ]);
    state.convs = list.conversations;
    state.counts = counts;
    renderList();
    renderSidebar();
  } catch (e) {
    if (!(e instanceof ApiError && e.message === 'Signed out')) console.error(e);
  }
}

let refreshTimer;
function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(loadList, 250);
}

// True on full-width pages (Cases, Settings), where the inbox columns are gone.
const inPage = () => !!els.shell?.classList.contains('page-mode');

function renderList() {
  if (!els.list || inPage()) return;
  const label = state.q ? 'Search' : (VIEWS.find(v => v[0] === state.view)?.[1] || 'Open');
  const body = h('div', { class: 'list-body' });
  if (!state.convs.length) {
    body.append(h('div', { class: 'empty' },
      state.q ? h('strong', {}, 'No matches')
        : h('strong', {}, state.view === 'open' ? 'Inbox zero' : 'Nothing here'),
      state.q ? 'Try a case number like HD-1035, a name, an email or words from the chat.'
        : state.view === 'open' ? 'New chats from your website will appear here.' : 'No conversations in this view.'));
  }
  for (const c of state.convs) {
    const name = contactName(c.contact);
    const assignee = c.assignee_id ? agentName(c.assignee_id) : null;
    body.append(h('a', {
      class: 'conv' + (c.unread ? ' unread' : '') + (state.conv?.id === c.id ? ' active' : ''),
      href: `#/c/${c.id}`, 'aria-current': state.conv?.id === c.id ? 'true' : null,
    },
      avatar(name, c.contact_id, { online: c.contact.online }),
      h('div', { style: 'min-width:0' },
        h('div', { class: 'top' }, h('span', { class: 'name' }, name), h('span', { class: 'time' }, relTime(c.last_message_at))),
        h('div', { class: 'preview' }, c.preview || c.subject),
        h('div', { class: 'meta' },
          statusPill(c),
          c.category ? h('span', { class: 'pill cat' }, c.category) : null,
          c.ai_state === 'active' && c.status !== 'closed' ? h('span', { class: 'pill ai' }, '✦ AI') : null,
          c.ai_state === 'handoff' && c.status !== 'closed' ? h('span', { class: 'pill pending' }, 'from AI') : null,
          h('span', {}, `${c.number} · ${assignee || 'Unassigned'}`),
          c.unread ? h('span', { class: 'unread-dot', 'aria-label': 'Unread' }) : null),
      ),
    ));
  }
  // Keep the search box (and its focus) when the list refreshes.
  if (!els.search) {
    let timer;
    els.search = h('input', {
      type: 'search', placeholder: 'Search case #, name, email or message', 'aria-label': 'Search conversations',
      oninput: (e) => {
        clearTimeout(timer);
        timer = setTimeout(() => { state.q = e.target.value.trim(); loadList(); }, 250);
      },
      onkeydown: (e) => { if (e.key === 'Escape') { e.target.value = ''; state.q = ''; loadList(); } },
    });
    els.searchBox = h('label', { class: 'search' }, h('span', { html: ICON.search, style: 'display:grid' }), els.search);
  }
  els.list.replaceChildren(
    h('div', { class: 'list-head' },
      h('div', { class: 'row' }, menuButton(), h('h2', {}, label),
        h('span', { class: 'n' }, `${state.convs.length}${state.convs.length === 50 ? '+' : ''} ${state.convs.length === 1 ? 'conversation' : 'conversations'}`)),
      els.searchBox),
    body,
  );
}

// ---------- conversation ----------
async function openConversation(id) {
  try {
    const [data, ct] = await Promise.all([
      api('GET', `/api/conversations/${id}`),
      api('GET', `/api/tasks?conversation=${id}`).catch(() => ({ tasks: [] })),
    ]);
    state.caseTasks = ct.tasks;
    state.conv = data.conversation;
    state.messages = data.messages;
    state.history = data.history;
    renderThread();
    renderList();
    if (state.conv.unread) {
      api('POST', `/api/conversations/${id}/read`).catch(() => {});
      state.conv.unread = false;
    }
  } catch (e) {
    if (e instanceof ApiError && e.message !== 'Signed out') {
      toast(e.message);
      location.hash = `#/view/${state.view}`;
    }
  }
}

function renderThread() {
  if (!els.thread || inPage()) return;
  const conv = state.conv;
  els.shell.classList.toggle('has-conv', !!conv);
  els.shell.classList.toggle('with-details', !!conv);
  if (!conv) {
    fill(els.thread, h('div', { class: 'placeholder' },
      h('div', {}, h('div', { style: 'font-size:15px;font-weight:600;color:var(--text)' }, 'Pick a conversation'),
        h('div', {}, 'Replies reach the customer instantly in the chat bubble, or by email if they\'ve left.'))));
    fill(els.details);
    return;
  }
  els.head = h('div', { class: 'thread-head' });
  els.messages = h('div', { class: 'messages', role: 'log', 'aria-live': 'polite' });
  els.thread.replaceChildren(els.head, els.messages, h('div', { class: 'composer-wrap' }, renderComposer()));
  renderThreadHead();
  renderMessages();
  renderDetails();
}

function renderThreadHead() {
  const conv = state.conv;
  if (!conv || !els.head) return;
  const assign = h('select', {
    class: 'input', 'aria-label': 'Assignee',
    onchange: async (e) => {
      const v = e.target.value;
      try {
        await api('POST', `/api/conversations/${conv.id}/assign`, { agent_id: v ? Number(v) : null });
      } catch (ex) { toast(ex.message); }
    },
  },
    h('option', { value: '' }, 'Unassigned'),
    state.agents.filter(a => !a.disabled || a.id === conv.assignee_id).map(a =>
      h('option', { value: a.id, selected: a.id === conv.assignee_id }, a.id === state.me.id ? `${a.name} (me)` : a.name)));

  const cats = categories();
  const category = h('select', {
    class: 'input', 'aria-label': 'Category',
    onchange: async (e) => {
      try { await api('POST', `/api/conversations/${conv.id}/category`, { category: e.target.value }); }
      catch (ex) { toast(ex.message); e.target.value = conv.category; }
    },
  },
    h('option', { value: '' }, 'No category'),
    // Keep a category that was removed from Settings visible on old cases.
    (conv.category && !cats.includes(conv.category) ? [conv.category, ...cats] : cats)
      .map(c => h('option', { value: c, selected: c === conv.category }, c)));

  const statusSel = h('select', {
    class: 'input status-select', 'aria-label': 'Case status',
    onchange: async (e) => {
      try { await api('POST', `/api/conversations/${conv.id}/status`, { status: e.target.value }); }
      catch (ex) { toast(ex.message); e.target.value = statusKey(conv); }
    },
  }, STATUS_OPTIONS.map(([v, t]) => h('option', { value: v, selected: v === statusKey(conv) }, t)));
  const actions = [statusSel];
  els.head.replaceChildren(
    h('div', { class: 'th-top' },
      h('a', { class: 'btn ghost back', href: `#/view/${state.view}`, 'aria-label': 'Back to list', html: ICON.back }),
      h('div', { class: 'title' },
        h('div', { class: 'line' },
          h('span', { class: 'nm' }, contactName(conv.contact)),
          h('span', { class: 'case-chip' }, `Case ${conv.number}`),
          conv.related_id ? h('a', { class: 'rel-chip', href: `#/c/${conv.related_id}`, title: 'Open the earlier case' },
            h('span', { html: ICON.link, style: 'display:grid' }),
            `Follows ${conv.related_number}` + (RELATED_HOW[conv.related_via] ? ` · ${RELATED_HOW[conv.related_via]}` : '')) : null),
        h('div', { class: 'small muted' },
          statusPill(conv), ' ',
          conv.contact.online ? 'On the site now' : (conv.contact.email || 'No email yet'))),
      ...actions),
    h('div', { class: 'th-meta' },
      h('label', {}, h('span', {}, 'Category'), category),
      h('label', {}, h('span', {}, 'Assignee'), assign),
      aiControl(conv),
      h('button', {
        class: 'btn ghost sm followup-btn', title: 'Add a follow-up for this case to Activities',
        onclick: () => openTaskDialog(null, followupDefaults(conv)),
      }, h('span', { class: 'k-ico', html: kindIcon('task') }), 'Follow-up')),
  );
}

// aiControl shows whether the AI agent is handling this case, with a way to take over.
function aiControl(conv) {
  const agentOn = state.ai?.enabled && state.settings.ai_agent && state.settings.ai_agent !== 'off';
  const setMode = (mode) => async () => {
    try { await api('POST', `/api/conversations/${conv.id}/ai`, { mode }); }
    catch (ex) { toast(ex.message); }
  };
  if (conv.ai_state === 'active') {
    return h('span', { class: 'ai-state' }, h('span', { class: 'pill ai' }, '✦ AI is answering'),
      h('button', { class: 'btn ghost sm', onclick: setMode('off') }, 'Take over'));
  }
  if (conv.ai_state === 'handoff') {
    return h('span', { class: 'ai-state' }, h('span', { class: 'pill pending' }, 'AI handed this to the team'));
  }
  if (conv.ai_state === 'off' && agentOn && conv.status !== 'closed') {
    return h('span', { class: 'ai-state' }, h('button', { class: 'btn ghost sm', onclick: setMode('auto') }, '✦ Let AI answer'));
  }
  return null;
}

function messageEl(m) {
  const kind = m.author_type === 'system' ? 'system' : m.is_note ? 'note' : m.author_type;
  const who = m.author_type === 'contact' ? (m.author_name || contactName(state.conv?.contact))
    : m.author_type === 'agent' ? (m.author_name || 'Agent')
      : m.author_type === 'bot' ? `✦ ${m.author_name || 'AI assistant'} (AI)` : '';
  return h('div', { class: 'msg ' + kind, 'data-id': m.id },
    kind !== 'system' ? h('div', { class: 'by' }, kind === 'note' ? `${who} · internal note · ` : `${who} · `, clock(m.created_at)) : null,
    h('div', { class: 'bubble' }, m.body, kind === 'system' ? ` · ${clock(m.created_at)}` : ''),
  );
}

function renderMessages() {
  if (!els.messages) return;
  const nodes = [];
  let lastDay = '';
  for (const m of state.messages) {
    const d = new Date(m.created_at).toDateString();
    if (d !== lastDay) {
      nodes.push(h('div', { class: 'day' }, h('span', {}, dayLabel(m.created_at))));
      lastDay = d;
    }
    nodes.push(messageEl(m));
  }
  fill(els.messages, ...nodes);
  els.messages.scrollTop = els.messages.scrollHeight;
}

function appendMessage(m) {
  if (state.messages.some(x => x.id === m.id)) return;
  const nearBottom = els.messages.scrollHeight - els.messages.scrollTop - els.messages.clientHeight < 120;
  const prev = state.messages[state.messages.length - 1];
  state.messages.push(m);
  if (!prev || new Date(prev.created_at).toDateString() !== new Date(m.created_at).toDateString()) {
    els.messages.append(h('div', { class: 'day' }, h('span', {}, dayLabel(m.created_at))));
  }
  els.messages.append(messageEl(m));
  if (nearBottom || m.author_type === 'agent') els.messages.scrollTop = els.messages.scrollHeight;
}

function renderComposer() {
  const conv = state.conv;
  const ta = h('textarea', {
    'aria-label': 'Message', rows: 3,
    placeholder: state.mode === 'note' ? 'Add an internal note — the customer won\'t see this' : 'Reply to the customer…',
    oninput: (e) => {
      state.drafts[conv.id] = e.target.value;
      e.target.style.height = 'auto';
      e.target.style.height = Math.min(e.target.scrollHeight, 240) + 'px';
    },
    onkeydown: (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        send();
      }
    },
  });
  ta.value = state.drafts[conv.id] || '';
  const sendBtn = h('button', { class: 'btn primary', onclick: () => send() }, state.mode === 'note' ? 'Add note' : 'Send');

  // Copilot: draft a reply or polish the agent's own draft.
  const sourcesEl = h('div', { class: 'copilot-sources', 'aria-live': 'polite' });
  const copilotBtn = (label, action, title) => h('button', {
    class: 'btn ghost sm copilot-btn', title,
    onclick: async (e) => {
      const btn = e.currentTarget;
      if (action === 'improve' && !ta.value.trim()) { toast('Write a draft first, then Improve it'); return; }
      const old = btn.textContent;
      box.querySelectorAll('.copilot-btn').forEach(b => { b.disabled = true; });
      btn.textContent = action === 'draft' ? 'Drafting…' : 'Improving…';
      try {
        const r = await api('POST', `/api/conversations/${conv.id}/copilot`, { action, draft: ta.value });
        ta.value = r.text;
        state.drafts[conv.id] = r.text;
        ta.dispatchEvent(new Event('input'));
        renderSources(r.sources);
        ta.focus();
      } catch (ex) { toast(ex.message); }
      finally {
        btn.textContent = old;
        box.querySelectorAll('.copilot-btn').forEach(b => { b.disabled = false; });
      }
    },
  }, label);
  function renderSources(src) {
    if (!src?.length) { fill(sourcesEl, h('span', { class: 'faint' }, 'Copilot draft · no matching articles or past cases — check it before sending.')); return; }
    fill(sourcesEl, h('span', { class: 'faint' }, 'Copilot used: '),
      ...src.flatMap((s, i) => [i ? h('span', { class: 'faint' }, ' · ') : null,
        h('a', { href: s.type === 'article' ? `#/knowledge/${s.id}` : `#/c/${s.id}`, title: s.title },
          s.type === 'article' ? `📄 ${clipText(s.title, 40)}` : `Case ${caseNo(s.id)}`)]).filter(Boolean));
  }
  const copilotBar = state.ai?.enabled && state.mode === 'reply'
    ? h('div', { class: 'copilot-bar' },
      copilotBtn('✦ Draft reply', 'draft', 'Copilot writes a reply from your knowledge base and similar resolved cases'),
      copilotBtn('Improve', 'improve', 'Copilot tidies up your draft without changing its meaning'))
    : null;

  const box = h('div', { class: 'composer' + (state.mode === 'note' ? ' note-mode' : '') },
    h('div', { class: 'tabs', role: 'tablist' },
      h('button', { class: 'tab' + (state.mode === 'reply' ? ' on' : ''), role: 'tab', onclick: () => setMode('reply') }, 'Reply'),
      h('button', { class: 'tab note' + (state.mode === 'note' ? ' on' : ''), role: 'tab', onclick: () => setMode('note') }, 'Note'),
      copilotBar),
    ta,
    sourcesEl,
    h('div', { class: 'row' },
      h('span', { class: 'hint' },
        state.mode === 'note' ? 'Only your team sees notes.'
          : conv.contact.online ? 'Customer is on the site — they\'ll see this instantly.'
            : conv.contact.email
              ? (state.emailEnabled ? `They're away — this will also be emailed to ${conv.contact.email}.` : 'They\'re away. Email isn\'t set up, so they\'ll see this next visit.')
              : 'They\'re away and left no email — they\'ll see this on their next visit.',
        ' Enter to send, Shift+Enter for a new line.'),
      sendBtn));

  async function send() {
    const body = ta.value.trim();
    if (!body) return;
    sendBtn.disabled = true;
    try {
      const res = await api('POST', `/api/conversations/${conv.id}/messages`, { body, note: state.mode === 'note' });
      ta.value = '';
      ta.style.height = '';
      fill(sourcesEl);
      delete state.drafts[conv.id];
      if (state.conv?.id === conv.id) appendMessage(res.message);
    } catch (ex) {
      toast(ex.message);
    } finally {
      sendBtn.disabled = false;
      ta.focus();
    }
  }
  function setMode(m) {
    state.mode = m;
    const fresh = renderComposer();
    box.replaceWith(fresh);
    fresh.querySelector('textarea').focus();
  }
  return box;
}

function renderDetails() {
  const conv = state.conv;
  if (!conv || !els.details) return;
  const c = conv.contact;
  els.details.replaceChildren(...[ // filter: replaceChildren would print null
    h('div', { class: 'who' },
      avatar(contactName(c), c.id, { online: c.online, lg: true }),
      h('div', { class: 'nm' }, contactName(c)),
      h('div', { class: 'small muted' }, c.online ? 'On the site now' : `Last seen ${relTime(c.last_seen_at)} ago`.replace('now ago', 'just now'))),
    h('dl', { class: 'kv' },
      h('dt', {}, 'Case'), h('dd', { style: 'font-variant-numeric:tabular-nums;font-weight:600' }, conv.number),
      conv.related_id ? [h('dt', {}, 'Follows'), h('dd', {}, h('a', { href: `#/c/${conv.related_id}` }, conv.related_number))] : null,
      h('dt', {}, 'Email'), h('dd', {}, c.email ? h('a', { href: `mailto:${c.email}` }, c.email) : h('span', { class: 'faint' }, 'Not given')),
      h('dt', {}, 'Page'), h('dd', {}, c.last_page && /^https?:\/\//.test(c.last_page)
        ? h('a', { href: c.last_page, target: '_blank', rel: 'noopener noreferrer' }, c.last_page.replace(/^https?:\/\//, ''))
        : h('span', { class: 'faint' }, '—')),
      h('dt', {}, 'Browser'), h('dd', {}, browserOf(c.user_agent)),
      h('dt', {}, 'First seen'), h('dd', {}, new Date(c.created_at).toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' })),
    ),
    h('h3', {}, `Cases (${state.history.length})`),
    h('div', { class: 'history' },
      state.history.slice(0, 8).map(x => h('a', { href: `#/c/${x.id}`, class: x.id === conv.id ? 'current' : '', 'aria-current': x.id === conv.id ? 'true' : null },
        h('div', { class: 'r' },
          h('b', {}, x.number),
          statusPill(x),
          h('span', { class: 't' }, relTime(x.last_message_at))),
        h('div', { class: 's' }, x.subject)))),
    state.history.length > 1 ? h('p', { class: 'small muted' }, 'Includes cases from other devices where they gave the same email.') : null,
    h('a', { class: 'small', href: `#/cases?contact=${c.id}`, style: 'display:inline-block;margin-top:8px' },
      state.history.length > 8 ? `See all ${state.history.length} cases →` : 'Open in case history →'),
    caseTasksPanel(conv),
    state.ai?.enabled ? copilotPanel(conv) : null,
  ].filter(Boolean));
}

// copilotPanel: case summary, and turning a resolved case into a knowledge article.
function copilotPanel(conv) {
  const out = h('div', { class: 'copilot-out', 'aria-live': 'polite' });
  if (state.summaries[conv.id]) out.textContent = state.summaries[conv.id];
  const run = (action, label) => h('button', {
    class: 'btn sm', onclick: async (e) => {
      const btn = e.currentTarget, old = btn.textContent;
      btn.disabled = true;
      btn.textContent = action === 'summarize' ? 'Summarizing…' : 'Writing article…';
      try {
        const r = await api('POST', `/api/conversations/${conv.id}/copilot`, { action });
        if (action === 'summarize') {
          state.summaries[conv.id] = r.text;
          out.textContent = r.text;
          if (!out.isConnected && state.conv?.id === conv.id) renderDetails(); // panel was redrawn meanwhile
        } else {
          state.pendingArticle = { title: r.title, body: r.text, source_case: conv.id };
          location.hash = '#/knowledge/new';
        }
      } catch (ex) { toast(ex.message); }
      finally { btn.disabled = false; btn.textContent = old; }
    },
  }, label);
  return h('div', {},
    h('h3', {}, '✦ Copilot'),
    h('div', { style: 'display:flex;gap:6px;flex-wrap:wrap' },
      run('summarize', state.summaries[conv.id] ? 'Summarize again' : 'Summarize case'),
      conv.status === 'closed' ? run('article', 'Turn into article') : null),
    out,
    conv.status !== 'closed' ? h('p', { class: 'small faint', style: 'margin:8px 0 0' }, 'Close the case to turn its fix into a knowledge article.') : null);
}

// ---------- live updates ----------
let es = null;
function startStream() {
  stopStream();
  const q = new URLSearchParams({ client: CLIENT_ID, mobile: IS_MOBILE ? '1' : '0', visible: visibleNow() ? '1' : '0' });
  es = new EventSource('/api/stream?' + q);
  es.onopen = () => {
    const wasDown = !state.live;
    state.live = true;
    reportPresence(); // the stream URL's "visible" may be stale after a reconnect
    renderSidebar();
    if (wasDown) { scheduleRefresh(); loadMyTasks(); if (state.conv) openConversation(state.conv.id); if (actsActive()) loadActs(); }
  };
  es.onerror = () => {
    state.live = false;
    renderSidebar();
  };
  es.onmessage = (e) => {
    let ev;
    try { ev = JSON.parse(e.data); } catch { return; }
    handleEvent(ev);
  };
}
function stopStream() {
  if (es) { es.close(); es = null; }
}

function handleEvent({ type, data }) {
  const conv = state.conv;
  switch (type) {
    case 'message':
      if (data.author_type === 'contact') chime();
      if (conv && data.conversation_id === conv.id && els.messages) {
        appendMessage(data);
        if (data.author_type === 'contact' && document.visibilityState === 'visible') {
          api('POST', `/api/conversations/${conv.id}/read`).catch(() => {});
        }
      }
      scheduleRefresh();
      break;
    case 'conversation':
      if (conv && data.id === conv.id) {
        const statusChanged = conv.status !== data.status;
        Object.assign(conv, data);
        renderThreadHead();
        renderDetails();
        if (statusChanged) renderList();
      }
      scheduleRefresh();
      break;
    case 'contact':
      if (conv && conv.contact_id === data.id) {
        Object.assign(conv.contact, { name: data.name, email: data.email });
        renderThreadHead();
        renderDetails();
      }
      scheduleRefresh();
      break;
    case 'visitor':
      if (conv && conv.contact_id === data.contact_id) {
        conv.contact.online = data.online;
        renderThreadHead();
        renderDetails();
      }
      for (const c of state.convs) if (c.contact_id === data.contact_id) c.contact.online = data.online;
      renderList();
      break;
    case 'read':
      scheduleRefresh();
      break;
    case 'agent':
      // Away / Available changed (possibly from another device).
      if (state.me && data.id === state.me.id) { state.me.away = data.away; renderSidebar(); }
      { const a = state.agents.find(x => x.id === data.id); if (a) a.away = data.away; }
      break;
    case 'task':
      applyTask(data);
      break;
    case 'task_deleted':
      removeTask(data.id);
      break;
    case 'tasks_reordered':
      tasksReordered(data.ids);
      break;
  }
}

document.addEventListener('visibilitychange', reportPresence);
window.addEventListener('pagehide', () => {
  if (!state.me) return;
  fetch('/api/me/presence', {
    method: 'POST', keepalive: true, credentials: 'same-origin',
    headers: { 'X-Helpdesk': '1', 'Content-Type': 'application/json' },
    body: JSON.stringify({ client: CLIENT_ID, visible: false }),
  }).catch(() => {});
});

document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible' && state.conv?.unread) {
    api('POST', `/api/conversations/${state.conv.id}/read`).catch(() => {});
  }
});

// ---------- settings ----------
// enterPage swaps the inbox columns for one full-width <main>.
function enterPage(cls) {
  if (!els.shell || !document.body.contains(els.shell)) renderShell();
  state.conv = null;
  els.shell.className = 'shell page-mode';
  const main = h('main', { class: cls });
  els.list.replaceWith(main);
  els.list = main;
  els.thread.remove();
  els.details.remove();
  renderSidebar();
  return main;
}

async function renderSettings() {
  const main = enterPage('settings');

  try {
    const [a, s] = await Promise.all([api('GET', '/api/agents'), api('GET', '/api/settings')]);
    state.agents = a.agents;
    state.settings = s.settings;
  } catch { return; }
  const isAdmin = state.me.role === 'admin';
  main.replaceChildren(...[ // filter: replaceChildren would print null
    h('div', { style: 'display:flex;align-items:center;gap:8px' }, menuButton(), h('h1', {}, 'Settings')),
    h('p', {}, isAdmin ? 'Set up the chat widget, your team and backups.' : 'Your account and team.'),
    isAdmin ? widgetCard() : null,
    notificationsCard(isAdmin),
    isAdmin ? aiCard() : null,
    isAdmin ? categoriesCard() : null,
    teamCard(isAdmin),
    accountCard(),
    isAdmin ? opsCard() : null,
  ].filter(Boolean));
}

function widgetCard() {
  const s = state.settings;
  const err = h('div', { class: 'error' });
  const f = {
    brand_name: h('input', { class: 'input', value: s.brand_name, maxlength: 60 }),
    accent_color: h('input', { class: 'input', value: s.accent_color, maxlength: 7, style: 'max-width:120px' }),
    greeting: h('textarea', { class: 'input', rows: 2, maxlength: 500 }),
    offline_message: h('textarea', { class: 'input', rows: 2, maxlength: 500 }),
  };
  f.greeting.value = s.greeting;
  f.offline_message.value = s.offline_message;
  const picker = h('input', { type: 'color', value: s.accent_color, 'aria-label': 'Pick colour', oninput: (e) => { f.accent_color.value = e.target.value; } });
  f.accent_color.addEventListener('input', () => { if (/^#[0-9a-f]{6}$/i.test(f.accent_color.value)) picker.value = f.accent_color.value; });
  const snippet = `<script src="${state.baseUrl}/widget.js" async></script>`;
  return h('section', { class: 'card' },
    h('h2', {}, 'Chat widget'),
    h('p', {}, 'Paste this before </body> on every page where the chat bubble should appear.'),
    h('pre', { class: 'snippet' }, snippet),
    h('div', { style: 'display:flex;gap:8px;margin-bottom:20px;flex-wrap:wrap' },
      h('button', { class: 'btn', onclick: () => navigator.clipboard.writeText(snippet).then(() => toast('Copied')) }, 'Copy snippet'),
      h('a', { class: 'btn', href: '/demo', target: '_blank', rel: 'noopener' }, 'Open test page')),
    h('p', { class: 'small muted', style: 'margin-top:-8px' }, 'Your site must also be listed in HELPDESK_ALLOWED_ORIGINS on the server.'),
    h('div', { class: 'grid2' },
      h('label', { class: 'field' }, 'Name shown in the widget', f.brand_name),
      h('label', { class: 'field' }, 'Accent colour', h('div', { class: 'swatch' }, picker, f.accent_color))),
    h('label', { class: 'field' }, 'Greeting', h('span', { class: 'hint' }, 'Shown when your team is online.'), f.greeting),
    h('label', { class: 'field' }, 'Away message', h('span', { class: 'hint' }, 'Shown when everyone is Away or signed out.'), f.offline_message),
    err,
    h('button', {
      class: 'btn primary', onclick: async () => {
        err.textContent = '';
        try {
          const r = await api('PUT', '/api/settings', Object.fromEntries(Object.entries(f).map(([k, el]) => [k, el.value])));
          state.settings = r.settings;
          renderSidebar();
          toast('Saved');
        } catch (ex) { err.textContent = ex.message; }
      },
    }, 'Save widget settings'),
  );
}

function notificationsCard(isAdmin) {
  const result = h('div', { class: 'small muted', role: 'status', style: 'margin-top:8px' });
  const soundBtn = h('button', {
    class: 'btn', type: 'button', onclick: () => {
      sound.on = !sound.on;
      unlockAudio();
      if (sound.on) chime(true);
      renderSettings();
    },
  }, sound.on ? 'Mute on this device' : 'Turn sound on');
  const off = h('span', { class: 'faint' }, 'not set up');
  return h('section', { class: 'card' },
    h('h2', {}, 'Notifications'),
    h('p', {}, 'When a customer writes in the chat:'),
    h('ul', { class: 'small', style: 'margin:0 0 14px;padding-left:18px;line-height:1.7' },
      h('li', {}, h('strong', {}, 'Someone is Available'), ' (inbox open, not set to Away): the inbox plays a sound.'),
      h('li', {}, h('strong', {}, 'Everyone is Away or signed out'), ': a notification is sent with ntfy ',
        state.ntfyEnabled ? h('span', { style: 'color:var(--good)' }, '(on)') : off, '.'),
      h('li', {}, 'If ntfy isn\'t set up or fails, the message is emailed to ',
        state.notifyEmail && state.emailEnabled ? h('strong', {}, state.notifyEmail) : off,
        '. Lines sent within a minute arrive as one email, and no email is sent if someone replies first.')),
    h('p', { class: 'small muted' },
      'A phone with the inbox open in the background counts as Away, because it can\'t play the sound. Set yourself to Away when you step away from the desk.'),
    h('div', { style: 'display:flex;gap:8px;flex-wrap:wrap' },
      soundBtn,
      h('button', {
        class: 'btn', type: 'button', onclick: () => {
          unlockAudio();
          if (!chime(true)) toast('Click anywhere on the page first, then try again');
        },
      }, 'Play test sound'),
      isAdmin ? h('button', {
        class: 'btn', type: 'button', onclick: async (e) => {
          e.target.disabled = true;
          result.textContent = 'Sending…';
          try {
            const r = await api('POST', '/api/admin/notify-test');
            result.textContent = `ntfy: ${r.ntfy} · email: ${r.email}`;
          } catch (ex) { result.textContent = ex.message; }
          e.target.disabled = false;
        },
      }, 'Send test ntfy + email') : null),
    result,
    isAdmin ? h('p', { class: 'small muted', style: 'margin-top:12px' },
      'Set NTFY_URL (and NTFY_TOKEN for a protected topic) and NOTIFY_EMAIL in the server\'s .env, then restart.') : null);
}

function aiCard() {
  const s = state.settings, ai = state.ai || {};
  const err = h('div', { class: 'error' });
  const mode = h('select', { class: 'input' },
    [['off', 'Off — Copilot only'], ['away', 'When no agent is signed in'], ['always', 'Always answer first']]
      .map(([v, t]) => h('option', { value: v, selected: v === s.ai_agent }, t)));
  const name = h('input', { class: 'input', value: s.ai_bot_name, maxlength: 40 });
  const max = h('input', { class: 'input', type: 'number', min: 1, max: 20, value: s.ai_max_replies, style: 'max-width:100px' });
  const handoff = h('textarea', { class: 'input', rows: 2, maxlength: 500 });
  handoff.value = s.ai_handoff_message || '';
  const guide = h('textarea', { class: 'input', rows: 4, maxlength: 2000, placeholder: 'e.g. Always answer in Australian English. Never troubleshoot payroll; hand those to a person.' });
  guide.value = s.ai_instructions || '';
  const status = ai.enabled
    ? h('p', {}, h('span', { class: 'pill open' }, 'Connected'), ` ${ai.provider === 'anthropic' ? 'Claude' : 'OpenAI-compatible'} · ${ai.model}`)
    : h('div', { class: 'callout' },
      h('strong', {}, 'AI isn\'t connected yet. '),
      'Set ', h('code', {}, 'AI_API_KEY'), ' (and optionally ', h('code', {}, 'AI_MODEL'), ') in the server\'s .env and restart. ',
      'For a local model, use ', h('code', {}, 'AI_PROVIDER=openai'), ' with ', h('code', {}, 'AI_BASE_URL'), ' pointing at Ollama.');
  return h('section', { class: 'card' },
    h('h2', {}, '✦ AI'),
    h('p', {}, 'Copilot helps agents draft and polish replies, summarize cases and write articles. The AI agent can also answer customers in the chat, using only published Knowledge articles, and hands off to your team when it isn\'t sure.'),
    status,
    h('div', { class: 'grid2', style: 'margin-top:12px' },
      h('label', { class: 'field' }, 'AI agent answers customers', mode),
      h('label', { class: 'field' }, 'AI agent name', name)),
    h('label', { class: 'field' }, 'Handoff message', h('span', { class: 'hint' }, 'Sent when the AI passes a case to your team.'), handoff),
    h('label', { class: 'field' }, 'Guidance for the AI', h('span', { class: 'hint' }, 'Tone, language and topics to avoid. Applies to Copilot and the AI agent.'), guide),
    h('label', { class: 'field' }, 'AI replies per case before handing off', max),
    err,
    h('button', {
      class: 'btn primary', onclick: async () => {
        err.textContent = '';
        try {
          const r = await api('PUT', '/api/settings', {
            ai_agent: mode.value, ai_bot_name: name.value, ai_max_replies: String(max.value),
            ai_handoff_message: handoff.value, ai_instructions: guide.value,
          });
          state.settings = r.settings;
          toast('Saved');
        } catch (ex) { err.textContent = ex.message; }
      },
    }, 'Save AI settings'));
}

// ---------- knowledge base ----------
async function renderKnowledge() {
  const main = enterPage('cases knowledge');
  const list = h('div', { class: 'kb-list' });
  let timer, seq = 0;
  async function load(q) {
    const my = ++seq;
    try {
      const r = await api('GET', '/api/knowledge' + (q ? '?q=' + encodeURIComponent(q) : ''));
      if (my !== seq) return;
      if (!r.articles.length) {
        fill(list, h('div', { class: 'empty' },
          h('strong', {}, q ? 'No articles match' : 'No articles yet'),
          q ? 'Try other words.' : 'Write how-tos and fixes here. Copilot uses all of them; the AI agent answers customers from published ones. Closing a case gives you a one-click "Turn into article".'));
        return;
      }
      fill(list, h('div', { class: 'case-table' }, r.articles.map(a => h('a', { class: 'kb-row', href: `#/knowledge/${a.id}` },
        h('span', { class: 'kb-main' },
          h('span', { class: 'c-title' }, a.title),
          h('span', { class: 'c-preview' }, a.snippet)),
        h('span', {}, a.published ? h('span', { class: 'pill open' }, 'AI agent can use') : h('span', { class: 'pill' }, 'Internal only')),
        h('span', { class: 'c-opened' }, a.source_case ? `From case ${caseNo(a.source_case)} · ` : '', `Updated ${shortDate(a.updated_at)}`)))));
    } catch (ex) {
      if (my === seq) fill(list, h('div', { class: 'empty' }, ex.message));
    }
  }
  main.replaceChildren(...[ // filter: replaceChildren would print null
    h('div', { class: 'cases-top' },
      h('div', { style: 'display:flex;align-items:center;gap:8px' }, menuButton(), h('h1', { style: 'flex:1' }, 'Knowledge'),
        h('a', { class: 'btn primary', href: '#/knowledge/new' }, '+ New article')),
      h('p', {}, 'What your AI learns from. Copilot reads every article plus your resolved cases; the AI agent only answers customers from articles marked "AI agent can use".')),
    state.ai?.enabled ? null : h('div', { class: 'callout', style: 'margin-bottom:16px' }, 'AI isn\'t connected yet — articles are saved, and will be used once an admin connects a model (Settings → AI).'),
    h('div', { class: 'cases-toolbar' }, h('div', { class: 'cases-search' },
      h('input', {
        class: 'input', type: 'search', placeholder: 'Search articles…', 'aria-label': 'Search articles',
        oninput: (e) => { clearTimeout(timer); timer = setTimeout(() => load(e.target.value.trim()), 250); },
      }))),
    list].filter(Boolean));
  load('');
}

async function renderArticleEditor(id) {
  const main = enterPage('cases knowledge');
  let art = { title: '', body: '', published: true, source_case: null };
  if (id === 'new' && state.pendingArticle) {
    art = { ...art, ...state.pendingArticle };
    state.pendingArticle = null;
  } else if (id !== 'new') {
    try { art = (await api('GET', `/api/knowledge/${id}`)).article; }
    catch (ex) { toast(ex.message); location.hash = '#/knowledge'; return; }
  }
  const err = h('div', { class: 'error' });
  const title = h('input', { class: 'input', value: art.title, maxlength: 150, placeholder: 'How do I reset my VPN password?', 'aria-label': 'Title' });
  const body = h('textarea', { class: 'input kb-body', rows: 16, 'aria-label': 'Article' });
  body.value = art.body;
  const pub = h('input', { type: 'checkbox', checked: art.published });
  const save = h('button', {
    class: 'btn primary', onclick: async () => {
      err.textContent = '';
      save.disabled = true;
      const payload = { title: title.value, body: body.value, published: pub.checked, source_case: art.source_case };
      try {
        if (id === 'new') {
          const r = await api('POST', '/api/knowledge', payload);
          toast('Article saved');
          location.hash = `#/knowledge/${r.article.id}`;
        } else {
          await api('PUT', `/api/knowledge/${id}`, payload);
          toast('Saved');
        }
      } catch (ex) { err.textContent = ex.message; }
      finally { save.disabled = false; }
    },
  }, id === 'new' ? 'Save article' : 'Save changes');
  fill(main, 
    h('div', { class: 'cases-top' },
      h('a', { class: 'small', href: '#/knowledge' }, '← Knowledge'),
      h('h1', { style: 'margin-top:6px' }, id === 'new' ? 'New article' : 'Edit article'),
      art.source_case ? h('p', {}, 'Drafted by Copilot from ', h('a', { href: `#/c/${art.source_case}` }, `case ${caseNo(art.source_case)}`), '. Check the steps are right and nothing personal slipped in before saving.') : h('p', {}, 'Write it the way you\'d explain it to a customer: the problem in a line, then the steps.')),
    h('section', { class: 'card kb-editor' },
      h('label', { class: 'field' }, 'Title', title),
      h('label', { class: 'field' }, 'Article', h('span', { class: 'hint' }, 'Plain text. Numbered steps work well.'), body),
      h('label', { class: 'check' }, pub, h('span', {}, h('strong', {}, 'AI agent can use this'), h('span', { class: 'hint' }, ' — when off, only agents and Copilot see it'))),
      err,
      h('div', { style: 'display:flex;gap:8px;margin-top:8px' }, save,
        id !== 'new' && state.me.role === 'admin' ? h('button', {
          class: 'btn danger', onclick: async () => {
            if (!confirm('Delete this article? The AI will stop using it.')) return;
            try { await api('DELETE', `/api/knowledge/${id}`); toast('Deleted'); location.hash = '#/knowledge'; }
            catch (ex) { toast(ex.message); }
          },
        }, 'Delete') : null)));
  if (!art.title) title.focus();
}

function categoriesCard() {
  const err = h('div', { class: 'error' });
  const ta = h('textarea', { class: 'input', rows: 7, 'aria-label': 'Categories, one per line' });
  ta.value = state.settings.categories || '';
  return h('section', { class: 'card' },
    h('h2', {}, 'Case categories'),
    h('p', {}, 'One per line. Agents pick one on each case, and All cases can group by them. Renaming one here doesn\'t change cases already filed under the old name.'),
    ta, err,
    h('button', {
      class: 'btn primary', style: 'margin-top:4px', onclick: async () => {
        err.textContent = '';
        try {
          const r = await api('PUT', '/api/settings', { categories: ta.value });
          state.settings = r.settings;
          ta.value = r.settings.categories;
          toast('Saved');
        } catch (ex) { err.textContent = ex.message; }
      },
    }, 'Save categories'));
}

// ---------- all cases ----------
const CASE_GROUPS = [['month', 'Month opened'], ['customer', 'Customer'], ['category', 'Category'], ['status', 'Status']];
const CASE_PERIODS = [['', 'All time'], ['7', 'Last 7 days'], ['30', 'Last 30 days'], ['90', 'Last 90 days'], ['365', 'Last 12 months']];
const CASE_STATUSES = [['', 'All'], ['active', 'Active'], ['open', 'Open'], ['pending', 'Pending'], ['closed', 'Closed']];

let casesSeq = 0;
async function renderCases(params) {
  const main = enterPage('cases');
  const f = {
    q: params.get('q') || '', status: params.get('status') || '', assignee: params.get('assignee') || '',
    category: params.get('category') || '', period: params.get('period') || '',
    group: CASE_GROUPS.some(g => g[0] === params.get('group')) ? params.get('group') : 'month',
    contact: params.get('contact') || '',
  };
  const data = { rows: [], stats: null, next: 0, loading: false, error: '' };

  // Filters live in the URL so Back from a case returns to the same view.
  function syncUrl() {
    const p = new URLSearchParams();
    for (const [k, v] of Object.entries(f)) if (v && !(k === 'group' && v === 'month')) p.set(k, v);
    const qs = p.toString();
    history.replaceState(null, '', '#/cases' + (qs ? '?' + qs : ''));
  }

  async function load(more) {
    const seq = ++casesSeq;
    data.loading = true;
    data.error = '';
    if (!more) renderBody();
    const p = new URLSearchParams();
    for (const k of ['q', 'status', 'assignee', 'category', 'contact']) if (f[k]) p.set(k, f[k]);
    if (f.period) p.set('since', String(Date.now() - Number(f.period) * 86400e3));
    if (more && data.next) p.set('before', String(data.next));
    try {
      const r = await api('GET', '/api/cases?' + p);
      if (seq !== casesSeq) return; // a newer search started
      data.rows = more ? data.rows.concat(r.cases) : r.cases;
      data.stats = r.stats;
      data.next = r.next_before;
    } catch (ex) {
      if (seq !== casesSeq) return;
      data.error = ex.message;
    }
    data.loading = false;
    renderStats();
    renderBody();
  }

  const set = (k) => (e) => { f[k] = e.target.value; syncUrl(); load(false); };
  let searchTimer;
  const search = h('input', {
    class: 'input', type: 'search', placeholder: 'Search case number (HD-1035), subject, customer or any message…',
    'aria-label': 'Search cases', value: f.q,
    oninput: (e) => {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(() => { f.q = e.target.value.trim(); syncUrl(); load(false); }, 300);
    },
  });
  const select = (key, label, options) => h('select', { class: 'input', 'aria-label': label, onchange: set(key) },
    options.map(([v, t]) => h('option', { value: v, selected: v === f[key] }, t)));

  const statusTabs = h('div', { class: 'seg', role: 'group', 'aria-label': 'Status' },
    CASE_STATUSES.map(([v, t]) => h('button', {
      class: 'seg-btn' + (f.status === v ? ' on' : ''), 'aria-pressed': f.status === v ? 'true' : 'false',
      onclick: (e) => {
        f.status = v;
        statusTabs.querySelectorAll('.seg-btn').forEach(b => { b.classList.remove('on'); b.setAttribute('aria-pressed', 'false'); });
        e.currentTarget.classList.add('on');
        e.currentTarget.setAttribute('aria-pressed', 'true');
        syncUrl(); load(false);
      },
    }, t)));

  const cats = categories();
  const toolbar = h('div', { class: 'cases-toolbar' },
    h('div', { class: 'cases-search' }, search),
    statusTabs,
    h('div', { class: 'cases-filters' },
      select('assignee', 'Assignee', [['', 'Anyone'], ['me', 'Assigned to me'], ['none', 'Unassigned'],
        ...state.agents.map(a => [String(a.id), a.name])]),
      select('category', 'Category', [['', 'Any category'], ['none', 'No category'], ...cats.map(c => [c, c])]),
      select('period', 'Opened', CASE_PERIODS),
      h('label', { class: 'groupby' }, h('span', { class: 'small muted' }, 'Group by'),
        h('select', {
          class: 'input', 'aria-label': 'Group by',
          onchange: (e) => { f.group = e.target.value; syncUrl(); renderBody(); },
        }, CASE_GROUPS.map(([v, t]) => h('option', { value: v, selected: v === f.group }, t))))));

  const statsEl = h('div', { class: 'case-stats' });
  const bodyEl = h('div', { class: 'case-groups' });
  const scopeEl = h('div');

  function renderStats() {
    const s = data.stats;
    if (!s) { fill(statsEl); return; }
    const tile = (label, value, cls) => h('div', { class: 'stat' + (cls ? ' ' + cls : '') },
      h('div', { class: 'stat-v' }, value), h('div', { class: 'stat-l' }, label));
    fill(statsEl, 
      tile('Cases', s.total),
      tile('Open', s.open, 'open'),
      tile('Pending', s.pending, 'pending'),
      tile('Closed', s.closed),
      tile('Avg. time to close', s.closed ? duration(s.avg_close_ms) : '—'),
    );
    // Customer scope chip (from the details panel link).
    if (f.contact) {
      const who = data.rows[0]?.contact;
      scopeEl.replaceChildren(h('div', { class: 'scope' },
        'Showing cases from ', h('strong', {}, who ? contactName(who) : `customer #${f.contact}`),
        who?.email && who.name ? h('span', { class: 'muted' }, ` · ${who.email}`) : null,
        h('button', {
          class: 'btn ghost', 'aria-label': 'Show all customers',
          onclick: () => { f.contact = ''; syncUrl(); load(false); },
        }, '× All customers')));
    } else fill(scopeEl);
  }

  function groupRows() {
    const groups = new Map();
    const add = (key, label, sub, row) => {
      if (!groups.has(key)) groups.set(key, { label, sub, rows: [] });
      groups.get(key).rows.push(row);
    };
    for (const c of data.rows) {
      if (f.group === 'month') {
        const d = new Date(c.created_at);
        add(`${d.getFullYear()}-${d.getMonth()}`, d.toLocaleDateString(undefined, { month: 'long', year: 'numeric' }), '', c);
      } else if (f.group === 'customer') {
        add(c.contact_id, contactName(c.contact), c.contact.name ? c.contact.email : '', c);
      } else if (f.group === 'category') {
        add(c.category || '￿', c.category || 'No category', '', c);
      } else {
        add(statusKey(c), statusInfo(c)[1], '', c);
      }
    }
    let list = [...groups.values()];
    if (f.group === 'month') list.sort((a, b) => b.rows[0].created_at - a.rows[0].created_at);
    if (f.group === 'category') list.sort((a, b) => b.rows.length - a.rows.length || (a.label === 'No category') - (b.label === 'No category'));
    if (f.group === 'status') { const order = STATUS_OPTIONS.map(o => o[1]); list.sort((a, b) => order.indexOf(a.label) - order.indexOf(b.label)); }
    return list;
  }

  function caseRow(c) {
    const name = contactName(c.contact);
    const age = c.status === 'closed' && c.closed_at
      ? h('span', {}, `Closed in ${duration(c.closed_at - c.created_at)}`)
      : h('span', { class: c.status === 'open' ? 'age-open' : '' }, `Open ${duration(Date.now() - c.created_at)}`);
    return h('a', { class: 'case-row', href: `#/c/${c.id}` },
      h('span', { class: 'c-id' }, c.number),
      h('span', { class: 'c-subject' },
        h('span', { class: 'c-title' }, c.subject || '(no subject)'),
        h('span', { class: 'c-preview' }, c.preview && c.preview !== c.subject ? c.preview : '')),
      h('span', { class: 'c-customer' },
        avatar(name, c.contact_id, { sm: true, online: c.contact.online }),
        h('span', { class: 'c-name' }, name)),
      h('span', { class: 'c-cat' }, c.category ? h('span', { class: 'pill cat' }, c.category) : h('span', { class: 'faint' }, '—')),
      h('span', { class: 'c-status' }, statusPill(c)),
      h('span', { class: 'c-agent' }, c.assignee_id ? agentName(c.assignee_id) || 'Former agent' : h('span', { class: 'faint' }, 'Unassigned')),
      h('span', { class: 'c-opened' }, shortDate(c.created_at)),
      h('span', { class: 'c-age' }, age, h('span', { class: 'faint' }, ` · ${c.message_count} msg`)),
    );
  }

  function renderBody() {
    if (data.error) {
      fill(bodyEl, h('div', { class: 'empty' }, h('strong', {}, 'Couldn\'t load cases'), data.error));
      return;
    }
    if (data.loading && !data.rows.length) {
      fill(bodyEl, h('div', { class: 'empty' }, 'Loading…'));
      return;
    }
    if (!data.rows.length) {
      const filtered = f.q || f.status || f.assignee || f.category || f.period || f.contact;
      fill(bodyEl, h('div', { class: 'empty' },
        h('strong', {}, filtered ? 'No cases match' : 'No cases yet'),
        filtered ? 'Try a different search or clear some filters.' : 'Every chat from your website becomes a numbered case here.'));
      return;
    }
    const head = h('div', { class: 'case-row case-head', 'aria-hidden': 'true' },
      ...['Case', 'Subject', 'Customer', 'Category', 'Status', 'Agent', 'Opened', 'Time'].map(t => h('span', {}, t)));
    const groups = groupRows().map(g => h('section', { class: 'case-group' },
      h('h2', { class: 'group-h' }, g.label,
        g.sub ? h('span', { class: 'muted group-sub' }, g.sub) : null,
        h('span', { class: 'group-n' }, `${g.rows.length} ${g.rows.length === 1 ? 'case' : 'cases'}`)),
      h('div', { class: 'case-table' }, g.rows.map(caseRow))));
    const more = data.next
      ? h('div', { class: 'more' },
        h('span', { class: 'small muted' }, `Showing ${data.rows.length} of ${data.stats?.total ?? '?'}`),
        h('button', { class: 'btn', disabled: data.loading, onclick: () => load(true) }, data.loading ? 'Loading…' : 'Load more'))
      : h('div', { class: 'more small muted' }, `${data.rows.length} ${data.rows.length === 1 ? 'case' : 'cases'}`);
    fill(bodyEl, head, ...groups, more);
  }

  fill(main, 
    h('div', { class: 'cases-top' },
      h('div', { style: 'display:flex;align-items:center;gap:8px' }, menuButton(), h('h1', {}, 'All cases')),
      h('p', {}, 'Every support case, newest first. Click one to open it.')),
    scopeEl, statsEl, toolbar, bodyEl);
  syncUrl();
  await load(false);
}

function randomPassword() {
  const abc = 'abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789';
  const b = crypto.getRandomValues(new Uint8Array(14));
  return Array.from(b, x => abc[x % abc.length]).join('');
}

function teamCard(isAdmin) {
  const patch = async (a, body, done) => {
    try {
      await api('PATCH', `/api/agents/${a.id}`, body);
      if (done) done();
      renderSettings();
    } catch (ex) { toast(ex.message); }
  };
  const rows = state.agents.map(a => h('tr', { class: a.disabled ? 'disabled' : '' },
    h('td', {}, h('div', { style: 'display:flex;gap:10px;align-items:center' },
      avatar(a.name, a.id, { sm: true, online: a.online }),
      h('div', {}, h('div', { style: 'font-weight:600' }, a.name, a.id === state.me.id ? ' (you)' : ''), h('div', { class: 'small muted' }, a.email)))),
    h('td', {}, isAdmin
      ? h('select', { class: 'input', style: 'width:auto;height:30px', 'aria-label': `Role for ${a.name}`, onchange: (e) => patch(a, { role: e.target.value }) },
        h('option', { value: 'agent', selected: a.role === 'agent' }, 'Agent'),
        h('option', { value: 'admin', selected: a.role === 'admin' }, 'Admin'))
      : a.role),
    h('td', { class: 'small' }, a.disabled ? 'Disabled' : a.away ? h('span', { style: 'color:var(--warn, #b7791f)' }, 'Away')
      : a.online ? h('span', { style: 'color:var(--good)' }, 'Online') : h('span', { class: 'muted' }, 'Offline')),
    isAdmin ? h('td', { class: 'actions' },
      h('button', {
        class: 'btn ghost', onclick: () => {
          const pw = randomPassword();
          if (!confirm(`Reset ${a.name}'s password? They'll be signed out everywhere.`)) return;
          patch(a, { password: pw }, () => prompt(`New password for ${a.email} — copy it now:`, pw));
        },
      }, 'Reset password'),
      a.id !== state.me.id ? h('button', {
        class: 'btn ghost' + (a.disabled ? '' : ' danger'),
        onclick: () => patch(a, { disabled: !a.disabled }),
      }, a.disabled ? 'Enable' : 'Disable') : null,
    ) : null,
  ));

  let addForm = null;
  if (isAdmin) {
    const err = h('div', { class: 'error' });
    const name = h('input', { class: 'input', placeholder: 'Sam Lee' });
    const email = h('input', { class: 'input', type: 'email', placeholder: 'sam@company.com' });
    const role = h('select', { class: 'input' }, h('option', { value: 'agent' }, 'Agent'), h('option', { value: 'admin' }, 'Admin'));
    addForm = h('form', {
      style: 'margin-top:20px', onsubmit: async (e) => {
        e.preventDefault();
        err.textContent = '';
        const pw = randomPassword();
        try {
          await api('POST', '/api/agents', { name: name.value, email: email.value, role: role.value, password: pw });
          prompt(`Agent added. Send ${email.value} this temporary password (they can change it in Settings):`, pw);
          renderSettings();
        } catch (ex) { err.textContent = ex.message; }
      },
    },
      h('h2', { style: 'font-size:14px;margin:0 0 10px' }, 'Add a teammate'),
      h('div', { class: 'grid2' }, h('label', { class: 'field' }, 'Name', name), h('label', { class: 'field' }, 'Email', email)),
      h('div', { style: 'display:flex;gap:10px;align-items:end' },
        h('label', { class: 'field', style: 'flex:0 0 160px;margin:0' }, 'Role', role),
        h('button', { class: 'btn primary', type: 'submit', style: 'height:36px' }, 'Add agent')),
      err);
  }
  return h('section', { class: 'card' },
    h('h2', {}, 'Team'),
    h('p', {}, `${state.agents.filter(a => !a.disabled).length} active agents. Admins can change settings and manage the team.`),
    h('table', { class: 'team' },
      h('thead', {}, h('tr', {}, h('th', {}, 'Name'), h('th', {}, 'Role'), h('th', {}, 'Status'), isAdmin ? h('th', {}) : null)),
      h('tbody', {}, rows)),
    addForm);
}

function accountCard() {
  const err = h('div', { class: 'error' });
  const cur = h('input', { class: 'input', type: 'password', autocomplete: 'current-password' });
  const nw = h('input', { class: 'input', type: 'password', autocomplete: 'new-password', minlength: 10 });
  return h('section', { class: 'card' },
    h('h2', {}, 'Your account'),
    h('p', {}, `Signed in as ${state.me.email}.`),
    h('div', { class: 'grid2' },
      h('label', { class: 'field' }, 'Current password', cur),
      h('label', { class: 'field' }, 'New password', h('span', { class: 'hint' }, 'At least 10 characters.'), nw)),
    err,
    h('div', { style: 'display:flex;gap:8px' },
      h('button', {
        class: 'btn primary', onclick: async () => {
          err.textContent = '';
          try {
            await api('PUT', '/api/me/password', { current: cur.value, new: nw.value });
            cur.value = nw.value = '';
            toast('Password changed — other sessions signed out');
          } catch (ex) { err.textContent = ex.message; }
        },
      }, 'Change password'),
      h('button', {
        class: 'btn', onclick: async () => {
          await api('POST', '/api/auth/logout').catch(() => {});
          state.me = null;
          stopStream();
          renderLogin();
        },
      }, 'Sign out')));
}

function opsCard() {
  return h('section', { class: 'card' },
    h('h2', {}, 'Email and backups'),
    h('p', {}, state.emailEnabled
      ? 'Email is on: customers who leave an address get replies by email when they\'re off the site.'
      : 'Email is off. Set SMTP_HOST, SMTP_FROM and (usually) SMTP_USERNAME / SMTP_PASSWORD on the server to email replies to customers who have left.'),
    h('a', { class: 'btn', href: '/api/admin/backup' }, 'Download database backup'));
}

// ---------- activities ----------
// Each agent's plan for the day: to-dos, follow-ups and calls, optionally tied
// to a case. Dates are calendar days (YYYY-MM-DD) in the browser's time zone.

const SVG = (body, vb = 20) => `<svg viewBox="0 0 ${vb} ${vb}" aria-hidden="true">${body}</svg>`;
const KINDS = [
  ['task', 'To-do', SVG('<circle cx="10" cy="10" r="6.5"/><path d="M7.2 10.2l2 2 3.8-4.2"/>')],
  ['chat', 'Chat', SVG('<path d="M4 5h12v8.5H9.5L6 16v-2.5H4z"/>')],
  ['email', 'Email', SVG('<rect x="3" y="5" width="14" height="10" rx="1.5"/><path d="M3.6 6.2L10 11l6.4-4.8"/>')],
  ['call', 'Call', SVG('<path d="M6.8 3.5l2 3.4-1.6 1.6a8 8 0 004.3 4.3l1.6-1.6 3.4 2-.9 2.4C10 16.2 3.8 10 4.4 4.4z"/>')],
  ['meeting', 'Meeting', SVG('<rect x="3.5" y="4.5" width="13" height="12" rx="2"/><path d="M3.5 8.5h13M7 3v3M13 3v3"/>')],
];
const ICON_CHECK = SVG('<path d="M4 8.4l2.6 2.6L12 5.6"/>', 16);
const ICON_FLAG = SVG('<path d="M5 17V3.5M5 4h9l-2 3.5 2 3.5H5"/>');
const ICON_GRIP = SVG('<circle cx="7.5" cy="5" r="1.2"/><circle cx="12.5" cy="5" r="1.2"/><circle cx="7.5" cy="10" r="1.2"/><circle cx="12.5" cy="10" r="1.2"/><circle cx="7.5" cy="15" r="1.2"/><circle cx="12.5" cy="15" r="1.2"/>');
const ICON_CAL = KINDS[4][2];
const kindIcon = (k) => (KINDS.find(x => x[0] === k) || KINDS[0])[2];
const kindLabel = (k) => (KINDS.find(x => x[0] === k) || KINDS[0])[1];

const pad2 = (n) => String(n).padStart(2, '0');
const ymd = (d) => `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
const todayYmd = () => ymd(new Date());
const ymdDate = (s) => { const [y, m, d] = s.split('-').map(Number); return new Date(y, m - 1, d); };
const addDays = (s, n) => { const d = ymdDate(s); d.setDate(d.getDate() + n); return ymd(d); };
const dayDiff = (s) => Math.round((ymdDate(s) - ymdDate(todayYmd())) / 864e5);
const nowHM = () => { const d = new Date(); return `${pad2(d.getHours())}:${pad2(d.getMinutes())}`; };
function nextMonday() { const d = new Date(); d.setDate(d.getDate() + ((8 - d.getDay()) % 7 || 7)); return ymd(d); }
function fmtTime(hm) {
  const [hh, mm] = hm.split(':').map(Number);
  return new Date(2000, 0, 1, hh, mm).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
}
function dayName(s) {
  const n = dayDiff(s);
  if (n === 0) return 'Today';
  if (n === 1) return 'Tomorrow';
  if (n === -1) return 'Yesterday';
  return ymdDate(s).toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long' });
}
function dueLabel(t) {
  if (!t.due_date) return null;
  const n = dayDiff(t.due_date);
  let text = n === 0 ? 'Today' : n === 1 ? 'Tomorrow' : n === -1 ? 'Yesterday'
    : n > 1 && n < 7 ? ymdDate(t.due_date).toLocaleDateString(undefined, { weekday: 'short' })
      : n < -1 && n > -7 ? `${-n}d late`
        : ymdDate(t.due_date).toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
  if (t.due_time) text += ` · ${fmtTime(t.due_time)}`;
  const late = !t.done_at && (n < 0 || (n === 0 && !!t.due_time && t.due_time < nowHM()));
  return { text, late };
}
const isDone = (t) => !!t.done_at;
const sortTasks = (a, b) => (a.due_date || '9999').localeCompare(b.due_date || '9999') || a.position - b.position || a.id - b.id;
const midnight = () => { const d = new Date(); d.setHours(0, 0, 0, 0); return d.getTime(); };

// Sidebar badge: my open tasks due today or earlier.
function myDue() {
  const today = todayYmd();
  const due = state.myTasks.filter(t => !isDone(t) && t.due_date && t.due_date <= today);
  return { n: due.length, late: due.some(t => t.due_date < today) };
}
function activitiesNav(active) {
  const d = myDue();
  return h('a', { class: 'nav-item' + (active ? ' active' : ''), href: '#/activities' }, 'Activities',
    d.n ? h('span', {
      class: 'count' + (d.late ? ' hot late' : ''),
      title: d.late ? 'Some of your tasks are overdue' : `${d.n} due today`,
    }, d.n) : null);
}

async function loadMyTasks(render = true) {
  try {
    const r = await api('GET', `/api/tasks?assignee=me&done_since=${midnight()}`);
    state.myTasks = r.tasks;
    if (render) renderSidebar();
  } catch { /* the badge just stays as it was */ }
}

// ----- one place that keeps every copy of a task in step -----
const acts = { el: null, tab: 'today', who: 'me', active: new Map(), done: [], loaded: false, drafts: {}, drag: null, dirty: false, reorderedAt: 0 };
const actsActive = () => !!acts.el && acts.el.isConnected;
function whoMatches(t) {
  if (acts.who === 'all') return true;
  if (acts.who === 'none') return t.assignee_id == null;
  const id = acts.who === 'me' ? state.me.id : Number(acts.who);
  return t.assignee_id === id;
}
function upsert(list, t, keep) {
  const i = list.findIndex(x => x.id === t.id);
  if (!keep) { if (i >= 0) list.splice(i, 1); return; }
  if (i >= 0) list[i] = t; else list.push(t);
}
function applyTask(t) {
  upsert(state.myTasks, t, t.assignee_id === state.me.id && (!isDone(t) || t.done_at >= midnight()));
  if (state.conv) {
    const before = state.caseTasks.some(x => x.id === t.id);
    upsert(state.caseTasks, t, t.conversation_id === state.conv.id);
    if (before || t.conversation_id === state.conv.id) refreshCaseTasks();
  }
  if (actsActive()) {
    if (whoMatches(t) && (!isDone(t) || t.done_at >= midnight())) acts.active.set(t.id, t); else acts.active.delete(t.id);
    upsert(acts.done, t, whoMatches(t) && isDone(t));
    acts.done.sort((a, b) => b.done_at - a.done_at);
    renderActsBody();
  }
  renderSidebar();
}
function removeTask(id) {
  state.myTasks = state.myTasks.filter(t => t.id !== id);
  if (state.caseTasks.some(t => t.id === id)) {
    state.caseTasks = state.caseTasks.filter(t => t.id !== id);
    refreshCaseTasks();
  }
  if (actsActive()) {
    acts.active.delete(id);
    acts.done = acts.done.filter(t => t.id !== id);
    renderActsBody();
  }
  renderSidebar();
}
let reorderTimer;
function tasksReordered() {
  if (Date.now() - acts.reorderedAt < 2000) return; // our own drag, already applied
  clearTimeout(reorderTimer);
  reorderTimer = setTimeout(() => { loadMyTasks(); if (actsActive()) loadActs(); }, 300);
}
// Redraw the case panel if it's on screen, without touching the rest of it.
function refreshCaseTasks() {
  const box = document.querySelector('.case-tasks');
  if (box && state.conv) box.replaceWith(caseTasksPanel(state.conv).querySelector('.case-tasks'));
  const head = document.querySelector('.case-tasks-h');
  if (head) head.textContent = caseTasksTitle();
}

async function toggleDone(t) {
  const done = !isDone(t);
  const prev = { ...t };
  applyTask({ ...t, done_at: done ? Date.now() : null, done_by: done ? state.me.id : null });
  try {
    const r = await api('PATCH', `/api/tasks/${t.id}`, { done });
    applyTask(r.task);
  } catch (ex) { applyTask(prev); toast(ex.message); }
}
async function patchTask(t, body, msg) {
  try {
    const r = await api('PATCH', `/api/tasks/${t.id}`, body);
    applyTask(r.task);
    if (msg) toast(msg);
  } catch (ex) { toast(ex.message); }
}

// ----- a task row -----
function taskRow(t, opts = {}) {
  const done = isDone(t), due = dueLabel(t);
  const owner = t.assignee_id ? state.agents.find(a => a.id === t.assignee_id) : null;
  const sub = [];
  if (t.case && !opts.inCase) {
    sub.push(h('a', { href: `#/c/${t.case.id}`, class: 't-case' }, `Case #${t.case.id}`), ` · ${t.case.contact_name}`);
  } else sub.push(kindLabel(t.kind));
  if (opts.inCase) sub.push(owner ? ` · ${owner.id === state.me.id ? 'You' : owner.name}` : ' · Unassigned');
  if (t.notes) sub.push(h('span', { class: 't-note' }, ` · ${clipText(t.notes.split('\n')[0], 70)}`));
  if (done && t.done_by && t.done_by !== t.assignee_id) sub.push(` · done by ${agentName(t.done_by) || 'a teammate'}`);

  const today = todayYmd();
  const quick = [];
  if (!done && !opts.inCase && t.due_date && t.due_date < today) {
    quick.push(h('button', { class: 'btn ghost sm', onclick: () => patchTask(t, { due_date: today }, 'Moved to today') }, 'Do today'));
  }
  if (!done && !opts.inCase && t.due_date && t.due_date <= today) {
    quick.push(h('button', { class: 'btn ghost sm', onclick: () => patchTask(t, { due_date: addDays(today, 1) }, 'Moved to tomorrow') }, 'Tomorrow'));
  }
  return h('div', {
    class: 't-row' + (done ? ' done' : '') + (opts.inCase ? ' compact' : ''),
    'data-id': t.id, draggable: opts.drag ? 'true' : null,
  },
    opts.slot && !opts.drag ? h('span', { class: 't-handle', 'aria-hidden': 'true' }) : null,
    opts.drag ? h('button', {
      class: 't-handle', html: ICON_GRIP, title: 'Drag to reorder',
      'aria-label': `Reorder “${t.title}”: arrow keys move it up or down`,
      onkeydown: (e) => keyMove(e, t),
    }) : null,
    h('button', {
      class: 't-check', role: 'checkbox', 'aria-checked': String(done), html: ICON_CHECK,
      'aria-label': `${t.title}: ${done ? 'done, mark not done' : 'mark done'}`,
      onclick: () => toggleDone(t),
    }),
    opts.inCase ? null : h('span', { class: 't-kind', html: kindIcon(t.kind), title: kindLabel(t.kind) }),
    h('div', { class: 't-main' },
      h('button', { class: 't-title', onclick: () => openTaskDialog(t), title: 'Open task' }, t.title),
      h('span', { class: 't-sub' }, ...sub)),
    quick.length ? h('span', { class: 't-quick' }, ...quick) : null,
    t.high && !done ? h('span', { class: 't-flag', html: ICON_FLAG, title: 'High priority', 'aria-label': 'High priority', role: 'img' }) : null,
    opts.showOwner ? (owner ? avatar(owner.name, owner.id, { sm: true }) : h('span', { class: 't-unowned', title: 'Unassigned' }, '—')) : null,
    due ? h('span', { class: 'pill t-due' + (due.late ? ' late' : '') }, due.text) : null,
  );
}

// ----- Activities page -----
const ACT_TABS = [['today', 'Today'], ['upcoming', 'Upcoming'], ['someday', 'No date'], ['done', 'Done']];

async function renderActivities(params) {
  const main = enterPage('cases activities');
  acts.el = main;
  acts.tab = ACT_TABS.some(t => t[0] === params.get('tab')) ? params.get('tab') : 'today';
  acts.who = params.get('who') || 'me';
  acts.active = new Map();
  acts.done = [];
  acts.loaded = false;
  acts.drafts = {};
  acts.tabsEl = h('div', { class: 'seg', role: 'tablist', 'aria-label': 'Which tasks' });
  acts.bodyEl = h('div', { class: 'acts-body' });
  const who = h('select', {
    class: 'input', 'aria-label': 'Whose tasks',
    onchange: (e) => { acts.who = e.target.value; acts.drafts = {}; syncActsUrl(); loadActs(); },
  },
    h('option', { value: 'me', selected: acts.who === 'me' }, 'My tasks'),
    h('option', { value: 'all', selected: acts.who === 'all' }, 'Whole team'),
    h('option', { value: 'none', selected: acts.who === 'none' }, 'Unassigned'),
    state.agents.filter(a => a.id !== state.me.id && !a.disabled).map(a =>
      h('option', { value: String(a.id), selected: acts.who === String(a.id) }, a.name)));
  fill(main, 
    h('div', { class: 'cases-top' },
      h('div', { style: 'display:flex;align-items:center;gap:8px' }, menuButton(), h('h1', {}, 'Activities')),
      h('p', {}, 'Plan your day: follow-ups, call-backs and to-dos, in the order you want to tackle them.')),
    h('div', { class: 'cases-toolbar' }, acts.tabsEl, h('div', { class: 'acts-who' }, who)),
    acts.bodyEl);
  renderActsTabs();
  renderActsBody();
  await loadActs();
}

function syncActsUrl() {
  const p = new URLSearchParams();
  if (acts.tab !== 'today') p.set('tab', acts.tab);
  if (acts.who !== 'me') p.set('who', acts.who);
  const qs = p.toString();
  history.replaceState(null, '', '#/activities' + (qs ? '?' + qs : ''));
}

let actsSeq = 0;
async function loadActs() {
  const seq = ++actsSeq;
  const whoQ = acts.who === 'me' ? 'me' : acts.who;
  try {
    const [act, done] = await Promise.all([
      api('GET', `/api/tasks?assignee=${encodeURIComponent(whoQ)}&done_since=${midnight()}`),
      acts.tab === 'done' ? api('GET', `/api/tasks?assignee=${encodeURIComponent(whoQ)}&scope=done`) : null,
    ]);
    if (seq !== actsSeq || !actsActive()) return;
    acts.active = new Map(act.tasks.map(t => [t.id, t]));
    if (done) acts.done = done.tasks;
    acts.loaded = true;
    acts.error = '';
  } catch (ex) {
    if (seq !== actsSeq) return;
    acts.error = ex.message;
  }
  renderActsBody();
}

function actsCounts() {
  const today = todayYmd();
  const c = { today: 0, upcoming: 0, someday: 0 };
  for (const t of acts.active.values()) {
    if (isDone(t)) continue;
    if (!t.due_date) c.someday++;
    else if (t.due_date <= today) c.today++;
    else c.upcoming++;
  }
  return c;
}

function renderActsTabs() {
  const c = actsCounts();
  fill(acts.tabsEl, ...ACT_TABS.map(([k, label]) => h('button', {
    class: 'seg-btn' + (acts.tab === k ? ' on' : ''), role: 'tab', 'aria-selected': String(acts.tab === k),
    onclick: () => {
      if (acts.tab === k) return;
      const needDone = k === 'done';
      acts.tab = k;
      syncActsUrl();
      renderActsTabs();
      renderActsBody();
      if (needDone) loadActs();
    },
  }, label, c[k] ? h('span', { class: 'seg-n' }, c[k]) : null)));
}

// The cards for the current tab: [{ key, title, date (for new tasks & drops), tasks, ... }]
function actsCards() {
  const today = todayYmd();
  const all = [...acts.active.values()].sort(sortTasks);
  if (acts.tab === 'today') {
    const late = all.filter(t => !isDone(t) && t.due_date && t.due_date < today);
    const now = all.filter(t => t.due_date === today);
    const cards = [];
    if (late.length) cards.push({ key: 'late', title: 'Overdue', tone: 'late', tasks: late, drag: true });
    cards.push({ key: 'today', title: 'Today', date: today, tasks: now, drag: true, add: true, progress: true });
    return cards;
  }
  if (acts.tab === 'upcoming') {
    const tomorrow = addDays(today, 1);
    const days = new Map([[tomorrow, []]]);
    for (const t of all) {
      if (isDone(t) || !t.due_date || t.due_date <= today) continue;
      if (!days.has(t.due_date)) days.set(t.due_date, []);
      days.get(t.due_date).push(t);
    }
    return [...days.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([d, tasks]) => ({
      key: 'd' + d, title: dayName(d), date: d, tasks, drag: true, add: true, pickDate: d === tomorrow,
    }));
  }
  if (acts.tab === 'someday') {
    return [{ key: 'someday', title: 'No date', date: null, tasks: all.filter(t => !isDone(t) && !t.due_date), drag: true, add: true }];
  }
  // done: grouped by the day it was finished
  const groups = new Map();
  for (const t of acts.done) {
    const d = ymd(new Date(t.done_at));
    if (!groups.has(d)) groups.set(d, []);
    groups.get(d).push(t);
  }
  return [...groups.entries()].map(([d, tasks]) => ({ key: 'done' + d, title: dayName(d), tasks }));
}

function renderActsBody() {
  if (!actsActive()) return;
  if (acts.drag) { acts.dirty = true; return; } // don't yank rows out from under a drag
  renderActsTabs();
  // Keep focus and typing in an add box across redraws.
  const ae = document.activeElement;
  const keep = ae?.dataset?.addKey ? { key: ae.dataset.addKey, field: ae.dataset.field, s: ae.selectionStart, e: ae.selectionEnd } : null;

  if (acts.error && !acts.loaded) {
    fill(acts.bodyEl, h('div', { class: 'empty' }, h('strong', {}, 'Couldn\'t load tasks'), acts.error));
    return;
  }
  if (!acts.loaded) {
    fill(acts.bodyEl, h('div', { class: 'empty' }, 'Loading…'));
    return;
  }
  const cards = actsCards();
  const showOwner = acts.who === 'all';
  if (acts.tab === 'done' && !cards.length) {
    fill(acts.bodyEl, h('div', { class: 'empty' }, h('strong', {}, 'Nothing finished yet'), 'Ticked-off tasks show up here.'));
    return;
  }
  fill(acts.bodyEl, ...cards.map(card => {
    const done = card.tasks.filter(isDone).length;
    const n = card.tasks.length;
    const rows = h('div', { class: 't-list', 'data-card': card.key },
      card.tasks.map(t => taskRow(t, { drag: card.drag && !isDone(t), slot: card.drag, showOwner })));
    if (card.drag) wireDrop(rows, card);
    let empty = null;
    if (!n && card.key === 'today') {
      empty = h('div', { class: 't-empty' }, acts.who === 'me'
        ? 'Nothing planned for today yet. Add what you\'re going to tackle below — or pull tasks in from Upcoming.'
        : 'Nothing planned for today.');
    } else if (!n && card.add) {
      empty = h('div', { class: 't-empty' }, card.key === 'someday' ? 'Ideas and "when I get a minute" jobs go here.' : 'Nothing planned.');
    }
    return h('section', { class: 't-card' + (card.tone ? ' ' + card.tone : '') },
      h('header', { class: 't-card-h' },
        h('span', { class: 't-card-ico', html: card.tone === 'late' ? ICON_FLAG : ICON_CAL }),
        h('h2', {}, card.title),
        h('span', { class: 't-card-n' }, card.progress && done ? `${n} ${n === 1 ? 'task' : 'tasks'} · ${done} done` : `${n} ${n === 1 ? 'task' : 'tasks'}`)),
      card.progress && n ? h('div', { class: 't-progress', role: 'progressbar', 'aria-valuemin': '0', 'aria-valuemax': String(n), 'aria-valuenow': String(done), 'aria-label': 'Done today' },
        h('span', { style: `width:${Math.round(done / n * 100)}%` })) : null,
      rows, empty,
      card.add ? addRow(card) : null);
  }));
  if (keep) {
    const el = acts.bodyEl.querySelector(`[data-add-key="${CSS.escape(keep.key)}"][data-field="${keep.field}"]`);
    if (el) {
      el.focus();
      if (keep.s != null && el.setSelectionRange) try { el.setSelectionRange(keep.s, keep.e); } catch { }
    }
  }
}

function defaultOwner() {
  if (acts.who === 'none') return null;
  if (acts.who === 'me' || acts.who === 'all') return state.me.id;
  return Number(acts.who);
}

function ownerSelect(value, onchange, label = 'Owner') {
  return h('select', { class: 'input', 'aria-label': label, onchange },
    h('option', { value: String(state.me.id), selected: value === state.me.id }, `${state.me.name} (me)`),
    state.agents.filter(a => a.id !== state.me.id && (!a.disabled || a.id === value)).map(a =>
      h('option', { value: String(a.id), selected: value === a.id }, a.name)),
    h('option', { value: '', selected: value == null }, 'Unassigned'));
}

function kindPicker(value, onpick, name) {
  return h('div', { class: 'kind-pick', role: 'radiogroup', 'aria-label': 'Type' },
    KINDS.map(([k, label, icon]) => h('button', {
      type: 'button', class: 'kind-btn' + (k === value ? ' on' : ''), role: 'radio', 'aria-checked': String(k === value),
      title: label, 'aria-label': label, 'data-add-key': name, 'data-field': 'kind-' + k,
      html: icon,
      onclick: (e) => {
        onpick(k);
        e.currentTarget.parentElement.querySelectorAll('.kind-btn').forEach(b => {
          const on = b === e.currentTarget;
          b.classList.toggle('on', on);
          b.setAttribute('aria-checked', String(on));
        });
      },
    })));
}

// The dashed "Add a task…" row at the bottom of a card.
function addRow(card) {
  const d = acts.drafts[card.key] ||= { title: '', kind: 'task', high: false, time: '', date: card.date || '', owner: defaultOwner() };
  const key = card.key;
  const submit = async () => {
    const title = d.title.trim();
    if (!title) return;
    try {
      const r = await api('POST', '/api/tasks', {
        title, kind: d.kind, high: d.high,
        due_date: d.date || null, due_time: d.date && d.time ? d.time : null, assignee_id: d.owner,
      });
      acts.drafts[key] = { ...d, title: '', high: false, time: '' };
      applyTask(r.task);
      if (!whoMatches(r.task)) toast(`Added to ${r.task.assignee_id ? agentName(r.task.assignee_id) + '\'s list' : 'Unassigned'}`);
      else if (r.task.due_date !== (card.date || null)) toast(`Added for ${r.task.due_date ? dayName(r.task.due_date) : 'no date'}`);
    } catch (ex) { toast(ex.message); }
  };
  const input = h('input', {
    class: 't-add-input', placeholder: 'Add a task…', value: d.title, maxlength: 200,
    'aria-label': `Add a task to ${card.title}`, 'data-add-key': key, 'data-field': 'title',
    oninput: (e) => { d.title = e.target.value; },
    onkeydown: (e) => {
      if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); submit(); }
      if (e.key === 'Escape') { d.title = ''; e.target.value = ''; e.target.blur(); }
    },
  });
  const high = h('button', {
    type: 'button', class: 'btn ghost sm t-high' + (d.high ? ' on' : ''), 'aria-pressed': String(d.high),
    'data-add-key': key, 'data-field': 'high', html: ICON_FLAG + '<span>High</span>', title: 'High priority',
    onclick: (e) => { d.high = !d.high; e.currentTarget.classList.toggle('on', d.high); e.currentTarget.setAttribute('aria-pressed', String(d.high)); input.focus(); },
  });
  const date = h('input', {
    type: 'date', class: 'input', value: d.date, 'aria-label': 'Due date', 'data-add-key': key, 'data-field': 'date',
    onchange: (e) => { d.date = e.target.value; },
  });
  const time = h('input', {
    type: 'time', class: 'input', value: d.time, 'aria-label': 'Time (optional)', 'data-add-key': key, 'data-field': 'time',
    onchange: (e) => { d.time = e.target.value; },
  });
  const owner = ownerSelect(d.owner, (e) => { d.owner = e.target.value ? Number(e.target.value) : null; });
  owner.dataset.addKey = key;
  owner.dataset.field = 'owner';
  return h('div', { class: 't-add' + (d.title ? ' open' : '') },
    h('div', { class: 't-add-line' }, h('span', { class: 't-check ghost', 'aria-hidden': 'true' }), input,
      h('button', { type: 'button', class: 'btn primary sm t-add-btn', onclick: submit, 'data-add-key': key, 'data-field': 'add' }, 'Add')),
    h('div', { class: 't-add-opts' },
      kindPicker(d.kind, (k) => { d.kind = k; input.focus(); }, key),
      high,
      h('label', { class: 't-opt' }, h('span', {}, 'Due'), date),
      h('label', { class: 't-opt' }, h('span', {}, 'At'), time),
      h('label', { class: 't-opt' }, h('span', {}, 'For'), owner)));
}

// ----- reordering: drag and drop, or arrow keys on the handle -----
function cardTasks(key) {
  return actsCards().find(c => c.key === key);
}
async function saveOrder(ids) {
  acts.reorderedAt = Date.now();
  ids.forEach((id, i) => {
    for (const list of [acts.active, null]) {
      const t = list ? list.get(id) : state.myTasks.find(x => x.id === id);
      if (t) t.position = i + 1;
    }
  });
  try { await api('POST', '/api/tasks/reorder', { ids }); }
  catch (ex) { toast(ex.message); loadActs(); }
}
function keyMove(e, t) {
  if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return;
  e.preventDefault();
  const list = e.currentTarget.closest('.t-list');
  const card = cardTasks(list.dataset.card);
  const ids = card.tasks.map(x => x.id);
  const i = ids.indexOf(t.id), j = i + (e.key === 'ArrowUp' ? -1 : 1);
  if (j < 0 || j >= ids.length) return;
  [ids[i], ids[j]] = [ids[j], ids[i]];
  saveOrder(ids);
  renderActsBody();
  acts.bodyEl.querySelector(`.t-row[data-id="${t.id}"] .t-handle`)?.focus();
}
function wireDrop(listEl, card) {
  listEl.addEventListener('dragstart', (e) => {
    const row = e.target.closest?.('.t-row');
    if (!row) return;
    acts.drag = { id: Number(row.dataset.id), from: card.key };
    row.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', row.dataset.id);
  });
  listEl.addEventListener('dragend', () => {
    document.querySelectorAll('.t-row.dragging, .drop-before, .drop-after').forEach(el => el.classList.remove('dragging', 'drop-before', 'drop-after'));
    acts.drag = null;
    if (acts.dirty) { acts.dirty = false; renderActsBody(); }
  });
  const target = (e) => {
    const rows = [...listEl.querySelectorAll('.t-row:not(.dragging)')];
    for (const r of rows) {
      const box = r.getBoundingClientRect();
      if (e.clientY < box.top + box.height / 2) return { row: r, before: true };
    }
    return { row: rows[rows.length - 1] || null, before: false };
  };
  listEl.addEventListener('dragover', (e) => {
    if (!acts.drag || card.date === undefined && acts.drag.from !== card.key) return;
    e.preventDefault();
    listEl.querySelectorAll('.drop-before, .drop-after').forEach(el => el.classList.remove('drop-before', 'drop-after'));
    const { row, before } = target(e);
    if (row) row.classList.add(before ? 'drop-before' : 'drop-after');
    else listEl.classList.add('drop-after');
  });
  listEl.addEventListener('dragleave', (e) => {
    if (!listEl.contains(e.relatedTarget)) listEl.querySelectorAll('.drop-before, .drop-after').forEach(el => el.classList.remove('drop-before', 'drop-after'));
    listEl.classList.remove('drop-after');
  });
  listEl.addEventListener('drop', async (e) => {
    if (!acts.drag) return;
    e.preventDefault();
    listEl.classList.remove('drop-after');
    const { row, before } = target(e);
    const id = acts.drag.id, from = acts.drag.from;
    const ids = card.tasks.map(t => t.id).filter(x => x !== id);
    let at = row ? ids.indexOf(Number(row.dataset.id)) + (before ? 0 : 1) : ids.length;
    if (at < 0) at = ids.length;
    ids.splice(at, 0, id);
    acts.drag = null;
    acts.dirty = false;
    const t = acts.active.get(id);
    if (from !== card.key && t && card.date !== undefined && t.due_date !== card.date) {
      // Dropped on another day: reschedule it, then put it where it was dropped.
      applyTask({ ...t, due_date: card.date, due_time: card.date ? t.due_time : null });
      await patchTask(t, { due_date: card.date }, card.date ? `Moved to ${dayName(card.date).toLowerCase()}` : 'Date removed');
    }
    saveOrder(ids);
    renderActsBody();
  });
}

// ----- add / edit dialog -----
function followupDefaults(conv) {
  return {
    title: `Follow up with ${contactName(conv.contact)}`,
    kind: conv.contact.email ? 'email' : 'chat',
    due_date: addDays(todayYmd(), 1),
    conversation_id: conv.id,
    case: { id: conv.id, subject: conv.subject, contact_name: contactName(conv.contact) },
  };
}

function openTaskDialog(task, defaults = {}) {
  const isNew = !task;
  const t = task ? { ...task } : {
    title: '', notes: '', kind: 'task', high: false, due_date: todayYmd(), due_time: null,
    assignee_id: state.me.id, conversation_id: null, case: null, ...defaults,
  };
  const err = h('div', { class: 'error', role: 'alert' });
  const title = h('input', { class: 'input', value: t.title, maxlength: 200, required: true, 'aria-label': 'Task' });
  const notes = h('textarea', { class: 'input', rows: 3, maxlength: 5000, placeholder: 'Details, phone number, what was promised…', 'aria-label': 'Notes' });
  notes.value = t.notes || '';
  let kind = t.kind;
  const date = h('input', { type: 'date', class: 'input', value: t.due_date || '', 'aria-label': 'Due date' });
  const time = h('input', { type: 'time', class: 'input', value: t.due_time || '', 'aria-label': 'Time (optional)' });
  const high = h('input', { type: 'checkbox', checked: t.high });
  const owner = ownerSelect(t.assignee_id ?? null, () => {});
  const caseNo = h('input', { class: 'input', type: 'number', min: 1, value: t.conversation_id || '', placeholder: '—', 'aria-label': 'Case number', style: 'max-width:110px' });
  const chip = (label, val) => h('button', { type: 'button', class: 'btn ghost sm', onclick: () => { date.value = val; if (!val) time.value = ''; } }, label);
  const today = todayYmd();
  const dlg = h('dialog', { class: 't-dialog', 'aria-label': isNew ? 'New task' : 'Edit task' });
  const close = () => dlg.close();
  const canDelete = !isNew && (t.created_by === state.me.id || t.assignee_id === state.me.id || state.me.role === 'admin');
  const save = h('button', { class: 'btn primary', type: 'submit' }, isNew ? 'Add task' : 'Save');
  const form = h('form', {
    method: 'dialog',
    onsubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      const body = {
        title: title.value, notes: notes.value, kind, high: high.checked,
        due_date: date.value || null, due_time: date.value && time.value ? time.value : null,
        assignee_id: owner.value ? Number(owner.value) : null,
        conversation_id: caseNo.value ? Number(caseNo.value) : null,
      };
      save.disabled = true;
      try {
        const r = isNew ? await api('POST', '/api/tasks', body) : await api('PATCH', `/api/tasks/${t.id}`, body);
        applyTask(r.task);
        close();
        toast(isNew ? (r.task.assignee_id === state.me.id ? 'Added to your Activities' : `Added for ${r.task.assignee_id ? agentName(r.task.assignee_id) : 'the team'}`) : 'Saved');
      } catch (ex) { err.textContent = ex.message; }
      finally { save.disabled = false; }
    },
  },
    h('div', { class: 't-dialog-h' }, h('h2', {}, isNew ? (t.conversation_id ? 'New follow-up' : 'New task') : 'Task'),
      h('button', { type: 'button', class: 'btn ghost sm', 'aria-label': 'Close', onclick: close }, '✕')),
    h('label', { class: 'field' }, 'What needs doing', title),
    h('div', { class: 'field' }, h('span', {}, 'Type'), kindPicker(kind, (k) => { kind = k; }, 'dlg')),
    h('div', { class: 'field' }, h('span', {}, 'When'),
      h('div', { class: 't-when' }, date, time),
      h('div', { class: 't-chips' }, chip('Today', today), chip('Tomorrow', addDays(today, 1)), chip('Next Monday', nextMonday()), chip('No date', ''))),
    h('div', { class: 'grid2' },
      h('label', { class: 'field' }, 'Owner', owner),
      h('label', { class: 'field' }, 'Case',
        h('div', { style: 'display:flex;gap:8px;align-items:center' }, h('span', { class: 'muted' }, '#'), caseNo,
          t.case ? h('a', { href: `#/c/${t.case.id}`, class: 'small', onclick: close, style: 'min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap' }, clipText(t.case.subject || t.case.contact_name, 40)) : null))),
    h('label', { class: 'check' }, high, h('span', {}, h('strong', {}, 'High priority'), h('span', { class: 'hint muted' }, ' — flagged in red'))),
    h('label', { class: 'field' }, 'Notes', notes),
    !isNew && t.done_at ? h('p', { class: 'small muted' }, `Done ${relTime(t.done_at) === 'now' ? 'just now' : relTime(t.done_at) + ' ago'}${t.done_by ? ' by ' + (t.done_by === state.me.id ? 'you' : agentName(t.done_by) || 'a teammate') : ''}.`) : null,
    err,
    h('div', { class: 't-dialog-f' },
      canDelete ? h('button', {
        type: 'button', class: 'btn ghost danger', onclick: async () => {
          if (!confirm('Delete this task?')) return;
          try { await api('DELETE', `/api/tasks/${t.id}`); removeTask(t.id); close(); toast('Deleted'); }
          catch (ex) { err.textContent = ex.message; }
        },
      }, 'Delete') : null,
      !isNew ? h('button', {
        type: 'button', class: 'btn', onclick: () => { toggleDone(task); close(); },
      }, t.done_at ? 'Mark not done' : 'Mark done') : null,
      h('span', { style: 'flex:1' }),
      h('button', { type: 'button', class: 'btn', onclick: close }, 'Cancel'),
      save));
  dlg.append(form);
  dlg.addEventListener('close', () => dlg.remove());
  dlg.addEventListener('click', (e) => { if (e.target === dlg) close(); }); // click on the backdrop
  document.body.append(dlg);
  dlg.showModal();
  if (isNew && !t.title) title.focus(); else title.select();
}

// ----- the case's own follow-ups, in the details panel -----
function caseTasksTitle() {
  const open = state.caseTasks.filter(t => !isDone(t)).length;
  return open ? `Activities (${open} open)` : 'Activities';
}
function caseTasksPanel(conv) {
  const list = h('div', { class: 'case-tasks' },
    state.caseTasks.length
      ? state.caseTasks.slice().sort((a, b) => isDone(a) - isDone(b) || sortTasks(a, b)).slice(0, 8).map(t => taskRow(t, { inCase: true }))
      : h('p', { class: 'small faint', style: 'margin:0 0 6px' }, 'No follow-ups planned for this case.'),
    h('button', { class: 'btn sm', onclick: () => openTaskDialog(null, followupDefaults(conv)) }, '+ Add follow-up'));
  return h('div', {}, h('h3', { class: 'case-tasks-h' }, caseTasksTitle()), list);
}

// Roll the badge and lists over at midnight.
let lastDay = todayYmd();
setInterval(() => {
  if (!state.me) return;
  const d = todayYmd();
  if (d === lastDay) { if (actsActive() && acts.tab === 'today') renderActsBody(); return; } // refresh "late" times
  lastDay = d;
  loadMyTasks();
  if (actsActive()) loadActs();
}, 60e3);

// ---------- boot ----------
async function boot() {
  try {
    const me = await api('GET', '/api/me');
    state.me = me.agent;
    state.emailEnabled = me.email_enabled;
    state.ai = me.ai;
    state.baseUrl = me.base_url || location.origin;
    state.ntfyEnabled = !!me.ntfy_enabled;
    state.notifyEmail = me.notify_email || '';
  } catch {
    if (!state.me) renderLogin();
    return;
  }
  const [a, s] = await Promise.all([api('GET', '/api/agents'), api('GET', '/api/settings')]);
  state.agents = a.agents;
  state.settings = s.settings;
  await loadMyTasks(false);
  renderShell();
  startStream();
  await route();
}

window.addEventListener('hashchange', route);
boot();
