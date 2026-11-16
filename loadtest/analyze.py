#!/usr/bin/env python3
"""Summarise one load-test run (stdlib only).

    python3 -I loadtest/analyze.py <OUT dir> <name> [bucket seconds, default 15]

Reads <name>-k6.csv.gz, <name>-stats.csv, <name>-pg.csv, <name>-metrics-{before,after}.txt and prints Markdown:
a time series (VUs, req/s, p95 per group, errors, 429), container peaks, pgx pool and job metric deltas.
"""
import csv
import gzip
import math
import os
import re
import sys
from collections import defaultdict


def pct(vals, p):
    if not vals:
        return float("nan")
    vals = sorted(vals)
    k = max(0, min(len(vals) - 1, math.ceil(p / 100 * len(vals)) - 1))
    return vals[k]


def f(v):
    if v != v:  # NaN
        return "-"
    return f"{v:.0f}" if v >= 100 else f"{v:.1f}"


def group_of(name):
    if name.startswith("action:"):
        return "action"
    return name


def series(out, name, bucket):
    path = os.path.join(out, f"{name}-k6.csv.gz")
    if not os.path.exists(path):
        return
    dur = defaultdict(lambda: defaultdict(list))
    reqs = defaultdict(int)
    fails = defaultdict(int)
    limited = defaultdict(int)
    vus = defaultdict(int)
    t0 = None
    with gzip.open(path, "rt", newline="") as fh:
        for row in csv.DictReader(fh):
            ts = int(float(row["timestamp"]))
            t0 = ts if t0 is None else min(t0, ts)
            m = row["metric_name"]
            b = ts // bucket * bucket
            if m == "http_req_duration":
                dur[b][group_of(row.get("name", ""))].append(float(row["metric_value"]))
                reqs[b] += 1
                if row.get("status") == "429":
                    limited[b] += 1
            elif m == "http_req_failed" and row["metric_value"] not in ("0", "0.0", "0.000000"):
                fails[b] += 1
            elif m == "vus":
                vus[b] = max(vus[b], int(float(row["metric_value"])))
    groups = ["state", "search", "action", "photo-upload"]
    print(f"### Time series ({bucket} s buckets)\n")
    print("| t (s) | VUs | req/s | " + " | ".join(f"p95 {g}" for g in groups) + " | failed | 429 |")
    print("|---:|---:|---:|" + "---:|" * len(groups) + "---:|---:|")
    for b in sorted(set(reqs) | set(vus)):
        cells = [f(pct(dur[b][g], 95)) for g in groups]
        print(f"| {b - t0} | {vus[b]} | {reqs[b] / bucket:.1f} | " + " | ".join(cells) + f" | {fails[b]} | {limited[b]} |")
    print()


def mib(s):
    m = re.match(r"([\d.]+)\s*([KMG]i?B|B)", s.strip())
    if not m:
        return float("nan")
    v, u = float(m.group(1)), m.group(2)
    return v * {"B": 1 / 1048576, "KiB": 1 / 1024, "kB": 1 / 1024, "MiB": 1, "MB": 1, "GiB": 1024, "GB": 1024}.get(u, 1)


def containers(out, name):
    path = os.path.join(out, f"{name}-stats.csv")
    if not os.path.exists(path):
        return
    cpu = defaultdict(list)
    mem = defaultdict(list)
    limit = {}
    with open(path, newline="") as fh:
        for row in csv.DictReader(fh):
            c = row["container"]
            cpu[c].append(float(row["cpu_pct"].rstrip("%") or 0))
            mem[c].append(mib(row["mem_used"]))
            limit[c] = row["mem_limit"]
    print("### Containers (docker stats, ~5 s)\n")
    print("| container | samples | CPU avg % | CPU max % | mem avg MiB | mem max MiB | limit |")
    print("|---|---:|---:|---:|---:|---:|---|")
    for c in sorted(cpu):
        print(f"| {c} | {len(cpu[c])} | {sum(cpu[c]) / len(cpu[c]):.0f} | {max(cpu[c]):.0f} | "
              f"{sum(mem[c]) / len(mem[c]):.0f} | {max(mem[c]):.0f} | {limit[c]} |")
    print()
    path = os.path.join(out, f"{name}-pg.csv")
    if os.path.exists(path):
        rows = list(csv.DictReader(open(path, newline="")))
        if rows:
            mx = lambda k: max(int(r[k] or 0) for r in rows)  # noqa: E731
            print(f"Postgres sessions (pg_stat_activity, 5 s): max active {mx('active')}, max idle-in-tx "
                  f"{mx('idle_in_tx')}, max waiting on lock {mx('waiting_lock')}, max total {mx('total')}.\n")


def prom(path):
    vals = {}
    if not os.path.exists(path):
        return vals
    for line in open(path):
        if line.startswith("#") or not line.strip():
            continue
        k, _, v = line.rpartition(" ")
        try:
            vals[k] = float(v)
        except ValueError:
            pass
    return vals


def metrics(out, name):
    a = prom(os.path.join(out, f"{name}-metrics-before.txt"))
    b = prom(os.path.join(out, f"{name}-metrics-after.txt"))
    if not b:
        return
    d = lambda k: b.get(k, 0) - a.get(k, 0)  # noqa: E731
    acq = d("sewain_db_pool_acquire_total")
    print("### Prometheus deltas (API :9090)\n")
    print(f"- pgx pool: max_conns {b.get('sewain_db_pool_max_conns', 0):.0f}, acquires {acq:.0f}, "
          f"acquires that had to wait (empty pool) {d('sewain_db_pool_empty_acquire_total'):.0f} "
          f"({100 * d('sewain_db_pool_empty_acquire_total') / acq if acq else 0:.1f}%), mean acquire wait "
          f"{1000 * d('sewain_db_pool_acquire_duration_seconds_total') / acq if acq else 0:.2f} ms, "
          f"canceled acquires {d('sewain_db_pool_canceled_acquire_total'):.0f}")
    jobs = sorted({re.sub(r",?result=\"[^\"]*\"", "", k.split("{", 1)[1].rstrip("}"))
                   for k in b if k.startswith("sewain_job_duration_seconds_count")})
    for j in jobs:
        cnt = sum(d(k) for k in b if k.startswith("sewain_job_duration_seconds_count") and j in k)
        tot = sum(d(k) for k in b if k.startswith("sewain_job_duration_seconds_sum") and j in k)
        if cnt:
            print(f"- job {j}: {cnt:.0f} runs, mean {1000 * tot / cnt:.1f} ms")
    rl = {k: d(k) for k in b if k.startswith("sewain_http_rate_limited_total") and d(k)}
    print(f"- rate limited (server side): {rl or 0}")
    print(f"- goroutines after: {b.get('go_goroutines', 0):.0f}, heap in use after: "
          f"{b.get('go_memstats_heap_inuse_bytes', 0) / 1048576:.0f} MiB")
    print()


if __name__ == "__main__":
    out, name = sys.argv[1], sys.argv[2]
    bucket = int(sys.argv[3]) if len(sys.argv) > 3 else 15
    series(out, name, bucket)
    containers(out, name)
    metrics(out, name)
