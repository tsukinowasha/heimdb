#!/usr/bin/env python3
"""Summarize run.sh K (docs/bench.md, "During a backup").

    backup-agg.py <res dir>

For each data size and case, averages the 2 runs: the backup times, the read and write page times of the pages
that overlapped a backup and of the others, the worst 1-second interval, errors, and the server CPU in the window.
The "none" case has no backup, so all its pages are "outside".
"""
import csv
import glob
import json
import os
import statistics
import sys

CASES = ["none", "same", "other", "enc"]


def mean(v):
    v = [x for x in v if x is not None]
    return statistics.mean(v) if v else None


def cpu(d, name, s, e):
    p = os.path.join(d, name + ".cpu.csv")
    if not os.path.exists(p):
        return None
    rows = [r for r in csv.DictReader(open(p)) if s <= float(r["unix"]) <= e + 0.5]
    if len(rows) < 2:
        return None
    return 100 * (float(rows[-1]["cpu_s"]) - float(rows[0]["cpu_s"])) / (float(rows[-1]["unix"]) - float(rows[0]["unix"]))


def one(d, path):
    j = json.load(open(path))
    name = os.path.basename(path)[:-5]
    r = {"pages_s": j["pages_per_s"], "failed": j["failed_pages"], "errors": sum(j["errors"].values()),
         "conn_errors": sum(j["connection_errors"].values()), "late": j.get("late_pages", 0),
         "cpu": cpu(d, name, j["start_unix"], j["end_unix"]),
         "worst_s_p99": max(i["p99_ms"] for i in j["series"]), "worst_s_max": max(i["max_ms"] for i in j["series"])}
    side = j.get("side") or []
    r["backup_ms"] = [x["ms"] for x in side]
    r["backup_errors"] = [x["error"] for x in side if x.get("error")]
    during, outside = j.get("during_side_ms") or {}, j.get("outside_side_ms") or j["latency_ms"]
    for where, m in (("in", during), ("out", outside)):
        for kind in ("read", "write"):
            x = m.get(kind) or {}
            for q in ("count", "p50", "p99", "max"):
                r[f"{where}_{kind}_{q}"] = x.get(q)
    return r


def fmt(v, n=1):
    if v is None:
        return "—"
    return f"{v:,.{n}f}"


def main():
    d = sys.argv[1]
    for size in ("small", "large"):
        print(f"\n## {size}\n")
        print("| Case | Backup ms (mean / max) | Read p50 / p99 / max, during | outside | Write p50 / p99 / max, during "
              "| outside | Pages during (read / write) | Worst 1 s p99 / max | CPU | Errors |")
        print("|---|---|---|---|---|---|---|---|---|---|")
        for c in CASES:
            runs = [one(d, p) for p in sorted(glob.glob(os.path.join(d, f"K-{size}-r*-{c}.json")))]
            if not runs:
                continue
            a = {k: mean([r[k] for r in runs]) for k in runs[0] if not isinstance(runs[0][k], list)}
            bms = [x for r in runs for x in r["backup_ms"]]
            berr = [x for r in runs for x in r["backup_errors"]]
            b = f"{fmt(statistics.mean(bms), 0)} / {fmt(max(bms), 0)}" if bms else "—"

            def t(w, k):
                return f"{fmt(a[f'{w}_{k}_p50'])} / {fmt(a[f'{w}_{k}_p99'])} / {fmt(a[f'{w}_{k}_max'])}"

            err = sum(a[k] or 0 for k in ("failed", "errors", "conn_errors")) + len(berr)
            print(f"| {c} | {b} | {t('in', 'read')} | {t('out', 'read')} | {t('in', 'write')} | {t('out', 'write')} "
                  f"| {fmt(a['in_read_count'], 0)} / {fmt(a['in_write_count'], 0)} "
                  f"| {fmt(a['worst_s_p99'])} / {fmt(a['worst_s_max'])} | {fmt(a['cpu'], 0) + '%' if a['cpu'] is not None else '—'} | {fmt(err, 0)} {berr or ''} |")


if __name__ == "__main__":
    main()
