#!/usr/bin/env python3
"""WordPress のプラグインが HeimDB で拒否される SQL を使うかを、ソースから調べる（docs/plugins.md、docs/plan.md の段階 56）。

  scripts/plugin-check/check.py redirection wp-cerber          # wordpress.org の最新バージョンを取ってきて調べる
  scripts/plugin-check/check.py ~/sites/x/wp-content/plugins/acfml  # 手元のディレクトリ（か zip）を調べる

調べるもの（HeimDB が明示的なエラーで拒否するもの。docs/plugins.md の「判定の基準」）:
  - CREATE TABLE の主キー: 整数、CHAR、VARCHAR、BINARY、VARBINARY、DATE、DATETIME、TIMESTAMP、ENUM のどれでもない
    列を含む主キー（docs/plan.md の段階 70）、前方の長さのある主キー、AUTO_INCREMENT の列があるのに主キーがない表
    （主キーのない表は段階 72 から作れる）
  - 生成列（GENERATED ALWAYS AS）
  - utf8mb4（utf8、utf8mb3、ascii を含む）以外の文字セットと照合順序（latin1 など）
  - 全文検索（MATCH ... AGAINST）、ストアドプロシージャ、関数、トリガー、ビュー、イベント
  - サーバのファイルを読み書きする SQL（LOAD DATA、INTO OUTFILE、LOAD_FILE）

拒否されはしないが、ブランチのマージで競合しうる表も「注意」として出す（終了コードには数えない）:
  - mediumint 以下の AUTO_INCREMENT の主キー（本番とブランチが同じ番号を振る。docs/plan.md の段階 61）

ソースの文字列を読むだけなので、実行時に組み立てる SQL は見落とすことがあり（大文字の CREATE TABLE で列の一覧が
読めないものは「表の定義が読めない」として出す）、ALTER TABLE で後から足す列や主キーは調べない。コードで組み立てる
表（Yoast SEO のマイグレーションなど）は見えない。コメントや別の用途の
文字列を拾うこともある。出た候補は目で確かめ、有効化での確かめ（activate.sh）と合わせて判定する。
"""

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.request
import zipfile

CACHE = os.path.join(os.environ.get("TMPDIR", "/tmp"), "heimdb-plugin-check")

INT_TYPES = {"tinyint", "smallint", "mediumint", "int", "integer", "bigint"}
# 整数のほかに主キーに使える型（docs/plan.md の段階 38、70）
PK_TYPES = {"char", "varchar", "binary", "varbinary", "enum", "date", "datetime", "timestamp"}
# ブランチの AUTO_INCREMENT の番号帯がない整数型（int と bigint にはある）
SMALL_INT_TYPES = {"tinyint", "smallint", "mediumint"}
# utf8mb4 として受け入れる文字コード（utf8mb3 は段階 37、ascii は段階 57 から utf8mb4 の同じ名前の照合順序として保存する）
ACCEPTED = {"utf8", "utf8mb3", "utf8mb4", "ascii"}

CREATE_TABLE = re.compile(r"CREATE\s+(?:TEMPORARY\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?", re.I)
# MySQL 8.0 の文字セット（SHOW CHARACTER SET）。変数（$charset_collate）や型の名前を拾わないよう、名前で絞る。
CHARSETS = {
    "armscii8", "ascii", "big5", "binary", "cp1250", "cp1251", "cp1256", "cp1257", "cp850", "cp852", "cp866",
    "cp932", "dec8", "eucjpms", "euckr", "gb18030", "gb2312", "gbk", "geostd8", "greek", "hebrew", "hp8", "keybcs2",
    "koi8r", "koi8u", "latin1", "latin2", "latin5", "latin7", "macce", "macroman", "sjis", "swe7", "tis620", "ucs2",
    "ujis", "utf16", "utf16le", "utf32", "utf8", "utf8mb3", "utf8mb4",
}
CHARSET = re.compile(r"(?<![\w$])(?:CHARACTER\s+SET|CHARSET)\s*=?\s*['\"]?([a-z0-9]+)\b", re.I)
COLLATE = re.compile(r"(?<![\w$])COLLATE\s*=?\s*['\"]?([a-z0-9]+)_\w+", re.I)
OTHERS = [
    ("全文検索", re.compile(r"\bMATCH\s*\([^()]*\)\s*AGAINST\b", re.I)),
    ("ストアドプロシージャなど", re.compile(r"\bCREATE\s+(?:OR\s+REPLACE\s+)?(?:DEFINER\s*=\s*\S+\s+)?(?:PROCEDURE|FUNCTION|TRIGGER|VIEW|EVENT)\s+[`'\"$\w]", re.I)),
    ("ファイルを読み書きする SQL", re.compile(r"\bLOAD\s+DATA\s+(?:LOW_PRIORITY\s+|CONCURRENT\s+)?(?:LOCAL\s+)?INFILE\b|\bINTO\s+(?:OUTFILE|DUMPFILE)\s+['\"$]|\bLOAD_FILE\s*\(", re.I)),
]


def fetch(slug):
    """wordpress.org の最新バージョンを取ってきて、展開したディレクトリとバージョンを返す。取れなければ理由を例外にする。"""
    os.makedirs(CACHE, exist_ok=True)
    url = f"https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&request%5Bslug%5D={slug}"
    try:
        info = json.load(urllib.request.urlopen(url))
    except urllib.error.HTTPError as e:
        info = json.load(e) if e.code == 404 else {"error": str(e)}
    if info.get("closed"):
        raise LookupError(f"wordpress.org で公開が止まっている（{info.get('closed_date', '')}、{info.get('reason', '')}）")
    if "error" in info or not info.get("version"):
        raise LookupError("wordpress.org にない（手元のディレクトリか zip を渡す）")
    version = info["version"]
    dest = os.path.join(CACHE, f"{slug}-{version}")
    if not os.path.isdir(dest):
        zpath = dest + ".zip"
        urllib.request.urlretrieve(f"https://downloads.wordpress.org/plugin/{slug}.latest-stable.zip", zpath)
        with zipfile.ZipFile(zpath) as z:
            z.extractall(dest)
    return os.path.join(dest, slug), version


def local(path):
    """手元のディレクトリか zip を、展開したディレクトリとバージョン（見つからなければ空）にする。"""
    path = os.path.expanduser(path)
    if path.endswith(".zip"):
        dest = os.path.join(CACHE, os.path.basename(path)[:-4])
        if not os.path.isdir(dest):
            with zipfile.ZipFile(path) as z:
                z.extractall(dest)
        subs = [os.path.join(dest, d) for d in os.listdir(dest) if os.path.isdir(os.path.join(dest, d))]
        path = subs[0] if len(subs) == 1 else dest
    version = ""
    for f in sorted(os.listdir(path)):
        if f.endswith(".php"):
            head = open(os.path.join(path, f), encoding="utf-8", errors="replace").read(8192)
            m = re.search(r"^[ \t/*#@]*Version:\s*(\S+)", head, re.M)
            if m and re.search(r"Plugin Name:", head):
                version = m.group(1)
                break
    return path, version


def line_of(s, i):
    return s.count("\n", 0, i) + 1


def in_comment(s, i):
    start = s.rfind("\n", 0, i) + 1
    return re.match(r"\s*(\*|//|#|/\*)", s[start:i]) is not None


def column_list(s, i):
    """s[i] の '(' に対応する ')' までの中身を返す（見つからなければ None）。"""
    depth = 0
    for j in range(i, min(len(s), i + 30000)):
        if s[j] == "(":
            depth += 1
        elif s[j] == ")":
            depth -= 1
            if depth == 0:
                return s[i + 1:j], j
    return None, None


def column_type(body, name):
    m = re.search(r"^\s*[`'\"]?" + re.escape(name) + r"[`'\"]?\s+([a-zA-Z]+)", body, re.I | re.M)
    return m.group(1).lower() if m else None


def merge_notes(body):
    """CREATE TABLE の列の一覧 body から、ブランチのマージで競合しうる理由を返す。"""
    m = re.search(r"^\s*[`'\"]?(\w+)[`'\"]?\s+(tinyint|smallint|mediumint)\b[^,\n]*\bAUTO_INCREMENT\b", body, re.I | re.M)
    if m:
        return [f"{m.group(1)} {m.group(2).lower()} の AUTO_INCREMENT（ブランチと本番が同じ番号を振るので、両側で足すとマージで競合する）"]
    return []


def check_table(body, tail):
    """CREATE TABLE の列の一覧 body と、その後ろ tail（表の文字セット）から、HeimDB が拒否する理由を返す。"""
    problems = []
    pk = re.search(r"PRIMARY\s+KEY\s*(?:[`\w]+\s*)?(?:USING\s+\w+\s*)?\(((?:[^()]|\([^()]*\))*)\)", body, re.I)
    if pk:
        if re.search(r"\(\s*\d+\s*\)", pk.group(1)):
            problems.append(f"主キーに前方の長さ（PRIMARY KEY ({pk.group(1).strip()})）")
        cols = [re.sub(r"\s*\(.*", "", c.strip().strip("`'\"")) for c in pk.group(1).split(",")]
    else:
        inline = re.search(r"^\s*[`'\"]?(\w+)[`'\"]?\s+[^,\n]*?\bPRIMARY\s+KEY\b", body, re.I | re.M)
        cols = [inline.group(1)] if inline else []
    if not cols:
        if re.search(r"^\s*['\"]?\s*\.?\s*\$\w+", body, re.M) or re.search(r"\{\$\w+|\$\w+\s*[,\n]", body):
            problems.append("主キーが見つからない（定義の一部が変数。目で確かめる）")
        elif re.search(r"\bAUTO_INCREMENT\b", body, re.I):
            problems.append("AUTO_INCREMENT の列があるのに主キーがない")
    else:
        types = [(c, column_type(body, c)) for c in cols]
        unknown = [c for c, t in types if t is None]
        if unknown:
            problems.append(f"主キーの列の型がわからない（{', '.join(unknown)}。目で確かめる）")
        else:
            bad = [f"{c} {t}" for c, t in types if t not in INT_TYPES | PK_TYPES]
            if bad:
                problems.append(f"主キーに使えない型の列（{', '.join(bad)}）")
    if re.search(r"\bGENERATED\s+ALWAYS\b|\bAS\s*\((?:[^()]|\([^()]*\))*\)\s*(?:STORED|VIRTUAL)\b", body, re.I):
        problems.append("生成列（GENERATED ALWAYS AS）")
    for text in (body, tail):
        for m in CHARSET.finditer(text):
            if m.group(1).lower() in CHARSETS - ACCEPTED:
                problems.append(f"文字セット {m.group(1)}")
        for m in COLLATE.finditer(text):
            if m.group(1).lower() in CHARSETS - ACCEPTED:
                problems.append(f"照合順序 {m.group(1)}_*")
    return sorted(set(problems))


def scan(path):
    """プラグインのディレクトリを調べて、(表の定義の数, 指摘の一覧, 注意の一覧) を返す。指摘は (ファイル:行, 内容)。"""
    tables = 0
    findings = []
    notes = []
    for root, _, files in os.walk(path):
        for f in sorted(files):
            if not f.endswith((".php", ".sql")):
                continue
            p = os.path.join(root, f)
            rel = os.path.relpath(p, path)
            s = open(p, encoding="utf-8", errors="replace").read()
            for m in CREATE_TABLE.finditer(s):
                if in_comment(s, m.start()) or re.search(r"SHOW\s*$", s[max(0, m.start() - 20):m.start()], re.I):
                    continue
                i = s.find("(", m.end())
                while i > 0 and s[i + 1:i + 2] == ")" and (s[i - 1].isalnum() or s[i - 1] == "_"):
                    i = s.find("(", i + 1)  # 表の名前の中の関数呼び出し（cerber_get_db_prefix()）
                # 大文字の CREATE TABLE で列の一覧が読めないもの（列の一覧が変数、表の名前に引数のある関数呼び出し）は、
                # 黙って飛ばさず、目で確かめる候補として出す。小文字のものは画面の文言などが多いので数えない。
                unreadable = ("表の定義が読めない（列の一覧が変数など。目で確かめる）"
                              if s[m.start():m.start() + 6] == "CREATE" and not re.search(r"\bLIKE\b|\bSELECT\b", s[m.end():m.end() + 120], re.I)
                              else None)
                # 表の名前の部分に、文の続き（LIKE、SELECT）や文の外（エラーの文の中の「create table」）がないこと
                if i < 0 or i - m.end() > 120 or re.search(r"\bLIKE\b|\bSELECT\b|[,;)]", re.sub(r"\w+\(\)", "", s[m.end():i]), re.I):
                    if unreadable:
                        findings.append((f"{rel}:{line_of(s, m.start())}", unreadable))
                    continue
                body, end = column_list(s, i)
                if body is None or end is None or not re.search(r"\w+\s+\w+", body):
                    if unreadable:
                        findings.append((f"{rel}:{line_of(s, m.start())}", unreadable))
                    continue
                tables += 1
                name = re.sub(r"\s+", " ", s[m.end():i]).strip()[:60]
                for problem in check_table(body, s[end:end + 200].split(";")[0]):
                    findings.append((f"{rel}:{line_of(s, m.start())}", f"{name}: {problem}"))
                for note in merge_notes(body):
                    notes.append((f"{rel}:{line_of(s, m.start())}", f"{name}: {note}"))
            for label, pat in OTHERS:
                for m in pat.finditer(s):
                    if not in_comment(s, m.start()) and not re.search(r"SHOW\s*$", s[max(0, m.start() - 20):m.start()], re.I):
                        findings.append((f"{rel}:{line_of(s, m.start())}", f"{label}: {s[m.start():m.end()][:60]}"))
    return tables, findings, notes


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("plugins", nargs="+", help="wordpress.org のスラッグ、または手元のディレクトリか zip")
    ap.add_argument("--quiet", action="store_true", help="指摘のないプラグインを出さない")
    args = ap.parse_args()
    found = False
    for arg in args.plugins:
        if os.path.exists(os.path.expanduser(arg)):
            path, version = local(arg)
        else:
            try:
                path, version = fetch(arg)
            except LookupError as e:
                print(f"{arg}: {e}")
                found = True
                continue
        tables, findings, notes = scan(path)
        slug = os.path.basename(path.rstrip("/"))
        if findings:
            found = True
        elif args.quiet and not notes:
            continue
        print(f"{slug} {version or '?'}: 表の定義 {tables} 個、指摘 {len(findings)} 件、注意 {len(notes)} 件")
        for where, what in findings:
            print(f"  {where}  {what}")
        for where, what in notes:
            print(f"  {where}  注意: {what}")
    sys.exit(1 if found else 0)


if __name__ == "__main__":
    main()
