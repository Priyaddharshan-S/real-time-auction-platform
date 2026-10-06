
'use strict';
const $ = s => document.querySelector(s);
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const when = x => x ? new Date(x).toLocaleString() : '';
const S = { cfg: null, sess: JSON.parse(localStorage.getItem('sess') || 'null'), csrf: null, tab: 'dash', aid: null, edit: null, timer: null, ws: null, deb: null, nav: 0 };
const stale = id => id !== S.nav; // a slow response from a tab we already left must never paint over the current tab
const TABS = [['dash', 'Dashboard'], ['auctions', 'Auctions'], ['form', 'Create / Edit'], ['monitor', 'Live Monitor'], ['users', 'Users'], ['results', 'Results'], ['audit', 'Audit log']];

function toast(msg, kind = 'err') { const t = document.createElement('div'); t.className = 'toast ' + kind; t.textContent = msg; $('#toasts').appendChild(t); setTimeout(() => t.remove(), 4000); }

// ---- auth + API (token refresh, CSRF header on every state-changing call) ----
const setCookie = t => { document.cookie = `sb_access=${t}; Path=/admin; SameSite=Strict; Max-Age=3600${location.protocol === 'https:' ? '; Secure' : ''}`; };
async function fresh() {
  if (S.sess.expires_at - Date.now() / 1000 > 60) return;
  const r = await fetch(S.cfg.supabase_url + '/auth/v1/token?grant_type=refresh_token', { method: 'POST',
    headers: { apikey: S.cfg.supabase_anon_key, 'Content-Type': 'application/json' }, body: JSON.stringify({ refresh_token: S.sess.refresh_token }) });
  const j = await r.json();
  if (!r.ok) { location.href = '/#/login'; throw { message: 'Session expired' }; }
  S.sess = { access_token: j.access_token, refresh_token: j.refresh_token, expires_at: j.expires_at };
  localStorage.setItem('sess', JSON.stringify(S.sess)); setCookie(j.access_token);
}
async function api(path, o = {}) {
  await fresh();
  const m = o.method || 'GET', h = { Authorization: 'Bearer ' + S.sess.access_token };
  if (o.body) h['Content-Type'] = 'application/json';
  if (m !== 'GET') { if (!S.csrf) S.csrf = (await api('/api/admin/csrf')).csrf_token; h['X-CSRF-Token'] = S.csrf; }
  const r = await fetch(path, { method: m, headers: h, body: o.body && JSON.stringify(o.body) });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) {
    if (j.error?.code === 'csrf_invalid' && !o.retried) { S.csrf = null; return api(path, { ...o, retried: true }); }
    throw j.error || { message: 'Request failed' };
  }
  return j;
}
const table = (head, rows) => `<div class="wrap"><table><tr>${head.map(h => `<th>${h}</th>`).join('')}</tr>${rows.join('') || `<tr><td colspan="${head.length}" class="muted">Nothing here yet.</td></tr>`}</table></div>`;
const btn = (act, id, label, cls = '') => `<button class="btn sm ${cls}" data-act="${act}" data-id="${esc(id)}">${label}</button>`;

// ---- tabs ----
function go(tab) {
  S.nav++;
  clearInterval(S.timer); if (S.ws && tab !== 'monitor') { S.ws.close(); S.ws = null; }
  S.tab = tab;
  $('#tabs').innerHTML = TABS.map(([k, v]) => `<button class="btn sm ${k === tab ? 'on' : ''}" data-tab="${k}">${v}</button>`).join('');
  VIEWS[tab]().catch(e => toast(e.message || 'Error'));
}

async function dash(quiet) {
  const id = S.nav, d = await api('/api/admin/dashboard');
  if (stale(id)) return;
  const max = Math.max(1, ...d.bids_per_minute_15m.map(p => p.bids));
  const st = (n, v) => `<div class="stat"><b>${v}</b><span class="muted">${n}</span></div>`;
  $('#view').innerHTML = `<div class="stats">${st('Active auctions', d.active_auctions)}${st('Scheduled', d.scheduled_auctions)}${st('Bids / minute', d.bids_last_minute)}
    ${st('Connected clients', d.connected_clients)}${st('Users', d.users)}${st('Banned', d.banned_users)}
    ${st('Database', `<span class="${d.database === 'ok' ? 'st-ok' : 'st-bad'}">${d.database}</span>`)}${st('Redis', `<span class="${d.redis === 'ok' ? 'st-ok' : 'st-bad'}">${d.redis}</span>`)}</div>
    <h3>Bids per minute (last 15 min)</h3><div class="bar">${d.bids_per_minute_15m.map(p => `<i style="height:${p.bids / max * 100}%" title="${p.bids}"></i>`).join('') || '<span class="muted">No bids yet</span>'}</div>
    <p class="muted">Connected clients counts this server instance. Refreshes every 5s.</p>`;
  S.timer = setInterval(() => S.tab === 'dash' && dash(true).catch(() => {}), 5000);
}

async function auctions() {
  const id = S.nav, j = await api('/api/admin/auctions');
  if (stale(id)) return;
  $('#view').innerHTML = `<div style="display:flex;justify-content:space-between"><h2>Auctions</h2><button class="btn pri" data-tab="form" data-new="1">+ New</button></div>` +
    table(['Title', 'Status', 'Price', 'Bids', 'Ends', 'Actions'], j.auctions.map(a => `<tr><td>${esc(a.title)}</td><td><span class="pill">${a.status}</span></td><td>${a.current_price}</td><td>${a.bid_count}</td><td>${when(a.ends_at)}</td><td>
      ${btn('monitor', a.id, 'Monitor')}${btn('edit', a.id, 'Edit')}
      ${a.status === 'active' || a.status === 'scheduled' ? btn('extend', a.id, '+30 min') + btn('close', a.id, 'Close now', 'danger') + btn('cancel', a.id, 'Cancel', 'danger') : ''}</td></tr>`));
}

const loc = iso => { const d = new Date(iso); d.setMinutes(d.getMinutes() - d.getTimezoneOffset()); return d.toISOString().slice(0, 16); };
async function form() {
  const id = S.nav; let a = null; if (S.edit) a = (await api('/api/auctions/' + S.edit)).auction;
  if (stale(id)) return;
  const locked = a && a.bid_count > 0, dis = locked ? 'disabled' : '', v = (x, d = '') => esc(a ? x : d);
  $('#view').innerHTML = `<div class="form" style="max-width:560px;margin:0"><h2>${a ? 'Edit auction' : 'Create auction'}</h2>
    ${locked ? '<p class="st-bad">This auction has bids: only title and description can be changed.</p>' : ''}
    <label>Title</label><input id="f_t" value="${v(a?.title)}">
    <label>Description</label><input id="f_d" value="${v(a?.description)}">
    <label>Image URL <span id="f_hint">(a direct image link: right-click an image, then "Copy image address")</span></label><input id="f_i" value="${v(a?.image_url)}" ${dis}>
    <img id="f_prev" alt="" referrerpolicy="no-referrer" hidden style="width:100%;max-height:180px;object-fit:cover;border-radius:8px;margin-top:6px">
    <label>Start price</label><input id="f_sp" type="number" min="0" value="${a ? a.start_price : 10}" ${dis}>
    <label>Min increment (cannot exceed the start price)</label><input id="f_mi" type="number" min="1" value="${a ? a.min_increment : 1}" ${dis}>
    <label>Starts (empty = now)</label><input id="f_s" type="datetime-local" value="${a ? loc(a.starts_at) : ''}" ${dis}>
    <label>Ends</label><input id="f_e" type="datetime-local" value="${loc(a ? a.ends_at : Date.now() + 864e5)}" ${dis}>
    <div class="row" style="margin-top:10px"><button class="btn pri" data-act="save">${a ? 'Save changes' : 'Create'}</button>${a ? '<button class="btn" data-act="newform">Cancel edit</button>' : ''}</div></div>`;
  prev();
}
const HINT = '(a direct image link: right-click an image, then "Copy image address")';
function prev() { // live preview so a bad link is obvious before saving
  const u = ($('#f_i')?.value || '').trim(), p = $('#f_prev'); if (!p) return;
  p.hidden = !u; $('#f_hint').textContent = HINT; $('#f_hint').style.color = ''; if (u) p.src = u;
}
document.addEventListener('error', e => {
  if (e.target.id === 'f_prev') { e.target.hidden = true; const h = $('#f_hint'); h.textContent = "(this link doesn't load as an image. Use the direct image address, not a web page link)"; h.style.color = 'var(--bad)'; }
}, true);
async function save() {
  const v = id => $('#' + id).value, locked = $('#f_sp').disabled;
  const body = { title: v('f_t'), description: v('f_d') };
  if (!locked) {
    Object.assign(body, { image_url: v('f_i'), start_price: +v('f_sp'), min_increment: +v('f_mi'), ends_at: new Date(v('f_e')).toISOString() });
    if (v('f_s')) body.starts_at = new Date(v('f_s')).toISOString();
  }
  await api(S.edit ? '/api/admin/auctions/' + S.edit : '/api/admin/auctions', { method: S.edit ? 'PATCH' : 'POST', body });
  toast(S.edit ? 'Saved' : 'Auction created', 'ok'); S.edit = null; go('auctions');
}

async function monitor() {
  const id = S.nav, l = await api('/api/admin/auctions');
  if (stale(id)) return;
  if (!S.aid && l.auctions[0]) S.aid = l.auctions[0].id;
  $('#view').innerHTML = `<h2>Live bid monitor</h2><select id="msel">${l.auctions.map(a => `<option value="${a.id}" ${a.id === S.aid ? 'selected' : ''}>${esc(a.title)} (${a.status})</option>`).join('')}</select><div id="mon"></div>`;
  if (!S.aid) return;
  await loadMon();
  if (stale(id)) return;
  const p = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = S.ws = new WebSocket(`${p}://${location.host}/ws?token=${encodeURIComponent(S.sess.access_token)}`);
  ws.onopen = () => ws.send(JSON.stringify({ action: 'subscribe', auction_id: S.aid }));
  ws.onmessage = m => { try { if (JSON.parse(m.data).auction_id === S.aid) { clearTimeout(S.deb); S.deb = setTimeout(() => loadMon().catch(() => {}), 150); } } catch {} };
}
async function loadMon() {
  const id = S.nav, j = await api(`/api/admin/auctions/${S.aid}/bids`);
  if (stale(id)) return;
  const a = j.auction, el = $('#mon'); if (!el) return;
  el.innerHTML = `<p><b>${a.current_price}</b> · leader: ${esc(a.leader_name || 'none')} · ${a.bid_count} bids · ends ${when(a.ends_at)} · <span class="pill">${a.status}</span></p>` +
    table(['Time', 'Bidder', 'Email', 'Amount', ''], j.bids.map(b => `<tr style="${b.voided ? 'opacity:.5;text-decoration:line-through' : ''}"><td>${when(b.created_at)}</td><td>${esc(b.display_name)}</td><td>${esc(b.email)}</td><td>${b.amount}</td><td>${b.voided ? 'voided' : btn('void', b.id, 'Void', 'danger')}</td></tr>`));
}

async function users(q = '') {
  const id = S.nav;
  const [u, c] = await Promise.all([api('/api/admin/users?q=' + encodeURIComponent(q)), api('/api/admin/clients')]);
  if (stale(id)) return;
  const keep = $('#uq') ? $('#uq').value : q;
  $('#view').innerHTML = `<h2>Users</h2><input id="uq" placeholder="Search email or name" value="${esc(keep)}">` +
    table(['Name', 'Email', 'Role', 'Status', 'Actions'], u.users.map(x => `<tr><td>${esc(x.display_name)}</td><td>${esc(x.email)}</td><td>${x.role}</td><td>${x.banned ? '<span class="st-bad">banned</span>' : 'ok'}</td><td>
      ${x.banned ? btn('unban', x.id, 'Unban') : btn('ban', x.id, 'Ban', 'danger')}${x.role === 'admin' ? btn('demote', x.id, 'Demote', 'danger') : btn('promote', x.id, 'Make admin')}</td></tr>`)) +
    `<h3>Connected WebSocket clients <span class="muted">(this instance)</span></h3>` +
    table(['User', 'Connected', 'Watching', ''], c.clients.map(x => `<tr><td>${esc(x.display_name)} (${esc(x.email)})</td><td>${when(x.connected_at)}</td><td>${x.auctions.length} auction(s)</td><td>${btn('kick', x.id, 'Kick', 'danger')}</td></tr>`));
  if (q || keep) { const i = $('#uq'); i.focus(); i.setSelectionRange(99, 99); }
}

async function results() {
  const id = S.nav, j = await api('/api/admin/results');
  if (stale(id)) return;
  $('#view').innerHTML = `<div style="display:flex;justify-content:space-between"><h2>Results</h2><button class="btn pri" data-act="csv">Download CSV</button></div>` +
    table(['Auction', 'Final price', 'Bids', 'Closed', 'Winner'], j.results.map(r => `<tr><td>${esc(r.title)}</td><td>${r.final_price}</td><td>${r.bid_count}</td><td>${when(r.closed_at)}</td><td>${r.winner_name ? esc(r.winner_name) + ' (' + esc(r.winner_email) + ')' : '—'}</td></tr>`));
}
async function csv() {
  await fresh();
  const r = await fetch('/api/admin/results.csv', { headers: { Authorization: 'Bearer ' + S.sess.access_token } });
  const a = document.createElement('a'); a.href = URL.createObjectURL(await r.blob()); a.download = 'auction-results.csv'; a.click(); URL.revokeObjectURL(a.href);
}
const AUDIT = { create_auction: 'Created auction', edit_auction: 'Edited auction', close_auction: 'Closed auction early', cancel_auction: 'Cancelled auction',
  extend_auction: 'Extended auction', void_bid: 'Voided bid', ban_user: 'Banned user', unban_user: 'Unbanned user',
  promote_user: 'Made admin', demote_user: 'Removed admin rights', kick_client: 'Disconnected a client' };
const fmtVal = v => typeof v === 'string' && /^\d{4}-\d{2}-\d{2}T/.test(v) ? when(v) : (v ?? '—');
function describe(x) { // turn the stored details into a plain sentence
  const d = x.details || {};
  switch (x.action) {
    case 'create_auction': return `"${d.title}" · start price ${d.start_price} · ends ${when(d.ends_at)}`;
    case 'edit_auction': return Object.entries(d.changes || {}).map(([k, v]) => k === 'description' ? 'description updated' : `${k.replaceAll('_', ' ')}: ${fmtVal(v.from)} → ${fmtVal(v.to)}`).join(' · ') || 'No changes';
    case 'close_auction': return `Final price ${d.final_price}${d.winner_id ? '' : ' · no bids'}`;
    case 'cancel_auction': return `${d.bid_count} bid(s) discarded`;
    case 'extend_auction': return d.minutes ? `Added ${d.minutes} minutes` : `New end time ${when(d.ends_at)}`;
    case 'void_bid': return `Bid of ${d.amount} removed · price is now ${d.new_price}`;
    case 'ban_user': case 'unban_user': case 'promote_user': case 'demote_user': return d.email || '';
    case 'kick_client': return 'Connection closed';
    default: return Object.entries(d).map(([k, v]) => `${k}: ${fmtVal(v)}`).join(' · ');
  }
}
async function auditLog() {
  const id = S.nav, j = await api('/api/admin/audit?limit=200');
  if (stale(id)) return;
  $('#view').innerHTML = '<h2>Audit log</h2>' + table(['Time', 'Admin', 'Action', 'Auction', 'Details'], j.actions.map(x =>
    `<tr><td>${when(x.created_at)}</td><td>${esc(x.admin_email)}</td><td>${esc(AUDIT[x.action] || x.action)}</td><td>${esc(x.auction_title || '—')}</td><td style="white-space:normal;min-width:240px">${esc(describe(x))}</td></tr>`));
}
const VIEWS = { dash, auctions, form, monitor, users: () => users(), results, audit: auditLog };

// ---- destructive actions always ask first ----
const post = (url, body) => api(url, { method: 'POST', body });
const ACTS = {
  monitor: id => { S.aid = id; go('monitor'); },
  edit: id => { S.edit = id; go('form'); },
  newform: () => { S.edit = null; go('form'); },
  save, csv,
  extend: async id => { await post(`/api/admin/auctions/${id}/extend`, { minutes: 30 }); toast('Extended by 30 minutes', 'ok'); go(S.tab); },
  close: async id => { if (confirm('Close this auction now? The current leader wins.')) { await post(`/api/admin/auctions/${id}/close`); toast('Auction closed', 'ok'); go(S.tab); } },
  cancel: async id => { if (confirm('Cancel this auction? There will be no winner.')) { await post(`/api/admin/auctions/${id}/cancel`); toast('Auction cancelled', 'ok'); go(S.tab); } },
  void: async id => { if (confirm('Void this bid? Price and leader are recalculated.')) { await post(`/api/admin/bids/${id}/void`); toast('Bid voided', 'ok'); loadMon(); } },
  ban: async id => { if (confirm('Ban this user? They are disconnected immediately.')) { await post(`/api/admin/users/${id}/ban`); toast('User banned', 'ok'); go('users'); } },
  unban: async id => { await post(`/api/admin/users/${id}/unban`); toast('User unbanned', 'ok'); go('users'); },
  promote: async id => { if (confirm('Give this user admin rights?')) { await post(`/api/admin/users/${id}/promote`); toast('Promoted', 'ok'); go('users'); } },
  demote: async id => { if (confirm('Remove admin rights from this user?')) { await post(`/api/admin/users/${id}/demote`); toast('Demoted', 'ok'); go('users'); } },
  kick: async id => { if (confirm('Disconnect this client?')) { await post(`/api/admin/clients/${id}/kick`); toast('Kick sent', 'ok'); setTimeout(() => go('users'), 600); } },
};
document.addEventListener('click', e => {
  const t = e.target.closest('[data-tab]'); if (t) { if (t.dataset.new) S.edit = null; return go(t.dataset.tab); }
  const a = e.target.closest('[data-act]'); if (a) Promise.resolve(ACTS[a.dataset.act]?.(a.dataset.id)).catch(err => toast(err.message || 'Error'));
});
document.addEventListener('input', e => { if (e.target.id === 'f_i') prev(); });
document.addEventListener('change', e => { if (e.target.id === 'msel') { S.aid = e.target.value; go('monitor'); } });
let uq; document.addEventListener('input', e => { if (e.target.id === 'uq') { clearTimeout(uq); uq = setTimeout(() => users(e.target.value).catch(() => {}), 300); } });

(async function boot() {
  if (!S.sess) { location.href = '/#/login'; return; }
  for (;;) { try { const r = await fetch('/api/config'); if (!r.ok) throw 0; S.cfg = await r.json(); break; } catch { await new Promise(r => setTimeout(r, 3000)); } }
  try { const me = await api('/api/me'); if (me.role !== 'admin') { location.href = '/'; return; } } catch { location.href = '/#/login'; return; }
  go('dash');
})();
