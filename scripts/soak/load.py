#!/usr/bin/env python3
"""長時間の負荷試験の読み（docs/bench.md の「段階 108」）。WordPress のページを、決まった割合で決まった速さで流す。

前の応答を待たずに 1/rate 秒ごとに要求を出す（開いた形。ただし同時に --workers 本まで）。上限に当たると出すのが
遅れて late に出し、60 秒待っても出せなければ skipped にする。要求ごとに 1 行を TSV に書く: 時刻、ページ、状態、全体の秒、DB の秒（mu-plugin の X-DB-Time）、
文の数、出すのが遅れた秒。

    scripts/soak/load.py --base http://localhost:18183 --rate 6 --hours 24 --out out/load.tsv
"""
import argparse
import concurrent.futures
import http.cookiejar
import json
import random
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

# ページと重み。管理画面はログインして開く。
PAGES = [
    ("front", "/", False, 25),
    ("single", None, False, 30),  # 記事は起動時に REST で集めた ID（古い順。試験で消す記事を除く）から選ぶ
    ("category", "/?cat={cat}&paged=2", False, 15),
    ("search", "/?s=heimdbneedle", False, 5),
    ("rest", "/?rest_route=/wp/v2/posts&per_page=10", False, 10),
    ("dashboard", "/wp-admin/", True, 5),
    ("edit", "/wp-admin/edit.php", True, 5),
    ("comments", "/wp-admin/edit-comments.php", True, 5),
]


def login(base):
    jar = http.cookiejar.CookieJar()
    op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    op.open(base + "/wp-login.php", timeout=60).read()
    data = urllib.parse.urlencode(
        {"log": "admin", "pwd": "adminpw", "wp-submit": "Log In", "redirect_to": base + "/wp-admin/", "testcookie": "1"}
    ).encode()
    op.open(base + "/wp-login.php", data, timeout=60).read()
    if not any(c.name.startswith("wordpress_logged_in_") for c in jar):
        raise SystemExit(f"login failed: {base}")
    return op


def post_ids(base, pages):
    ids = []
    for page in range(1, pages + 1):
        url = f"{base}/?rest_route=/wp/v2/posts&per_page=100&_fields=id&orderby=id&order=asc&page={page}"
        try:
            with urllib.request.urlopen(url, timeout=120) as r:
                got = [p["id"] for p in json.load(r)]
        except urllib.error.HTTPError as e:
            if e.code == 400:  # 記事の数より先のページ
                break
            raise
        ids += got
        if len(got) < 100:
            break
    return ids


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://localhost:18183")
    ap.add_argument("--rate", type=float, default=6.0, help="requests per second")
    ap.add_argument("--hours", type=float, default=24.0)
    ap.add_argument("--cat", type=int, default=1)
    ap.add_argument("--workers", type=int, default=32)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    plain = urllib.request.build_opener()
    admin = login(args.base)
    ids = post_ids(args.base, 10)
    weights = [w for *_, w in PAGES]
    out = open(args.out, "a", buffering=1)
    lock = threading.Lock()
    inflight = threading.Semaphore(args.workers)

    def one(label, path, op, due):
        late = max(0.0, time.time() - due)
        start = time.perf_counter()
        status, dbt, nq = 0, "", ""
        try:
            with op.open(args.base + path, timeout=120) as r:
                r.read()
                status, dbt, nq = r.status, r.headers.get("X-DB-Time", ""), r.headers.get("X-DB-Queries", "")
        except urllib.error.HTTPError as e:
            status = e.code
        except Exception as e:  # 接続の失敗や時間切れ
            status = f"err:{type(e).__name__}"
        elapsed = time.perf_counter() - start
        with lock:
            out.write(f"{due:.3f}\t{label}\t{status}\t{elapsed:.4f}\t{dbt}\t{nq}\t{late:.3f}\n")
        inflight.release()

    pool = concurrent.futures.ThreadPoolExecutor(max_workers=args.workers)
    rnd = random.Random(108)
    start = time.time()
    end = start + args.hours * 3600
    n = 0
    while True:
        due = start + n / args.rate
        if due >= end:
            break
        now = time.time()
        if due > now:
            time.sleep(due - now)
        label, path, needs_login, _ = rnd.choices(PAGES, weights)[0]
        if label == "single":
            path = f"/?p={rnd.choice(ids)}"
        path = path.format(cat=args.cat)
        if not inflight.acquire(timeout=60):
            with lock:
                out.write(f"{due:.3f}\t{label}\tskipped\t\t\t\t{time.time() - due:.3f}\n")
            n += 1
            continue
        pool.submit(one, label, path, admin if needs_login else plain, due)
        n += 1
    pool.shutdown(wait=True)
    print(f"sent {n} requests", file=sys.stderr)


if __name__ == "__main__":
    main()
