#!/usr/bin/env python3
"""HeimDB のログ（標準入力）から、文のエラーを「エラー」と「文」の組で数えて出す（activate.sh が使う）。

表の有無を確かめる文（dbDelta の DESCRIBE、SHOW ... LIKE など）の「table not found」は MySQL でも同じエラーになる
ので、件数だけを最後に出す。それ以外のエラーがあれば終了コード 1。

  --tsv: 「回数、エラー、文」をタブで区切って出す（compare.sh が MySQL のエラーと比べる。表の有無の確かめは出さない）。
"""

import collections
import re
import sys

FIELD = re.compile(r'(\w+)=("(?:\\.|[^"\\])*"|\S+)')
PROBE = re.compile(r"^\s*(DESCRIBE|DESC|SHOW|SELECT\s+1\s+FROM|SELECT\s+COUNT)", re.I)


def unquote(v):
    if v.startswith('"'):
        return re.sub(r'\\(.)', r'\1', v[1:-1])
    return v


def main():
    tsv = sys.argv[1:] == ["--tsv"]
    errors = collections.Counter()
    probes = 0
    for line in sys.stdin:
        if 'msg="error running query"' not in line:
            continue
        f = {k: unquote(v) for k, v in FIELD.findall(line)}
        err, query = f.get("error", ""), re.sub(r"\s+", " ", f.get("query", "")).strip()
        if err.startswith("table not found") and PROBE.match(query):
            probes += 1
            continue
        errors[(err, query[:300])] += 1
    if tsv:
        for (err, query), n in errors.most_common():
            print(f"{n}\t{re.sub(r'\s+', ' ', err)}\t{query}")
        return
    for (err, query), n in errors.most_common():
        print(f"  {n} 回: {err}")
        print(f"      文: {query}")
    if probes:
        print(f"  （表の有無の確かめで table not found: {probes} 回。MySQL でも同じ）")
    sys.exit(1 if errors else 0)


if __name__ == "__main__":
    main()
