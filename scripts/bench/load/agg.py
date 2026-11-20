#!/usr/bin/env python3
"""負荷の測り（docs/bench.md の段階 126）の結果をまとめる。

    agg.py <res の置き場>            測りごとの行と、回（-r1、-r2）の平均の行を出す
    agg.py <res の置き場> --series E-heimdb   区切りごとの値（長い時間の測り）

置き場には、負荷のインスタンスの <名前>.json と <名前>.load.csv、DB のインスタンスの <名前>.cpu.csv を一緒に置く。
サーバーの CPU は、loadgen の測りの窓（温めの後）の標本だけで平均する。cpu_pct は CPU 1 つを 100 とした値
（4 vCPU で 400 が上限）、ms/1k は 1,000 文あたりのサーバーの CPU の時間（ミリ秒）。
"""
import argparse
import collections
import csv
import glob
import json
import os
import re
import statistics


def window_rows(path, s, e):
    if not os.path.exists(path):
        return []
    with open(path) as f:
        return [r for r in csv.DictReader(f) if s <= float(r["unix"]) <= e + 0.5]


def summarize(d, name):
    j = json.load(open(os.path.join(d, name + ".json")))
    s, e = j["start_unix"], j["end_unix"]
    srv = window_rows(os.path.join(d, name + ".cpu.csv"), s, e)
    load = window_rows(os.path.join(d, name + ".load.csv"), s, e)
    cpu_s = float(srv[-1]["cpu_s"]) - float(srv[0]["cpu_s"]) if len(srv) > 1 else float("nan")
    span = float(srv[-1]["unix"]) - float(srv[0]["unix"]) if len(srv) > 1 else float("nan")
    cpu_pct = 100 * cpu_s / span if span else float("nan")
    a = j["latency_ms"].get("all", {})
    r = {
        "name": name,
        "pages_s": j["pages_per_s"],
        "stmts_s": j["statements_per_s"],
        "p50": a.get("p50", 0),
        "p95": a.get("p95", 0),
        "p99": a.get("p99", 0),
        "max": a.get("max", 0),
        "cpu_pct": cpu_pct,
        "ms_per_1k": 1000 * 1000 * cpu_pct / 100 / j["statements_per_s"] if j["statements_per_s"] else float("nan"),
        "rss_max": max((float(x["rss_mb"]) for x in srv), default=float("nan")),
        "rss_end": float(srv[-1]["rss_mb"]) if srv else float("nan"),
        "db_host": statistics.mean(float(x["host_busy_pct"]) for x in srv) if srv else float("nan"),
        "load_host": statistics.mean(float(x["host_busy_pct"]) for x in load) if load else float("nan"),
        "failed": j["failed_pages"],
        "errors": j["errors"],
        "conn_errors": j["connection_errors"],
        "connect_failures": j["connect_failures"],
        "never_opened": j["connections_never_opened"],
        "not_started": j.get("not_started_pages", 0),
        "late": j.get("late_pages", 0),
        "lateness_p99": (j.get("lateness_ms") or {}).get("p99", 0),
        "service_p50": (j.get("service_ms") or {}).get("all", {}).get("p50", 0),
        "service_p99": (j.get("service_ms") or {}).get("all", {}).get("p99", 0),
        "target": j.get("target_pages_per_s", 0),
        "read_p99": j["latency_ms"].get("read", {}).get("p99", 0),
        "write_p50": j["latency_ms"].get("write", {}).get("p50", 0),
        "write_p99": j["latency_ms"].get("write", {}).get("p99", 0),
        "write_pages": j["write_pages"],
    }
    for k in ("post_create", "comment_add", "option_update", "post_delete"):
        if k in j["latency_ms"]:
            r[k + "_p50"] = j["latency_ms"][k]["p50"]
            r[k + "_p99"] = j["latency_ms"][k]["p99"]
    return r


COLS = ["pages_s", "stmts_s", "p50", "p95", "p99", "max", "cpu_pct", "ms_per_1k", "rss_max", "db_host", "load_host"]


def fmt(v):
    if isinstance(v, float):
        return f"{v:.1f}" if abs(v) >= 10 else f"{v:.2f}"
    return str(v)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dir")
    ap.add_argument("--series", default="")
    ap.add_argument("--prefix", default="")
    a = ap.parse_args()
    if a.series:
        j = json.load(open(os.path.join(a.dir, a.series + ".json")))
        srv = window_rows(os.path.join(a.dir, a.series + ".cpu.csv"), j["start_unix"], j["end_unix"])
        print("| 分 | ページ毎秒 | p50 | p99 | 最大 | CPU% | RSS (MB) |")
        print("|---|---|---|---|---|---|---|")
        for iv in j["series"]:
            lo, hi = j["start_unix"] + iv["t"] - 60, j["start_unix"] + iv["t"]
            rows = [x for x in srv if lo <= float(x["unix"]) <= hi]
            cpu = (float(rows[-1]["cpu_s"]) - float(rows[0]["cpu_s"])) / max(1, float(rows[-1]["unix"]) - float(rows[0]["unix"])) * 100 if len(rows) > 1 else float("nan")
            rss = float(rows[-1]["rss_mb"]) if rows else float("nan")
            print(f"| {iv['t'] / 60:.0f} | {iv['pages_per_s']} | {iv['p50_ms']} | {iv['p99_ms']} | {iv['max_ms']} | {cpu:.0f} | {rss:.0f} |")
        return
    names = sorted(os.path.basename(p)[:-5] for p in glob.glob(os.path.join(a.dir, a.prefix + "*.json")))
    rows = [summarize(a.dir, n) for n in names]
    print("name\t" + "\t".join(COLS) + "\tfailed\terrors\tconn_errors\tconnect_failures\tnot_started\tlate\tlateness_p99\tservice_p50\tservice_p99")
    for r in rows:
        print(r["name"] + "\t" + "\t".join(fmt(r[c]) for c in COLS) +
              f"\t{r['failed']}\t{r['errors']}\t{r['conn_errors']}\t{r['connect_failures']}\t{r['not_started']}\t{r['late']}\t{r['lateness_p99']}\t{r['service_p50']}\t{r['service_p99']}")
    groups = collections.OrderedDict()
    for r in rows:
        groups.setdefault(re.sub(r"-r\d+", "", r["name"]), []).append(r)
    print()
    print("mean of rounds")
    print("name\tn\t" + "\t".join(COLS))
    for g, rs in groups.items():
        if len(rs) < 2:
            continue
        print(g + f"\t{len(rs)}\t" + "\t".join(fmt(statistics.mean(r[c] for r in rs)) for c in COLS))
    if any("write_pages" in r and r["write_pages"] for r in rows):
        print()
        print("writes")
        ks = ["read_p99", "write_p50", "write_p99", "post_create_p50", "post_create_p99", "comment_add_p50", "comment_add_p99",
              "option_update_p50", "option_update_p99", "post_delete_p50", "post_delete_p99"]
        print("name\t" + "\t".join(ks))
        for r in rows:
            if r["write_pages"]:
                print(r["name"] + "\t" + "\t".join(fmt(r.get(k, 0)) for k in ks))


if __name__ == "__main__":
    main()
