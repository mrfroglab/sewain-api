// Shared helpers for the Sewain v2 k6 scenarios (see loadtest/README.md).
//
// Traffic goes to the local Caddy (BASE_URL, default http://127.0.0.1) with the staging Host header, never through
// Cloudflare. Caddy runs on gateway-net, which the API trusts (TRUSTED_PROXY_CIDRS), so CF-Connecting-IP is taken
// as the client IP: each VU gets its own address from 100.64.0.0/10 unless a scenario pins one (campus NAT).
import http from 'k6/http';
import { Counter, Trend } from 'k6/metrics';
import exec from 'k6/execution';

export const BASE = __ENV.BASE_URL || 'http://127.0.0.1';
export const HOST = __ENV.HOST_HEADER || 'sewain-staging.hallojanu.me';
export const ORIGIN = __ENV.ORIGIN || `https://${HOST}`;

// Business-rule rejections (HTTP 200 + ok:false, e.g. stock/date clash) are expected outcomes, not errors.
export const ruleRejected = new Counter('rule_rejected');
export const rateLimited = new Counter('rate_limited');
export const snapshotBytes = new Trend('snapshot_bytes');

// randomIP returns an address in the shared CGNAT range 100.64.0.0/10 (never routed on the internet).
export function randomIP() {
  const n = Math.floor(Math.random() * (1 << 22));
  return `100.${64 + (n >> 16)}.${(n >> 8) & 255}.${(n & 255) || 1}`;
}

// ipForVU keeps one address per VU for the whole run, like a real phone on mobile data.
const vuIPs = {};
export function ipForVU(fixed) {
  if (fixed) return fixed;
  const k = `${__VU}`;
  if (!vuIPs[k]) vuIPs[k] = randomIP();
  return vuIPs[k];
}

export function headers(ip, token, extra) {
  const h = { Host: HOST, 'CF-Connecting-IP': ip, Origin: ORIGIN };
  if (token) h.Authorization = `Bearer ${token}`;
  return Object.assign(h, extra || {});
}

// track records 429s (with the profile seen in X-RateLimit-Limit) for every response.
// A 401/403 on a pool session means the stack was reset or redeployed under us (resetDemo drops the pool
// accounts): stop instead of measuring fast rejections.
let authFailures = 0;
export function track(res) {
  if ((res.status === 401 || res.status === 403) && res.request && res.request.headers && res.request.headers.Authorization) {
    authFailures++;
    if (authFailures >= 5) exec.test.abort(`pool session rejected (HTTP ${res.status}): stack was reset/redeployed, run prepare again`);
  }
  if (res.status === 429) rateLimited.add(1, { name: (res.request && res.request.tags && res.request.tags.name) || '' });
  return res;
}

export function get(path, ip, token, name, extra) {
  return track(http.get(`${BASE}${path}`, { headers: headers(ip, token, extra), tags: { name } }));
}

export function postJSON(path, body, ip, token, name, extra) {
  const h = headers(ip, token, Object.assign({ 'Content-Type': 'application/json' }, extra || {}));
  return track(http.post(`${BASE}${path}`, JSON.stringify(body), { headers: h, tags: { name } }));
}

export function uuid() {
  const hex = '0123456789abcdef';
  let s = '';
  for (let i = 0; i < 32; i++) s += hex[Math.floor(Math.random() * 16)];
  return `${s.slice(0, 8)}-${s.slice(8, 12)}-4${s.slice(13, 16)}-a${s.slice(17, 20)}-${s.slice(20)}`;
}

// action calls POST /api/v1/actions/{name} with an Idempotency-Key. It returns {res, body}; body.ok === false is a
// rule rejection (counted, not an error).
export function action(name, args, ip, token, opts) {
  const o = opts || {};
  const payload = { args };
  if (o.id) payload.id = o.id;
  const res = postJSON(`/api/v1/actions/${name}`, payload, ip, token, `action:${name}`, { 'Idempotency-Key': o.key || uuid() });
  let body = null;
  try { body = res.json(); } catch (_) { body = null; }
  if (res.status === 200 && body && body.ok === false) ruleRejected.add(1, { name });
  return { res, body };
}

// snapshot fetches GET /api/v1/state with If-None-Match. The caller keeps etag per VU.
export function snapshot(ip, token, etag) {
  const extra = etag ? { 'If-None-Match': etag } : {};
  const res = get('/api/v1/state', ip, token, 'state', extra);
  if (res.status === 200) snapshotBytes.add(res.body ? res.body.length : 0);
  return { res, etag: res.headers.Etag || res.headers.ETag || etag };
}

export function demoLogin(userId, ip) {
  const res = postJSON('/api/v1/auth/demo-login', { userId }, ip, null, 'demo-login');
  if (res.status !== 200) return null;
  return res.json('token');
}

// Areas around UGM (internal/domain/catalog.go) for search variation.
export const AREAS = [
  [-7.7713, 110.3776], [-7.7596, 110.3727], [-7.7652, 110.3752], [-7.7738, 110.3858],
  [-7.7662, 110.3812], [-7.7684, 110.3853], [-7.7779, 110.3731], [-7.7571, 110.3949],
];
export const QUERIES = ['', '', '', 'kamera', 'tenda', 'termos', 'proyektor', 'sepeda', 'jas hujan', 'speaker', 'tripod', 'kompor gas'];
export const CATEGORIES = ['', '', '', 'camping', 'alat-masak', 'kamera', 'elektronik', 'pakaian-acara', 'musim-hujan', 'olahraga'];

export function pick(a) { return a[Math.floor(Math.random() * a.length)]; }

export function isoDate(ms) { return new Date(ms + 7 * 3600 * 1000).toISOString().slice(0, 10); }

export function searchPath(baseMs) {
  const [lat, lng] = pick(AREAS);
  const p = [`lat=${lat}`, `lng=${lng}`, `radiusKm=${pick([1, 2, 3, 5, 10])}`];
  const q = pick(QUERIES);
  if (q) p.push(`q=${encodeURIComponent(q)}`);
  const c = pick(CATEGORIES);
  if (c) p.push(`kategori=${c}`);
  if (Math.random() < 0.3) {
    const start = baseMs + Math.floor(Math.random() * 30) * 86400000;
    p.push(`mulai=${isoDate(start)}`, `selesai=${isoDate(start + 2 * 86400000)}`);
  }
  if (Math.random() < 0.2) p.push(`max=${pick([20000, 50000, 100000])}`);
  return `/api/v1/listings/search?${p.join('&')}`;
}
