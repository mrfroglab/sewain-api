// main.js: mixed load (ROADMAP F5.5). Three scenarios ramp together 0 → 100 → 300 VUs, hold, then ramp down:
//   browse 70%  guests + signed-in users: snapshot (If-None-Match), search, config, a short WebSocket session;
//   rent   25%  createBooking → acceptBooking (as the listing's provider) → payBooking → sendMessage → snapshot;
//   upload  5%  small evidence photo (≤ 300 KB) to the VU's own booking.
// Every VU has its own client IP (CF-Connecting-IP from 100.64.0.0/10) unless FIXED_IP is set.
// Needs pool.json from prepare.js (POOL env, default /out/pool.json).
import { sleep, check } from 'k6';
import exec from 'k6/execution';
import http from 'k6/http';
import { WebSocket } from 'k6/websockets';
import { setTimeout, clearTimeout } from 'k6/timers';
import {
  BASE, HOST, ORIGIN, action, get, headers, ipForVU, isoDate, pick, randomIP, searchPath, snapshot, track, rateLimited,
} from './lib/common.js';
import { summarize } from './lib/summary.js';

const pool = JSON.parse(open(__ENV.POOL || '/out/pool.json'));
const photo = open('./fixtures/bukti.jpg', 'b');
const SCALE = parseFloat(__ENV.SCALE || '1'); // 1 = 300 VUs at peak
const HOLD = __ENV.HOLD || '3m';
const FIXED_IP = __ENV.FIXED_IP || '';
const DAY = 86400000;

function stages(share) {
  const v = (n) => Math.max(1, Math.round(n * share * SCALE));
  return [
    { duration: '30s', target: v(100) },
    { duration: '45s', target: v(300) },
    { duration: HOLD, target: v(300) },
    { duration: '15s', target: 0 },
  ];
}

export const ENDPOINTS = [
  'state', 'search', 'config', 'events', 'photo-upload',
  'action:createBooking', 'action:acceptBooking', 'action:payBooking', 'action:sendMessage',
];

const thresholds = {
  'http_req_duration{name:state}': ['p(95)<500'],
  'http_req_failed': ['rate<0.001'],
  rate_limited: ['count<1'],
};
for (const n of ENDPOINTS) {
  if (n.startsWith('action:')) thresholds[`http_req_duration{name:${n}}`] = ['p(95)<300'];
  else if (n !== 'state') thresholds[`http_req_duration{name:${n}}`] = ['p(95)>=0'];
  thresholds[`http_reqs{name:${n}}`] = ['count>=0'];
  thresholds[`http_req_failed{name:${n}}`] = ['rate>=0'];
}

export const options = {
  discardResponseBodies: false,
  summaryTrendStats: ['avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  thresholds,
  scenarios: {
    browse: { executor: 'ramping-vus', exec: 'browse', startVUs: 0, stages: stages(0.70), gracefulRampDown: '10s', gracefulStop: '15s' },
    rent: { executor: 'ramping-vus', exec: 'rent', startVUs: 0, stages: stages(0.25), gracefulRampDown: '10s', gracefulStop: '15s' },
    upload: { executor: 'ramping-vus', exec: 'upload', startVUs: 0, stages: stages(0.05), gracefulRampDown: '10s', gracefulStop: '15s' },
  },
};

// Each VU id (unique across scenarios) maps to its own pool user, so per-user limits and WebSocket caps are real.
function me() { return pool.users[(exec.vu.idInTest - 1) % pool.users.length]; }
const state = {}; // per VU: etag, booking

const provIPs = {};
function providerIP(p) { return provIPs[p.id] || (provIPs[p.id] = randomIP()); }

function think(min, max) { sleep(min + Math.random() * (max - min)); }

function openSocket(ip, token, holdMs) {
  const h = { Host: HOST, 'CF-Connecting-IP': ip, Origin: ORIGIN };
  if (token) h.Authorization = `Bearer ${token}`;
  const url = `${BASE.replace(/^http/, 'ws')}/api/v1/events`;
  const ws = new WebSocket(url, null, { headers: h, tags: { name: 'events' } });
  let timer = setTimeout(() => ws.close(), holdMs + 5000);
  ws.onmessage = () => {
    // first message is "hello": the connection is live; keep it like an open tab for a while
    clearTimeout(timer);
    timer = setTimeout(() => ws.close(), holdMs);
  };
  ws.onerror = (e) => { if (String(e.error || '').includes('429')) rateLimited.add(1, { name: 'events' }); };
}

export function browse() {
  const ip = ipForVU(FIXED_IP);
  const guest = exec.vu.idInTest % 2 === 0;
  const token = guest ? null : me().token;
  const s = state[exec.vu.idInTest] || (state[exec.vu.idInTest] = {});
  if (exec.vu.iterationInScenario === 0) {
    get('/api/v1/config', ip, null, 'config');
    if (Math.random() < 0.5) openSocket(ip, token, 3000 + Math.random() * 7000);
  }
  const snap = snapshot(ip, token, s.etag);
  s.etag = snap.etag;
  check(snap.res, { 'state 200/304': (r) => r.status === 200 || r.status === 304 });
  think(1, 4);
  const n = 1 + Math.floor(Math.random() * 3);
  for (let i = 0; i < n; i++) {
    const r = get(searchPath(pool.serverNow), ip, token, 'search');
    check(r, { 'search 200': (x) => x.status === 200 });
    think(1, 4);
  }
  if (Math.random() < 0.15) get('/api/v1/config', ip, null, 'config');
  think(3, 8);
}

export function rent() {
  const ip = ipForVU(FIXED_IP);
  const u = me();
  const prov = pick(pool.providers);
  const startMs = pool.serverNow + (1 + Math.floor(Math.random() * 150)) * DAY;
  const days = Math.floor(Math.random() * 3);
  const c = action('createBooking', [u.id, {
    listingId: prov.listingId, startDate: isoDate(startMs), endDate: isoDate(startMs + days * DAY),
    handover: prov.handover, collateralOptionId: prov.collateralId, note: 'uji beban',
  }], ip, u.token);
  check(c.res, { 'createBooking 200': (r) => r.status === 200 });
  think(1, 3);
  if (c.body && c.body.ok && c.body.value) {
    const id = c.body.value;
    // the provider answers from their own phone (own IP)
    const a = action('acceptBooking', [prov.id, id], FIXED_IP || providerIP(prov), prov.token);
    check(a.res, { 'acceptBooking 200': (r) => r.status === 200 });
    think(1, 3);
    if (a.body && a.body.ok) {
      const p = action('payBooking', [u.id, id, 'qris'], ip, u.token);
      check(p.res, { 'payBooking 200': (r) => r.status === 200 });
      think(1, 2);
    }
    const m = action('sendMessage', [u.id, id, { text: 'Halo kak, aku ambil sesuai jadwal ya.' }], ip, u.token);
    check(m.res, { 'sendMessage 200': (r) => r.status === 200 });
  }
  const s = state[exec.vu.idInTest] || (state[exec.vu.idInTest] = {});
  const snap = snapshot(ip, u.token, s.etag);
  s.etag = snap.etag;
  think(4, 10);
}

export function upload() {
  const ip = ipForVU(FIXED_IP);
  const u = me();
  const s = state[exec.vu.idInTest] || (state[exec.vu.idInTest] = {});
  if (!s.booking) {
    // A request far in the future on a provider listing: REQUESTED never blocks stock, so it always succeeds.
    const prov = pool.providers[exec.vu.idInTest % pool.providers.length];
    const startMs = pool.serverNow + (200 + (exec.vu.idInTest % 100)) * DAY;
    const c = action('createBooking', [u.id, {
      listingId: prov.listingId, startDate: isoDate(startMs), endDate: isoDate(startMs + DAY),
      handover: prov.handover, collateralOptionId: prov.collateralId,
    }], ip, u.token);
    if (!(c.body && c.body.ok)) { think(5, 10); return; }
    s.booking = c.body.value;
  }
  const res = track(http.post(`${BASE}/api/v1/photos`, {
    file: http.file(photo, 'bukti.jpg', 'image/jpeg'), stage: 'checkout', bookingId: s.booking,
  }, { headers: headers(ip, u.token), tags: { name: 'photo-upload' } }));
  check(res, { 'upload 200': (r) => r.status === 200 });
  think(2, 5);
  const snap = snapshot(ip, u.token, s.etag);
  s.etag = snap.etag;
  think(8, 15);
}

export function handleSummary(data) { return summarize(data, __ENV.NAME || 'main', ENDPOINTS); }
