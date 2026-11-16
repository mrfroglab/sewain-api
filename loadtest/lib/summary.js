// summary.js turns the k6 end-of-test data into a per-endpoint table (stdout + $OUT_DIR/<name>-summary.txt) and the
// raw JSON ($OUT_DIR/<name>-summary.json).
const OUT = __ENV.OUT_DIR || '/out';

function fmt(v) {
  if (v === undefined || v === null || Number.isNaN(v)) return '-';
  return v >= 100 ? v.toFixed(0) : v.toFixed(1);
}

function pad(s, n) { s = String(s); return s.length >= n ? s : s + ' '.repeat(n - s.length); }

export function summarize(data, name, endpoints) {
  const m = data.metrics;
  const lines = [];
  lines.push(`# ${name}`);
  lines.push('');
  lines.push(`| endpoint | requests | failed % | p50 ms | p95 ms | p99 ms | max ms |`);
  lines.push(`|---|---:|---:|---:|---:|---:|---:|`);
  for (const ep of endpoints) {
    const d = m[`http_req_duration{name:${ep}}`];
    const c = m[`http_reqs{name:${ep}}`];
    const f = m[`http_req_failed{name:${ep}}`];
    if (!d || !c || !c.values.count) continue;
    lines.push(`| ${ep} | ${c.values.count} | ${fmt(100 * (f ? f.values.rate : 0))} | ${fmt(d.values.med)} | ${fmt(d.values['p(95)'])} | ${fmt(d.values['p(99)'])} | ${fmt(d.values.max)} |`);
  }
  const all = m.http_req_duration;
  if (all) {
    lines.push(`| **all** | ${m.http_reqs.values.count} | ${fmt(100 * m.http_req_failed.values.rate)} | ${fmt(all.values.med)} | ${fmt(all.values['p(95)'])} | ${fmt(all.values['p(99)'])} | ${fmt(all.values.max)} |`);
  }
  lines.push('');
  const kv = [
    ['peak VUs', m.vus_max && m.vus_max.values.max],
    ['requests/s', m.http_reqs && m.http_reqs.values.rate.toFixed(1)],
    ['429 responses', m.rate_limited ? m.rate_limited.values.count : 0],
    ['rule rejections (200 ok:false)', m.rule_rejected ? m.rule_rejected.values.count : 0],
    ['checks passed %', m.checks ? (100 * m.checks.values.rate).toFixed(2) : '-'],
    ['snapshot body p50/p95 bytes', m.snapshot_bytes ? `${fmt(m.snapshot_bytes.values.med)} / ${fmt(m.snapshot_bytes.values['p(95)'])}` : '-'],
    ['ws sessions', m.ws_sessions ? m.ws_sessions.values.count : 0],
    ['ws connecting p95 ms', m.ws_connecting ? fmt(m.ws_connecting.values['p(95)']) : '-'],
    ['iterations', m.iterations ? m.iterations.values.count : 0],
  ];
  for (const [k, v] of kv) lines.push(`- ${pad(k, 32)} ${v}`);
  lines.push('');
  lines.push('Thresholds:');
  for (const [k, v] of Object.entries(m)) {
    if (!v.thresholds) continue;
    for (const [t, r] of Object.entries(v.thresholds)) {
      if (t.endsWith('>=0')) continue;
      lines.push(`- ${r.ok ? 'PASS' : 'FAIL'} ${k} ${t}`);
    }
  }
  const text = lines.join('\n') + '\n';
  const out = { stdout: text };
  out[`${OUT}/${name}-summary.txt`] = text;
  out[`${OUT}/${name}-summary.json`] = JSON.stringify(data, null, 1);
  return out;
}
