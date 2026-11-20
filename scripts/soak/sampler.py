#!/usr/bin/env python3
"""長時間の負荷試験の記録（docs/bench.md の「段階 108」）。HeimDB と同じマシンで、1 分ごとに本番とレプリカの
/metrics、プロセスの RSS、CPU の時間、スレッド、開いたファイル、goroutine、データ（WAL と sstable）とバックアップの
大きさを JSON の 1 行にする。goroutine は、pprof を足した試験用の建て方（HEIMDB_PPROF_ADDR。リポジトリの HeimDB には
ない）でだけ取れる。バックアップの大きさは、本番の sstable とのハードリンクも数える（実際の使用量は df_used）。10 分ごとに、本番の wp_options の行数と autoload の内訳、alloptions の文の計画も取る。

    SOAK_DIR=/srv/soak scripts/soak/sampler.py --hours 25 --out out/samples.jsonl
"""
import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.request

ROLES = {
    "primary": {"metrics": 19104, "pprof": 19106, "port": 3356},
    "replica": {"metrics": 19105, "pprof": 19107, "port": 3357},
}
LINE = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)")
# WordPress が接続ごとに付ける sql_mode（MySQL 8.0 の既定から、WordPress が嫌うものを外した形）
WP_SQL_MODE = "NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"
ALLOPTIONS = "SELECT option_name, option_value FROM wp_options WHERE autoload IN ('yes','on','auto','auto-on')"


def get(url, timeout=10):
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return r.read().decode()


def metrics(port):
    out = {}
    for line in get(f"http://127.0.0.1:{port}/metrics").splitlines():
        m = LINE.match(line)
        if not m or line.startswith("#"):
            continue
        name, labels, value = m.groups()
        if name.endswith("_bucket"):
            continue
        try:
            out[name + (labels or "")] = float(value)
        except ValueError:
            pass
    return out


def proc(pid):
    out = {}
    try:
        with open(f"/proc/{pid}/status") as f:
            for line in f:
                k, _, v = line.partition(":")
                if k in ("VmRSS", "VmHWM"):
                    out[k] = int(v.split()[0]) * 1024
                elif k == "Threads":
                    out[k] = int(v)
        out["fds"] = len(os.listdir(f"/proc/{pid}/fd"))
        with open(f"/proc/{pid}/stat") as f:
            st = f.read().rsplit(")", 1)[1].split()
        out["cpu_s"] = (int(st[11]) + int(st[12])) / os.sysconf("SC_CLK_TCK")  # utime と stime
    except FileNotFoundError:
        # /proc のない macOS（手元の試し）では ps と lsof で取る
        try:
            rss = subprocess.run(["ps", "-o", "rss=", "-p", str(pid)], capture_output=True, text=True).stdout.strip()
            out["VmRSS"] = int(rss) * 1024 if rss else None
            lsof = subprocess.run(["lsof", "-p", str(pid)], capture_output=True, text=True).stdout
            out["fds"] = max(0, len(lsof.splitlines()) - 1)
        except Exception:
            pass
    return out


def goroutines(port):
    try:
        first = get(f"http://127.0.0.1:{port}/debug/pprof/goroutine?debug=1").split("\n", 1)[0]
        return int(first.rsplit(" ", 1)[1])
    except Exception:
        return None


def du(path):
    try:
        r = subprocess.run(["du", "-sk", path], capture_output=True, text=True)
        return int(r.stdout.split()[0]) * 1024
    except Exception:
        return None


def mysql(port, sql):
    r = subprocess.run(
        ["mysql", "-h127.0.0.1", f"-P{port}", "-uroot", "-prootpw", "-N", "-B", "wpbench", "-e", sql],
        capture_output=True, text=True, timeout=60,
    )
    return r.stdout.strip(), r.stderr.strip().replace("mysql: [Warning] Using a password on the command line interface can be insecure.", "").strip()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--hours", type=float, default=25)
    ap.add_argument("--interval", type=float, default=60)
    ap.add_argument("--plan-every", type=int, default=10, help="take the wp_options numbers every N samples")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    d = os.environ["SOAK_DIR"]
    out = open(args.out, "a", buffering=1)
    end = time.time() + args.hours * 3600
    n = 0
    while time.time() < end:
        t0 = time.time()
        row: dict = {"t": round(t0, 3)}
        for role, cfg in ROLES.items():
            r = {}
            try:
                r["m"] = metrics(cfg["metrics"])
            except Exception as e:
                r["err"] = f"metrics: {e}"
            try:
                with open(f"{d}/{role}.pid") as f:
                    r["proc"] = proc(int(f.read()))
            except Exception as e:
                r["err_proc"] = str(e)
            r["goroutines"] = goroutines(cfg["pprof"])
            r["du_data"] = du(f"{d}/{role}")
            try:
                # Pebble の WAL（memtable を書き出すまで残る）と sstable
                names = os.listdir(f"{d}/{role}")
                r["wal_bytes"] = sum(os.path.getsize(f"{d}/{role}/{n}") for n in names if n.endswith(".log"))
                r["sst_bytes"] = sum(os.path.getsize(f"{d}/{role}/{n}") for n in names if n.endswith(".sst"))
            except OSError:
                pass
            row[role] = r
        row["du_backups"] = du(f"{d}/backups")
        disk = shutil.disk_usage(d)
        row["df_used"] = disk.used
        if n % args.plan_every == 0:
            # 1 回の失敗で記録を止めない（時間切れ、思わぬ行）
            try:
                port = ROLES["primary"]["port"]
                counts, err = mysql(port, "SELECT autoload, COUNT(*) FROM wp_options GROUP BY autoload ORDER BY autoload")
                row["options"] = dict(line.split("\t", 1) for line in counts.splitlines() if "\t" in line) if counts else err
                plan, err = mysql(port, f"SET SESSION sql_mode = '{WP_SQL_MODE}'; EXPLAIN PLAN {ALLOPTIONS}")
                row["alloptions_plan"] = plan or err
                status, err = mysql(port, "CALL heimdb_status()")
                row["status"] = dict(line.split("\t", 1) for line in status.splitlines() if "\t" in line) if status else err
            except Exception as e:
                row["err_plan"] = f"{type(e).__name__}: {e}"
        out.write(json.dumps(row) + "\n")
        n += 1
        time.sleep(max(0, args.interval - (time.time() - t0)))
    print(f"{n} samples", file=sys.stderr)


if __name__ == "__main__":
    main()
