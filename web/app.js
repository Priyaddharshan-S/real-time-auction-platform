'use strict';
const CUR = '$'; // currency symbol shown next to prices
const S = { cfg: null, sess: JSON.parse(localStorage.getItem('sess') || 'null'), me: null, off: 0,
  ws: null, open: false, retry: 0, cur: null, d: null, busy: false, poll: null, lastRef: 0 };
const $ = s => document.querySelector(s);
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const money = n => CUR + Number(n).toLocaleString();
const sleep = ms => new Promise(r => setTimeout(r, ms));
const PLACEHOLDER = 'data:image/svg+xml;utf8,' + encodeURIComponent('<svg xmlns="http://www.w3.org/2000/svg" width="600" height="400"><rect width="100%" height="100%" fill="#d9dde5"/><text x="50%" y="50%" fill="#8a93a6" font-family="sans-serif" font-size="28" text-anchor="middle">No image</text></svg>');
const imgOk = u => /^https?:\/\//i.test(u || '') ? esc(u) : esc(PLACEHOLDER);
// A broken image link must never leave an empty box: swap in the placeholder once.
document.addEventListener('error', e => { const i = e.target; if (i.tagName === 'IMG' && !i.dataset.fb) { i.dataset.fb = 1; i.src = PLACEHOLDER; } }, true);

function toast(msg, kind = 'err') {
  const t = document.createElement('div'); t.className = 'toast ' + kind; t.textContent = msg;
  $('#toasts').appendChild(t); setTimeout(() => t.remove(), 4000);
}
// One friendly message per server error code.
const MSG = {
  bid_too_low: e => `Bid too low. Minimum is ${money(e.details?.min_next_bid ?? 0)}.`,
  auction_closed: () => 'This auction is not accepting bids.',
  rate_limited: () => 'Too many bids. Wait a few seconds.',
  banned: () => 'Your account has been banned.',
  unauthorized: () => 'Please sign in again.',
  forbidden: () => 'You are not allowed to do that.',
  not_found: () => 'Auction not found.',
  admin_cannot_bid: () => 'Admins cannot place bids. Use a member account.',
};
const errText = e => (MSG[e.code] ? MSG[e.code](e) : e.message) || 'Something went wrong.';

// ---------- Supabase Auth (plain REST, no library) ----------
const AUTHERR = {
  over_email_send_rate_limit: 'Too many sign-up emails were sent. Please wait a while and try again.',
  user_already_exists: 'An account with this email already exists. Try logging in.',
  invalid_credentials: 'Wrong email or password.',
  email_not_confirmed: 'Please confirm your email first (check your inbox and spam).',
  weak_password: 'Password is too weak. Use at least 6 characters.',
};
async function authCall(path, body) {
  const r = await fetch(S.cfg.supabase_url + path, { method: 'POST',
    headers: { apikey: S.cfg.supabase_anon_key, 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw { message: AUTHERR[j.error_code] || j.msg || j.error_description || j.message || 'Authentication failed.' };
  return j;
}
// Cookie used ONLY to let the server guard the /admin page (Path=/admin: never sent to the API).
const setCookie = (t, age = 3600) => { document.cookie = `sb_access=${t}; Path=/admin; SameSite=Strict; Max-Age=${age}${location.protocol === 'https:' ? '; Secure' : ''}`; };
function save(j) {
  S.sess = { access_token: j.access_token, refresh_token: j.refresh_token,
    expires_at: j.expires_at || Math.floor(Date.now() / 1000) + j.expires_in };
  localStorage.setItem('sess', JSON.stringify(S.sess)); setCookie(S.sess.access_token);
}
async function fresh() {
  if (S.sess && S.sess.expires_at - Date.now() / 1000 < 60) {
    try { save(await authCall('/auth/v1/token?grant_type=refresh_token', { refresh_token: S.sess.refresh_token })); }
    catch { logout(); }
  }
}
function logout() {
  localStorage.removeItem('sess'); setCookie('', 0); S.sess = null; S.me = null;
  if (S.ws) S.ws.close(1000); nav(); route();
}

// ---------- API ----------
async function api(path, { method = 'GET', body } = {}) {
  await fresh();
  const h = {}; if (body) h['Content-Type'] = 'application/json';
  if (S.sess) h.Authorization = 'Bearer ' + S.sess.access_token;
  let r;
  try { r = await fetch(path, { method, headers: h, body: body && JSON.stringify(body) }); }
  catch { throw { code: 'network', message: 'Network problem. Check your connection.' }; }
  const j = await r.json().catch(() => ({}));
  if (!r.ok) {
    const e = j.error || {};
    if (r.status === 401 && S.sess) logout();
    throw { status: r.status, code: e.code, message: e.message, details: e.details };
  }
  if (j.server_time) S.off = Date.parse(j.server_time) - Date.now(); // clock offset for countdowns
  return j;
}

// ---------- WebSocket with auto-reconnect ----------
async function connectWS() {
  if (!S.sess || S.ws) return;
  await fresh(); if (!S.sess) return;
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(`${proto}://${location.host}/ws?token=${encodeURIComponent(S.sess.access_token)}`);
  S.ws = ws;
  ws.onopen = () => { S.open = true; S.retry = 0; banner(null); if (S.cur) { wsSend({ action: 'subscribe', auction_id: S.cur }); loadDetail(S.cur).catch(() => {}); } syncBtn(); };
  ws.onmessage = m => { let ev; try { ev = JSON.parse(m.data); } catch { return; }
    if (ev.type === 'bid_voided' && ev.auction_id === S.cur) loadDetail(S.cur).catch(() => {});
    else if (ev.auction && ev.auction_id === S.cur) apply(ev.auction, ev.bid); };
  ws.onclose = e => {
    S.open = false; S.ws = null; syncBtn();
    if (e.code === 1008 || !S.sess) { if (e.code === 1008) toast('You were disconnected by an admin.'); return; } // 1008 = kicked: do not reconnect
    banner('Reconnecting…');
    setTimeout(connectWS, Math.min(15000, 500 * 2 ** S.retry++) + Math.random() * 300); // exponential backoff
  };
}
const wsSend = o => S.ws && S.open && S.ws.send(JSON.stringify(o));
function banner(t) { const b = $('#banner'); b.hidden = !t; if (t) b.textContent = t; }

// ---------- Views ----------
function nav() {
  $('#nav').innerHTML = S.me
    ? `${S.me.role === 'admin' ? '<a class="btn" href="/admin">Admin</a>' : ''}<a class="muted" href="#/profile" title="Edit your profile">${esc(S.me.display_name)}</a><button class="btn" data-act="logout">Log out</button>`
    : '<a class="btn" href="#/login">Log in</a><a class="btn pri" href="#/signup">Sign up</a>';
}
function route() {
  clearInterval(S.poll);
  if (S.cur && S.open) wsSend({ action: 'unsubscribe', auction_id: S.cur });
  S.cur = null; S.d = null;
  const m = location.hash.match(/^#\/a\/([0-9a-f-]{36})$/i);
  if (m) return showDetail(m[1]);
  if (location.hash === '#/login') return authForm('login');
  if (location.hash === '#/signup') return authForm('signup');
  if (location.hash === '#/profile') return showProfile();
  showList();
}
const card = a => `<a class="card" href="#/a/${esc(a.id)}"><img src="${imgOk(a.image_url)}" alt="" referrerpolicy="no-referrer" loading="lazy">
  <div class="cb"><h3>${esc(a.title)}</h3><div class="price">${money(a.current_price)}</div>
  <div class="muted">${a.leader_name ? 'Leading: ' + esc(a.leader_name) : 'No bids yet'}${a.status === 'scheduled' ? ' · Upcoming' : ''}</div>
  <div class="cd" data-end="${esc(a.ends_at)}"></div></div></a>`;

async function showList() {
  const needName = S.me && S.me.display_name === S.me.email.split('@')[0]; // still the auto-generated name
  $('#app').innerHTML = `<h2>Auctions</h2>${needName ? '<div class="notice"><span>Pick a display name, so other bidders see a proper name instead of your email prefix.</span><a class="btn pri" href="#/profile">Set name</a></div>' : ''}<div id="list" class="grid">Loading…</div>`;
  const load = async () => {
    try { const j = await api('/api/auctions'); const el = $('#list');
      if (el) el.innerHTML = j.auctions.length ? j.auctions.map(card).join('') : '<p class="muted">No active auctions right now.</p>';
    } catch (e) { toast(errText(e)); }
  };
  await load(); S.poll = setInterval(load, 8000);
}

function authForm(mode) {
  const su = mode === 'signup';
  $('#app').innerHTML = `<div class="form"><h2>${su ? 'Create your account' : 'Welcome back'}</h2>
    ${su ? '<label for="dn">Display name</label><input id="dn" maxlength="30" placeholder="How other bidders will see you" autocomplete="nickname">' : ''}
    <label for="em">Email</label><input id="em" type="email" placeholder="you@example.com" autocomplete="email">
    <label for="pw">Password</label><input id="pw" type="password" placeholder="${su ? 'At least 6 characters' : 'Your password'}" autocomplete="${su ? 'new-password' : 'current-password'}">
    <button class="btn pri" style="width:100%;margin-top:12px" data-act="${su ? 'signup' : 'login'}">${su ? 'Sign up' : 'Log in'}</button>
    <p class="muted center">${su ? 'Already have an account? <a href="#/login">Log in</a>' : 'Don\'t have an account? <a href="#/signup">Sign up</a>'}</p></div>`;
  $(su ? '#dn' : '#em').focus();
}
async function doAuth(signup) {
  const email = $('#em').value.trim(), password = $('#pw').value, name = signup ? $('#dn').value.trim() : '';
  if (signup && (name.length < 2 || name.length > 30)) return toast('Choose a display name (2-30 characters).');
  if (!email || password.length < 6) return toast('Enter your email and a password of at least 6 characters.');
  const b = $('.form .pri'); b.disabled = true;
  try {
    const j = signup ? await authCall('/auth/v1/signup', { email, password, data: { display_name: name } })
                     : await authCall('/auth/v1/token?grant_type=password', { email, password });
    if (!j.access_token) { toast('Account created. Confirm your email, then log in.', 'ok'); location.hash = '#/login'; return; }
    save(j); S.me = await api('/api/me'); nav(); connectWS(); location.hash = '#/';
  } catch (e) { toast(e.message || errText(e)); b.disabled = false; }
}
function showProfile() {
  if (!S.me) { location.hash = '#/login'; return; }
  $('#app').innerHTML = `<div class="form"><h2>Your profile</h2><p class="muted">${esc(S.me.email)}</p>
    <label for="dn">Display name</label><input id="dn" maxlength="30" value="${esc(S.me.display_name)}">
    <button class="btn pri" style="width:100%;margin-top:12px" data-act="saveprofile">Save</button></div>`;
}
async function saveProfile() {
  const name = $('#dn').value.trim();
  if (name.length < 2) return toast('Display name must be 2-30 characters.');
  try { S.me = await api('/api/me', { method: 'PATCH', body: { display_name: name } }); nav(); toast('Display name updated', 'ok'); location.hash = '#/'; }
  catch (e) { toast(errText(e)); }
}

async function loadDetail(id) {
  const j = await api('/api/auctions/' + id);
  if (id !== S.cur) return;
  S.d = { a: j.auction, bids: j.bids }; paint();
}
async function showDetail(id) {
  S.cur = id;
  $('#app').innerHTML = `<a href="#/" class="back">← All auctions</a><div class="detail">
    <img id="img" alt="" referrerpolicy="no-referrer"><h2 id="title"></h2><p id="desc" class="muted"></p>
    <div id="price" class="big"></div><div id="leader" class="muted"></div><div id="cd" class="cd big2"></div>
    <div id="bidbox"></div><h3>Bid history</h3><ul id="hist"></ul></div>`;
  try { await loadDetail(id); } catch (e) { toast(errText(e)); return; }
  if (S.open) wsSend({ action: 'subscribe', auction_id: id });
  // Without a live socket (guests, or while reconnecting) fall back to polling.
  S.poll = setInterval(() => { if (!S.open) loadDetail(id).catch(() => {}); }, 3000);
}

function paint() {
  const { a, bids } = S.d; if (!$('#price')) return;
  $('#img').src = a.image_url && /^https?:\/\//i.test(a.image_url) ? a.image_url : PLACEHOLDER;
  $('#title').textContent = a.title; $('#desc').textContent = a.description;
  $('#price').textContent = money(a.current_price);
  $('#leader').textContent = a.status === 'closed' ? (a.leader_name ? 'Won by ' + a.leader_name : 'Closed with no bids')
    : a.status === 'cancelled' ? 'Auction cancelled'
    : a.leader_id && S.me && a.leader_id === S.me.id ? "You're leading!" : a.leader_name ? 'Leading: ' + a.leader_name : 'No bids yet';
  $('#cd').dataset.end = a.ends_at;
  $('#hist').innerHTML = bids.map(b => `<li><span>${esc(b.display_name)}</span><b>${money(b.amount)}</b></li>`).join('') || '<li class="muted">No bids yet</li>';
  const box = $('#bidbox');
  if (!S.me) box.innerHTML = '<div class="notice"><span>Sign in to place a bid.</span><a class="btn pri" href="#/login">Log in / Sign up</a></div>';
  else if (S.me.role === 'admin') box.innerHTML = '<div class="notice">Admins cannot bid. Use a member account to take part.</div>';
  else if (!$('#amt')) box.innerHTML = `<div class="quick" id="quick"></div>
    <input id="amt" type="number" inputmode="numeric" min="1" placeholder="Custom amount"><button id="placeBtn" data-act="bid">Place Bid</button>`;
  const amt = $('#amt'); if (amt) amt.placeholder = `Min ${money(a.current_price + a.min_increment)}`;
  const q = $('#quick'); if (q) q.innerHTML = [1, 2, 5].map(k => `<button data-act="quick" data-n="${k * a.min_increment}">+${k * a.min_increment}</button>`).join('');
  syncBtn();
}
function apply(a, bid) {
  if (!S.d || a.id !== S.cur) return;
  const old = S.d.a.current_price; S.d.a = a;
  if (bid && !S.d.bids.some(b => b.id === bid.id)) { S.d.bids.unshift(bid); S.d.bids.length = Math.min(S.d.bids.length, 20); }
  paint();
  if (a.current_price !== old) { const p = $('#price'); p.classList.remove('win', 'lose'); void p.offsetWidth;
    p.classList.add(S.me && a.leader_id === S.me.id ? 'win' : 'lose'); } // green = you lead, red = outbid
}
const remaining = () => S.d ? Date.parse(S.d.a.ends_at) - (Date.now() + S.off) : 0;
function syncBtn() {
  const b = $('#placeBtn'); if (!b) return;
  const ended = !S.d || S.d.a.status !== 'active' || remaining() <= 0;
  b.disabled = S.busy || !S.open || ended; // never bid while in flight or disconnected
  b.textContent = ended ? 'Auction ended' : S.busy ? 'Placing…' : !S.open ? 'Connecting…' : 'Place Bid';
  document.querySelectorAll('[data-act=quick]').forEach(q => q.disabled = ended);
}
async function placeBid() {
  const amount = parseInt($('#amt').value, 10);
  if (!amount || amount < 1) return toast('Enter a bid amount.');
  if (S.busy) return; S.busy = true; syncBtn();
  try {
    // Only a server-confirmed response updates the UI as "success".
    const r = await api(`/api/auctions/${S.cur}/bids`, { method: 'POST', body: { amount, idempotency_key: crypto.randomUUID() } });
    apply(r.auction, r.bid); $('#amt').value = ''; toast('Bid placed: ' + money(amount), 'ok');
  } catch (e) {
    toast(errText(e));
    if (e.code !== 'unauthorized') loadDetail(S.cur).catch(() => {}); // resync after any failure
  } finally { S.busy = false; syncBtn(); }
}

const fmt = ms => { const s = Math.max(0, Math.floor(ms / 1000)), d = Math.floor(s / 86400);
  const t = [Math.floor(s % 86400 / 3600), Math.floor(s % 3600 / 60), s % 60].map(x => String(x).padStart(2, '0')).join(':');
  return (d ? d + 'd ' : '') + t; };
setInterval(() => {
  const now = Date.now() + S.off;
  document.querySelectorAll('[data-end]').forEach(el => {
    const ms = Date.parse(el.dataset.end) - now;
    el.textContent = ms <= 0 ? 'Ended' : 'Ends in ' + fmt(ms); el.classList.toggle('hot', ms > 0 && ms < 30000);
  });
  syncBtn();
  if (S.d && S.d.a.status === 'active' && remaining() <= 0 && Date.now() - S.lastRef > 3000) { // time is up: ask the server for the result
    S.lastRef = Date.now(); loadDetail(S.cur).catch(() => {});
  }
}, 500);

const actions = {
  logout, login: () => doAuth(false), signup: () => doAuth(true), saveprofile: saveProfile, bid: placeBid,
  quick: el => { const a = S.d.a; $('#amt').value = a.current_price + Math.max(+el.dataset.n, a.min_increment); },
};
document.addEventListener('click', e => { const el = e.target.closest('[data-act]'); if (el) actions[el.dataset.act]?.(el); });
window.addEventListener('hashchange', route);

(async function boot() {
  for (;;) { // the free tier can take ~50s to wake up: keep retrying quietly
    try { const r = await fetch('/api/config'); if (!r.ok) throw 0; S.cfg = await r.json(); break; } catch { await sleep(3000); }
  }
  banner(null);
  if (S.sess) { try { S.me = await api('/api/me'); setCookie(S.sess.access_token); connectWS(); } catch (e) { if (e.code === 'network') banner('Connecting…'); } }
  if (S.me && ['#/login', '#/signup'].includes(location.hash)) history.replaceState(null, '', '#/');
  nav(); route();
})();

// Enter submits the form on the login / sign-up / profile pages.
document.addEventListener('keydown', e => { if (e.key === 'Enter' && e.target.closest('.form')) { e.preventDefault(); $('.form .pri')?.click(); } });
