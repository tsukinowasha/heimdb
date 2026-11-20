#!/usr/bin/env python3
"""ページの表示の時間を、本物の MySQL と heimdb の上の WordPress で比べる（docs/bench.md）。

1 つずつ順にリクエストし、各ページで応答時間、DB にかかった時間（mu-plugin の X-DB-Time）、クエリの数
（X-DB-Queries）の中央値と p95 を出す。管理画面はログインしてから測る。

    scripts/bench/pages.py --newest 5003 --cat 1 [--runs 50]
"""
import argparse
import http.cookiejar
import json
import statistics
import time
import urllib.parse
import urllib.request

TARGETS = {"mysql": "http://localhost:18181", "heimdb": "http://localhost:18182"}


def opener_for(base):
    jar = http.cookiejar.CookieJar()
    op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    op.open(base + "/wp-login.php").read()  # wordpress_test_cookie を受け取る
    data = urllib.parse.urlencode(
        {"log": "admin", "pwd": "adminpw", "wp-submit": "Log In", "redirect_to": base + "/wp-admin/", "testcookie": "1"}
    ).encode()
    op.open(base + "/wp-login.php", data).read()
    if not any(c.name.startswith("wordpress_logged_in_") for c in jar):
        raise SystemExit(f"login failed: {base}")
    return op


def fetch(op, url):
    start = time.perf_counter()
    with op.open(url) as r:
        body = r.read()
        elapsed = time.perf_counter() - start
        return elapsed, float(r.headers.get("X-DB-Time", "nan")), int(r.headers.get("X-DB-Queries", "-1")), r.status, len(body)


def p95(xs):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(0.95 * (len(xs) - 1))))]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--newest", type=int, required=True, help="ID of the newest post (with 60 comments)")
    ap.add_argument("--cat", type=int, required=True, help="category ID with the most posts")
    ap.add_argument("--runs", type=int, default=50)
    ap.add_argument("--warmup", type=int, default=3)
    ap.add_argument("--out", default="")
    args = ap.parse_args()

    pages = [
        ("トップページ", "/", False),
        ("個別の記事（コメント 60 件）", f"/?p={args.newest}", False),
        ("カテゴリの 2 ページ目", f"/?cat={args.cat}&paged=2", False),
        ("検索", "/?s=heimdbneedle", False),
        ("REST API（100 件）", "/?rest_route=/wp/v2/posts&per_page=100", False),
        ("管理画面のダッシュボード", "/wp-admin/", True),
        ("投稿一覧", "/wp-admin/edit.php", True),
    ]
    results = {}
    for name, base in TARGETS.items():
        plain = urllib.request.build_opener()
        admin = opener_for(base)
        for label, path, login in pages:
            op = admin if login else plain
            for _ in range(args.warmup):
                fetch(op, base + path)
            samples = [fetch(op, base + path) for _ in range(args.runs)]
            statuses = {s[3] for s in samples}
            results.setdefault(label, {})[name] = {
                "total_median": statistics.median(s[0] for s in samples),
                "total_p95": p95([s[0] for s in samples]),
                "db_median": statistics.median(s[1] for s in samples),
                "queries": statistics.median(s[2] for s in samples),
                "status": sorted(statuses),
                "bytes": samples[-1][4],
            }
            r = results[label][name]
            print(f"{name:6} {label:24} total {r['total_median']*1000:8.1f} ms (p95 {r['total_p95']*1000:8.1f})  "
                  f"db {r['db_median']*1000:8.1f} ms  queries {r['queries']:5.0f}  status {r['status']}  {r['bytes']} B", flush=True)
    if args.out:
        with open(args.out, "w") as f:
            json.dump(results, f, ensure_ascii=False, indent=2)


if __name__ == "__main__":
    main()
