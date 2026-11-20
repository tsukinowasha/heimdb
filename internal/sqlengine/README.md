# sqlengine

HeimDB の SQL エンジン。[go-mysql-server](https://github.com/dolthub/go-mysql-server) v0.20.0（コミット `5d7248826`、2025-05-13）から派生したもの（フォーク）。

This directory is derived from go-mysql-server v0.20.0 by DoltHub, Inc., licensed under the Apache License 2.0 (see [LICENSE](LICENSE)). All files in this directory have been modified by Tsukinowa Inc.: import paths were changed when the code was imported, and further changes are listed in [docs/engine.md](../../docs/engine.md). Files with changes beyond import paths carry a notice in their header. The original copyright notices are kept.

- 取り込んだ時点では、パスの書き換え（`github.com/dolthub/go-mysql-server` から `github.com/tsukinowasha/heimdb/internal/sqlengine`）と gofmt による import の並べ替えのほかは、元のままにしてある。
- 文字コードの表（`sql/encodings`）は取り込まず、元のモジュール（`github.com/dolthub/go-mysql-server` v0.20.0）から読む。
- 変更の一覧、取り込まなかったもの、上流の修正を取り込む手順は [`docs/engine.md`](../../docs/engine.md)。
- このディレクトリの不具合は、HeimDB の不具合として扱う（上流の go-mysql-server の問題ではない）。
