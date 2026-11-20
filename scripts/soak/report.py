#!/usr/bin/env python3
"""長時間の負荷試験の結果をまとめる（docs/bench.md の「段階 108」）。

時間ごとの表（要求の数、200 以外、全体と DB の時間の p50 と p99、本番とレプリカのメモリ、ディスク、開いたファイル、
goroutine、GC の境の古さと時間、エラーの数）と、伸びの傾き（後半の窓の中央値と前半の窓の中央値の差、
1 時間あたりの回帰の傾き）、GC が消したバージョンの数、ログの警告とエラーの数を出す。

    scripts/soak/report.py --dir out --bucket 3600 --windows 6-12,18-24
"""
import argparse
import collections
import json
import os
import re
import statistics

GC_LINE = re.compile(r'time="([^"]+)".*gc: removed (\d+) versions, (\d+) tables, (\d+) indexes, (\d+) merge records')


def pct(xs, q):
    if not xs:
        return float("nan")
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(q * (len(xs) - 1))))]


def slope(points):
    """最小二乗の傾き（x は時間、1 時間あたり）"""
    if len(points) < 2:
        return float("nan")
    mx = statistics.fmean(x for x, _ in points)
    my = statistics.fmean(y for _, y in points)
    den = sum((x - mx) ** 2 for x, _ in points)
    return sum((x - mx) * (y - my) for x, y in points) / den if den else float("nan")


def metric(r, name):
    return (r.get("m") or {}).get(name)


def mb(v):
    return None if v is None else v / 2**20


def errors_by_code(r):
    out = {}
    for k, v in (r.get("m") or {}).items():
        if k.startswith("heimdb_statement_errors_total"):
            out[k.split('"')[1] if '"' in k else k] = v
    return out


def restarted(ss, role):
    """区間の中でプロセスが起動し直したか（数の差と CPU の時間が使えない）"""
    starts = {metric(s.get(role) or {}, "heimdb_start_time_seconds") for s in ss}
    return len(starts - {None}) > 1


SERIES = {
    "rss_mb": lambda r: mb((r.get("proc") or {}).get("VmRSS")),
    "live_mb": lambda r: mb(metric(r, "heimdb_memory_live_bytes")),
    "cache_mb": lambda r: mb(metric(r, "heimdb_cache_used_bytes")),
    "disk_mb": lambda r: mb(metric(r, "heimdb_disk_bytes")),
    "du_mb": lambda r: mb(r.get("du_data")),
    "wal_mb": lambda r: mb(r.get("wal_bytes")),
    "sst_mb": lambda r: mb(r.get("sst_bytes")),
    "fds": lambda r: (r.get("proc") or {}).get("fds"),
    "threads": lambda r: (r.get("proc") or {}).get("Threads"),
    "goroutines": lambda r: r.get("goroutines"),
    "conns": lambda r: metric(r, "heimdb_connections"),
    "gc_age_m": lambda r: None if metric(r, "heimdb_gc_boundary_age_seconds") is None else metric(r, "heimdb_gc_boundary_age_seconds") / 60,
    "gc_s": lambda r: metric(r, "heimdb_gc_last_duration_seconds"),
    "snap_age": lambda r: metric(r, "heimdb_oldest_snapshot_age_seconds"),
    "branches": lambda r: metric(r, "heimdb_branches"),
    "cpu_s": lambda r: (r.get("proc") or {}).get("cpu_s"),
    "behind": lambda r: metric(r, "heimdb_replication_behind_seconds"),
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dir", required=True)
    ap.add_argument("--bucket", type=float, default=3600, help="seconds per row")
    ap.add_argument("--windows", default="6-12,18-24", help="two windows in buckets to compare, like 6-12,18-24")
    ap.add_argument("--logs", default="", help="directory with primary.log and replica.log (default: --dir)")
    args = ap.parse_args()
    d = args.dir

    samples = []
    for line in open(os.path.join(d, "samples.jsonl")):
        try:
            samples.append(json.loads(line))
        except json.JSONDecodeError:  # 止めたときに切れた最後の行
            pass
    t0 = samples[0]["t"]
    loads = []
    for line in open(os.path.join(d, "load.tsv")):
        f = line.rstrip("\n").split("\t")
        if len(f) < 7:
            continue
        loads.append((float(f[0]), f[1], f[2], float(f[3]) if f[3] else None, float(f[4]) if f[4] else None, float(f[6] or 0)))
    t0 = min(t0, loads[0][0]) if loads else t0
    b = lambda t: int((t - t0) // args.bucket)

    req = collections.defaultdict(list)
    for row in loads:
        req[b(row[0])].append(row)
    smp = collections.defaultdict(list)
    for s in samples:
        smp[b(s["t"])].append(s)
    buckets = sorted(set(req) | set(smp))

    print(f"# 長時間の負荷試験（{len(samples)} 標本、{len(loads)} 要求、1 行 {args.bucket / 3600:g} 時間）\n")
    print("| 時間 | 要求 | 200 以外 | 遅れ>1s | 全体 p50 | 全体 p99 | DB p50 | DB p99 | 本番 RSS | live | cache | disk | du | fds | gor | GC 境(分) | GC 秒 | 接続 | レプリカ RSS | live | disk | gor | 遅れ最大(秒) | エラー | 本番 CPU% | レプリカ CPU% |")
    print("|" + "---|" * 26)
    prev_err = None
    for k in buckets:
        rs = req.get(k, [])
        bad = sum(1 for r in rs if r[2] != "200")
        late = sum(1 for r in rs if r[5] > 1)
        tot = [r[3] for r in rs if r[3] is not None and r[2] == "200"]
        db = [r[4] for r in rs if r[4] is not None and r[2] == "200"]
        ss = smp.get(k, [])

        def med(role, name):
            xs = [SERIES[name](s.get(role) or {}) for s in ss]
            xs = [x for x in xs if x is not None]
            return statistics.median(xs) if xs else float("nan")

        def mx(role, name):
            xs = [SERIES[name](s.get(role) or {}) for s in ss]
            xs = [x for x in xs if x is not None]
            return max(xs) if xs else float("nan")

        def cpu(role):
            if restarted(ss, role):
                return float("nan")
            xs = [(s["t"], SERIES["cpu_s"](s.get(role) or {})) for s in ss]
            xs = [(t, c) for t, c in xs if c is not None]
            # 止める途中の標本は、数を取れずに CPU の時間だけ取れることがある（起動し直しを数から見分けられない）
            if any(b[1] < a[1] for a, b in zip(xs, xs[1:])):
                return float("nan")
            return (xs[-1][1] - xs[0][1]) / (xs[-1][0] - xs[0][0]) * 100 if len(xs) > 1 and xs[-1][0] > xs[0][0] else float("nan")

        # 番号ごとの増え（区間の最後の標本と、前の区間の最後の標本の差）。手元の mysql 9.x のクライアントは、つなぐたびに
        # select $$ で 1105 を 1 つ出す（GCE の MariaDB のクライアントは出さない）
        err = errors_by_code(ss[-1].get("primary") or {}) if ss else None
        if err is None or prev_err is None or restarted(ss, "primary"):
            derr = ""
        else:
            derr = " ".join(f"{c}:{v - prev_err.get(c, 0):.0f}" for c, v in sorted(err.items()) if v - prev_err.get(c, 0))
        prev_err = err if err is not None else prev_err
        print(
            f"| {k} | {len(rs)} | {bad} | {late} | {pct(tot, .5) * 1000:.0f} | {pct(tot, .99) * 1000:.0f} | {pct(db, .5) * 1000:.1f} | {pct(db, .99) * 1000:.1f} "
            f"| {med('primary', 'rss_mb'):.0f} | {med('primary', 'live_mb'):.0f} | {med('primary', 'cache_mb'):.0f} | {med('primary', 'disk_mb'):.1f} | {med('primary', 'du_mb'):.1f} "
            f"| {med('primary', 'fds'):.0f} | {med('primary', 'goroutines'):.0f} | {med('primary', 'gc_age_m'):.0f} | {med('primary', 'gc_s'):.2f} | {med('primary', 'conns'):.0f} "
            f"| {med('replica', 'rss_mb'):.0f} | {med('replica', 'live_mb'):.0f} | {med('replica', 'disk_mb'):.1f} | {med('replica', 'goroutines'):.0f} | {mx('replica', 'behind'):.1f} | {derr} | {cpu('primary'):.0f} | {cpu('replica'):.0f} |"
        )

    # 窓どうしの比べと傾き
    (a0, a1), (b0, b1) = [tuple(int(x) for x in w.split("-")) for w in args.windows.split(",")]
    print(f"\n## 窓 {a0}-{a1} と {b0}-{b1} の中央値と、{a0} から {b1} までの 1 時間あたりの傾き\n")
    print("| 値 | 役 | 前の窓 | 後の窓 | 差 | 傾き/時 |")
    print("|---|---|---|---|---|---|")
    for role in ("primary", "replica"):
        for name in ("rss_mb", "live_mb", "disk_mb", "du_mb", "wal_mb", "sst_mb", "fds", "goroutines", "gc_s"):
            pts = [((s["t"] - t0) / 3600, SERIES[name](s.get(role) or {})) for s in samples]
            pts = [(x, y) for x, y in pts if y is not None]
            wa = [y for x, y in pts if a0 <= x * 3600 / args.bucket < a1]
            wb = [y for x, y in pts if b0 <= x * 3600 / args.bucket < b1]
            if not wa or not wb:
                continue
            ma, mb = statistics.median(wa), statistics.median(wb)
            sl = slope([(x, y) for x, y in pts if a0 <= x * 3600 / args.bucket < b1])
            print(f"| {name} | {role} | {ma:.2f} | {mb:.2f} | {mb - ma:+.2f} ({(mb - ma) / ma * 100 if ma else 0:+.1f}%) | {sl:+.3f} |")
    for label, sel in (("全体", 3), ("DB", 4)):
        wa = [r[sel] for r in loads if r[sel] is not None and r[2] == "200" and a0 <= b(r[0]) < a1]
        wb = [r[sel] for r in loads if r[sel] is not None and r[2] == "200" and b0 <= b(r[0]) < b1]
        if wa and wb:
            for q in (0.5, 0.99):
                x, y = pct(wa, q) * 1000, pct(wb, q) * 1000
                print(f"| {label} p{int(q * 100)} ms | 要求 | {x:.1f} | {y:.1f} | {y - x:+.1f} ({(y - x) / x * 100:+.1f}%) | |")

    # ページごと
    print("\n## ページごとの p50 と p99（全体 ms、DB ms。前の窓と後の窓）\n")
    print("| ページ | 前 p50 | 後 p50 | 前 p99 | 後 p99 | 前 DB p50 | 後 DB p50 |")
    print("|---|---|---|---|---|---|---|")
    for label in sorted({r[1] for r in loads}):
        wa = [r for r in loads if r[1] == label and r[2] == "200" and a0 <= b(r[0]) < a1]
        wb = [r for r in loads if r[1] == label and r[2] == "200" and b0 <= b(r[0]) < b1]
        if wa and wb:
            f = lambda rs, i, q: pct([r[i] for r in rs if r[i] is not None], q) * 1000
            print(f"| {label} | {f(wa, 3, .5):.0f} | {f(wb, 3, .5):.0f} | {f(wa, 3, .99):.0f} | {f(wb, 3, .99):.0f} | {f(wa, 4, .5):.1f} | {f(wb, 4, .5):.1f} |")

    # 200 以外の内訳
    bad = collections.Counter((r[1], r[2]) for r in loads if r[2] != "200")
    if bad:
        print("\n## 200 以外\n")
        for (label, st), n in bad.most_common():
            print(f"- {label} {st}: {n}")

    # GC とログ
    logs = args.logs or d
    for role in ("primary", "replica"):
        path = os.path.join(logs, f"{role}.log")
        if not os.path.exists(path):
            continue
        runs, levels, notable = [], collections.Counter(), collections.Counter()
        for line in open(path, errors="replace"):
            m = GC_LINE.search(line)
            if m:
                runs.append((m.group(1)[11:19], int(m.group(2)), int(m.group(5))))
            lv = re.search(r"level=(\w+)", line)
            if lv:
                levels[lv.group(1)] += 1
                if lv.group(1) in ("warning", "error", "fatal", "panic"):
                    mm = re.search(r'msg="?([^"]*)', line)
                    msg = re.sub(r"\d+", "N", (mm.group(1) if mm else line)[:90])
                    notable[msg] += 1
        print(f"\n## {role} のログ\n")
        print(f"- 水準ごとの行数: {dict(levels)}")
        nz = [r for r in runs if r[1] > 0]
        print(f"- GC: {len(runs)} 回、バージョンを消したのは {len(nz)} 回" + (f"、1 回あたり {statistics.median(r[1] for r in nz):.0f} バージョン（中央値）、最後の 5 回 {[r[1] for r in runs[-5:]]}" if nz else ""))
        for msg, n in notable.most_common(15):
            print(f"  - {n} × {msg}")

    # 本番のエラーの内訳（最後の標本）
    last = samples[-1].get("primary", {}).get("m", {})
    errs = {k: v for k, v in last.items() if k.startswith("heimdb_statement_errors_total") and v}
    if errs:
        print("\n## 本番の文のエラー（累計）\n")
        for k, v in sorted(errs.items(), key=lambda kv: -kv[1]):
            print(f"- {k}: {v:.0f}")
    opts = [s for s in samples if "options" in s]
    if opts:
        print("\n## wp_options と alloptions の計画（最初と最後）\n")
        for s in (opts[0], opts[-1]):
            plan = s.get("alloptions_plan", "")
            access = "IndexedTableAccess" if "IndexedTableAccess" in plan else ("TableScan/other" if plan else "?")
            print(f"- {(s['t'] - t0) / 3600:.1f} 時間: {s['options']}、{access}")


if __name__ == "__main__":
    main()
