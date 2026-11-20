#!/usr/bin/env python3
"""capture-writes.php で取った書き込みの文から、loadgen の書き込みのページ（pages-write.json）を作る（docs/bench.md の段階 126）。

    mkwrites.py writes-capture.json > pages-write.json

最初の回はタグを作る文を含むので、--iter の回（既定 3）を型にし、その次の回も同じ型になることを確かめる。
回ごとに変わる値を loadgen の変数にする: 日時は {{now}}、題名などの番号は {{u}}、記事の ID は {{post}}（作成の INSERT の
LAST_INSERT_ID）、コメントの ID は {{comment}}、作成で足すメタの ID は {{meta1}} と {{meta2}}。数え直しの UPDATE の値は、
直前の SELECT COUNT(*) の答え（{{cntN}}）にする。タグの bench-a と bench-b は、setup で決まった ID（900001 と 900002）で作る。
"""
import argparse
import json
import re
import sys

TAG_IDS = {"bench-a": 900001, "bench-b": 900002}
KINDS = [("post_create", "post_create"), ("comment_add", "comment_add"), ("option_update", "option_update"), ("post_delete", "post_delete")]


def num(n):
    return re.compile(r"(?<![\d.])" + re.escape(str(n)) + r"(?![\d.])")


def template(it):
    subs = [
        (re.compile(re.escape(it["post_date"])), "{{now}}"),
        (re.compile(re.escape(it["post_date_gmt"])), "{{now}}"),
        (re.compile(re.escape(it["comment_date"])), "{{now}}"),
        (re.compile(r"9000000" + str(it["i"]) + r"(?!\d)"), "{{u}}"),
        (num(it["comment"]), "{{comment}}"),
        (num(it["post"]), "{{post}}"),
    ]
    for slug, (term_id, tt_id) in it["tags"].items():
        subs.append((num(tt_id), str(TAG_IDS[slug])))
        if term_id != tt_id:
            subs.append((num(term_id), str(TAG_IDS[slug])))
    pages = []
    meta = 0
    cnt = 0
    for key, name in KINDS:
        steps = []
        stmts = it[key]
        for s in stmts:
            for rx, rep in subs:
                s = rx.sub(rep, s)
            step = {"sql": s}
            if key == "post_create" and s.startswith("INSERT INTO `wp_posts`"):
                step["save_id"] = "post"
            if key == "comment_add" and s.startswith("INSERT INTO `wp_comments`"):
                step["save_id"] = "comment"
            if key == "post_create" and s.startswith("INSERT INTO `wp_postmeta`"):
                meta += 1
                step["save_id"] = f"meta{meta}"
            m = re.match(r"(UPDATE `wp_(?:term_taxonomy|posts)` SET `(?:count|comment_count)` = )(\d+)( WHERE .*)", s, re.S)
            if m and steps and steps[-1]["sql"].startswith("SELECT COUNT(*)"):
                cnt += 1
                steps[-1]["save"] = [f"cnt{cnt}"]
                step["sql"] = m.group(1) + "{{cnt%d}}" % cnt + m.group(3)
            steps.append(step)
        pages.append({"name": name, "kind": "write", "steps": steps})
    # 削除のページのメタの ID は、作成の INSERT で取っておいたもの（作った順に番号が増える）。
    meta_ids = sorted({int(x) for x in re.findall(r"WHERE meta_id = (\d+)", " ".join(it["post_delete"]))})
    for p in pages:
        for st in p["steps"]:
            for j, mid in enumerate(meta_ids):
                st["sql"] = num(mid).sub("{{meta%d}}" % (j + 1), st["sql"])
    if len(meta_ids) != meta:
        raise SystemExit(f"meta ids {meta_ids} but {meta} meta inserts")
    return pages


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("capture")
    ap.add_argument("--iter", type=int, default=3)
    a = ap.parse_args()
    cap = json.load(open(a.capture))
    pages = template(cap[a.iter])
    if a.iter + 1 < len(cap) and template(cap[a.iter + 1]) != pages:
        raise SystemExit("iterations differ after templating")
    for p in pages:
        for st in p["steps"]:
            left = re.findall(r"(?<![\d.])\d{4,}(?![\d.])", st["sql"])
            left = [x for x in left if int(x) not in TAG_IDS.values() and x not in ("1000",)]
            if left:
                print(f"note: {p['name']}: numbers left {left}: {st['sql'][:120]}", file=sys.stderr)
    out = {
        "session": [
            "SET NAMES 'utf8mb4' COLLATE 'utf8mb4_unicode_520_ci'",
            "SET SESSION sql_mode = 'NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'",
        ],
        "setup": [
            {
                "check": f"SELECT term_id FROM wp_terms WHERE term_id = {tid}",
                "steps": [
                    {"sql": f"INSERT INTO `wp_terms` (`term_id`, `name`, `slug`, `term_group`) VALUES ({tid}, '{slug}', '{slug}', 0)"},
                    {"sql": f"INSERT INTO `wp_term_taxonomy` (`term_taxonomy_id`, `term_id`, `taxonomy`, `description`, `parent`, `count`) VALUES ({tid}, {tid}, 'post_tag', '', 0, 0)"},
                ],
            }
            for slug, tid in TAG_IDS.items()
        ]
        + [
            {
                "check": "SELECT option_id FROM wp_options WHERE option_name = 'bench_option'",
                "steps": [{"sql": "INSERT INTO `wp_options` (`option_name`, `option_value`, `autoload`) VALUES ('bench_option', 'value 0', 'auto')"}],
            }
        ],
        "pages": pages,
    }
    json.dump(out, sys.stdout, indent=1, ensure_ascii=False)
    print()


if __name__ == "__main__":
    main()
