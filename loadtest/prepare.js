// prepare.js builds the account pool the scenarios use, on a DEMO_MODE stack:
//   - USERS renters/browsers: registerUser → demo-login → submitVerification (demo eKYC, auto-approved);
//   - PROVIDERS: the same + one listing each (a copy of a seed listing, stock 1, manual approval), so `rent` VUs
//     accept as their own provider without sharing the seed personas' per-user write bucket.
// Output: $OUT_DIR/pool.json (tokens are demo sessions; resetDemo removes the accounts afterwards).
//
//   docker run --rm --network host -v "$PWD/loadtest":/lt -v "$OUT":/out -e OUT_DIR=/out grafana/k6:2.3.0 run /lt/prepare.js
import http from 'k6/http';
import { BASE, randomIP, headers, uuid } from './lib/common.js';

const USERS = parseInt(__ENV.USERS || '320', 10);
const PROVIDERS = parseInt(__ENV.PROVIDERS || '40', 10);
const OUT = __ENV.OUT_DIR || '/out';

export const options = {
  setupTimeout: '10m',
  iterations: 1,
  vus: 1,
  summaryTrendStats: ['med', 'p(95)', 'max'],
};

function batch(reqs, size) {
  const out = [];
  for (let i = 0; i < reqs.length; i += size) {
    const res = http.batch(reqs.slice(i, i + size));
    for (const r of res) out.push(r);
  }
  return out;
}

function actionReq(name, args, token, id) {
  const body = { args };
  if (id) body.id = id;
  return {
    method: 'POST', url: `${BASE}/api/v1/actions/${name}`, body: JSON.stringify(body),
    params: { headers: headers(randomIP(), token, { 'Content-Type': 'application/json', 'Idempotency-Key': uuid() }), tags: { name: `prepare:${name}` } },
  };
}

function must(res, what) {
  let b = null;
  try { b = res.json(); } catch (_) { /* below */ }
  if (res.status !== 200 || !b || b.ok === false) {
    throw new Error(`${what}: HTTP ${res.status} ${res.body && res.body.slice(0, 200)}`);
  }
  return b;
}

export function setup() {
  const run = `${Date.now().toString(36).slice(-5)}`;
  const st = http.get(`${BASE}/api/v1/state`, { headers: headers(randomIP()) });
  const state = must(st, 'state');
  const seedListings = state.listings.filter((l) => l.status === 'aktif' && l.category !== 'kendaraan');
  const total = USERS + PROVIDERS;
  const nikPrefix = `34${Math.floor(10 + Math.random() * 89)}`;

  // 1. register
  const ids = [];
  const reg = [];
  for (let i = 0; i < total; i++) {
    const id = `u-lt${run}${i}`;
    ids.push(id);
    const phone = `0896${run.replace(/[^0-9]/g, '').padEnd(3, '7').slice(0, 3)}${String(i).padStart(5, '0')}`;
    reg.push(actionReq('registerUser', [{ name: `Penguji Beban ${i}`, phone, accountType: 'individu', areaId: 'ugm' }], null, id));
  }
  batch(reg, 20).forEach((r, i) => { ids[i] = must(r, `registerUser ${ids[i]}`).value; });

  // 2. demo login
  const logins = ids.map((id) => ({
    method: 'POST', url: `${BASE}/api/v1/auth/demo-login`, body: JSON.stringify({ userId: id }),
    params: { headers: headers(randomIP(), null, { 'Content-Type': 'application/json' }), tags: { name: 'prepare:demo-login' } },
  }));
  const tokens = batch(logins, 20).map((r, i) => must(r, `demo-login ${ids[i]}`).token);

  // 3. verify (demo eKYC)
  const ver = ids.map((id, i) => actionReq('submitVerification',
    [id, { method: 'demo', nameOnKtp: `Penguji Beban ${i}`, nik: `${nikPrefix}0101${String(i).padStart(4, '0')}${String(i).padStart(4, '0')}` }], tokens[i]));
  batch(ver, 20).forEach((r, i) => must(r, `submitVerification ${ids[i]}`));

  // 4. provider listings
  const providers = [];
  const lreq = [];
  for (let p = 0; p < PROVIDERS; p++) {
    const i = USERS + p;
    const src = seedListings[p % seedListings.length];
    const coID = `co-lt${run}${p}`;
    providers.push({ id: ids[i], token: tokens[i], listingId: `l-lt${run}${p}`, collateralId: coID, handover: src.handover[0] });
    lreq.push(actionReq('createListing', [ids[i], {
      title: `${src.title} (uji beban ${p})`, category: src.category, description: src.description, photos: src.photos,
      conditionNotes: src.conditionNotes, pricePerDay: src.pricePerDay, itemValue: Math.min(src.itemValue, 1000000),
      collateralOptions: [{ id: coID, kind: 'identitas', cards: ['KTM', 'SIM'] }], location: src.location,
      radiusKm: 5, handover: src.handover, approval: 'manual', stock: 1,
    }], tokens[i], `l-lt${run}${p}`));
  }
  batch(lreq, 10).forEach((r, i) => { const b = must(r, `createListing ${providers[i].listingId}`); providers[i].listingId = b.value; });

  return {
    run, serverNow: state.serverNow,
    users: ids.slice(0, USERS).map((id, i) => ({ id, token: tokens[i] })),
    providers,
  };
}

export default function () {}

export function handleSummary(data) {
  const pool = data.setup_data;
  const out = {};
  out[`${OUT}/pool.json`] = JSON.stringify(pool);
  out.stdout = `pool: ${pool ? pool.users.length : 0} users, ${pool ? pool.providers.length : 0} providers → ${OUT}/pool.json\n`;
  return out;
}
