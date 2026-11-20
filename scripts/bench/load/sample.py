#!/usr/bin/env python3
"""負荷の測り（docs/bench.md の段階 126）で、サーバーのプロセスの CPU とメモリを 1 秒ごとに CSV に書く。

    sample.py --pid <PID> --out cpu.csv [--metrics http://127.0.0.1:9104/metrics --metrics-out m.txt --metrics-every 10]

/proc/<PID>/stat の utime と stime、/proc/<PID>/status の VmRSS、VmHWM、Threads と、/proc/stat のホスト全体の CPU の使われ方を読む。
cpu_pct は CPU 1 つを 100 とした値、host_busy_pct はホストの全部の CPU を 100 とした値。
--pid 0 はホストだけ（負荷のインスタンスが詰まっていないことを示す）。プロセスが消えたら止まる。
--metrics を付けると、その間隔で HeimDB の /metrics の中身を時刻付きで足していく（長い時間の測りで GC とメモリを見る）。
"""
import argparse
import os
import time
import urllib.request

TCK = os.sysconf("SC_CLK_TCK")


def proc_cpu(pid):
    with open(f"/proc/{pid}/stat") as f:
        s = f.read()
    fields = s[s.rindex(")") + 2 :].split()
    return (int(fields[11]) + int(fields[12])) / TCK


def proc_status(pid):
    out = {}
    with open(f"/proc/{pid}/status") as f:
        for line in f:
            k, _, v = line.partition(":")
            if k in ("VmRSS", "VmHWM"):
                out[k] = int(v.split()[0]) / 1024
            elif k == "Threads":
                out[k] = int(v)
    return out


def host_cpu():
    with open("/proc/stat") as f:
        v = [int(x) for x in f.readline().split()[1:]]
    idle = v[3] + v[4]
    return sum(v), idle


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--pid", type=int, required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--interval", type=float, default=1.0)
    ap.add_argument("--metrics", default="")
    ap.add_argument("--metrics-out", default="")
    ap.add_argument("--metrics-every", type=float, default=10.0)
    a = ap.parse_args()
    t0 = time.time()
    last_m = 0.0
    pc0 = proc_cpu(a.pid) if a.pid else 0.0
    ht0, hi0 = host_cpu()
    with open(a.out, "w", buffering=1) as f:
        f.write("unix,t,cpu_s,cpu_pct,rss_mb,hwm_mb,threads,host_busy_pct\n")
        while True:
            time.sleep(a.interval - ((time.time() - t0) % a.interval))
            t = time.time() - t0
            try:
                pc = proc_cpu(a.pid) if a.pid else 0.0
                st = proc_status(a.pid) if a.pid else {}
            except (FileNotFoundError, ProcessLookupError):
                break
            ht, hi = host_cpu()
            busy = 100.0 * (1 - (hi - hi0) / max(1, ht - ht0))
            f.write(
                f"{time.time():.1f},{t:.1f},{pc:.2f},{100 * (pc - pc0) / a.interval:.1f},{st.get('VmRSS', 0):.1f},"
                f"{st.get('VmHWM', 0):.1f},{st.get('Threads', 0)},{busy:.1f}\n"
            )
            pc0, ht0, hi0 = pc, ht, hi
            if a.metrics and t - last_m >= a.metrics_every:
                last_m = t
                try:
                    body = urllib.request.urlopen(a.metrics, timeout=5).read().decode()
                    with open(a.metrics_out, "a") as m:
                        m.write(f"# t={t:.1f}\n")
                        m.write("".join(l + "\n" for l in body.splitlines() if not l.startswith("#")))
                except OSError as e:
                    with open(a.metrics_out, "a") as m:
                        m.write(f"# t={t:.1f} error {e}\n")


if __name__ == "__main__":
    main()
