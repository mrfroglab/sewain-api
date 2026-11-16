// campus-nat.js: 300 students behind ONE campus Wi-Fi NAT address (docs/integration/ops.md §2). Normal pattern per
// student: snapshot with If-None-Match, a few searches, an occasional favorite toggle, one WebSocket tab. 10% are
// guests (they share the IP bucket). Pass criterion: zero 429.
import { sleep, check } from 'k6';
import exec from 'k6/execution';
import { WebSocket } from 'k6/websockets';
import { setTimeout, clearTimeout } from 'k6/timers';
import { BASE, HOST, ORIGIN, action, get, pick, searchPath, snapshot, rateLimited } from './lib/common.js';
import { summarize } from './lib/summary.js';

const pool = JSON.parse(open(__ENV.POOL || '/out/pool.json'));
const IP = __ENV.FIXED_IP || '100.64.0.1';
const SCALE = parseFloat(__ENV.SCALE || '1');
const HOLD = __ENV.HOLD || '3m';
const v = (n) => Math.max(1, Math.round(n * SCALE));

export const ENDPOINTS = ['state', 'search', 'config', 'events', 'action:toggleFavorite'];

const thresholds = {
  'http_req_duration{name:state}': ['p(95)<500'],
  'http_req_duration{name:action:toggleFavorite}': ['p(95)<300'],
  http_req_failed: ['rate<0.001'],
  rate_limited: ['count<1'],
};
for (const n of ENDPOINTS) {
  if (!thresholds[`http_req_duration{name:${n}}`]) thresholds[`http_req_duration{name:${n}}`] = ['p(95)>=0'];
  thresholds[`http_reqs{name:${n}}`] = ['count>=0'];
  thresholds[`http_req_failed{name:${n}}`] = ['rate>=0'];
}

export const options = {
  summaryTrendStats: ['avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  thresholds,
  scenarios: {
    campus: {
      executor: 'ramping-vus', exec: 'campus', startVUs: 0, gracefulRampDown: '10s', gracefulStop: '15s',
      stages: [
        { duration: '30s', target: v(100) },
        { duration: '45s', target: v(300) },
        { duration: HOLD, target: v(300) },
        { duration: '15s', target: 0 },
      ],
    },
  },
};

const state = {};
const listingIds = pool.providers.map((p) => p.listingId);

function think(min, max) { sleep(min + Math.random() * (max - min)); }

export function campus() {
  const id = exec.vu.idInTest;
  const guest = id % 10 === 0;
  const u = pool.users[(id - 1) % pool.users.length];
  const token = guest ? null : u.token;
  const s = state[id] || (state[id] = {});
  if (exec.vu.iterationInScenario === 0) {
    get('/api/v1/config', IP, null, 'config');
    const h = { Host: HOST, 'CF-Connecting-IP': IP, Origin: ORIGIN };
    if (token) h.Authorization = `Bearer ${token}`;
    const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/api/v1/events`, null, { headers: h, tags: { name: 'events' } });
    let t = setTimeout(() => ws.close(), 15000);
    ws.onmessage = () => { clearTimeout(t); t = setTimeout(() => ws.close(), 5000 + Math.random() * 10000); };
    ws.onerror = (e) => { if (String(e.error || '').includes('429')) rateLimited.add(1, { name: 'events' }); };
  }
  const snap = snapshot(IP, token, s.etag);
  s.etag = snap.etag;
  check(snap.res, { 'state 200/304': (r) => r.status === 200 || r.status === 304 });
  think(2, 5);
  for (let i = 0; i < 1 + Math.floor(Math.random() * 2); i++) {
    get(searchPath(pool.serverNow), IP, token, 'search');
    think(2, 5);
  }
  if (!guest && Math.random() < 0.2) {
    action('toggleFavorite', [u.id, pick(listingIds)], IP, token);
  }
  think(5, 12);
}

export function handleSummary(data) { return summarize(data, __ENV.NAME || 'campus-nat', ENDPOINTS); }
