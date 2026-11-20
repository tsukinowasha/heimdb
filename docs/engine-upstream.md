# 上流の修正の一覧（段階 110）

HeimDB のエンジン（`internal/sqlengine`、go-mysql-server v0.20.0 の `5d7248826` のフォーク）の後に、上流の main に合併した PR をすべて見て、HeimDB に効くかを分けた記録。手順と判定の決まりは [`plan.md`](plan.md) の段階 110、エンジンの変更の一覧は [`engine.md`](engine.md)。

## 見た範囲

| | |
| --- | --- |
| 上流の main | `c57d5b4c1`（2026-11-09） |
| PR | 2025-05-13 から後に合併した 879 本（#2103〜#4136）。PR を経ずに入った 4 コミットは、入れてすぐ戻した 2 組 |
| vitess | 範囲の外。HeimDB は `v0.0.0-20250512224608-8fb9c6ea092c` のまま（上流の main は `v0.0.0-20261106002823-dae4b00ae0a4`）。解析器の panic は段階 105 から構文エラーになる |
| 依存 | `go.mod` の差分を見た。HeimDB が古いのは go-icu-regex、jsonpath、vitess だけ。go-icu-regex の `REGEXP_REPLACE` の読み過ぎ（5448fed）は HeimDB の wasm のバージョンにもあるが、wasm の中に閉じ、返す長さは正しい。プロセスが落ちた dolt#11183 は cgo のバージョンの並列の GROUP BY で、HeimDB に当たらない。WordPress も `REGEXP_REPLACE` を使わない |
| 見た日 | 2026-11-10 |

## 分け方

- 取り込んだ（72）: 段階 111 で 16 本、段階 112 で 13 本（#3211 の続きの #3361 を含む。#3361 は段階 110 で「再現しない」にしていたが、#3211 だけを当てると NOT IN（空）の行を落とすので一緒に入れた）、段階 113 で 11 本、段階 114 で 23 本、段階 115 で 9 本（#3110 の続きの #3141 を含む。#3141 も段階 110 で「再現しない」にしていたが、#3110 だけを当てると 3 つの UNION の列が入れ替わるので一緒に入れた）を取り込んだ。どう取り込んだかは [`engine.md`](engine.md) の変更の一覧の 111、112、113、114、115 の行。
- 取り込む候補（0）: HeimDB で再現し、WordPress かプラグインが出しうる。漏れ、ロック、誤った書き込み、権限の抜け、プロセスが落ちる panic は文の形を問わない。段階 114 の後に残った 8 本（計画に触る #3191、#3960、#4054 と、SQL の形の直しの #3052、#3060、#3073、#3110、#3260）は段階 115 ですべて取り込んだ。
- 見送った（62）: HeimDB で再現するが、WordPress が出さない文。再現の文がないか、SQL で確かめられないものも含む。
- 再現しない（49）: 再現の文を HeimDB と本物の MySQL 8.0.46 に流して同じ結果。45 本の文（と、段階 112 と 115 で取り込んだ #3361 と #3141 の文）は `internal/testdata/stage110_golden.txt`（`TestStage110UpstreamNotReproduced`）にある。ほかは、取り込み済み（#3116）、MySQL と同じ文が残らない（#3045。違いは PR と関係ない DEFAULT NULL の表示）、権限の文で別のユーザーの接続が要る（#3941、#4072。手で確かめた）。
- 効かない（696）: 読むだけで落とせる。変えたファイルがテスト、`memory/`、文書、`go.mod` だけのもの（181）、機能の追加、Doltgres だけ、性能だけ、整理、WordPress が使わない機能（窓関数、CTE、JSON、トリガ、空間、外部キー、生成列など）、フォークにないコード、HeimDB が拒否する文、MySQL も拒否する文。

「再現する」は、再現の文を新しい HeimDB（TCP、`serve()`）と MySQL のデータベースに流して比べた結果で決めた。memory のエンジンでの再現は使っていない。文の後にエンジンの `MemoryManager` にキャッシュが残れば漏れとした。取り込む候補と見送ったの、最後に流した文と両側の結果は `internal/testdata/stage110_candidates.json`（段階 111 の入力）。

## 段階 40 の 3 件

- ENUM の値のクォート: #2995。段階 111 で取り込んだ（`'` を含む ENUM の CREATE TABLE が 1105 で失敗し、表が残って DROP TABLE でも消せなかった。`\` を含む値は読み直すと別の値になった。`\` のエスケープは HeimDB が足した）。
- LIKE のエスケープ: #3918。再現しない（HeimDB は構文解析の後に `keepLikeEscapes` で範囲に置き換えない）。題で見つかる #3784 は `Like.String()` に ESCAPE を出すだけの別の修正。
- ゼロの日時: #3305（関数の値）と #3476（精度の表示）は再現する。#3305 は段階 113 で取り込んだ（ゼロの時刻の内部の値は変えず、関数の側で見分ける）。#3476 は段階 114 で取り込んだ（型に書いた精度の桁で表示し、式の結果の型は値の桁のまま）。

## PR ごと

| PR | 合併 | 分類 | 題 | 理由 |
| --- | --- | --- | --- | --- |
| #2985 | 2025-05-20 | 取り込んだ（段階 114） | fix default value for non null enum columns | NOT NULL で既定値のない ENUM の列を省いた INSERT が、既定の sql_mode で HeimDB は 1105 `Field 'e' doesn't have a default value`、MySQL は 1 行入り 'abc'。WordPress の sql_mode でも HeimDB は '' を入れて警告 1364、MySQL は警告なしで 'abc'。 |
| #2995 | 2025-05-28 | 取り込んだ（段階 111） | Fixed enum escaping issue | ENUM('it''s','b') の CREATE TABLE が HeimDB は 1105 `HeimDB: column t.e: parse type "enum('it's','b') ...": syntax error`（MySQL は通る）。しかも表は作られて残り、SHOW CREATE、SELECT、DROP TABLE もすべて同じ 1105 になる。\ を含む値（ENUM('a\\b')）も型の文字列に \ がそのまま入り、読み直すと値が変わって INSERT 'a\\b' が 1105（MySQL は 615C62）。 |
| #2997 | 2025-05-29 | 取り込んだ（段階 114） | Properly escape special characters in comments | SHOW CREATE TABLE が HeimDB は COMMENT 'it's'、'a\b<改行>c'、COMMENT='x\y' と生のまま出し、MySQL は 'it''s'、'a\\b\nc'、'x\\y' とエスケープする。出力を読み直すと構文の誤りになる。 |
| #3000 | 2025-05-30 | 取り込んだ（段階 111） | make update join case insensitive | UPDATE MiXeDcAsE SET j = 999 WHERE i IN (SELECT 1) が、同じ書き方の表名でも HeimDB は affected=0 で何も変えず、MySQL は 1 行更新。wp_Posts のような大文字の接頭辞でも同じ。 |
| #3025 | 2025-06-11 | 取り込んだ（段階 114） | Convert if.Eval result to correct type | SELECT IF(c0 = 123, TRUE, c0) AS ref0, MIN(c0) ... GROUP BY ref0 が、HeimDB は (1,1),(1,123) の 2 グループ、MySQL は (1,1) の 1 グループ（true と 1 を別の値にしている）。 |
| #3030 | 2025-06-13 | 取り込んだ（段階 113） | Distinguish nils in firstBuffer | GROUP BY の集計しない値が、最初の行の NULL の後の行の値で上書きされる。WordPress の sql_mode で SELECT k, a FROM g GROUP BY k（行 (NULL,1,1),(5,2,1)）が HeimDB は 5、MySQL は NULL。上流の例でも HeimDB 1、MySQL NULL。 |
| #3052 | 2025-06-27 | 取り込んだ（段階 115） | Use IsJoin flag to check if keyless tables are actually allowed | 主キーのない表への UPDATE t SET i = 10 WHERE i IN (SELECT 1) が、HeimDB は 1105 `unsupported feature: error: keyless tables unsupported for UPDATE JOIN`、MySQL は 1 行更新。 |
| #3060 | 2025-07-02 | 取り込んだ（段階 115） | Merge scopeColumn types in SetOp scope | SELECT * FROM (SELECT e FROM enum_table UNION SELECT v FROM uv) sq が、HeimDB は 1105 `value bug is not valid for this Enum`（左の ENUM の型に揃える）、MySQL は 5 行。NULL の扱い（WHERE e IS NULL の数）は同じ 1。 |
| #3068 | 2025-07-07 | 取り込んだ（段階 114） | dolthub/dolt#9426 - Support enum string context in functions | ENUM の列で e LIKE 'a%' が HeimDB 0（MySQL 1）、CONCAT(e,'test') が '1test'（MySQL 'abctest'）、LENGTH(e) が 1（MySQL 3）、GROUP_CONCAT(e) が '1,2,3'（MySQL 'abc,defg,hjikl'）。内部の番号で扱っている。 |
| #3073 | 2025-08-14 | 取り込んだ（段階 115） | Allow select aliases to be in group by/having | SELECT y AS z FROM xy GROUP BY (y) HAVING AVG(z) > 0 が HeimDB は 1105 `column "z" could not be found in any table in scope`、MySQL は 1,2,3。 |
| #3078 | 2025-07-14 | 取り込んだ（段階 114） | dolthub/dolt#8893 - Fix charset validation to match MySQL behavior for issue #8893 | WordPress の sql_mode で INSERT ... CONCAT('ab', UNHEX('FF'), 'cd') が、HeimDB は 1105 `invalid string for charset utf8mb4: '[255]'` で入らず、MySQL は 'ab' に切り詰めて警告 1366 `Incorrect string value: '\xFFcd' for column 's' at row 1`。STRICT でも HeimDB 1105、MySQL 1366。 |
| #3103 | 2025-08-01 | 取り込んだ（段階 111） | dolthub/dolt#9530 - Fix auto-increment overflow handling | BIGINT UNSIGNED の 4294967296 を INT UNSIGNED に INSERT ... SELECT すると、非 STRICT で HeimDB は 0、MySQL は 4294967295。STRICT でも HeimDB は 1 行入り、MySQL はエラー 1264。 |
| #3110 | 2025-07-21 | 取り込んだ（段階 115） | Use child column id's for union when assigning exec indexes | SELECT * FROM (SELECT 'parent' AS tbl, id, words FROM t UNION SELECT 'child', id, words FROM t2) AS combined WHERE combined.id = 1 が、HeimDB は空、MySQL は 2 行。 |
| #3134 | 2025-08-04 | 取り込んだ（段階 111） | Properly convert enum/set columns to string columns with alter table | ENUM と SET の列を ALTER TABLE ... MODIFY で VARCHAR にすると、HeimDB は値が番号に変わる（(1,'1','3'),(2,'2','2')）、MySQL は (1,'abc','x,y'),(2,'def','y')。データが壊れる。 |
| #3135 | 2025-08-01 | 取り込んだ（段階 112） | include null values when evaluating anti joins created from `WHERE NOT EXISTS` | 右の相関の列に NULL がある NOT EXISTS (SELECT 1 FROM ab_hasnull a WHERE a.b = x.y) が、HeimDB は空、MySQL は (1,0),(3,NULL)。計画は LeftOuterHashJoinExcludeNulls と a.b IS NULL で、右に NULL が 1 つあるとすべて落ちる。WP_Tax_Query の NOT EXISTS の形（列は NOT NULL）は MySQL と同じ。 |
| #3174 | 2025-08-25 | 取り込んだ（段階 114） | Make `SHOW VARIABLES` display boolean values as ON/OFF to match MySQL | SHOW VARIABLES LIKE 'autocommit' と 'foreign_key_checks' が HeimDB は 1、MySQL は ON。 |
| #3175 | 2025-08-25 | 取り込んだ（段階 114） | Support filtering by comment in `show table status` | SHOW TABLE STATUS WHERE Comment = 'hello' が HeimDB は 1105 `column "Comment" could not be found in any table in scope`（列名が Comments）、MySQL は 1 行。LIKE の結果でも HeimDB は最後の列が NULL、MySQL は 'hello'。information_schema.TABLES の TABLE_COMMENT は HeimDB でも 'hello'。 |
| #3191 | 2025-09-03 | 取り込んだ（段階 115） | Use Decimal.String() for key in hashjoin lookups | DECIMAL の列の等号のハッシュ結合（/*+ HASH_JOIN(t1, t2) */）が、HeimDB は空、MySQL は (30.000, 30.00)。(u1.c0 % 2) = (u2.c0 % 2) のハッシュ結合も HeimDB は 1 行（MySQL は 2 行）。ヒントのないときは HeimDB も別の結合を選んで同じ。 |
| #3200 | 2025-09-09 | 取り込んだ（段階 112） | implement double truncation and find correct hash comparison type | BOOLEAN の列と VARCHAR の列の等号（WHERE t1.c2 = t0.c1 と JOIN ... ON）が、HeimDB は (0,'z') だけ、MySQL は 'g'、'O5'、'Qo9'、'z' の 4 行（数にできない文字列は 0）。ハッシュ結合で左の型に揃えている。 |
| #3211 | 2025-09-11 | 取り込んだ（段階 112） | Do not return EOF in existsIter when right iter is empty | NOT EXISTS (SELECT 1 FROM ab_hasnull WHERE b <> xy_hasnull.y) が HeimDB は空、MySQL は 3（y が NULL の行は条件が NULL で一致しない）。計画は AntiJoin で、条件が NULL の組を一致として扱う（上流の nullRej の判定の逆転）。右が空の NOT EXISTS は HeimDB も MySQL と同じ（NULL,NULL）。 |
| #3212 | 2025-09-11 | 取り込んだ（段階 112） | Wrap right side in Distinct when converting semi-joins to inner joins | 半結合を内部結合に変えたとき右の重複を除かない。SELECT p.ID FROM p WHERE p.ID IN (SELECT post_id FROM pm2 WHERE meta_value = 'red' OR meta_value = 'blue')（wp_postmeta の形、post 1 に 2 行）が HeimDB は 1,1,2、MySQL は 1,2。EXISTS (SELECT 1 FROM pm WHERE pm.post_id = p.ID) と上流の例も重複する。meta_key = '_color' の 1 つの等号や WP_Tax_Query の EXISTS の形では重複しなかった（計画による）。 |
| #3215 | 2025-09-15 | 取り込んだ（段階 114） | Trim strings before converting to bool | '3bxu' OR FALSE、'3bxu' AND TRUE、' 1' AND TRUE が HeimDB は 0、MySQL は 1。WHERE s（'1a','0a','a'）が HeimDB は空、MySQL は '1a'。 |
| #3260 | 2025-10-09 | 取り込んだ（段階 115） | dolthub/go-mysql-server#3259: Fix system variable lookup to only happen on no qualifier | 存在しない列の A.timestamp、A.version、UPPER(A.sql_mode) が、HeimDB はシステム変数の値（1000、8.0.31、sql_mode）を返し、MySQL は 1054 Unknown column。 |
| #3263 | 2025-10-14 | 取り込んだ（段階 112） | Do not prune tables in semi-joins | SELECT * FROM (SELECT id, name, description, archived FROM t0 AS test0 WHERE (id IN (SELECT id FROM t0))) AS dummy_alias ORDER BY dummy_alias.name が HeimDB は 1105 `HeimDB: internal error: runtime error: index out of range [5] with length 4`（panic）、MySQL は 1 行。 |
| #3276 | 2025-10-21 | 取り込んだ（段階 114） | Truncate strings for datetime conversion | CAST('2020-01-01 a' AS DATETIME) などが HeimDB は NULL、MySQL は 2020-01-01 00:00:00 など。INSERT '2020-01-01 a' は既定の sql_mode で HeimDB 1105、MySQL は警告 1265 で 2020-01-01 00:00:00 を保存。WordPress の sql_mode では HeimDB は 0000-00-00 00:00:00 を保存（警告 1265 は同じ）。 |
| #3277 | 2025-10-22 | 取り込んだ（段階 111） | dolthub/dolt#9984: Fix `nil` dereference by `Dispose()` in `GROUP BY` iterator when `Next()` is never called | SELECT SUM(value) FROM test_table LIMIT 0 と SELECT COUNT(*) FROM test_table LIMIT 0 が HeimDB は 1105 `HeimDB: internal error: runtime error: invalid memory address or nil pointer dereference`（panic）、MySQL は空。GROUP BY のある LIMIT 0 は同じ。キャッシュは 0。 |
| #3280 | 2025-10-28 | 取り込んだ（段階 114） | Allow string truncation when casting to `date` | CAST('2020-01-01 a' AS DATE) は HeimDB が NULL、MySQL は 2020-01-01（警告）。WordPress の sql_mode で DATE の列に '2020-01-01 a' を入れると HeimDB は 0000-00-00、MySQL は 2020-01-01。既定の sql_mode の INSERT は両方エラー（HeimDB 1105、MySQL 1292）。 |
| #3302 | 2025-11-12 | 取り込んだ（段階 113） | Wrap nullable hoisted filter in IsTrue | NOT EXISTS (SELECT 1 FROM (SELECT 1) alias0 WHERE CASE -1 WHEN t0.c1 THEN false END) が HeimDB は空、MySQL は (1,-2)。EXPLAIN PLAN で外の列だけの条件が NOT(EXISTS ... AND CASE ...) に持ち上げられ、NULL で行が落ちている。WEEKDAY の形も同じ。 |
| #3305 | 2025-11-18 | 取り込んだ（段階 113） | Fix datetime functions to return correct results for `0` and `false` | WordPress の sql_mode で DAY('0000-00-00') が HeimDB 1、MySQL 0。DAYNAME、DATEDIFF、DATE_ADD(0, ...)、TIMESTAMPADD は MySQL が NULL、HeimDB は Saturday、-106752、0000-01-02 など。'0000-00-00 00:00:00' の列の MONTH/DAY/DAYOFWEEK/DATEDIFF/DATE_FORMAT も違う（HeimDB 1、1、7、-106752、0000-01-01、MySQL 0、0、NULL、NULL、0000-00-00）。 |
| #3318 | 2025-11-27 | 取り込んだ（段階 112） | dolthub/dolt#10113: Fix DELETE queries with NOT EXISTS uninitialized subqueries | DELETE FROM student_hobby WHERE NOT EXISTS (SELECT 1 FROM student WHERE student.id = student_hobby.student_id) が HeimDB は 1105（attempted to evaluate uninitialized subquery）、MySQL は 2 行を消す。 |
| #3320 | 2025-12-04 | 取り込んだ（段階 114） | Mark `innodb_lock_wait_timeout` as being in both global and session scopes | SET SESSION innodb_lock_wait_timeout = 50（SET innodb_lock_wait_timeout = 50、SET @@session.… も）が HeimDB は 1105（is a GLOBAL variable）、MySQL は通る。@@innodb_lock_wait_timeout は HeimDB 1、MySQL 50。 |
| #3343 | 2025-12-15 | 取り込んだ（段階 112） | Check if key is convertible before memoizing lookup join | t0 NATURAL RIGHT JOIN t1 が HeimDB は (NULL, NULL)、MySQL は (1, 'i')。s.k = n.v（文字列の主キーと数の列）の JOIN が HeimDB は '12' だけ、MySQL は ' 12', '12', '12abc'。数のキーで文字列の索引を引いて行を落としている。 |
| #3355 | 2025-12-23 | 取り込んだ（段階 112） | Do not use nullable columns as null filter when converting antijoins to left joins | NOT EXISTS (SELECT 1 FROM t0 WHERE t0.c0 OR t1.c0) が HeimDB は 1、MySQL は空。EXPLAIN PLAN は LeftOuterJoin と t9.c0 IS NULL（NULL になりうる右の列を NULL の判定に使う）。 |
| #3365 | 2026-01-06 | 取り込んだ（段階 114） | Handle more types in `abs` | ABS('hi')、ABS('12.3hi')、ABS('-342.12hi')、ABS(1 AND TRUE) が HeimDB はすべて NULL、MySQL は 0、12.3、342.12、1。LONGTEXT の列の ABS(v) もすべて NULL で、ORDER BY ABS(v) の並びも違う。 |
| #3406 | 2026-01-29 | 取り込んだ（段階 111） | Set table names to lower when creating table map | 大文字を含む表の名前を別名なしで使う UPDATE Items INNER JOIN Items2 ... SET Items.Version = Items2.Version が HeimDB は 0 行、MySQL は 1 行。WordPress 風の WP_posts / WP_postmeta でも同じ（別名なしは 0 行、小文字の別名 p、pm なら効く）。大文字の別名（P、M）は効く。 |
| #3407 | 2026-01-30 | 取り込んだ（段階 114） | Calculate `year` for `timestampdiff` based on date and clock time values | TIMESTAMPDIFF(YEAR, DATE '2011-07-05', DATE '2026-07-04') が HeimDB 15、MySQL 14（逆向きも -15 と -14）。 |
| #3409 | 2026-02-02 | 取り込んだ（段階 114） | Calculate `month` and `quarter` for timestampdiff based on date and clock time values | TIMESTAMPDIFF(MONTH, '2026-01-31', '2026-03-01') が HeimDB 0、MySQL 1。TIMESTAMPDIFF(QUARTER, '2025-01-01', '2025-12-31') が HeimDB 4、MySQL 3。 |
| #3423 | 2026-02-10 | 取り込んだ（段階 112） | Do not allow sort-based joins between text and number type columns | t0.c0 (VARCHAR の主キー) = t1.c0 (INT の主キー) の JOIN が HeimDB は ('123a',123),('223',223) の 2 行、MySQL は 4 行。BETWEEN の範囲の結合も HeimDB は空、MySQL は 1 行。 |
| #3428 | 2026-02-13 | 取り込んだ（段階 111） | Refactor how we manage outer scopes in join iteration | SELECT id FROM a WHERE id NOT IN (SELECT v FROM b GROUP BY v)（b.v に NULL）の後、MemoryManager にキャッシュが 3 個残る（左の行の数だけ）。結果は MySQL と同じ（空）。DISTINCT の形、GROUP BY でも NULL のない形、GROUP BY のない形は 0 個。EXPLAIN PLAN は AntiJoin（existsIter）で、条件が NULL のとき esRejectNull から esIncLeft に進み、右の iterator（GroupBy のキャッシュ）を閉じずに次の左の行で作り直している。Close も右を閉じない。 |
| #3447 | 2026-02-25 | 取り込んだ（段階 114） | Handle Decimal types in `greatest` and `least` functions | GREATEST(a, b, c)、LEAST(a, b, c)（DECIMAL の列）と GREATEST(10.5, 9.75) が HeimDB は 1105（unsupported type for greatest/least argument: decimal.Decimal）、MySQL は 8.80000 \| 2.75000、10.50 \| 9.75。 |
| #3451 | 2026-03-04 | 取り込んだ（段階 111） | Route unknown-user auth failures to `ER_ACCESS_DENIED` for clients | 知らないユーザーが 2012、パスワード違いは 1045。ユーザーの有無が外から分かる |
| #3455 | 2026-03-03 | 取り込んだ（段階 112） | Do not convert AntiJoins to LeftOuterJoin when inside an Update node | UPDATE p SET status = 'closed' WHERE id NOT IN (SELECT id FROM k WHERE status = 'keep') が HeimDB は 1105（unable to find field with index 3 in row of 3 columns）、MySQL は 2 行。UPDATE ... WHERE NOT EXISTS (...) も HeimDB は 1105（uninitialized subquery、#3318 と同じ）。 |
| #3461 | 2026-03-06 | 取り込んだ（段階 111） | Update `ON UPDATE` columns during `INSERT...ON DUPLICATE KEY UPDATE` | DEFAULT を '2000-01-01 00:00:00' にした表（1 回目の DEFAULT 0 は MySQL で 1067）で、INSERT ... ON DUPLICATE KEY UPDATE v1 = VALUES(v1) が値を変えた行の dt を、MySQL は今の時刻に進める（dt = 初期値 が 0）、HeimDB は進めない（1）。影響の数は両方 2、同じ値の 2 回目は両方 0。 |
| #3468 | 2026-03-13 | 取り込んだ（段階 111） | Separate update expressions into explicit and derived update expressions | ON UPDATE CURRENT_TIMESTAMP の列のある表で、同じ値の UPDATE t SET v = 1 が HeimDB は影響 1 行で u を進める、MySQL は影響 0 行で u はそのまま。値を変える UPDATE は両方 1 行で u が進む。 |
| #3476 | 2026-03-20 | 取り込んだ（段階 114） | Display correct precision for time types | WordPress の sql_mode で、DATETIME(3) の '...56.100000' を HeimDB は 56.1、MySQL は 56.100。DATETIME(6) の .000000 を HeimDB は落とす。0 の日時も HeimDB は 0000-00-00 00:00:00、MySQL は .0 / .000 / .000000 付き。CAST(... AS DATETIME(3)) も .000 を落とす。 |
| #3503 | 2026-04-07 | 取り込んだ（段階 112） | Preserve whether left join excludes null when building best plan | SELECT id FROM hasnullid WHERE id NOT IN (SELECT NULL) が HeimDB は 1, 2, 3, NULL の 4 行、MySQL は空。NOT IN (SELECT 1) と NOT IN (SELECT v FROM r)（r に NULL）は同じ。 |
| #3527 | 2026-04-27 | 取り込んだ（段階 111） | Truncate invalid UTF-8 on `INSERT IGNORE` and warn on `LIKE` with bad charset pattern | INSERT IGNORE で不正な UTF-8（...2E 35 85 20...）を入れると HeimDB は空の値、MySQL は 'SSD 1TB 2.5' に切って入れる。WordPress の sql_mode の普通の INSERT も HeimDB は 1105、MySQL は切って入れる。LIKE CONCAT('%', CONVERT(UNHEX('85') USING utf8mb4), '%') は HeimDB 1105、MySQL は 3 行。 |
| #3556 | 2026-05-21 | 取り込んだ（段階 113） | Fix `CHAR_LENGTH` and `LIKE` failing on the replacement character | U+FFFD を含む値で CHAR_LENGTH(s) が HeimDB は 1105（malformed string encountered while checking length）、MySQL は 5。LIKE '%ab%'、LIKE 'ab_cd' は HeimDB が空、MySQL は 1。パターンに U+FFFD を入れると HeimDB は 1105。 |
| #3581 | 2026-06-09 | 取り込んだ（段階 113） | Fix `LIKE 'prefix%'` dropping rows with multibyte text | utf8mb4_unicode_520_ci の列で LIKE '## %' が HeimDB は 2, 4、MySQL は 1〜5（'## z'、'## za'、'## 中 body' が落ちる）。NOT LIKE も逆に誤る。LIKE '山田%' は HeimDB 空、MySQL 1。LIKE 'yama%' は HeimDB 2、MySQL 2, 3。 |
| #3626 | 2026-07-15 | 取り込んだ（段階 113） | Prevent index offset when comparing rows in `topRowsIter` | ORDER BY (SELECT MAX(r.approved_on) FROM bar r WHERE r.foo_id = foo.id) ASC LIMIT 1 が HeimDB は foo-1、MySQL は foo-8。DESC LIMIT 2、ASC LIMIT 4 も HeimDB は挿入順（foo-1, foo-2, ...）で、キーが効いていない。 |
| #3630 | 2026-09-02 | 取り込んだ（段階 111） | server/listener.go: Fix a race on listener shutdown where an Accept goroutine could try to send on a closed conns channel. | 停止の競合で閉じた channel へ送り panic（プロセスが落ちる）。フォークに同じコード |
| #3643 | 2026-07-27 | 取り込んだ（段階 111） | Use `buildScalar` to get `GetField` expression for `ON UPDATE` columns | WITH r(id) AS (SELECT 'a') UPDATE t2 SET val = 2 WHERE id IN (SELECT id FROM r) が HeimDB は 1105（HeimDB: table t2: primary key column id: expected string for varchar(64) ..., got time.Time）、MySQL は 1 行を更新。ON UPDATE の列の値を別の列（主キー）に書こうとしていて、HeimDB の保存層の型の確かめで止まっている（データは壊れない）。 |
| #3675 | 2026-09-02 | 取り込んだ（段階 114） | sql/types: trim whitespace before parsing date/datetime strings | CAST(' 2024-01-01 ' AS DATE)、CAST(CONCAT(' ', '2024-01-01') AS DATE)、DATE(' 2024-01-01 ')、CAST(' 2024-01-01 10:20:30 ' AS DATETIME) が HeimDB は NULL、MySQL は値。LONGTEXT の ' 2024-05-01' を CAST(v AS DATE) > '2024-04-15' で絞ると HeimDB は空、MySQL は 1。 |
| #3723 | 2026-09-08 | 取り込んだ（段階 114） | Refactor and improve `DATE` parsing and `SQL_MODE` handling | HOUR('13:04:05') が HeimDB は NULL、MySQL は 13。SECOND('2007-12-11T20:21:22Z') は HeimDB 22、MySQL 7。DATE(20010203) は HeimDB NULL、MySQL 2001-02-03。DATE('0000-00-00') と DATE(0) は HeimDB 0000-00-00、MySQL NULL。CAST('2024/01/05 3:4:5' AS DATETIME) は HeimDB NULL、MySQL 2024-01-05 03:04:05。 |
| #3733 | 2026-08-27 | 取り込んだ（段階 113） | Evaluate sort key expressions once per row instead of once per comparison | ORDER BY (@i := @i + 1) は HeimDB が構文エラー（1105 near ':='）で使えないので、ORDER BY RAND() の偏りで確かめた。8 行の表で SELECT id FROM t ORDER BY RAND() を 400 回流した先頭の行の数は、HeimDB が 115, 111, 80, 50, 28, 5, 7, 4（id 1〜8）、MySQL が 56, 54, 43, 48, 54, 57, 44, 44。LIMIT 1 は HeimDB も一様（段階 96 の TopN）。 |
| #3739 | 2026-08-28 | 取り込んだ（段階 113） | Fix HAVING aggregate alias resolution | 上流の 2 つ目の文（customers c JOIN orders p1 ... LEFT JOIN orders p2 ... HAVING COUNT(p2.id) > 0）が HeimDB は 1146（table not found: p2）、MySQL は (1, 2, 2)。HAVING の COUNT(p2.id) の列を名前だけで探して p1.id に結び付けている。同じ表の自己結合でも、HAVING の集計の列が名前で重ならない形（COUNT(p2.id) だけ、SUM(p2.total) だけ）は同じ。 |
| #3760 | 2026-09-02 | 取り込んだ（段階 113） | Fix aggregate reuse across scalar subquery scopes | SELECT MAX(d), (SELECT MAX(d) FROM s WHERE tag = 'keep') FROM s が HeimDB は 2030 と 2030、MySQL は 2030 と 2020。COUNT(id)、SUM(id) も外の値を使い回す（3 \| 3、6 \| 6）。MIN(id) の形は HeimDB が 1105（unable to find field with index 4 in row of 4 columns）。COUNT(*) の形は MySQL と同じ。 |
| #3761 | 2026-09-02 | 取り込んだ（段階 114） | Fix STR_TO_DATE NULL propagation | STR_TO_DATE(NULL, '%Y-%m-%d') と STR_TO_DATE('2024-01-02', NULL) が HeimDB は 1105（invalid type: <nil> / string）、MySQL は NULL。NULL を含む列にかけると、1 行の NULL で文全体が HeimDB は 1105。 |
| #3771 | 2026-09-02 | 取り込んだ（段階 114） | Apply AM and PM in STR_TO_DATE | STR_TO_DATE('2024-03-05 09:30:17 PM', '%Y-%m-%d %h:%i:%s %p') が HeimDB 09:30:17、MySQL 21:30:17。'12:05:00 AM' も HeimDB 12:05:00、MySQL 00:05:00（%p を無視）。 |
| #3780 | 2026-09-09 | 取り込んだ（段階 112） | Fix EXISTS over ungrouped aggregate subqueries | SELECT id FROM t WHERE EXISTS(SELECT COUNT(*) FROM u WHERE u.k = t.k) が HeimDB 1 だけ、MySQL 1,2。NOT EXISTS は HeimDB 2、MySQL 0 行。 |
| #3810 | 2026-09-09 | 取り込んだ（段階 111） | Fix windows over grouped inputs | SELECT g, c, ROW_NUMBER() OVER (ORDER BY g) FROM (SELECT g, COUNT(*) AS c FROM t GROUP BY g) s の結果は MySQL と同じだが、流した後に MemoryManager のキャッシュが 1 残る。窓関数のない GROUP BY の派生表と、GROUP BY のない窓関数は 0（対照）。 |
| #3862 | 2026-09-16 | 取り込んだ（段階 114） | Always generate format column `ON UPDATE` clauses in `EXTRA` | SHOW COLUMNS は MySQL と同じだが、information_schema.columns の extra が HeimDB u: DEFAULT_GENERATED / d: 空、MySQL u: DEFAULT_GENERATED on update CURRENT_TIMESTAMP / d: on update CURRENT_TIMESTAMP。 |
| #3920 | 2026-09-24 | 取り込んだ（段階 114） | Fix panic converting strings with a dangling exponent | panic は起きないが、指数の数字が無い文字列を decimal に変える経路でエラーになる: ROUND('1E') と ROUND(HEX(age)) が HeimDB 1366 '1E' is not a valid value for 'decimal(65,30)'（WordPress の sql_mode でも）、MySQL 1（警告 1292）。strict の INSERT INTO t VALUES (1, '1E', '1E') も HeimDB 1366、MySQL は 1 を入れる。CAST('1E' AS DECIMAL(10,2)) と '1E' + 0 は同じ。 |
| #3940 | 2026-09-29 | 取り込んだ（段階 111） | Require UPDATE privileges for duplicate-key inserts | SELECT, INSERT だけの権限のユーザーで INSERT INTO test VALUES (0, 9) ON DUPLICATE KEY UPDATE v1 = 9 が、HeimDB は通って行 0 を 9 に書き換える、MySQL は 1142 UPDATE command denied。（-no-auth でない HeimDB を別のポートで起動して確かめた。プローブの道具はユーザーを替えられない） |
| #3960 | 2026-10-01 | 取り込んだ（段階 115） | Preserve NULL semantics for indexed NOT IN subqueries | 索引のある NULL を持てる列の `parent NOT IN (SELECT id FROM p)` が、HeimDB は parent が NULL の行も返す（MySQL は返さない）。右に NULL がなくても起きる。索引のない列と DELETE は正しい |
| #3980 | 2026-11-06 | 取り込んだ（段階 114） | Fix INSTR collation handling and wrapped substrings | utf8mb4_unicode_520_ci の列で `INSTR(s, 'A')` が HeimDB 0、MySQL 4（'xyza'）。`LOCATE` と `POSITION` は正しく、INSTR だけ照合順序を見ない（`function/substring.go` の Instr.Eval）。プラグインがよく使う関数 |
| #4014 | 2026-10-15 | 取り込んだ（段階 113） | Return a NULL row for MAX/MIN without GROUP BY when no row matches | 主キーの MAX/MIN で表が空のとき、SELECT MAX(i) FROM emptytable が HeimDB 0 行、MySQL NULL の 1 行。SELECT COALESCE(MAX(i), 0) + 1 FROM emptytable も HeimDB 0 行、MySQL 1。主キーでない列の MAX(v) と、行のある表の WHERE i > 5 は同じ NULL。 |
| #4021 | 2026-10-15 | 取り込んだ（段階 113） | Do not remove GroupBy node when replacing sorted MIN/MAX aggregates | #4014 と同じく SELECT MAX(i) FROM emptytable（主キー）が HeimDB 0 行、MySQL NULL。複合主キーの MAX(pk3) WHERE pk1=.. AND pk2=.. と GROUP BY の形は同じ（v0.20.0 にはその置き換えが無い）。 |
| #4054 | 2026-10-23 | 取り込んだ（段階 115） | create larger decimal precision during comparisons | DECIMAL(10,2) の 123.45 で d = 123.45000001 が HeimDB 1、MySQL 0（d < 123.45000001 も 0 と 1）。WHERE d = 123.451 の COUNT(*) が HeimDB 1、MySQL 0（リテラルを列の桁に丸めて比べる）。 |
| #4106 | 2026-11-05 | 取り込んだ（段階 111） | Make the quadratic range overlap check opt-in, and check for cancelation during analysis | 2 万個の `a IN (...)` で、範囲の重なりの確かめが範囲の数の 2 乗になり、HeimDB は SELECT 6.0 秒、DELETE 4.8 秒（MySQL はすぐ）。DELETE の間は書き込みのロックを持ち、解析の間は KILL も効かない |
| #3020 | 2025-06-11 | 見送った | Support OCT() function and fix CONV() mishandling of negative floats and empty string N | CONV('', 10, 2) が HeimDB は 1105 `HeimDB: internal error: index out of range [0] with length 0`（panic）、MySQL は NULL。CONV(-1.5, 10, 2) は HeimDB 1、MySQL は 64 個の 1、CONV('-17.5', 10, 16) は HeimDB 11、MySQL FFFFFFFFFFFFFFEF。 |
| #3023 | 2025-06-11 | 見送った | `like` match when collation is unspecified | JSON_TABLE の TEXT の列（照合順序なし）への tag LIKE 'foo' が、HeimDB は空、MySQL は foo。 |
| #3095 | 2025-07-15 | 見送った | Convert SetType to strings | SET の列を CASE WHEN ... THEN s ELSE 'x' END と CONCAT(s,'') にすると、HeimDB は 5（ビットの数）、MySQL は 'a,c'。 |
| #3111 | 2025-07-23 | 見送った | Enum and set conversions in comparison.Compare | 違う定義の ENUM どうしの e1 = e2（どちらも 'a'）が HeimDB 0、MySQL 1。SET の s = 'b,a' が HeimDB 1、MySQL 0。 |
| #3114 | 2025-07-25 | 見送った | dolthub/dolt#9511 - Fix SET type casting to CHAR and BINARY | SET の列の CAST(s AS CHAR)、CAST(s AS BINARY) が HeimDB は 5、MySQL は 'a,c'。 |
| #3118 | 2025-07-24 | 見送った | Validate time zone before setting it | SET time_zone = 'Not/AZone' を HeimDB は受け付け、後の SELECT NOW() が 1105 `invalid time zone: Not/AZone`。MySQL は SET の時点でエラー 1298 `Unknown or incorrect time zone: 'Not/AZone'`。 |
| #3131 | 2025-07-30 | 見送った | Unwrap ParenExpr when evaluating OrderBy expressions | WITH cte(i) AS (SELECT i FROM t) SELECT * FROM cte ORDER BY (i) が、HeimDB は並べずに 2,0,9,4、MySQL は 0,2,4,9。派生表の ORDER BY ((i)) と式の ORDER BY (i*10+j) は同じ。 |
| #3138 | 2025-08-04 | 見送った | dolthub/dolt#9624 - Fix wildcard user authentication for IP patterns | IP のパターンのホスト（'u'@'10.0.%'）の認証の修正。HeimDB が作るユーザーは root@'%' とサイトのユーザー@'%' だけ（`internal/backend/auth.go`）で、ホストのパターンを使わない |
| #3145 | 2025-08-06 | 見送った | fix panic over `SetOp`s with joins and subqueries | SELECT * FROM t1 UNION (SELECT j FROM t2 WHERE (j > 10 OR j IN (SELECT j FROM t1 JOIN t2))) が HeimDB は 1105 `HeimDB: internal error: runtime error: slice bounds out of range [1:0]`（panic。接続は保たれる）、MySQL は 0,1,2。 |
| #3161 | 2025-08-14 | 見送った | Allow hash functions to work on BINARY columns when the binary is not valid UTF8 | MD5(UNHEX('FF'))、SHA1(...)、TO_BASE64(...) が、STRICT でも WordPress の sql_mode でも HeimDB は 1105 `invalid string for charset utf8mb4: '[255]'`、MySQL は 00594fd4...、85e53271...、'/w=='。 |
| #3172 | 2025-08-22 | 見送った | Add correlated columns from subqueries to GroupBy select dependencies | 選択の項目の相関副問い合わせの別名 x で GROUP BY すると、HeimDB は x が NULL（(2,NULL),(3,NULL)）、MySQL は (2,1),(3,2)。v0.20.0 のフォークでも起きる。 |
| #3193 | 2025-09-03 | 見送った | Left and right joins should not be optimized into cross joins | t LEFT JOIN (SELECT ... WHERE FALSE) AS subq ON TRUE と、その RIGHT JOIN が、HeimDB は空、MySQL は 1。交差結合に変えている。 |
| #3208 | 2025-09-10 | 見送った | dolthub/dolt#9817 - Fix binary operations return type to be uint64 | -1 \| 0 が HeimDB 9223372036854775807、MySQL 18446744073709551615。1 << 63 が HeimDB 9223372036854775807、MySQL 9223372036854775808。 |
| #3209 | 2025-09-10 | 見送った | Convert values to strings when evaluating `bit_length` | BIT_LENGTH(123), BIT_LENGTH(1.5), BIT_LENGTH(DATE(...)) が HeimDB は 1105 `Invalid argument to bit_length: 1.5`、MySQL は 24, 24, 80。 |
| #3219 | 2025-09-15 | 見送った | dolthub/go-mysql-server#3216: Fix `UNION ALL` with `NULL BLOB SELECT` returning `NULL` for all vals | SELECT name, data FROM a UNION ALL SELECT name, NULL FROM b（data は BLOB）が、HeimDB は a の行の data も NULL、MySQL は 0xDEADBEEF。HEX(data) と data の UNION ALL は同じ。v0.20.0 のフォークでも起きる。 |
| #3228 | 2025-09-22 | 見送った | [no-release-notes] refactor truncation tests | HEX(CHAR(0)) が HeimDB は ''、MySQL は '00'。CHAR(NULL, 65) と CHAR(256) は同じ。 |
| #3256 | 2025-10-09 | 見送った | dolthub/dolt#9927: Fix double negation overflow with Literals | -(-128) が HeimDB 127（MySQL 128）、-(-2147483648) が 2147483647（MySQL 2147483648）、-(-9223372036854775808) が 1105 `BIGINT out of range`（MySQL 9223372036854775808）、INT と TINYINT の最小値の列の否定も 1 小さい。 |
| #3265 | 2025-10-15 | 見送った | Do not allow inserting NaN and Inf values into numeric type columns | POW(10,308)*10 の INSERT が HeimDB は通って +Inf を保存し（MySQL は 1690 DOUBLE value is out of range）、その後の SELECT * FROM t WHERE d > 1.5 が 1105 `HeimDB: internal error: Cannot create a Decimal from +Inf`（panic）。CAST(POW(10,308)*10 AS DECIMAL) も同じ 1105（MySQL 1690）。 |
| #3308 | 2025-11-18 | 見送った | make `IsNullable` return true for nullable datetime functions | NOT EXISTS (SELECT 1 FROM (SELECT 1) AS subQuery WHERE WEEKDAY(t1.c0)) が HeimDB は空、MySQL は 1。行が落ちる直接の原因は #3302 の持ち上げ（IsTrue で包まない）で、#3302 を当てた後に WEEKDAY の IsNullable が false だと包まれず、同じ誤りが残る。 |
| #3325 | 2025-12-04 | 見送った | Make `IsNullable` return `true` for log and math functions where applicable | NOT EXISTS (SELECT 1 FROM (SELECT 1) AS sq WHERE LOG(t0.c0)) と SQRT(t0.c0) が HeimDB は空、MySQL は -1。#3308 と同じく、直接の原因は #3302 の持ち上げで、LOG/SQRT の IsNullable も直さないと #3302 の後も残る。 |
| #3350 | 2025-12-18 | 見送った | Updated `IsNullable` for all functions to match `Eval` output | 再現の文なし。46 ファイルの機械的な変更 |
| #3382 | 2026-01-15 | 見送った | Ensure join correctness for enums of different types | 値の並びの違う ENUM どうしの JOIN（a.e = b.e）が HeimDB は ('x','z'),('z','x')、MySQL は ('x','x'),('z','z')。内部の番号で比べている。 |
| #3388 | 2026-01-27 | 見送った | Apply filter simplifications to Join condition | t1 INNER JOIN t0 ON (t1.c0 BETWEEN t0.c2 AND t0.c2) が HeimDB は空、MySQL は (0, NULL, NULL, NULL, 'KZ')。#3415 と同じ文。 |
| #3410 | 2026-02-03 | 見送った | Replace `Time.Sub` call in `TIMESTAMPDIFF` with `microsecondsDiff` | TIMESTAMPDIFF(MICROSECOND, '0001-01-01 00:00:00', '9999-12-31 23:59:59') が HeimDB 9223372036854775、MySQL 315537897599000000。SECOND と DAY で '1000-01-01' からの差も桁あふれ（HeimDB 9223372036、106751、MySQL 32377449600、374739）。 |
| #3415 | 2026-02-04 | 見送った | Simplify Between expressions for GetField arguments | #3388 と同じ文で、HeimDB は空、MySQL は (0, NULL, NULL, NULL, 'KZ')。 |
| #3417 | 2026-02-05 | 見送った | Do not join remaining tables with CrossJoins during buildSingleLookupPlan | 主キーのない表を含む本当の交差結合（`FROM t6, t1 JOIN t0 ON ...` で t6 とつなぐ条件がない）で、`COUNT(*)` が HeimDB 12、MySQL 4。つなぐ条件があれば正しい。WordPress の結合はどれも条件でつなぐ |
| #3433 | 2026-02-18 | 見送った | Correctly compute scopeLen for joins | 2 段の JOIN LATERAL の文が HeimDB は 1105（internal error: slice bounds out of range [8:7]、panic の回復）、MySQL は 8 行。LATERAL を使わない入れ子（EXISTS の中の EXISTS の中の JOIN、スカラーの副問い合わせの中の JOIN など 3 通り）は MySQL と同じ。 |
| #3435 | 2026-07-22 | 見送った | [no-release-notes] sql/types: fix dropped errors | 再現の文なし。エラーを捨てる 3 行 |
| #3618 | 2026-07-22 | 見送った | Error on SELECT @@SESSION.<global-only-var>, matching MySQL | SELECT @@SESSION.innodb_autoinc_lock_mode が HeimDB は 2、MySQL は 1238（GLOBAL だけの変数）。 |
| #3651 | 2026-07-29 | 見送った | Correctly do NullUnsafe comparisons with tuples. | (0, NULL) = (0, NULL) と (1, NULL) IN ((1, NULL)) が HeimDB は真（1 行）、MySQL は NULL（0 行）。(1, NULL) <=> (1, 2) と (2, NULL) > (1.5, 2) は HeimDB が 1105（value not nil: 2）、MySQL は 0 と 1。(a, b) NOT IN ((1, 5)) は HeimDB 1, 2、MySQL 2。 |
| #3658 | 2026-07-29 | 見送った | When comparing tuples for equality, widen each pair of input values. | (1, 1) = (1.1, 1.1) が HeimDB 1、MySQL 0。主キー (a, b) の表で WHERE (a, b) = (1.1, 1.1) が HeimDB は 1 行、MySQL は空。 |
| #3702 | 2026-08-14 | 見送った | fix panic in `format()` | FORMAT(123456, 3, 1) が HeimDB は 1105（internal error: interface conversion: interface {} is int8, not string、panic の回復）、MySQL は '123,456.000'。FORMAT('notanumber', 2)、FORMAT('12abc', 2)、FORMAT(123.456, 'notanumber') は HeimDB が NULL、MySQL は 0.00、12.00、123。 |
| #3717 | 2026-08-19 | 見送った | bug fix: outer scope visibility in multiple levels of nested derived tables | SELECT a, (SELECT colid FROM (SELECT x AS colid FROM (SELECT outer_t.a AS x FROM inner_t) inner1) outer1) FROM outer_t が HeimDB は 1105（unable to find field with index 1 in row of 0 columns）、MySQL は (1,1),(2,2)。3 段の形も同じ。 |
| #3770 | 2026-09-03 | 見送った | Fix INET_NTOA for unsigned IPv4 values | INET_NTOA(3232235777)、INET_NTOA(4294967295)、INET_NTOA(INET_ATON('203.0.113.9')) が HeimDB はすべて 127.255.255.255、MySQL は 192.168.1.1、255.255.255.255、203.0.113.9。 |
| #3777 | 2026-09-03 | 見送った | String inputs to integer division should be treated as decimals to match MySQL | '1.2' DIV '0.2' が HeimDB 5、MySQL 6。'1.2' DIV '0.4' が 2 と 3（float で割っている）。 |
| #3789 | 2026-09-03 | 見送った | Fix tuple IN comparisons containing NULL | (c0, c1) IN ((20, NULL), (30, 5)) が行 (20, NULL) で HeimDB 1、MySQL NULL。WHERE では HeimDB が id=2 を返し MySQL は 0 行。 |
| #3797 | 2026-10-05 | 見送った | Resolve correlated aggregates at their outer scope | SELECT grp, (SELECT GROUP_CONCAT(a.val ...) FROM concat_probe p) FROM concat_scope a GROUP BY grp が HeimDB 1105 unable to find field with index 2 in row of 1 columns、MySQL (1,'a\|b'),(2,'c')。HAVING の形は HeimDB 1146 table not found: a。MAX(a.val)、COUNT(a.val) でも同じ 1105。 |
| #3823 | 2026-09-10 | 見送った | Fix BIGINT arithmetic overflow handling | BIGINT UNSIGNED の 18446744073709551615 で u + 0 が HeimDB 9223372036854775807、MySQL 18446744073709551615（境界でなくても値が化ける）。u + 1、s + 1 は HeimDB -9223372036854775808（折り返し）、MySQL 1690。 |
| #3824 | 2026-09-09 | 見送った | Fix collation coercibility resolution and binary collation detection | COLLATION(CONCAT(a, b))（a は utf8mb4_unicode_520_ci、b は VARBINARY）が HeimDB utf8mb4_unicode_520_ci、MySQL binary。COLLATION(CONCAT(a, 'x' COLLATE utf8mb4_bin)) が HeimDB utf8mb4_unicode_520_ci、MySQL utf8mb4_bin。比較の結果（CONCAT(a, b) = 'aA'）は同じ 1。 |
| #3845 | 2026-09-09 | 見送った | Fix `CHAR` values differing only in trailing space to be equal | CHAR(3) に 'a ' を入れると、HeimDB は末尾の空白を残す（LENGTH 2）、MySQL は落とす（LENGTH 1）。そのため GROUP BY c が HeimDB 3 組、MySQL 2 組、DISTINCT、WHERE c = 'a'（HeimDB 1 だけ、MySQL 1,2）、PARTITION BY c も違う。 |
| #3864 | 2026-09-15 | 見送った | Implement conversions from `DATE`, `DATETIME`, and `TIMESTAMP` to numbers | d + 0 が HeimDB 20250331000000、MySQL 20250331。ts + 0 が 2.0250331123456e+13 と 20250331123456、CAST(d AS SIGNED) が 1743379200（Unix 時刻）と 20250331、SUM(d) が 1.7433792e+09 と 20250331、ABS(dt) が NULL と 2.0250331123456e+13。 |
| #3901 | 2026-09-22 | 見送った | handle no delimiter datetime string parsing | cast('200101' as date) が HeimDB NULL、MySQL 2020-01-01。cast('00200101.0203' as date) が NULL と 0020-01-01、cast('20101' as datetime(6)) が NULL と 2020-10-01 00:00:00.000000。 |
| #3923 | 2026-09-25 | 見送った | Resolve untyped `NULL` columns in CTAS to `VARBINARY(0)` | CREATE TABLE t AS SELECT NULL AS a, 1 AS b が HeimDB 1105 HeimDB: column t.a: parse type "null"（保存層が型 null を読めない）、MySQL は a binary(0) で作る。 |
| #3950 | 2026-09-30 | 見送った | Fix correlated subquery dependencies in grouped joins | select mt.s, (select count(*) from othertable where i2 <= mt.i) from mytable mt left join othertable ot on mt.i = ot.i2 where mt.i = 2 group by mt.i, mt.s が HeimDB ('second row', 3)、MySQL ('second row', 2)。 |
| #3955 | 2026-09-30 | 見送った | Convert STR_TO_DATE arguments to strings | STR_TO_DATE(20250331, '%Y%m%d') が HeimDB 1105 invalid type: int32、STR_TO_DATE(BINARY '2025-03-31', ...) が 1105 invalid type: []uint8、MySQL はどちらも 2025-03-31。 |
| #3956 | 2026-09-30 | 見送った | Preserve NOT IN predicates that cannot be unnested | id NOT IN (SELECT ... tags) AND id NOT IN (SELECT ... UNION SELECT ...) が HeimDB leaf, root（UNION 側の NOT が落ちて IN になる）、MySQL b。 |
| #3957 | 2026-09-30 | 見送った | Preserve unsigned bits when casting to SIGNED | CAST(u AS SIGNED) が 18446744073709551615 で HeimDB 9223372036854775807、MySQL -1。9223372036854775808 で 9223372036854775807 と -9223372036854775808（飽和させている）。 |
| #3958 | 2026-09-30 | 見送った | Reject malformed UTF-8 in CONVERT USING utf8mb4 | CONVERT(0xC328 USING utf8mb4) が HeimDB は壊れた文字列（�(）、MySQL NULL と警告 1300。HEX(CONVERT(0x61FF62 USING utf8mb4)) が 61FF62 と NULL。 |
| #3964 | 2026-09-30 | 見送った | Validate twelve-hour STR_TO_DATE inputs | STR_TO_DATE('12:00:00 AM', '%h:%i:%s %p') が HeimDB 12:00:00、MySQL 00:00:00。STR_TO_DATE('13:00 PM', '%h:%i %p') が HeimDB 13:00:00、MySQL NULL。 |
| #3971 | 2026-10-02 | 見送った | Delegate `IN` element evaluation to comparison expressions and apply coercibility to HashInTuple | label IN ('cafe' COLLATE utf8mb4_bin, 'other')（列は utf8mb4_unicode_ci）が HeimDB 1,2,4,5,6（列の照合順序で比べる）、MySQL 2,4。LOWER(TRIM(label)) IN、組の IN、列の IN は同じ。 |
| #3977 | 2026-10-08 | 見送った | Sort set operation ORDER BY by the output column | select 1000 union select 1 order by 1 が HeimDB 1000, 1、MySQL 1, 1000。'b' union 'a' order by 1、abs(-1000) union 1 order by 1 も並びが違う（定数の ORDER BY が効かない）。 |
| #3979 | 2026-10-01 | 見送った | Preserve decimal precision in GREATEST and LEAST | greatest(a, b, c)（DECIMAL の列）が HeimDB 1105 unsupported type for greatest/least argument: decimal.Decimal、MySQL 8.80000。greatest(1.5, 2.25) のような DECIMAL のリテラルでも同じ 1105。 |
| #3995 | 2026-10-08 | 見送った | Fix `SIGN` to handle signed floating points < 0.5 and string truncation | SIGN(0.3)、SIGN(-0.3)、SIGN(0.49)、SIGN(-0.0001)、SIGN(0.3e0)、SIGN(-1e-5) がすべて HeimDB 0、MySQL 1/-1。SIGN(' 0.3')、SIGN('+.5') も HeimDB 0、MySQL 1。段階 119 で、PR を取り込まずに HeimDB の直し（MySQL と同じく DOUBLE で決める。`docs/engine.md` の 119 の行）で同じ違いを直した。 |
| #4002 | 2026-10-22 | 見送った | Preserve column metadata for parenthesized GROUP BY references | SELECT COUNT(*) FROM tab0 GROUP BY (col1), col2 HAVING col1 >= COUNT(*) が HeimDB 1105 column "col1" could not be found in any table in scope、MySQL 0 行。行を足した後の 2 つの文も 1105 と (1),(2) / (1)。 |
| #4011 | 2026-10-19 | 見送った | Prune unselected columns from `Columns()` | 再現の文なし。計画の期待を大きく変える |
| #4012 | 2026-10-14 | 見送った | Match MySQL Punctuation for `DATETIME` string parsing | CAST('2025/03/31 12.34.56' AS DATETIME) が HeimDB NULL、MySQL 2025-03-31 12:34:56（警告 4095）。CAST('2025-03-31 12:34:56xyz' AS DATETIME) が NULL と 2025-03-31 12:34:56（警告 1292 Truncated）。 |
| #4020 | 2026-10-15 | 見送った | Handle `locate` case when `str` length is 0 and `position` is greater than 0 | locate('', 'abc', 4) が HeimDB 0、MySQL 4（探す文字列が空で位置が文字列の長さ + 1）。ほかの組み合わせは同じ。 |
| #4032 | 2026-10-16 | 見送った | Fix nullsafe logic for tuples containing null values | SELECT (CAST(NULL AS DATETIME), 5) <=> (1, 5) が HeimDB 1105 Incorrect datetime value: '1'、MySQL 0。ほかの 2 つは同じ。 |
| #4034 | 2026-10-16 | 見送った | Set `orderByScope.cols` properties for SetOps using that of the merged columns | #3977 と同じ文で、select 1000 union select 1 order by 1 などの並びが HeimDB と MySQL で逆。 |
| #4047 | 2026-10-23 | 見送った | Fix type coercision for `GREATEST` and `LEAST` functions | GREATEST(1, 'a') が HeimDB 1、MySQL 'a'。GREATEST('2025-01-01', CAST('2024-12-31' AS DATE)) が 0 と 2025-01-01、LEAST(10, '9') が 9 と 10（MySQL は文字列で比べる）、GREATEST(1, 2.5, '3') が 1105 と 3。 |
| #4070 | 2026-10-26 | 見送った | Allow `COUNT(1)` and `COUNT(*)` in the same `SELECT` | SELECT COUNT(1), COUNT(*) FROM count_t が HeimDB 1105 column "COUNT(*)" does not exist in scope, but there is an alias ...、MySQL (3,3)。GROUP BY の形も 1105。順が COUNT(*), COUNT(1) なら通る。 |
| #4112 | 2026-11-05 | 見送った | Preserve datetime precision in INTERVAL arithmetic | CAST('2026-01-02 12:34:56.527248' AS DATETIME(6)) + INTERVAL 1 MICROSECOND が HeimDB 2026-01-02 12:34:56、MySQL 12:34:56.527249。DATETIME(3) の列 + INTERVAL 1 HOUR も小数秒が落ちる。 |
| #2983 | 2025-05-20 | 再現しない | fix explain plan on server engine | EXPLAIN SELECT COUNT(*) FROM t と主キーの等号の EXPLAIN は、HeimDB でも MySQL でもエラーにならない（max1Row のエラーは出ない）。段階 48 の analyzeNode の直しで済んでいる。 |
| #3027 | 2025-06-18 | 再現しない | make `sql.HashOf()` collation aware | sql.HashOf を型と照合順序を見る形にし、DISTINCT、UNION、INTERSECT、EXCEPT、ハッシュ結合、IN (SELECT) などで照合順序で等しい値をまとめる。 |
| #3032 | 2025-06-16 | 再現しない | Generalize types in `CASE`, `IF`, and `IFNULL` | CASE、IF、IFNULL の結果の型を MySQL の規則（GeneralizeTypes）で決め、値もその型に変換する。 |
| #3041 | 2025-06-27 | 再現しない | fix `group_concat` with `order by` subquery clauses | GROUP_CONCAT(... ORDER BY (相関副問い合わせ)) の添字の割り当ての誤り（結果の誤りかエラー）。 |
| #3043 | 2025-06-25 | 再現しない | fix missed case for nullable enums | #2985 の追補。NULL を許す ENUM の列には最初の値を入れない（NULL のまま）。 |
| #3045 | 2025-06-26 | 再現しない | Fix 9280 escape ctrl z comment strs | MySQL 8.0.46 も SHOW CREATE TABLE のコメントの Ctrl+Z（0x1A）をエスケープせずそのまま出し、HeimDB も同じ 'a\x1ab'。違いは DEFAULT NULL の有無だけ。 |
| #3049 | 2025-06-30 | 再現しない | Fix 7300 date time overflow underflow | 日時と INTERVAL の計算（TimeDelta.apply）を Go の time で作り直し、範囲を超えたときの NULL、DATETIME の最大値、秒より細かい桁の丸めを MySQL に合わせる。 |
| #3113 | 2025-07-23 | 再現しない | Fix double counted time delta when converting time zones | HeimDB は起動時に time.Local を UTC にする（cmd/heimdb/main.go:43）ので、時刻の位置の二重計算は起きない。time_zone '+09:00'、'-05:00'、'+00:00' での TIMESTAMPDIFF(…, UTC_TIMESTAMP(6), NOW(6)) と CONVERT_TZ は MySQL と同じ（プロセスの時刻帯が JST の試験のサーバでも同じだった）。 |
| #3116 | 2025-07-24 | 再現しない | fix panic in stats histogram union and intersect | 段階 46 で取り込み済み |
| #3122 | 2025-07-25 | 再現しない | Allow SYSTEM as valid time zone | #3118 の追補。time_zone の検査で SYSTEM を正しい値として受け付ける。 |
| #3141 | 2025-08-06 | 取り込んだ（段階 115） | dolthub/dolt#9628 - Fix UNION column mapping bug in nested multi-way operations | 3 つ以上の UNION（(A UNION B) UNION C）で、入れ子の SetOp の列の ID を並べ替えた集合から取り、列が入れ替わる（重複の行と列の取り違え）。 |
| #3148 | 2025-08-18 | 再現しない | dolthub/dolt#9641 - Fix BIT Overflow | UNION の結果の列の型を GeneralizeTypes で決め、BIT(1) の列と数の UNION ALL で値が溢れる（48 が入らない）のを直す。UNION の副問い合わせの Max1Row の印も外す。 |
| #3176 | 2025-08-27 | 再現しない | Trim strings to number prefix when cast | CAST('12abc' AS SIGNED)=12、CAST('1e3' AS SIGNED)=1、CAST('12.345x' AS DECIMAL(10,2))=12.35、CAST('7abc' AS DECIMAL(10,2))=7.00 と警告 1292、CAST(meta_value AS SIGNED) の並べ替えは MySQL と同じ（段階 85 で直し済み）。 |
| #3204 | 2025-09-12 | 再現しない | dolthub/dolt#9812: Coalesce `IN` and `=` operator logic | 文字列と数の IN と = の振る舞いを揃える（SELECT 'A' IN (0) は 1）。 |
| #3206 | 2025-09-09 | 再現しない | fix equality check in `buildSingleLookupPlan()` | buildSingleLookupPlan が、単純な等号でない結合の条件（>= など）にも索引の結合の計画を作り、条件を落として結果を誤る。 |
| #3230 | 2025-09-29 | 再現しない | truncation refactoring and partial decimal truncation implementation | 型の変換の切り詰めの作り直し（DECIMAL の部分の切り詰め、IN の比較の作り直し、負の値の符号なしへの CAST の警告、索引の比較での変換の誤りの無視、JSON_TABLE のエラーの文）。 |
| #3237 | 2025-09-30 | 再現しない | fix string to boolean comparison for `HashInTuple` expressions | HashInTuple（IN の定数の組をハッシュにしたもの）で、文字列と数の比較の切り詰めを正す（'123abc' IN ('string', 1, 2, 123) は 1、'123.45a' IN (123.5) は 0）。 |
| #3242 | 2025-10-01 | 再現しない | Added collation ignoring for information_schema | information_schema.COLUMNS の COLUMN_NAME COLLATE utf8mb4_0900_ai_ci が HeimDB ではエラーにならない（上流の直す不具合は起きない）。MySQL 8.0.46 はエラー 1253 `COLLATION 'utf8mb4_0900_ai_ci' is not valid for CHARACTER SET 'utf8mb3'`。COLLATE utf8mb3_general_ci は両方通る。 |
| #3258 | 2025-10-09 | 再現しない | dolthub/dolt#9935: Add fix for boolean evaluation in analyzer for EXISTS | WHERE の NOT にリテラルの真偽を含む OR/AND（!(1 \|\| EXISTS(...))）を簡約で誤る。 |
| #3261 | 2025-10-10 | 再現しない | Do not convert keys if key type is incompatible with column type | 文字列の列の索引の範囲を数の定数で組むとき、数を列の型（文字列）に変換して範囲にし、数として比べる MySQL と結果が違う（1630944823 >= c0）。 |
| #3278 | 2025-10-22 | 再現しない | Respect precision when casting to datetime | CAST(... AS DATETIME) が型の精度を見ず、いつも 6 桁の精度で変換する（MySQL は DATETIME で秒未満を丸める）。#3276 の追補。 |
| #3300 | 2025-11-11 | 再現しない | Move `hoistOutOfScopeFilters` rule to `DefaultRules` | hoistOutOfScopeFilters が UNION の各辺（finalizeUnions）で動かず、外の列だけの条件を持つ NOT EXISTS の副問い合わせが UNION の中で誤る。 |
| #3333 | 2025-12-10 | 再現しない | fix overflow indexed table access | 索引の列の型に入らない定数（TINYINT の列に 999、UNSIGNED の列に負の数）を、型の端の値に丸めて索引を引くので、= 999 が 127 の行を返し、UNSIGNED の > -1 が空になる。 |
| #3356 | 2025-12-26 | 再現しない | Return boolean literal when simplifying `AND` and `OR` filters | WHERE の AND / OR の簡約で、真偽が決まる側の子（19 や 's'）をそのまま返し、真偽の値でない型のまま比べて結果が誤る。 |
| #3361 | 2026-01-05 | 取り込んだ（段階 112） | Handle empty right iterators in exists iterator as an EOF | 存在の結合（EXISTS / NOT EXISTS の semi / anti join）で右の iterator が空の印（EmptyIter）だと、フォークは反結合でも iterator 全体を EOF にして左の行を全部落とす。 |
| #3419 | 2026-02-06 | 再現しない | Use placeholder `ColumnId`s for `EmptyTable` | EmptyTable が列の ID を持たないので、列から実行の位置への対応がずれ、LEFT JOIN ... ON FALSE などで誤った列を読む。 |
| #3438 | 2026-02-19 | 再現しない | Implement `sql.StringType` interface for `systemStringType` | CASE / IF で @@session.time_zone と @@system_time_zone を返す文は panic せず値を返す。値は HeimDB 'JST'、MySQL 'UTC' で、system_time_zone（HeimDB はホストの時間帯、MySQL のコンテナは UTC）の違い。COALESCE(@@session.time_zone, 'x') は同じ。 |
| #3442 | 2026-02-24 | 再現しない | sql/types: implement NumberType for system numeric types | システム変数の数の型（bool、int、uint、double）が sql.NumberType を満たさず、数の型としての扱い（統計のバケット、結合、索引のキーの判定）から外れる。 |
| #3561 | 2026-05-26 | 再現しない | fix(rowexec): dispose CachedResults child when closing hashLookupGeneratingIter (#3560) | hashLookupGeneratingIter.Close が子を閉じず、ハッシュ結合の右側の途中結果のキャッシュが MemoryManager に残り続ける漏れの修正。 |
| #3572 | 2026-06-08 | 再現しない | Rewrite `CachedResults` node and `cachedResultsIter` to not use `MemoryManager` | #3561 の追補。読み終えず閉じられもしない cachedResultsIter が MemoryManager にキャッシュを残すのが本当の原因だったとして、CachedResults を MemoryManager を使わない形に書き直した。 |
| #3591 | 2026-07-09 | 再現しない | Combine `pushFilters` rule with `moveJoinConditionsToFilter` | pushFilters と moveJoinConditionsToFilter を一つにまとめた書き直し。片側だけを参照する結合の条件を Filter で包む旧来のやり方では、入れ子の結合の途中に取り残された条件が reorderJoin で落ちる（dolthub/dolt#9868）。#3231（2025-09、フォークにない）はこれを結合の条件に押し込んで部分的に直したが別の誤り（dolt#10899）を生み、本 PR で両方を直した。 |
| #3667 | 2026-08-05 | 再現しない | Clear autocommit transactions on err on all paths | 存在しない表 / 列を読む SELECT、存在しない列への UPDATE、重なりのエラーの INSERT、存在しない表を読む UPDATE を同じ接続で流してそのまま開いておき、別の接続の UPDATE t SET v = 2 が待たずに（0.03 秒）通り、元の接続でも v = 2 が見えた。HeimDB の CommandEnd が自動コミットの残りを片付けている。 |
| #3798 | 2026-09-04 | 再現しない | Fix count distinct tuple hashing | COUNT(DISTINCT a, b) が値を ',' でつないだ文字列をハッシュしていたため、('a,b','c') と ('a','b,c') が同じと数えられた。型ごとのハッシュ（sql/hash）に替えた。 |
| #3825 | 2026-09-09 | 再現しない | Support multi-byte strings and collation derivation in `LPAD` and `RPAD` | LPAD/RPAD で多バイトの文字を文字の数で数え、結果の照合順序を文字列の引数から決める。 |
| #3863 | 2026-09-14 | 再現しない | Decorrelate inner join subqueries in `EXISTS` subqueries | EXISTS サブクエリの中の INNER JOIN の ON の相関条件を脱相関するとき、外部結合の NULL 側の条件を持ち上げて、一致する行を落としていた。 |
| #3899 | 2026-09-18 | 再現しない | Fix dropped residual filters and false branch matching in concat lookup join | ON に OR のある索引の結合（concat lookup join）で、右の表の別の条件（extraFilters）だけで合った索引を OR の枝の引き方に使い、残りの条件を落としていた。 |
| #3918 | 2026-10-01 | 再現しない | Support escaped `LIKE` wildcard in analyzer prefix extraction and pattern matching | LIKE のパターンの \_ と \% のエスケープを、等号や前方一致の範囲への置き換え（simplifyFilters）と照合で正しく扱う。vitess の文字列の \_ と \% を残す修正（vitess#497）と合わせたもの。 |
| #3941 | 2026-09-30 | 再現しない | Resolved table and CTE authorization | SELECT だけの権限のユーザーで INSERT INTO test2 WITH test2 AS (SELECT pk + 10, v1 FROM test) SELECT * FROM test2 は HeimDB も拒む（書き込まれない）。WITH cte AS (...) SELECT は両方通る。CTE と実の表の取り違えの抜けは起きない。WITH ... INSERT の形は MySQL では 1064。 |
| #3967 | 2026-10-01 | 再現しない | `IN` expr: compare every element of the right tuple | IN の右の組の要素の型が混ざるとき（整数と小数）、最初の要素の型に揃えて比べ、0.49 を 0 に丸めて 0 IN (9, 0.49) を真にしていた。要素ごとに比べる。 |
| #3973 | 2026-10-01 | 再現しない | Preserve literal defaults during ALTER table rewrites | 表を書き直す ALTER（MODIFY など）で、保存した表の定義から始めると、日時のリテラルの既定値を式に分類し直し、後で COLUMN_DEFAULT に引用符が付いた。 |
| #4001 | 2026-10-09 | 再現しない | Preserve aggregate input column IDs for HAVING | 集約の入力の列を足すとき GetField の列の ID を保たず、HAVING がグループの列の誤った値を見ていた。 |
| #4005 | 2026-10-13 | 再現しない | Generate correct GetField expressions when optimizing joins on table aliases with pruned columns. | 列を刈り込んだ表の別名の上の結合の最適化（convertSemiToInnerJoin は別名でなく元の表の名前で GetField を作り、convertAntiToLeftJoin は刈り込んだ表の定義を使った）で panic していた。 |
| #4039 | 2026-10-22 | 再現しない | GROUP_CONCAT: concatenate BIT values as raw bytes | GROUP_CONCAT で BIT の値を 10 進の文字ではなく生のバイトでつなぐ（HEX(GROUP_CONCAT(v)) が MySQL と同じ 0305 に）。 |
| #4041 | 2026-10-16 | 再現しない | Clear sign on negative zero during `DecimalMod` | DECIMAL の MOD の結果の負の 0 の符号を落とす（-5 MOD 5 が '-0' でなく '0'）。 |
| #4056 | 2026-10-23 | 再現しない | Fix accidentally dropping filters in complex joins | 結合の順の計画が、新しい計画にすべての結合の条件が入ったかを確かめず、多くの表の結合で内部結合の条件を落としていた。 |
| #4061 | 2026-10-26 | 再現しない | Do not apply outer scope index lookups below windows and limits | 相関の条件を索引の引き方に変えるとき（applyIndexesFromOuterScope）、窓や LIMIT/OFFSET/TopN の下の表にまで引き方を入れ、LIMIT が引いた 1 行だけを見ていた。 |
| #4072 | 2026-10-28 | 再現しない | Require SUPER to SET global and persisted variables | SELECT だけの権限のユーザー（認証ありの HeimDB を別のポートで起動）で SET GLOBAL max_connections、SET PERSIST、SET @@global.sql_mode、SET @@persist.x、SESSION と GLOBAL を混ぜた SET がすべて 1227 Access denied ...SUPER or SYSTEM_VARIABLES_ADMIN で拒まれ、値は変わらない（段階 42 の analyzer/global_privileges.go）。 |
| #4084 | 2026-11-09 | 再現しない | Truncate strings to the column length under `INSERT IGNORE` and `UPDATE IGNORE` | INSERT IGNORE と UPDATE IGNORE（と ON DUPLICATE KEY UPDATE）で列より長い文字列を列の長さに切る処理を文字セットで正しく行う。フォークの convertDataAndWarn（sql/rowexec/insert.go の 343 行）は row[idx].(string)[:maxLength] とバイトの位置で切るので、多バイトの文字の途中で切って不正な UTF-8（か少ない文字）を書き、文字列でない値（数、日時、[]byte）では型アサーションで panic する。 |
| #4117 | 2026-11-05 | 再現しない | Report actual column collations in SHOW FULL COLUMNS | SHOW FULL COLUMNS の Collation に、既定（utf8mb4_0900_bin）ではなく列の実の照合順序を出す（ENUM と SET も）。 |
| #4132 | 2026-11-08 | 再現しない | Open foreign key editors when a plan runs, not when it's analyzed | UPDATE a JOIN b ... SET a.v = b.v, b.v = a.v、エラーで終わる INSERT ... SELECT、EXPLAIN UPDATE JOIN の後、キャッシュは 0 で、結果も MySQL と同じ。HeimDB の Session.CommandEnd が文の終わりに書き直しと走査を閉じる。保存層の開いた編集器の数はこの道具では測っていない。 |
| #4136 | 2026-11-09 | 再現しない | Leave a statement's autocommit transaction to that statement | 解釈実行の関数（Doltgres の PL/pgSQL）から流した文が失敗すると clearAutocommitTransaction が外側の文の自動コミットのトランザクションを手放していたのを止め、代わりに失敗した自動コミットの文のトランザクションを、iterator を閉じた後（TransactionCommittingIter.Close）でロールバックする。Close で失敗した文がトランザクションを次の文に残していたのも直す。 |
| #2103 | 2026-01-28 | 効かない | Bump google.golang.org/grpc from 1.53.0 to 1.56.3 | 依存の上げだけ |
| #2894 | 2025-05-28 | 効かない | [planbuilder] nil database doesn't panic building procedure call | WordPress が使わない機能 |
| #2976 | 2025-05-13 | 効かない | [auto-bump] [no-release-notes] dependency by jycor | 依存の上げだけ |
| #2977 | 2025-05-14 | 効かない | Bug fix for offset without limit | Doltgres だけ |
| #2978 | 2025-05-14 | 効かない | Bug fix: ensure an active transaction is set before preparing a query | サーバ側の準備文。WordPress は使わない |
| #2979 | 2025-05-16 | 効かない | fix indexing for `GROUP BY`s and `WINDOW`s in `INSERT` and `REPLACE` statements in `TRIGGERS` | WordPress が使わない機能 |
| #2980 | 2025-05-15 | 効かない | Unwrap inputs to JSON_OBJECT. | WordPress が使わない機能 |
| #2981 | 2025-05-15 | 効かない | Unwrap wrapper values used in JSON aggregation functions, and un-skip accidentally-skipped tests for this behavior. | WordPress が使わない機能 |
| #2982 | 2025-05-20 | 効かない | Bug fix: use ExtendedType serialization if present | Doltgres だけ |
| #2984 | 2025-05-20 | 効かない | Add lock_warnings system variable. | 機能の追加 |
| #2986 | 2025-05-21 | 効かない | add returning clause case for assign exec index rule | Doltgres だけ |
| #2988 | 2025-05-23 | 効かない | Implement `REVOKE ALL PRIVILEGES, GRANT OPTION ...` | 機能の追加 |
| #2989 | 2025-05-22 | 効かない | fix: Fix create new server in example. | 整理、テスト、文言だけ |
| #2991 | 2025-08-26 | 効かない | test: optimize database connection test logic | 変えたファイルがテスト、memory、文書だけ |
| #2992 | 2025-05-27 | 効かない | Hacky extension point for aggregate function determination | Doltgres だけ |
| #2993 | 2025-05-27 | 効かない | Added undeclared variable error message | 整理、テスト、文言だけ |
| #2994 | 2025-06-13 | 効かない | fix NewServer call | 変えたファイルがテスト、memory、文書だけ |
| #2996 | 2025-05-28 | 効かない | Revert "Merge pull request #2894 from dolthub/max/nil-proc-db" | WordPress が使わない機能 |
| #3001 | 2025-05-31 | 効かない | Support for injected expressions with ORDER BY clauses | Doltgres だけ |
| #3002 | 2025-05-30 | 効かない | fix table functions in stored procedures | WordPress が使わない機能 |
| #3004 | 2025-06-03 | 効かない | Add support for `IS NULL` expressions with records | Doltgres だけ |
| #3005 | 2025-08-14 | 効かない | Added support for `@@port`, `@@hostname`, and other system variables | 機能の追加 |
| #3006 | 2025-06-05 | 効かない | implement `if [not] exists` logic for DDL around `view`s and `index`es | 機能の追加 |
| #3009 | 2025-06-03 | 効かない | New skipped tests for group_concat | 変えたファイルがテスト、memory、文書だけ |
| #3010 | 2025-06-03 | 効かない | Add query time to logger | 機能の追加 |
| #3011 | 2025-06-05 | 効かない | [auto-bump] [no-release-notes] dependency by angelamayxie | 依存の上げだけ |
| #3012 | 2025-06-06 | 効かない | Handle `insert...returning...` queries | 機能の追加 |
| #3013 | 2025-06-06 | 効かない | handle insert returning for server context | 変えたファイルがテスト、memory、文書だけ |
| #3014 | 2025-06-06 | 効かない | Simplify implementation of `hasSingleOutput` | 整理、テスト、文言だけ |
| #3015 | 2025-06-09 | 効かない | server var tests | 整理、テスト、文言だけ |
| #3016 | 2025-06-09 | 効かない | Minor updates to support `UPDATE ... FROM` in Doltgres | Doltgres だけ |
| #3017 | 2025-06-09 | 効かない | Support `*` in `returning` clauses | 機能の追加 |
| #3018 | 2025-06-09 | 効かない | fix text storage for `left` and `instr` function | WordPress が使わない機能 |
| #3019 | 2025-06-09 | 効かない | [auto-bump] [no-release-notes] dependency by angelamayxie | 依存の上げだけ |
| #3021 | 2025-06-10 | 効かない | Added test for `insert...select...returning` | 変えたファイルがテスト、memory、文書だけ |
| #3022 | 2025-06-11 | 効かない | [no-release-notes] integration test for `go-mysql-org` | 依存の上げだけ |
| #3024 | 2025-06-16 | 効かない | Adding skipped tests for `UPDATE ... JOIN` bugs | 変えたファイルがテスト、memory、文書だけ |
| #3028 | 2025-06-12 | 効かない | add mysql dialect tag to new `group by` test | 変えたファイルがテスト、memory、文書だけ |
| #3029 | 2025-06-13 | 効かない | Fix 9347 panic case statements info schema | 回復する panic で、WordPress が出さない文 |
| #3031 | 2025-06-13 | 効かない | Fix 7998 json escape double quotes | WordPress が使わない機能 |
| #3033 | 2025-06-16 | 効かない | [no-release-notes] make test less flakey | 変えたファイルがテスト、memory、文書だけ |
| #3034 | 2025-06-17 | 効かない | Allow `drop trigger ...` when trigger is invalid | WordPress が使わない機能 |
| #3036 | 2025-06-18 | 効かない | Cache `REGEX` | 性能だけ |
| #3037 | 2025-06-21 | 効かない | Apply foreign key constraints to `UPDATE JOIN` | WordPress が使わない機能 |
| #3038 | 2025-06-21 | 効かない | Fix schema for call to `hash.HashOf()` in `HashLookups` | フォークに該当のコードがない |
| #3040 | 2025-06-24 | 効かない | backtick column names in check constraints | WordPress が使わない機能 |
| #3042 | 2025-06-27 | 効かない | Apply multiple triggers to `UPDATE JOIN` | WordPress が使わない機能 |
| #3046 | 2025-06-27 | 効かない | Fix SET statements to return OkResult instead of empty rows | 整理、テスト、文言だけ |
| #3047 | 2025-07-01 | 効かない | Detect when one index is always better than another. | 性能だけ |
| #3048 | 2025-06-27 | 効かない | Added field to ScriptTest to skip test | 変えたファイルがテスト、memory、文書だけ |
| #3050 | 2025-06-27 | 効かない | fix exists subqueries in stored procedures | WordPress が使わない機能 |
| #3051 | 2025-06-27 | 効かない | Fixes 7201 non json data err comparison errs | 変えたファイルがテスト、memory、文書だけ |
| #3053 | 2025-06-30 | 効かない | moved keyless subquery test to correct place | 変えたファイルがテスト、memory、文書だけ |
| #3054 | 2025-07-03 | 効かない | Support for set-returning functions | Doltgres だけ |
| #3056 | 2025-06-28 | 効かない | Revert "Merge pull request #3046 from dolthub/macneale4-claude/query-ok" | 整理、テスト、文言だけ |
| #3057 | 2025-07-01 | 効かない | Changing how `NO_MERGE_JOIN` hint is applied to fix a panic | 回復する panic で、WordPress が出さない文 |
| #3058 | 2025-07-01 | 効かない | Add variety of Enum tests | 変えたファイルがテスト、memory、文書だけ |
| #3059 | 2025-07-05 | 効かない | Fix SET statements to return OkResult instead of empty rows | 整理、テスト、文言だけ |
| #3061 | 2025-07-02 | 効かない | Fix #9428: Allow default keyword as value for generated columns. | WordPress が使わない機能 |
| #3062 | 2025-07-03 | 効かない | Allow DEFAULT keyword in UPDATE for generated columns (issue #9438) | WordPress が使わない機能 |
| #3063 | 2025-07-03 | 効かない | Fix `count(*)` for added generated columns | WordPress が使わない機能 |
| #3064 | 2025-07-09 | 効かない | Abstract `IsNull` and `IsNotNull` expression logic | Doltgres だけ |
| #3065 | 2025-07-03 | 効かない | Fix enum columns cannot have auto_increment | 整理、テスト、文言だけ |
| #3067 | 2025-07-03 | 効かない | dolthub/dolt#9427 - Prevent user and system variables in column defaults and generated values | 整理、テスト、文言だけ |
| #3070 | 2025-07-07 | 効かない | Don't prune VirtualColumnTable tables | WordPress が使わない機能 |
| #3074 | 2025-07-08 | 効かない | dolthub/dolt#9424 - Fix enum foreign key constraints to match MySQL behavior | WordPress が使わない機能 |
| #3076 | 2025-07-10 | 効かない | dolthub/dolt#9425 - Fix enum zero validation in strict mode | 機能の追加 |
| #3077 | 2025-07-08 | 効かない | Add various `SET` type tests | 変えたファイルがテスト、memory、文書だけ |
| #3079 | 2025-07-09 | 効かない | INSERT string function | 機能の追加 |
| #3080 | 2025-07-09 | 効かない | add `auto_increment` tests with various types | 変えたファイルがテスト、memory、文書だけ |
| #3081 | 2025-07-09 | 効かない | make_set and export_set functions | 機能の追加 |
| #3082 | 2025-07-10 | 効かない | Allow GroupBy primary key (and some test clean up) | 機能の追加 |
| #3083 | 2025-07-10 | 効かない | Split function-specific engine test queries into their own file | 整理、テスト、文言だけ |
| #3084 | 2025-07-11 | 効かない | dolthub/dolt#9481 - Move auto_increment validation to GMS | 整理、テスト、文言だけ |
| #3085 | 2025-07-10 | 効かない | Fix enum DEFAULT NULL validation in analyzer | フォークに該当のコードがない |
| #3086 | 2025-07-11 | 効かない | [no-release-notes] Moved more function tests into their own file | 変えたファイルがテスト、memory、文書だけ |
| #3087 | 2025-07-11 | 効かない | Mixed Type Foreign Key | 変えたファイルがテスト、memory、文書だけ |
| #3088 | 2025-07-11 | 効かない | [no-release-notes] Refactored some shared functions into common.go | 整理、テスト、文言だけ |
| #3089 | 2025-07-11 | 効かない | `DECIMALS` with foreign keys tests | 変えたファイルがテスト、memory、文書だけ |
| #3090 | 2025-07-11 | 効かない | pull from main | 整理、テスト、文言だけ |
| #3091 | 2025-07-14 | 効かない | sql/types: JSONWrapper: Thread Context on some methods on JSON-related types, such as ToInterface and Compare. | WordPress が使わない機能 |
| #3092 | 2025-07-11 | 効かない | add UTC() to unix time min and max Timestamp | 整理、テスト、文言だけ |
| #3093 | 2025-07-14 | 効かない | go.mod: Bump to Go 1.24.0. | 整理、テスト、文言だけ |
| #3094 | 2025-07-16 | 効かない | dolthub/dolt#9496 - Fix DECIMAL foreign key constraint validation to match MySQL behavior | WordPress が使わない機能 |
| #3096 | 2025-07-16 | 効かない | Allow empty strings in set string conversions | WordPress が使わない機能 |
| #3097 | 2025-07-15 | 効かない | test aggregates over indexes with false filter | 変えたファイルがテスト、memory、文書だけ |
| #3100 | 2025-07-16 | 効かない | add skipped tests for auto_increment and max integer | 変えたファイルがテスト、memory、文書だけ |
| #3101 | 2025-07-16 | 効かない | dolthub/dolt#9472 - Fix SET column foreign key constraintsfix specific errs | WordPress が使わない機能 |
| #3102 | 2025-07-16 | 効かない | dolthub/dolt#9494 - Clean up mixed string foreign key logic | WordPress が使わない機能 |
| #3104 | 2025-07-17 | 効かない | dolthub/dolt#9519 - Fix SQL syntax error for mixed named columns and * in SELECT | 整理、テスト、文言だけ |
| #3105 | 2025-07-17 | 効かない | support set returning function cases in table function wrapper | Doltgres だけ |
| #3106 | 2025-07-17 | 効かない | adding skipped tests for time types and foreign keys | 変えたファイルがテスト、memory、文書だけ |
| #3107 | 2025-07-17 | 効かない | change Compute method signature to return error separately for window functions | WordPress が使わない機能 |
| #3108 | 2025-07-23 | 効かない | remove unnecessary sort over merge joins | 性能だけ |
| #3109 | 2025-07-25 | 効かない | dolthub/dolt#9544 - Fix time type foreign key constraints | WordPress が使わない機能 |
| #3115 | 2025-07-23 | 効かない | Bug fix: correctly parse/resolve trigger definitions across multiple databases | WordPress が使わない機能 |
| #3117 | 2025-07-30 | 効かない | drop sorts in cross joins and inner joins | 性能だけ |
| #3119 | 2025-07-24 | 効かない | Add test for setting time zone from table value | 変えたファイルがテスト、memory、文書だけ |
| #3120 | 2025-07-25 | 効かない | [no-release-notes] generate query script plans | 変えたファイルがテスト、memory、文書だけ |
| #3121 | 2025-07-25 | 効かない | [no-release-notes] remove dialect option from merge join tests | 変えたファイルがテスト、memory、文書だけ |
| #3123 | 2025-07-28 | 効かない | Support decimal unique keys | 変えたファイルがテスト、memory、文書だけ |
| #3126 | 2025-07-29 | 効かない | Added test for adding check constraint when modifying column | WordPress が使わない機能 |
| #3127 | 2025-07-30 | 効かない | [auto-bump] [no-release-notes] dependency by angelamayxie | 依存の上げだけ |
| #3128 | 2025-07-30 | 効かない | [auto-bump] [no-release-notes] dependency by jycor | 依存の上げだけ |
| #3137 | 2025-08-05 | 効かない | Bug fix: ordered aggregates in triggers | WordPress が使わない機能 |
| #3139 | 2025-08-08 | 効かない | Remove the go-kit dependency | 整理、テスト、文言だけ |
| #3142 | 2025-08-07 | 効かない | Add support for `DELETE ... RETURNING` | Doltgres だけ |
| #3143 | 2025-08-06 | 効かない | Add stack trace in error from panic handler | 整理、テスト、文言だけ |
| #3146 | 2025-08-11 | 効かない | allocate fewer `sql.Row`s | 性能だけ |
| #3147 | 2025-08-07 | 効かない | dolthub/go-mysql-server#3144 - Fix parseErr tracing to show error messages instead of memory addresses | 整理、テスト、文言だけ |
| #3150 | 2025-08-11 | 効かない | Misc code clean up | 整理、テスト、文言だけ |
| #3152 | 2025-08-11 | 効かない | Validate trigger name length is not over 96 chars | WordPress が使わない機能 |
| #3154 | 2025-08-12 | 効かない | avoid using `sql.Row.Append()` | 性能だけ |
| #3155 | 2025-08-12 | 効かない | Made LOAD DATA handle NULL values and defaults correctly | WordPress が使わない機能 |
| #3156 | 2025-08-13 | 効かない | Add support for altering table comments | 機能の追加 |
| #3157 | 2025-08-13 | 効かない | [auto-bump] [no-release-notes] dependency by angelamayxie | 依存の上げだけ |
| #3158 | 2025-08-19 | 効かない | cache `sql.Schema` in `plan.Project` and `plan.TableAlias` | 性能だけ |
| #3160 | 2025-08-14 | 効かない | [auto-bump] [no-release-notes] dependency by nicktobey | 依存の上げだけ |
| #3162 | 2025-09-03 | 効かない | Add support for VECTOR type | WordPress が使わない機能 |
| #3163 | 2025-08-15 | 効かない | [no-release-notes] Refactor repeated getEmptyPort functions into shared util | 整理、テスト、文言だけ |
| #3164 | 2025-08-15 | 効かない | handle RowIter as result from Eval function in TableFunctionWrapper | Doltgres だけ |
| #3166 | 2025-08-20 | 効かない | Unskip GroupBy validation on Project, Having, and Sort nodes | 機能の追加 |
| #3167 | 2025-09-02 | 効かない | Bump go-sql-driver/mysql | 依存の上げだけ |
| #3168 | 2025-08-28 | 効かない | align struct fields to reduce memory usage | 性能だけ |
| #3169 | 2025-08-20 | 効かない | go.mod: Bump go-icu-regex. Picks up a wazero bump which improves amd64 behavior on builds with go 1.24.0+. | 依存の上げだけ |
| #3170 | 2025-08-20 | 効かない | [no-release-notes] Implement Describable for Alias and Subquery | 整理、テスト、文言だけ |
| #3173 | 2025-08-22 | 効かない | Mask column names from non-triggered tables in UpdateJoins | WordPress が使わない機能 |
| #3178 | 2025-08-28 | 効かない | Do not materialize any output for WindowPartitionIter if input is empty | WordPress が使わない機能 |
| #3180 | 2025-08-29 | 効かない | dolthub/dolt#9738 - Add CREATE VIEW parentheses tests for MySQL compatibility | 依存の上げだけ |
| #3182 | 2025-08-29 | 効かない | [no-release-notes] Added test for create table from aggregate function | 依存の上げだけ |
| #3183 | 2025-08-29 | 効かない | [auto-bump] [no-release-notes] dependency by angelamayxie | 依存の上げだけ |
| #3185 | 2025-09-11 | 効かない | [no-release-notes] .github/workflows/test.yml: Prepare for go-icu-regex dependency on ICU4C by ensuring it is available to Cgo in all test environments. | 変えたファイルがテスト、memory、文書だけ |
| #3186 | 2025-09-02 | 効かない | /go.mod: bump go | 依存の上げだけ |
| #3187 | 2025-09-02 | 効かない | Allow aggregate/window functions with match expressions | WordPress が使わない機能 |
| #3188 | 2025-09-02 | 効かない | [auto-bump] [no-release-notes] dependency by coffeegoddd | 依存の上げだけ |
| #3189 | 2025-09-03 | 効かない | [auto-bump] [no-release-notes] dependency by nicktobey | 依存の上げだけ |
| #3190 | 2025-09-08 | 効かない | Refactorings to support index scans for pg catalog tables | Doltgres だけ |
| #3192 | 2025-09-03 | 効かない | fix update join with conflicting aliases | WordPress が使わない機能 |
| #3194 | 2025-09-04 | 効かない | Group by validation for aggregated queries | 機能の追加 |
| #3195 | 2025-09-05 | 効かない | Initialize newChildren with node.Children to avoid nil child during replaceIdxSort | フォークに該当のコードがない |
| #3196 | 2025-09-05 | 効かない | Do not push down filters for Full Outer joins | WordPress が使わない機能 |
| #3197 | 2025-09-06 | 効かない | dolthub/dolt#9794 - Fix string function panics with Dolt TextStorage | WordPress が使わない機能 |
| #3199 | 2025-09-05 | 効かない | Add added column to CreateConstraint scope | WordPress が使わない機能 |
| #3201 | 2025-09-08 | 効かない | dolthub/dolt#9807 - FULL OUTER JOIN with empty left subquery rets unmatched right rows | WordPress が使わない機能 |
| #3203 | 2025-09-16 | 効かない | allow setting session variable default value | 機能の追加 |
| #3205 | 2025-09-09 | 効かない | Relax restriction that was preventing us from caching the result of subqueries. | 性能だけ |
| #3210 | 2025-09-10 | 効かない | Copy parent row in lateralJoinIterator.buildRow | WordPress が使わない機能 |
| #3213 | 2025-09-12 | 効かない | Copy parent row in fullJoinIter | WordPress が使わない機能 |
| #3214 | 2025-09-15 | 効かない | Implement `PIPES_AS_CONCAT` mode | 機能の追加 |
| #3217 | 2025-09-17 | 効かない | Add `CREATE DATABASE` fall back on `CREATE SCHEMA` to mirror MySQL | 機能の追加 |
| #3218 | 2025-09-15 | 効かない | [auto-bump] [no-release-notes] dependency by angelamayxie | 整理、テスト、文言だけ |
| #3220 | 2025-09-16 | 効かない | go.mod: Bump go-icu-regex. Pick up ICU4C Cgo dependency. | 整理、テスト、文言だけ |
| #3222 | 2025-09-16 | 効かない | [no-release-notes] Clean up SQL modes | 整理、テスト、文言だけ |
| #3223 | 2025-10-08 | 効かない | README.md: Add notes to the readme indicating cgo dependency. | 整理、テスト、文言だけ |
| #3224 | 2025-09-18 | 効かない | Validate expressions in `ORDER BY` clause during `GROUP BY` validation | 機能の追加 |
| #3225 | 2025-09-18 | 効かない | [no-release-notes] bump golang version | 依存の上げだけ |
| #3226 | 2025-09-23 | 効かない | fix Expressions() of TableFunctionWrapper to make the function expression visible | Doltgres だけ |
| #3227 | 2025-09-19 | 効かない | dolthub/dolt#9857: Add UUID_SHORT() support | 機能の追加 |
| #3231 | 2025-09-24 | 効かない | Push filters down into join condition | 性能だけ |
| #3232 | 2025-09-24 | 効かない | Add `TEXT(m)` support | 変えたファイルがテスト、memory、文書だけ |
| #3233 | 2025-09-24 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3234 | 2025-09-25 | 効かない | dolthub/dolt#9873: Add tests for `FOR UPDATE OF` | 機能の追加 |
| #3235 | 2025-09-25 | 効かない | Allow caching subqueries in a tree of joins. | 性能だけ |
| #3236 | 2025-09-25 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3238 | 2025-09-28 | 効かない | fix load data when escaped and enclosed are the same | WordPress が使わない機能 |
| #3241 | 2025-09-30 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3243 | 2025-10-01 | 効かない | Added armscii charset and collations | 機能の追加 |
| #3244 | 2025-10-01 | 効かない | /go.{mod,sum}: add patch version | 依存の上げだけ |
| #3245 | 2025-10-01 | 効かない | Allow triggers to fire with `INSERT...RETURNING` statements | WordPress が使わない機能 |
| #3246 | 2025-10-03 | 効かない | sql/rowexec: Add a SessionCommandSafepoint session callback for long-running write commands. Call it in some rowexec implementations. | 機能の追加 |
| #3247 | 2025-10-02 | 効かない | dolthub/dolt#9865: Fix EOF on procedure | WordPress が使わない機能 |
| #3248 | 2025-11-03 | 効かない | Implement `sql.ValueRow` | 性能だけ |
| #3249 | 2025-10-03 | 効かない | dolthub/dolt#9916: Add TRUNCATE(X,D) Support | 機能の追加 |
| #3250 | 2025-10-03 | 効かない | Replace SubqueryAlias that selects an entire table with TableAlias | 性能だけ |
| #3251 | 2025-10-08 | 効かない | Condense nested `select * from` SubqueryAliases into the innermost SubqueryAlias | 性能だけ |
| #3252 | 2025-10-07 | 効かない | Add pure Go regex implementation for non-CGO builds | 機能の追加 |
| #3253 | 2025-10-06 | 効かない | [no-release-notes] Add default VarChar and VarBinary types to `types/strings.go` | 整理、テスト、文言だけ |
| #3254 | 2025-10-07 | 効かない | [no-release-notes] Tweak ComparableJSON interface to take context parameter and have mor… | WordPress が使わない機能 |
| #3262 | 2025-10-10 | 効かない | dolthub/dolt#9927: Fix SQL regression on UnaryMinus NULL CAST | フォークに該当のコードがない |
| #3267 | 2025-10-16 | 効かない | Fix GroupBy validation for queries with Having | フォークに該当のコードがない |
| #3268 | 2025-10-16 | 効かない | [no-release-notes] Refactor NaN and infinity checks into separate function | 整理、テスト、文言だけ |
| #3269 | 2025-10-16 | 効かない | Add Grafana to listed Backends | 変えたファイルがテスト、memory、文書だけ |
| #3270 | 2025-10-20 | 効かない | dolthub/dolt#9969: Fix `ENCLOSED BY` ignoring terminator inside of field when using `LOAD DATA INFILE` | WordPress が使わない機能 |
| #3274 | 2025-10-20 | 効かない | Do not convert full outer joins to cross joins | WordPress が使わない機能 |
| #3275 | 2025-10-21 | 効かない | dolthub/dolt#9977: Prevent filter pushdown for Anti joins for `NOT IN` | フォークに該当のコードがない |
| #3279 | 2025-11-05 | 効かない | dolthub/dolt#9887: Add `BINLOG` and `mariadb-binlog` support | WordPress が使わない機能 |
| #3282 | 2025-10-29 | 効かない | /.github/workflows/bump-dependency.yaml: sanatize stuff | 変えたファイルがテスト、memory、文書だけ |
| #3283 | 2025-11-03 | 効かない | dolthub/dolt#9316: Fix PK setting and add new fields for `CREATE TABLE ... SELECT` | HeimDB が拒否する文 |
| #3284 | 2025-10-30 | 効かない | Use IndexedTableAccess for Sorts over a SubqueryAlias | 性能だけ |
| #3285 | 2025-10-30 | 効かない | [no-release-notes] /.github/workflows/bump-dependency.yaml: fix command | 変えたファイルがテスト、memory、文書だけ |
| #3287 | 2025-10-31 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3289 | 2025-11-06 | 効かない | [no-release-notes] Trace debugger for join planner, bug fix for Doltgres indexed joins | Doltgres だけ |
| #3290 | 2025-11-05 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3291 | 2025-11-10 | 効かない | Add support for configuring a user's TLS connection requirements | 機能の追加 |
| #3292 | 2025-11-07 | 効かない | Updating auth interfaces to pass connection | 機能の追加 |
| #3293 | 2025-11-07 | 効かない | Don't make string unless in Debug mode | 性能だけ |
| #3294 | 2025-11-07 | 効かない | [no-release-notes] Removed unnecessary RowIter implementations (handled by row exec builder | 整理、テスト、文言だけ |
| #3295 | 2025-11-07 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 整理、テスト、文言だけ |
| #3296 | 2025-11-11 | 効かない | use type switch instead of `Fprintf` for grouping key | 性能だけ |
| #3297 | 2025-11-11 | 効かない | dolthub/dolt#10050: Fix JSON conversion for `dolt` when using `TextStorage` | WordPress が使わない機能 |
| #3298 | 2025-11-14 | 効かない | Validate connection security properties | 機能の追加 |
| #3299 | 2025-11-11 | 効かない | Prevent panic when using table functions in joins and subqueries | 回復する panic で、WordPress が出さない文 |
| #3303 | 2025-12-12 | 効かない | Explicit Analyzer/Builder Overrides | 整理、テスト、文言だけ |
| #3304 | 2025-11-14 | 効かない | Support table functions with non-literal arguments in subqueries and lateral joins | WordPress が使わない機能 |
| #3306 | 2025-11-18 | 効かない | dolthub/dolt#10083: Honor *definer privileges* when rebinding views | WordPress が使わない機能 |
| #3309 | 2025-11-19 | 効かない | unsafe methods in `SQL` and `SQLValue` | 性能だけ |
| #3310 | 2025-11-19 | 効かない | Split `Iter.Next()`, `RowToSQL`, and `callback` into separate threads | 性能だけ |
| #3311 | 2026-01-06 | 効かない | Bump golang.org/x/crypto from 0.0.0-20191011191535-87dc89f01550 to 0.45.0 | 依存の上げだけ |
| #3314 | 2025-11-24 | 効かない | server/handler: Add ConnectionAuthenticated callback which Vitess can call once the connection is authenticated. | 機能の追加 |
| #3315 | 2025-11-24 | 効かない | cache static groupby schema | 性能だけ |
| #3316 | 2025-11-24 | 効かない | [auto-bump] [no-release-notes] dependency by reltuk | 整理、テスト、文言だけ |
| #3319 | 2025-12-02 | 効かない | Fix `TimestampFuncExpr` and `SetOp` in stored procedures | WordPress が使わない機能 |
| #3321 | 2025-12-02 | 効かない | rewrite last query info | 性能だけ |
| #3322 | 2025-12-02 | 効かない | custom `AppendDateFormat` | 性能だけ |
| #3323 | 2025-12-04 | 効かない | [no-release-notes] Create Datetime3 type | 機能の追加 |
| #3324 | 2025-12-04 | 効かない | [no-release-notes] Add cost estimates to GroupBy, Sort, HashLookup, SubqueryAlias, and Case nodes in plan tests | 整理、テスト、文言だけ |
| #3326 | 2025-12-04 | 効かない | Skip expected estimates and analysis for keyless table plan tests | 変えたファイルがテスト、memory、文書だけ |
| #3327 | 2025-12-04 | 効かない | [no-release-notes] Fix plangen to correctly generate test files when explicitly skipping the row estimate | 変えたファイルがテスト、memory、文書だけ |
| #3328 | 2025-12-05 | 効かない | Avoid underestimating rows in outer joins. | 性能だけ |
| #3329 | 2026-01-06 | 効かない | Bump github.com/sirupsen/logrus from 1.8.1 to 1.8.3 | 依存の上げだけ |
| #3330 | 2025-12-12 | 効かない | Add fast path for simple `IN` filters | 性能だけ |
| #3331 | 2025-12-09 | 効かない | Introduce notion of conditional equivalence sets in FDS for optimizing outer joins. | 性能だけ |
| #3332 | 2025-12-09 | 効かない | Fix create view error message | 整理、テスト、文言だけ |
| #3334 | 2025-12-11 | 効かない | Index lookup type conversion issues | Doltgres だけ |
| #3335 | 2025-12-12 | 効かない | For large joins, prefer sticking to the user-supplied join order over full enumeration. | 性能だけ |
| #3336 | 2025-12-10 | 効かない | Return a helpful error message when attempting to use a table function where a non-table function is expected. | 整理、テスト、文言だけ |
| #3337 | 2025-12-10 | 効かない | [auto-bump] [no-release-notes] dependency by coffeegoddd | 依存の上げだけ |
| #3339 | 2025-12-12 | 効かない | Do not erase projections for insert sources inside triggers | WordPress が使わない機能 |
| #3340 | 2025-12-12 | 効かない | enable native indexes on all in-memory tables by default | 整理、テスト、文言だけ |
| #3342 | 2025-12-12 | 効かない | Set `SubqueryAlias.OuterScopeVisibility` to true if inside trigger scope | WordPress が使わない機能 |
| #3345 | 2025-12-16 | 効かない | various `analyzer` and `planbuilder` optimizations | 性能だけ |
| #3347 | 2025-12-16 | 効かない | Customizable filter walk for costed index scan | Doltgres だけ |
| #3348 | 2025-12-17 | 効かない | avoid heap allocs in `transform.Inspect` and `transform.InspectExpr` | 性能だけ |
| #3349 | 2025-12-19 | 効かない | Add User Variable discards and fix per-row field to column indices for `LOAD DATA` | WordPress が使わない機能 |
| #3352 | 2025-12-22 | 効かない | Use concurrency in groupby `compute` | 性能だけ |
| #3354 | 2026-01-06 | 効かない | Add required attribution for mascot image to README | 変えたファイルがテスト、memory、文書だけ |
| #3357 | 2025-12-30 | 効かない | [no-release-notes] Add test for lookup join with out of range key | 変えたファイルがテスト、memory、文書だけ |
| #3358 | 2025-12-30 | 効かない | condense `time.Time` library calls | 性能だけ |
| #3360 | 2026-01-03 | 効かない | [no-release-notes] Add test for indexed varchar column filtered by boolean | 変えたファイルがテスト、memory、文書だけ |
| #3362 | 2026-01-05 | 効かない | [no-release-notes] Join cleanups | 整理、テスト、文言だけ |
| #3363 | 2026-01-06 | 効かない | [no-release-notes] More cleanup | 整理、テスト、文言だけ |
| #3364 | 2026-01-06 | 効かない | Add panic handling to spawned goroutine | フォークに該当のコードがない |
| #3366 | 2026-01-07 | 効かない | Fix query ok for `drop view` | WordPress が使わない機能 |
| #3367 | 2026-01-08 | 効かない | drop schema support | Doltgres だけ |
| #3368 | 2026-01-08 | 効かない | Do not parse trigger bodies in `LoadOnly` context | WordPress が使わない機能 |
| #3369 | 2026-01-10 | 効かない | Added DISTINCT ON handling | Doltgres だけ |
| #3370 | 2026-01-21 | 効かない | Wrap `errgroup.Group.Go()` calls for consistent panic recovery | 整理、テスト、文言だけ |
| #3371 | 2026-01-08 | 効かない | Error out for `NATURAL FULL JOIN` | WordPress が使わない機能 |
| #3372 | 2026-01-08 | 効かない | [auto-bump] [no-release-notes] dependency by angelamayxie | 依存の上げだけ |
| #3376 | 2026-01-09 | 効かない | Add `IsExpr` case to `replaceVariablesInExpr` | WordPress が使わない機能 |
| #3379 | 2026-01-23 | 効かない | Create scope mapping for views | 性能だけ |
| #3380 | 2026-01-15 | 効かない | Generate index lookups for filters that appear in lateral joins | 性能だけ |
| #3381 | 2026-01-15 | 効かない | Use key type for memory table index filter range | 整理、テスト、文言だけ |
| #3383 | 2026-01-15 | 効かない | When applying indexes from outer scopes, resolve references to table aliases | 性能だけ |
| #3384 | 2026-01-21 | 効かない | Look at every join node parent when computing outer scopes. | 性能だけ |
| #3385 | 2026-01-20 | 効かない | [no-release-notes] Add skipped JoinOp test for enums of different types | 整理、テスト、文言だけ |
| #3386 | 2026-01-23 | 効かない | Push filters that contain references to outer/lateral scopes. | 性能だけ |
| #3387 | 2026-01-27 | 効かない | add TODOs and update simplified Not type (wasn't expecting this branch to merge) | 整理、テスト、文言だけ |
| #3389 | 2026-01-21 | 効かない | remove `convertLeftAndRight` | 性能だけ |
| #3391 | 2026-01-21 | 効かない | [no-release-notes] Add test to make sure serial is the right type | 依存の上げだけ |
| #3392 | 2026-01-21 | 効かない | [auto-bump] [no-release-notes] dependency by angelamayxie | 依存の上げだけ |
| #3393 | 2026-01-22 | 効かない | add `transform.InspectWithOpaque` function | 整理、テスト、文言だけ |
| #3395 | 2026-01-22 | 効かない | fewer `strings.ToLower` calls in `gatherTableAlias` | 性能だけ |
| #3396 | 2026-01-26 | 効かない | avoid converting `float64` to `float64` | 性能だけ |
| #3397 | 2026-01-26 | 効かない | [auto-bump] [no-release-notes] dependency by NathanGabrielson | 依存の上げだけ |
| #3399 | 2026-01-27 | 効かない | GMS warning on functional indices | 機能の追加 |
| #3400 | 2026-02-02 | 効かない | Allow pushing filters past Project nodes | 性能だけ |
| #3403 | 2026-01-28 | 効かない | /go.{mod,sum}: bump go version | 依存の上げだけ |
| #3404 | 2026-01-28 | 効かない | Bump google.golang.org/protobuf from 1.28.1 to 1.33.0 | 依存の上げだけ |
| #3405 | 2026-01-28 | 効かない | [auto-bump] [no-release-notes] dependency by coffeegoddd | 依存の上げだけ |
| #3408 | 2026-01-31 | 効かない | Added runner to hooks | 機能の追加 |
| #3414 | 2026-02-02 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 依存の上げだけ |
| #3416 | 2026-02-05 | 効かない | allow Doltgres to add more information schema tables | Doltgres だけ |
| #3424 | 2026-02-10 | 効かない | [no-release-notes] Update `left join` join op tests | 変えたファイルがテスト、memory、文書だけ |
| #3425 | 2026-02-11 | 効かない | Do not set scope length during join planning | 回復する panic で、WordPress が出さない文 |
| #3426 | 2026-02-12 | 効かない | Do not push filters into SubqueryAliases that wrap RecursiveCTEs | WordPress が使わない機能 |
| #3427 | 2026-02-12 | 効かない | Do not wrap hoisted subquery in Limit node until AFTER checking if it's a SubqueryAlias | 性能だけ |
| #3429 | 2026-02-16 | 効かない | Disallow creating VECTOR indexes on nullable columns | WordPress が使わない機能 |
| #3432 | 2026-02-21 | 効かない | `TopN` optimizations | 性能だけ |
| #3434 | 2026-02-18 | 効かない | /_integration/php: fix cve | 変えたファイルがテスト、memory、文書だけ |
| #3436 | 2026-02-19 | 効かない | Get index column name using offset from table name prefix length | WordPress が使わない機能 |
| #3437 | 2026-03-03 | 効かない | Bump filippo.io/edwards25519 from 1.1.0 to 1.1.1 | 依存の上げだけ |
| #3439 | 2026-03-02 | 効かない | Bump filippo.io/edwards25519 from 1.1.0 to 1.1.1 in /_integration/go | 変えたファイルがテスト、memory、文書だけ |
| #3440 | 2026-02-20 | 効かない | [no-release-notes] Rename variable names from date to time | 整理、テスト、文言だけ |
| #3441 | 2026-02-20 | 効かない | bug fix: allow `ALTER TABLE DROP CONSTRAINT` to remove unique indexes | 機能の追加 |
| #3443 | 2026-02-23 | 効かない | [auto-bump] [no-release-notes] dependency by reltuk | 依存の上げだけ |
| #3445 | 2026-02-24 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 依存の上げだけ |
| #3446 | 2026-02-25 | 効かない | [auto-bump] [no-release-notes] dependency by jennifersp | 依存の上げだけ |
| #3448 | 2026-02-26 | 効かない | Move `/` guard on new databases to Dolt and add `sql.ErrWrongDBName` and `sql.SQLError` to automatically add other error metadata | 整理、テスト、文言だけ |
| #3449 | 2026-02-25 | 効かない | keep top-level `plan.Limit` for doltgres `generate_series` | Doltgres だけ |
| #3450 | 2026-03-10 | 効かない | [no-release-notes] handle authorization for functions (doltgres only) | Doltgres だけ |
| #3452 | 2026-03-02 | 効かない | lazy sql-mode string | 性能だけ |
| #3457 | 2026-03-04 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3458 | 2026-03-11 | 効かない | Move prepared queries to `sql.Session` and add parsed query cache | 整理、テスト、文言だけ |
| #3460 | 2026-03-09 | 効かない | [no-release-notes] .github/workflows/dependency-test.yml: Do not trigger this workflow on issue_comment. | 変えたファイルがテスト、memory、文書だけ |
| #3463 | 2026-03-09 | 効かない | remove hacky handling of trigger parsing | WordPress が使わない機能 |
| #3465 | 2026-03-10 | 効かない | support IF EXISTS in ALTER TABLE | 機能の追加 |
| #3466 | 2026-03-11 | 効かない | fix create db if not exists and drop db if exists to not return errors under concurrency | CREATE DATABASE は HeimDB が拒否する |
| #3467 | 2026-03-12 | 効かない | add sql.Databaser to alter default and alter index | 整理、テスト、文言だけ |
| #3470 | 2026-03-16 | 効かない | [no-release-notes] Added timezone to integration tests | 変えたファイルがテスト、memory、文書だけ |
| #3471 | 2026-03-19 | 効かない | Changed some reflection checks to use type equality | 整理、テスト、文言だけ |
| #3472 | 2026-03-19 | 効かない | Match MySQL output when marshalling datetimes in JSON objects. | WordPress が使わない機能 |
| #3473 | 2026-03-18 | 効かない | fix missing check expressions in child expressions of update | WordPress が使わない機能 |
| #3474 | 2026-03-26 | 効かない | Bump google.golang.org/grpc from 1.56.3 to 1.79.3 | 依存の上げだけ |
| #3479 | 2026-03-24 | 効かない | sql: background_threads.go: Have Shutdown return nil error on success. | 整理、テスト、文言だけ |
| #3480 | 2026-03-25 | 効かない | Improve performance when displaying timestamps with precision. | 性能だけ |
| #3481 | 2026-03-25 | 効かない | Improve performance when displaying zero timestamps. | 性能だけ |
| #3482 | 2026-03-27 | 効かない | Allow infinity and NaN float64 values | Doltgres だけ |
| #3483 | 2026-03-25 | 効かない | Fix `MATCH AGAINST` fails when fulltext-indexed table is accessed via `JOIN` alias | WordPress が使わない機能 |
| #3484 | 2026-03-30 | 効かない | sql/plan: cached_results.go: Remove the global CachedResultsManager. The Analyzer now owns and threads it. | 整理、テスト、文言だけ |
| #3485 | 2026-03-25 | 効かない | Fix prefix index validation for non-utf8mb4 charsets | HeimDB が拒否する文 |
| #3487 | 2026-03-25 | 効かない | /.github/workflows: bump checkout action | 変えたファイルがテスト、memory、文書だけ |
| #3488 | 2026-03-27 | 効かない | Fix `WHERE` filter on view columns backed by string literals fails with index out of bounds | WordPress が使わない機能 |
| #3489 | 2026-03-30 | 効かない | [no-release-notes] go.mod: Bump dolthub/sqllogictest version to match doltgresql. Makes multi-repo refactoring easier. | 依存の上げだけ |
| #3490 | 2026-03-26 | 効かない | bug fix: add check for unmatched column | 性能だけ |
| #3491 | 2026-04-22 | 効かない | Feature: Indexed functional expressions | 機能の追加 |
| #3492 | 2026-03-30 | 効かない | eventscheduler: Add the ability for eventscheduler to quiesce if there are no registered events and all registered databases promise to notify the scheduler anytime events change. | WordPress が使わない機能 |
| #3493 | 2026-03-31 | 効かない | Make QuiescableEvents discoverable on the PrivilegedDatabase wrapper. | WordPress が使わない機能 |
| #3494 | 2026-06-04 | 効かない | [no-release-notes] Unskip and extend test-suite for unquoted identifiers | 変えたファイルがテスト、memory、文書だけ |
| #3496 | 2026-03-31 | 効かない | Revert "Make QuiescableEvents discoverable on the PrivilegedDatabase wrapper." | WordPress が使わない機能 |
| #3497 | 2026-03-31 | 効かない | sql: Change from a marker method on QuiescableEventDatabase to something that returns a bool. | WordPress が使わない機能 |
| #3498 | 2026-04-01 | 効かない | eventscheduler: Block event scheduler's Close() on finishing its spawned goroutines. | WordPress が使わない機能 |
| #3499 | 2026-04-01 | 効かない | Support for unwrapping geometry values, which may be stored inline or out of band | WordPress が使わない機能 |
| #3501 | 2026-04-13 | 効かない | Several new spatial funcs | WordPress が使わない機能 |
| #3505 | 2026-04-08 | 効かない | trim analyzer rules for simple selects | 性能だけ |
| #3508 | 2026-04-12 | 効かない | go.mod: Bump go-icu-regex. | 依存の上げだけ |
| #3510 | 2026-04-15 | 効かない | Ignore unique key errors when inserting into fulltext position table | WordPress が使わない機能 |
| #3511 | 2026-04-16 | 効かない | cache arithmetic type | 性能だけ |
| #3512 | 2026-04-23 | 効かない | new signal error to skip modifying rows during DML operation | Doltgres だけ |
| #3513 | 2026-04-22 | 効かない | Threaded context everywhere | 整理、テスト、文言だけ |
| #3514 | 2026-04-21 | 効かない | handle foreign key errors for Doltgres | Doltgres だけ |
| #3516 | 2026-04-21 | 効かない | Fix `SHOW CREATE VIEW` fail when underlying table is dropped | WordPress が使わない機能 |
| #3517 | 2026-04-21 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3519 | 2026-04-21 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 依存の上げだけ |
| #3522 | 2026-04-23 | 効かない | go.mod: Bump to golang 1.26.2. | 依存の上げだけ |
| #3523 | 2026-05-04 | 効かない | Add handling of TargetRowSize table attribute to GMS interfaces | 機能の追加 |
| #3524 | 2026-04-24 | 効かない | [no-release-notes] don't create new context in `expression.Alias.String()` | 性能だけ |
| #3526 | 2026-04-24 | 効かない | Update for functional expression index parser changes | 機能の追加 |
| #3528 | 2026-04-30 | 効かない | Bump go.opentelemetry.io/otel from 1.39.0 to 1.41.0 | 依存の上げだけ |
| #3529 | 2026-04-24 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 整理、テスト、文言だけ |
| #3532 | 2026-05-13 | 効かない | replace all `decimal.Decimal` with `*apd.Decimal` | 整理、テスト、文言だけ |
| #3533 | 2026-05-01 | 効かない | fixed context reassignment error, which causes a panic in a running d… | WordPress が使わない機能 |
| #3534 | 2026-05-01 | 効かない | [no-release-notes] Remove uses of context.Background() by threading in context from callers. | 整理、テスト、文言だけ |
| #3535 | 2026-05-01 | 効かない | Enforce inline column references as foreign key constraints | WordPress が使わない機能 |
| #3537 | 2026-05-28 | 効かない | sql: opt-in SQL trace redaction | 機能の追加 |
| #3538 | 2026-05-05 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3539 | 2026-05-07 | 効かない | Bug fix for dropping sort nodes | WordPress が使わない機能 |
| #3542 | 2026-05-11 | 効かない | [no-release-notes] Mark test with MySQL dialect to prevent it from failing on Doltgres | 変えたファイルがテスト、memory、文書だけ |
| #3543 | 2026-05-20 | 効かない | Improve costed index scans for tuples by destructuring tuple operations when possible. | 性能だけ |
| #3544 | 2026-05-15 | 効かない | [no-release-notes] Remove unused Makefile. | 変えたファイルがテスト、memory、文書だけ |
| #3545 | 2026-05-15 | 効かない | [no-release-notes] Update MAINTAINERS. | 変えたファイルがテスト、memory、文書だけ |
| #3546 | 2026-05-15 | 効かない | Cache context to use for `String()` methods | 整理、テスト、文言だけ |
| #3548 | 2026-05-15 | 効かない | [no-release-notes] Slight context fixes | Doltgres だけ |
| #3549 | 2026-05-15 | 効かない | [no-release-notes] Misc cleanups related to filter pushdown | 整理、テスト、文言だけ |
| #3550 | 2026-05-19 | 効かない | modify CompareJSON to use sorted object keys | WordPress が使わない機能 |
| #3551 | 2026-05-19 | 効かない | star expression used in function | Doltgres だけ |
| #3552 | 2026-05-21 | 効かない | Do not allow `CASCADE` and `SET NULL` foreign keys on columns that are referenced by `STORED` generated column expressions | WordPress が使わない機能 |
| #3553 | 2026-05-27 | 効かない | add partial index on WHERE predicate | 機能の追加 |
| #3554 | 2026-05-21 | 効かない | [auto-bump] [no-release-notes] dependency by jennifersp | 依存の上げだけ |
| #3555 | 2026-05-21 | 効かない | Lookup table when not specified in `DROP INDEX` statements | Doltgres だけ |
| #3557 | 2026-05-21 | 効かない | Include `CHAR` and `VARCHAR` in `IN` expression optimization | 性能だけ |
| #3558 | 2026-05-22 | 効かない | Unwrap wrapped strings before casting to string during `RegexpReplace.Eval` | WordPress が使わない機能 |
| #3559 | 2026-07-22 | 効かない | [no-release-notes] docs: mention MariaDB client compatibility in README | 変えたファイルがテスト、memory、文書だけ |
| #3562 | 2026-05-29 | 効かない | pass schema name when retreiving function | Doltgres だけ |
| #3563 | 2026-06-01 | 効かない | Introduce `SchemaObjectNameValidator` interface | Doltgres だけ |
| #3565 | 2026-05-28 | 効かない | Allow unparenthesized function expressions in column defaults. | 機能の追加 |
| #3566 | 2026-05-28 | 効かない | analyzer: export BuildForeignKeyEditor for callers outside DML analysis | 整理、テスト、文言だけ |
| #3567 | 2026-05-28 | 効かない | fix for division on decimals | フォークに該当のコードがない |
| #3568 | 2026-06-04 | 効かない | bug fix for non-found search paths | WordPress が使わない機能 |
| #3569 | 2026-06-03 | 効かない | [no-release-notes] Pass context to extended type deserialization | Doltgres だけ |
| #3570 | 2026-06-02 | 効かない | improvements to time to wire format | 性能だけ |
| #3571 | 2026-06-03 | 効かない | Add `IndexNameGenerator` interface | Doltgres だけ |
| #3573 | 2026-06-04 | 効かない | support NOT VALID on foreign key and check constraints | Doltgres だけ |
| #3574 | 2026-06-03 | 効かない | Ensure SchemaObjectNameValidator is used when a table is renamed | Doltgres だけ |
| #3575 | 2026-06-04 | 効かない | decompose tuple inequalities | 性能だけ |
| #3576 | 2026-06-04 | 効かない | [auto-bump] [no-release-notes] dependency by jennifersp | 依存の上げだけ |
| #3577 | 2026-06-05 | 効かない | fix: return ok result schema for `ALTER TABLE` comment | フォークに該当のコードがない |
| #3578 | 2026-06-05 | 効かない | fix: retain table comment when primary key is a separate clause | WordPress が使わない機能 |
| #3579 | 2026-06-04 | 効かない | make hashjoin compatible with doltgres | Doltgres だけ |
| #3582 | 2026-06-10 | 効かない | fix: panic in regexp functions over large text in group by | 依存の上げだけ |
| #3584 | 2026-07-22 | 効かない | Add LockSubsystem.TryLock for non-blocking lock acquisition | 整理、テスト、文言だけ |
| #3585 | 2026-06-15 | 効かない | Add right side lookup overhead for LookupJoins | 性能だけ |
| #3587 | 2026-06-12 | 効かない | go.mod: Bump github.com/lestrrat-go/strftime to v1.2.0. Fixes a lock leak which can deadlock queries in Dolt. | 依存の上げだけ |
| #3589 | 2026-06-16 | 効かない | fix JSON control-character escaping and out-of-band text reads | WordPress が使わない機能 |
| #3590 | 2026-06-16 | 効かない | server: Use pollForClosedConnection on all query paths, including DML, DDL, OkResults, empty result schemas, etc. | 整理、テスト、文言だけ |
| #3592 | 2026-06-16 | 効かない | [auto-bump] [no-release-notes] dependency by reltuk | 依存の上げだけ |
| #3593 | 2026-06-16 | 効かない | server/handler.go: Use event-driven notification to quickly cancel a running query after a client-disconnect, instead of polling socket state in a platform-dependent way. | 機能の追加 |
| #3594 | 2026-06-17 | 効かない | allow using star expr after sql value for Doltgres | Doltgres だけ |
| #3595 | 2026-06-17 | 効かない | Rollback event-driven stale-client detection. | 整理、テスト、文言だけ |
| #3596 | 2026-06-17 | 効かない | [auto-bump] [no-release-notes] dependency by reltuk | 依存の上げだけ |
| #3597 | 2026-06-17 | 効かない | Revert "Merge pull request #3590 from dolthub/aaron/poll-for-closed-connection-more-cases" | 整理、テスト、文言だけ |
| #3598 | 2026-06-23 | 効かない | Fix `JSON_LENGTH` and member-access lookups to match MySQL | WordPress が使わない機能 |
| #3600 | 2026-07-08 | 効かない | Allow filter pushdown in subqueries when analyzing insert sources | 性能だけ |
| #3601 | 2026-06-22 | 効かない | cache `GeneralizeTypes` output and avoid `fmt.Sprintf` in `HashOfSimple` | 性能だけ |
| #3602 | 2026-06-23 | 効かない | Create single override point for `SplitConjunction` | Doltgres だけ |
| #3603 | 2026-06-23 | 効かない | Only get projection expressions once when creating filterSet during filter pushdown | 整理、テスト、文言だけ |
| #3605 | 2026-06-24 | 効かない | Use `TableId`s instead of names when mapping filters to tables | 同じ名前の表を別名なしで結ぶ形で、MySQL も 1066（Not unique table/alias）で拒否する。別名を付ければ HeimDB のブランチの表どうしでも正しい |
| #3606 | 2026-06-25 | 効かない | [auto-bump] [no-release-notes] dependency by reltuk | 整理、テスト、文言だけ |
| #3607 | 2026-06-25 | 効かない | [no-release-notes] Move `t.Skip()` to correct run for `TestQueryPlan` | 変えたファイルがテスト、memory、文書だけ |
| #3608 | 2026-07-08 | 効かない | Rewrite `pushFilters` to use single top-down node traversal | 整理、テスト、文言だけ |
| #3609 | 2026-06-26 | 効かない | server: Add client-disconnect watching to more platforms and more query types. | 機能の追加 |
| #3610 | 2026-06-26 | 効かない | pushdown inequalities to merge joins | 性能だけ |
| #3611 | 2026-08-05 | 効かない | avoid `fmt.Sprintf` in groupby hash | Doltgres だけ |
| #3612 | 2026-07-06 | 効かない | Bug fix: allow running analysis on indexed join queries with bindvars | フォークに該当のコードがない |
| #3613 | 2026-07-09 | 効かない | Apply `IndexSearchable` optimization to `sql.Equality` expressions | Doltgres だけ |
| #3614 | 2026-07-13 | 効かない | Report Sum/Avg's true float64 type; add window-func override hook | Doltgres だけ |
| #3615 | 2026-07-13 | 効かない | Bump golang.org/x/crypto from 0.46.0 to 0.52.0 | 依存の上げだけ |
| #3616 | 2026-07-10 | 効かない | [no-release-notes] Refactor set types into new `sets` package | 整理、テスト、文言だけ |
| #3617 | 2026-07-16 | 効かない | special case on extended type for getting compare type | Doltgres だけ |
| #3619 | 2026-07-13 | 効かない | sql: Fix ALTER USER to allow the Identity field of the User record to be updated. | WordPress が使わない機能 |
| #3621 | 2026-07-15 | 効かない | bug fix: honor named window reference | WordPress が使わない機能 |
| #3623 | 2026-07-22 | 効かない | memory: use stable sort for secondary index ordering | 変えたファイルがテスト、memory、文書だけ |
| #3625 | 2026-09-02 | 効かない | Advertise binary charset for `BIT` result columns | WordPress が使わない機能 |
| #3627 | 2026-07-17 | 効かない | fix stats functions to include schema | Doltgres だけ |
| #3628 | 2026-07-20 | 効かない | [no-release-notes] Refactor: new `sorters` package | 整理、テスト、文言だけ |
| #3629 | 2026-07-17 | 効かない | [no-release-notes] Rename SortField(s) to SortCondition(s) | 整理、テスト、文言だけ |
| #3633 | 2026-07-28 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 依存の上げだけ |
| #3634 | 2026-07-20 | 効かない | Add support for multiple expressions in functional indexes | 機能の追加 |
| #3637 | 2026-07-21 | 効かない | [no-release-notes] Move heap sort to `sorters` package and make `RowSorter` fields private | 整理、テスト、文言だけ |
| #3638 | 2026-07-22 | 効かない | Bug fixes for secondary indexes using virtual columns | WordPress が使わない機能 |
| #3639 | 2026-07-28 | 効かない | have coster consider not picking secondary indexes | 性能だけ |
| #3641 | 2026-07-23 | 効かない | Bug fix for a panic when calling a stored procedure without a database selected | 回復する panic で、WordPress が出さない文 |
| #3644 | 2026-07-27 | 効かない | add ExtendedTableFunction interface to support table function schema … | Doltgres だけ |
| #3646 | 2026-07-27 | 効かない | [no-release-notes] Reorganize query script tests | 変えたファイルがテスト、memory、文書だけ |
| #3647 | 2026-07-27 | 効かない | [no-release-notes] Revert "[no-release-notes] Reorganize query script tests" | 変えたファイルがテスト、memory、文書だけ |
| #3648 | 2026-07-27 | 効かない | [no-release-notes] Reorganize query script tests | 変えたファイルがテスト、memory、文書だけ |
| #3652 | 2026-07-28 | 効かない | Allow aliasing column names in a table function | 機能の追加 |
| #3653 | 2026-07-28 | 効かない | adding prepare tests for insert and update | 変えたファイルがテスト、memory、文書だけ |
| #3654 | 2026-07-28 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 依存の上げだけ |
| #3655 | 2026-07-29 | 効かない | [no-release-notes] Add test for left outer join with duplicate join keys and non-merge filter | 変えたファイルがテスト、memory、文書だけ |
| #3656 | 2026-07-29 | 効かない | Bug fix for `CASE` expressions with `ExtendedType` instances | Doltgres だけ |
| #3657 | 2026-07-29 | 効かない | check stats prov for empty stat | フォークに該当のコードがない |
| #3659 | 2026-07-30 | 効かない | fallback to gms types for extended type histograms | Doltgres だけ |
| #3660 | 2026-07-30 | 効かない | bug fixes for schema in table statistics | Doltgres だけ |
| #3661 | 2026-07-31 | 効かない | create schema from inscope columns of the table being parsed as column | Doltgres だけ |
| #3662 | 2026-08-04 | 効かない | add case for injected exprs in node batch | Doltgres だけ |
| #3663 | 2026-08-03 | 効かない | bug fix: infer a bindvar's column type | Doltgres だけ |
| #3664 | 2026-08-03 | 効かない | Check if SubqueryAlias itself is lateral when prepending rows | HeimDB はビューを拒否する。ビューを使わない派生表の形（入れ子の派生表の EXISTS）は MySQL と同じ |
| #3665 | 2026-08-05 | 効かない | Adding a new `TimeDeltaExpression` interface | Doltgres だけ |
| #3666 | 2026-08-05 | 効かない | Include `orderBy` in various window functions to return correct default framer | WordPress が使わない機能 |
| #3669 | 2026-08-06 | 効かない | Combine `assignmentExprsToUpdateExprs` and `buildOnDupUpdateExprs` | 回復する panic で、WordPress が出さない文 |
| #3670 | 2026-08-10 | 効かない | Fix panic when a RANGE window frame offset pushes a SET/ENUM order-by value outside its valid domain | WordPress が使わない機能 |
| #3671 | 2026-08-06 | 効かない | Handle negative start positions in `LOCATE` function | 回復する panic で、WordPress が出さない文 |
| #3672 | 2026-08-06 | 効かない | Pass context through aggregate/window check funcs | Doltgres だけ |
| #3674 | 2026-08-07 | 効かない | Error out for undefined window names | WordPress が使わない機能 |
| #3676 | 2026-08-07 | 効かない | Update various window aggregation functions to expect the correct number of children | WordPress が使わない機能 |
| #3677 | 2026-08-08 | 効かない | Bump jsonpath and add new test for parsing JSON null. | 依存の上げだけ |
| #3687 | 2026-08-10 | 効かない | SECURITY: Document security stance on panics generated by GMS. | 整理、テスト、文言だけ |
| #3689 | 2026-08-10 | 効かない | Refactor window function tests into `ScriptTests` | 変えたファイルがテスト、memory、文書だけ |
| #3690 | 2026-08-11 | 効かない | Check for null before casting `LAST_INSERT_ID` input | 回復する panic で、WordPress が出さない文 |
| #3691 | 2026-08-11 | 効かない | block certain statements from `PREPARE` | WordPress が使わない機能 |
| #3692 | 2026-08-12 | 効かない | When serializing bytes for a MySQL client, correctly handle wrapped values, such as adaptive encoded columns | WordPress が使わない機能 |
| #3693 | 2026-08-11 | 効かない | fix panic for `GeometryFromWKT` and various GeoSpatial bugs | WordPress が使わない機能 |
| #3694 | 2026-08-12 | 効かない | [no-release-notes] Unskip tests that now pass in Doltgres | 変えたファイルがテスト、memory、文書だけ |
| #3695 | 2026-08-12 | 効かない | Allow `NULL` and other numerical swap flags for `BIN_TO_UUID` | 回復する panic で、WordPress が出さない文 |
| #3696 | 2026-08-12 | 効かない | cap initial heapsize | フォークに該当のコードがない |
| #3697 | 2026-08-13 | 効かない | fix inet6 type assertions | 回復する panic で、WordPress が出さない文 |
| #3699 | 2026-08-19 | 効かない | Correctly parse and compare JSON docs with numbers that are too large to fit in a float64 without losing precision. | WordPress が使わない機能 |
| #3700 | 2026-08-13 | 効かない | fix panic on `rand(null)` | 回復する panic で、WordPress が出さない文 |
| #3701 | 2026-08-13 | 効かない | [no-release-notes] skip large limit test for non mysql dialects | 変えたファイルがテスト、memory、文書だけ |
| #3703 | 2026-08-17 | 効かない | Fix panic for `LIKE` expression with empty `ESCAPE` character | 回復する panic で、WordPress が出さない文 |
| #3704 | 2026-09-01 | 効かない | transaction-scoped session var support | Doltgres だけ |
| #3705 | 2026-08-27 | 効かない | Actually resolve column `DEFAULT` expression when building scalar expressions | 機能の追加 |
| #3710 | 2026-08-18 | 効かない | fix panic in TRIM functions | 回復する panic で、WordPress が出さない文 |
| #3714 | 2026-08-19 | 効かない | fix panic on invalid `N` argument to `NTILE` | WordPress が使わない機能 |
| #3716 | 2026-08-19 | 効かない | fix panic for `UNION`, `INTERSECT` and `EXCEPT` with unequal schema lengths | 回復する panic で、WordPress が出さない文 |
| #3718 | 2026-08-20 | 効かない | Fix functional index display in `SHOW INDEX` and `information_schema.statistics` | WordPress が使わない機能 |
| #3719 | 2026-08-21 | 効かない | fix panic on date type in `Round()` | 回復する panic で、WordPress が出さない文 |
| #3724 | 2026-08-28 | 効かない | Added extended vector support | WordPress が使わない機能 |
| #3725 | 2026-08-25 | 効かない | Support PostgreSQL scalar function aliases in FROM | Doltgres だけ |
| #3726 | 2026-08-25 | 効かない | Handle empty set results in table function wrappers | Doltgres だけ |
| #3727 | 2026-08-25 | 効かない | Support extended types in COUNT DISTINCT | Doltgres だけ |
| #3728 | 2026-08-26 | 効かない | Expose DISTINCT aggregate retention | Doltgres だけ |
| #3730 | 2026-08-28 | 効かない | Return an error for a bind variable in an AS OF clause | WordPress が使わない機能 |
| #3731 | 2026-08-27 | 効かない | Reject unsafe partial index join scans | フォークに該当のコードがない |
| #3732 | 2026-09-01 | 効かない | Validate DISTINCT windows after function resolution | Doltgres だけ |
| #3735 | 2026-08-28 | 効かない | handle parenthesized select with trailing order by and limit | 機能の追加 |
| #3736 | 2026-08-28 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3737 | 2026-08-28 | 効かない | Preserve LEFT JOIN semantics for lateral joins with no join filters | WordPress が使わない機能 |
| #3738 | 2026-08-28 | 効かない | Don't set the max1Row query flag for plans containing RowIter exprs | Doltgres だけ |
| #3740 | 2026-08-28 | 効かない | Convert `LOAD_FILE` filename to text type | フォークに該当のコードがない |
| #3741 | 2026-08-31 | 効かない | Don't re-evaluate set-returning expressions in the final projection | Doltgres だけ |
| #3742 | 2026-08-31 | 効かない | Avoid redundant window output sort | 性能だけ |
| #3744 | 2026-08-31 | 効かない | sql/mysql_db: Make OverwriteUsersAndGrantsData leave ephemeral users in place. | WordPress が使わない機能 |
| #3745 | 2026-09-01 | 効かない | Plumb DROP TABLE ... CASCADE through to plan.DropTable | Doltgres だけ |
| #3746 | 2026-09-01 | 効かない | Support conditional duplicate updates | Doltgres だけ |
| #3747 | 2026-09-01 | 効かない | sql.ValueRows: Correctly handle filter expressions that aren't bools | フォークに該当のコードがない |
| #3748 | 2026-09-01 | 効かない | sql.ValueRows: Correctly handle NULLs in comparisons. | フォークに該当のコードがない |
| #3750 | 2026-09-01 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 依存の上げだけ |
| #3751 | 2026-09-02 | 効かない | [no-release-notes] add dolthub/dolt#11469 regression tests | 変えたファイルがテスト、memory、文書だけ |
| #3753 | 2026-09-03 | 効かない | Prevent insert expression slice aliasing | WordPress が使わない機能 |
| #3756 | 2026-09-03 | 効かない | [no-release-notes] add #11468 regressions | 変えたファイルがテスト、memory、文書だけ |
| #3758 | 2026-09-03 | 効かない | Limit PostgreSQL conflict ignores to duplicate keys | Doltgres だけ |
| #3759 | 2026-09-02 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 依存の上げだけ |
| #3762 | 2026-09-02 | 効かない | Allow REGEXP_REPLACE position after source | WordPress が使わない機能 |
| #3763 | 2026-09-02 | 効かない | Support binary values in CRC32 | WordPress が使わない機能 |
| #3764 | 2026-09-02 | 効かない | Preserve non-ASCII Soundex initial case | WordPress が使わない機能 |
| #3765 | 2026-09-02 | 効かない | Reject ABS of minimum BIGINT | WordPress が使わない機能 |
| #3766 | 2026-09-03 | 効かない | Fix recursive CTE result type coercion | WordPress が使わない機能 |
| #3768 | 2026-09-03 | 効かない | Match MySQL default `SQL_MODE` | 整理、テスト、文言だけ |
| #3769 | 2026-09-02 | 効かない | Fix decimal CEIL and FLOOR input mutation | フォークに該当のコードがない |
| #3772 | 2026-09-02 | 効かない | Support extended hours in TIME_TO_SEC | WordPress が使わない機能 |
| #3773 | 2026-09-03 | 効かない | Fix TIME_FORMAT hour specifiers | WordPress が使わない機能 |
| #3774 | 2026-09-03 | 効かない | Preserve empty strings in GROUP_CONCAT | WordPress が使わない機能 |
| #3775 | 2026-09-03 | 効かない | fix: change INFORMATION_SCHEMA default collation from utf8mb3 to utf8mb4 | 整理、テスト、文言だけ |
| #3776 | 2026-09-03 | 効かない | Warn when removing `NO_ZERO_IN_DATE` from `@@SQL_MODE` | 機能の追加 |
| #3778 | 2026-09-03 | 効かない | Add `MAKEDATE` function | 機能の追加 |
| #3779 | 2026-09-03 | 効かない | enginetest: cover keyless count column projection | 変えたファイルがテスト、memory、文書だけ |
| #3782 | 2026-09-03 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 依存の上げだけ |
| #3783 | 2026-09-03 | 効かない | Fix CURRENT_TIME precision in window expressions | WordPress が使わない機能 |
| #3784 | 2026-09-03 | 効かない | Preserve LIKE escape clauses in window expressions | WordPress が使わない機能 |
| #3785 | 2026-09-04 | 効かない | Preserve literal case in window expression identity | WordPress が使わない機能 |
| #3786 | 2026-09-03 | 効かない | Include bucket count in NTILE identity | WordPress が使わない機能 |
| #3788 | 2026-09-04 | 効かない | Return NULL for all-NULL window sums | WordPress が使わない機能 |
| #3790 | 2026-09-03 | 効かない | Enable bitwise window aggregates | WordPress が使わない機能 |
| #3791 | 2026-09-04 | 効かない | Distinguish window frame ending bounds | WordPress が使わない機能 |
| #3792 | 2026-09-04 | 効かない | Mark FIRST_VALUE results nullable | WordPress が使わない機能 |
| #3793 | 2026-09-04 | 効かない | Support TIME values in HOUR | 機能の追加 |
| #3794 | 2026-09-04 | 効かない | Return ROW_NUMBER values as INT64 | WordPress が使わない機能 |
| #3795 | 2026-09-03 | 効かない | Fix correlated subqueries alongside window functions | WordPress が使わない機能 |
| #3796 | 2026-09-03 | 効かない | Fix constant window expression partitions | WordPress が使わない機能 |
| #3800 | 2026-09-03 | 効かない | Validate set ops for aggregations in `ORDER BY` clause | 機能の追加 |
| #3802 | 2026-09-04 | 効かない | Add non-window CURRENT_TIME precision regression test | 変えたファイルがテスト、memory、文書だけ |
| #3804 | 2026-09-04 | 効かない | Omit `INVISIBLE` columns projections from wildcard projections and default inserts | 機能の追加 |
| #3805 | 2026-09-04 | 効かない | [no-release-notes] enginetest: add tests for directed TRIM without removal pattern | 依存の上げだけ |
| #3806 | 2026-09-05 | 効かない | [no-release-notes] enginetest: add regression tests for nested parentheses subqueries | 変えたファイルがテスト、memory、文書だけ |
| #3807 | 2026-09-09 | 効かない | Fix `SELECT` list alias resolution in window clauses | WordPress が使わない機能 |
| #3808 | 2026-09-04 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3809 | 2026-09-08 | 効かない | Count nonnumeric strings as non-null in window AVG and SUM | WordPress が使わない機能 |
| #3811 | 2026-09-04 | 効かない | Return error for standalone interval expressions | 回復する panic で、WordPress が出さない文 |
| #3812 | 2026-09-04 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3813 | 2026-09-04 | 効かない | Fix merging nondeterministic window expressions | WordPress が使わない機能 |
| #3815 | 2026-09-05 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3816 | 2026-09-08 | 効かない | Make sure window framer end is never after the partition start | WordPress が使わない機能 |
| #3818 | 2026-09-09 | 効かない | [no-release-notes] ci: optimize dolt and doltgresql dependency integration tests | 変えたファイルがテスト、memory、文書だけ |
| #3820 | 2026-09-09 | 効かない | Added DESC indexes and NULL ordering | 機能の追加 |
| #3821 | 2026-09-08 | 効かない | Fix correlated subqueries in window sorting | WordPress が使わない機能 |
| #3822 | 2026-09-10 | 効かない | Fix empty inserts with generated columns | WordPress が使わない機能 |
| #3832 | 2026-09-09 | 効かない | Always return `transform.NewTree` when replacing `GetField` with aliased projection expression | HeimDB はビューを拒否する。ビューを使わない派生表の形は MySQL と同じ |
| #3833 | 2026-09-10 | 効かない | [1/6] Fix independent expression strings and descriptions | 整理、テスト、文言だけ |
| #3834 | 2026-09-10 | 効かない | [2/6] Fix subquery strings and scalar alias resolution | 整理、テスト、文言だけ |
| #3835 | 2026-09-10 | 効かない | [3/6] Fix user-variable strings and prepared bindings | WordPress が使わない機能 |
| #3836 | 2026-09-10 | 効かない | [4/6] Test grouped windows and iterator cleanup | 変えたファイルがテスト、memory、文書だけ |
| #3840 | 2026-09-09 | 効かない | Support pluggable UPDATE assignment evaluation | Doltgres だけ |
| #3841 | 2026-09-09 | 効かない | Delimit window partition and order expressions in PartitionId | WordPress が使わない機能 |
| #3842 | 2026-09-15 | 効かない | Automate dependency bump approval and merge | 変えたファイルがテスト、memory、文書だけ |
| #3847 | 2026-09-10 | 効かない | Remove schema length check when analyzing subquery aliases | WordPress が使わない機能 |
| #3848 | 2026-09-10 | 効かない | [no-release-notes] Remove `resolve_subqueries_test.go` | 変えたファイルがテスト、memory、文書だけ |
| #3851 | 2026-09-11 | 効かない | [no-release-notes] Fixes for integrator functionality | 整理、テスト、文言だけ |
| #3852 | 2026-09-10 | 効かない | Reject unsupported DISTINCT window aggregates | WordPress が使わない機能 |
| #3855 | 2026-09-10 | 効かない | Implement Numeric to Date conversions | 機能の追加 |
| #3857 | 2026-09-10 | 効かない | Set return type for population statistic aggregate functions | WordPress が使わない機能 |
| #3858 | 2026-09-10 | 効かない | Cover vector index nullability across DDL paths | 変えたファイルがテスト、memory、文書だけ |
| #3860 | 2026-09-11 | 効かない | [no-release-notes] Remove aggregates | 整理、テスト、文言だけ |
| #3866 | 2026-09-14 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 依存の上げだけ |
| #3867 | 2026-09-29 | 効かない | Resolve `order by` columns using column ID instead of name to avoid ambiguity | WordPress が使わない機能 |
| #3868 | 2026-09-14 | 効かない | [no-release-notes] Rename `test` package to `testutils` | 整理、テスト、文言だけ |
| #3869 | 2026-09-15 | 効かない | [no-release-notes] Test foreign keys with spatial parent columns | 変えたファイルがテスト、memory、文書だけ |
| #3870 | 2026-09-16 | 効かない | [no-release-notes] Test CTE definition ordering | 変えたファイルがテスト、memory、文書だけ |
| #3871 | 2026-09-15 | 効かない | [no-release-notes] Test invalid binary decimal parameters | 変えたファイルがテスト、memory、文書だけ |
| #3872 | 2026-09-15 | 効かない | [no-release-notes] Test oversized primary-key lookups | 変えたファイルがテスト、memory、文書だけ |
| #3873 | 2026-09-16 | 効かない | [no-release-notes] Test INSERT aliases in duplicate-key updates | 変えたファイルがテスト、memory、文書だけ |
| #3874 | 2026-09-16 | 効かない | [no-release-notes] Test cursor continue-handler checksum loops | 変えたファイルがテスト、memory、文書だけ |
| #3875 | 2026-09-15 | 効かない | [no-release-notes] Test large DOUBLE arithmetic | 変えたファイルがテスト、memory、文書だけ |
| #3876 | 2026-09-16 | 効かない | [no-release-notes] Test consistent JSON object comparisons | 変えたファイルがテスト、memory、文書だけ |
| #3877 | 2026-09-16 | 効かない | [no-release-notes] Test virtual-column filtering and sorting | 変えたファイルがテスト、memory、文書だけ |
| #3878 | 2026-09-30 | 効かない | [no-release-notes] Fix column projection in memory vector scans | 変えたファイルがテスト、memory、文書だけ |
| #3879 | 2026-10-01 | 効かない | [no-release-notes] Fix memory vector scan projections with generated columns | 変えたファイルがテスト、memory、文書だけ |
| #3880 | 2026-09-16 | 効かない | [no-release-notes] Test JSON object paths on arrays | 変えたファイルがテスト、memory、文書だけ |
| #3881 | 2026-09-15 | 効かない | [no-release-notes] Test prepared decimal fractional digits | 変えたファイルがテスト、memory、文書だけ |
| #3882 | 2026-09-15 | 効かない | [no-release-notes] Test unique indexes on virtual columns | 変えたファイルがテスト、memory、文書だけ |
| #3883 | 2026-09-16 | 効かない | [no-release-notes] Test stored-procedure IF results over the wire | 変えたファイルがテスト、memory、文書だけ |
| #3884 | 2026-09-29 | 効かない | [no-release-notes] Migrate `CustomValueValidator` to `testutils` | 整理、テスト、文言だけ |
| #3885 | 2026-09-15 | 効かない | Support wildcard replication filter values | WordPress が使わない機能 |
| #3886 | 2026-09-15 | 効かない | Simplify partial join parent for window functions in `exists` subqueries | WordPress が使わない機能 |
| #3888 | 2026-09-15 | 効かない | Fix LOAD_FILE directory handle leaks | フォークに該当のコードがない |
| #3889 | 2026-09-16 | 効かない | Filled generation_expression and extra for generated columns in information_schema.columns | WordPress が使わない機能 |
| #3890 | 2026-10-07 | 効かない | Fix nullable RANGE frame boundaries | WordPress が使わない機能 |
| #3891 | 2026-10-06 | 効かない | Fix implicit peer frames for window functions | WordPress が使わない機能 |
| #3893 | 2026-10-06 | 効かない | Fix window expression identity collisions | WordPress が使わない機能 |
| #3894 | 2026-09-16 | 効かない | [auto-bump] [no-release-notes] dependency by zachmu | 依存の上げだけ |
| #3895 | 2026-09-16 | 効かない | [no-release-notes] refactor time functions | 整理、テスト、文言だけ |
| #3896 | 2026-09-16 | 効かない | [no-release-notes] test CTAS and INSERT with window functions and correlated subqueries | 変えたファイルがテスト、memory、文書だけ |
| #3897 | 2026-09-16 | 効かない | Optimize `SPACE` with `strings.Repeat` | 性能だけ |
| #3898 | 2026-09-17 | 効かない | Use common extended type for VALUES rows | Doltgres だけ |
| #3900 | 2026-09-21 | 効かない | Fixed duplicate window columns | WordPress が使わない機能 |
| #3904 | 2026-09-21 | 効かない | Fix `ONLY_FULL_GROUP_BY` validation for window functions | WordPress が使わない機能 |
| #3907 | 2026-09-23 | 効かない | Optimize aggregate functions and remove sort nodes on multi-column indexes provided that any columns before the aggregated/sorted column are constant | 性能だけ |
| #3908 | 2026-09-23 | 効かない | Unwrap `ANY_VALUE` for aggregates and window functions | WordPress が使わない機能 |
| #3909 | 2026-09-23 | 効かない | [no-release-notes] server: Expose the sql.Session for a *mysql.Conn through a GetSession accessor. | 整理、テスト、文言だけ |
| #3911 | 2026-09-25 | 効かない | Add delimited time regex | フォークに該当のコードがない |
| #3912 | 2026-09-23 | 効かない | [no-release-notes] Fix failing `TestWindowFunctions` in Doltgres CI tests | Doltgres だけ |
| #3914 | 2026-09-23 | 効かない | [no-release-notes] set the system timezone for DoltgreSQL CI tests | 変えたファイルがテスト、memory、文書だけ |
| #3915 | 2026-09-23 | 効かない | [no-release-notes] Mark any_value to mysql dialect | 変えたファイルがテスト、memory、文書だけ |
| #3916 | 2026-10-15 | 効かない | Prune table columns beneath HAVING | 性能だけ |
| #3919 | 2026-09-25 | 効かない | implement precision for TIME types | 機能の追加 |
| #3921 | 2026-09-25 | 効かない | Fix non-literal evaluation in `unix_timestamp` | 回復する panic で、WordPress が出さない文 |
| #3924 | 2026-09-28 | 効かない | Fix panic on nil decimal values in `VALUES` derived tables | 回復する panic で、WordPress が出さない文 |
| #3932 | 2026-10-20 | 効かない | When replacing a subquery with a table alias, push the subquery columnIds into the underlying table. | WordPress が使わない機能 |
| #3934 | 2026-09-28 | 効かない | [no-release-notes] Make `UNIX_TIMESTAMP` subquery string test timezone-independent | 変えたファイルがテスト、memory、文書だけ |
| #3935 | 2026-09-28 | 効かない | [no-release-notes] Fail when Dolt/DoltgreSQL integration tests also fail against main | 変えたファイルがテスト、memory、文書だけ |
| #3936 | 2026-10-02 | 効かない | Convert `ANY_VALUE` from an aggregate to a scalar function | WordPress が使わない機能 |
| #3937 | 2026-09-28 | 効かない | [no-release-notes] Accept `dolthub/dolt#N` and `dolthub/doltgresql#N` PR references in integration test workflow | 変えたファイルがテスト、memory、文書だけ |
| #3938 | 2026-09-29 | 効かない | [no-release-notes] Always run integration tests; gate merging on separate jobs | 変えたファイルがテスト、memory、文書だけ |
| #3939 | 2026-09-29 | 効かない | fix panic for `CAST` and `CONVERT` to types with invalid precision/scale | 回復する panic で、WordPress が出さない文 |
| #3946 | 2026-09-29 | 効かない | Default to `InternalDecimalType` for `CONVERT` with decimal arguments | フォークに該当のコードがない |
| #3947 | 2026-10-01 | 効かない | Execute duplicate-key update triggers | WordPress が使わない機能 |
| #3948 | 2026-09-30 | 効かない | Implement numeric converts to `TIME` with precision | 機能の追加 |
| #3949 | 2026-09-30 | 効かない | Fix column statistics queries without initialized privileges | WordPress が使わない機能 |
| #3951 | 2026-09-30 | 効かない | Validate temporal cast precision | 回復する panic で、WordPress が出さない文 |
| #3952 | 2026-09-30 | 効かない | Account for COUNT DISTINCT memory usage | 性能だけ |
| #3953 | 2026-09-30 | 効かない | Allow grouped expressions without column dependencies | WordPress が使わない機能 |
| #3954 | 2026-09-30 | 効かない | Respect context cancellation errors during group by aggregation | フォークに該当のコードがない |
| #3959 | 2026-09-30 | 効かない | Recompute stored generated columns after definition changes | WordPress が使わない機能 |
| #3961 | 2026-09-30 | 効かない | Store binary collation metadata explicitly | 性能だけ |
| #3962 | 2026-09-30 | 効かない | Clamp unsigned MEDIUMINT serialization to its maximum value | 整理、テスト、文言だけ |
| #3963 | 2026-09-30 | 効かない | Reset regular expressions between window partitions | WordPress が使わない機能 |
| #3965 | 2026-09-30 | 効かない | Reject out-of-range INET_NTOA arguments | WordPress が使わない機能 |
| #3970 | 2026-09-30 | 効かない | Support PostgreSQL UPDATE row counts | Doltgres だけ |
| #3972 | 2026-10-01 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #3974 | 2026-10-01 | 効かない | Test listener shutdown with pending connections | 変えたファイルがテスト、memory、文書だけ |
| #3975 | 2026-10-01 | 効かない | Test EXISTS over empty scalar aggregate inputs | 変えたファイルがテスト、memory、文書だけ |
| #3976 | 2026-10-01 | 効かない | Test EXISTS and NOT EXISTS with window functions | 変えたファイルがテスト、memory、文書だけ |
| #3982 | 2026-10-02 | 効かない | [no-release-notes] enginetest: add regression test for dolthub/dolt#11558 | 変えたファイルがテスト、memory、文書だけ |
| #3985 | 2026-10-13 | 効かない | Add regression test for ValueRowIter | 変えたファイルがテスト、memory、文書だけ |
| #3987 | 2026-10-06 | 効かない | [no-release-notes] Cleanup QueryTest functions | 変えたファイルがテスト、memory、文書だけ |
| #3989 | 2026-10-06 | 効かない | Infer precision for `TIME` function | 機能の追加 |
| #3990 | 2026-10-10 | 効かない | [no-release-notes] organize test queries scripts, breaking up script_queries.go | 変えたファイルがテスト、memory、文書だけ |
| #3991 | 2026-10-12 | 効かない | Optimize wildcard-only LIKE filters | 性能だけ |
| #3992 | 2026-10-08 | 効かない | Added view recursion detection | HeimDB が拒否する文 |
| #3993 | 2026-10-08 | 効かない | Moved the external function provider into the builder overrides | 整理、テスト、文言だけ |
| #3994 | 2026-10-08 | 効かない | Improve `TIME` parsing | WordPress が使わない機能 |
| #3996 | 2026-10-07 | 効かない | [auto-bump] [no-release-notes] dependency by nicktobey | 依存の上げだけ |
| #3997 | 2026-10-08 | 効かない | Fix extended-type hash key conversion | Doltgres だけ |
| #3998 | 2026-10-09 | 効かない | Prevent subquery filter pushdown from substituting set-returning functions into scalar predicates | Doltgres だけ |
| #4000 | 2026-10-09 | 効かない | Have nested stored procedures reuse existing transaction | WordPress が使わない機能 |
| #4003 | 2026-10-12 | 効かない | Estimate a join of two empty estimates as empty on every architecture | 性能だけ |
| #4006 | 2026-10-12 | 効かない | [no-release-notes] Initialize status variables in process list tests so they pass when run alone. | 変えたファイルがテスト、memory、文書だけ |
| #4007 | 2026-10-12 | 効かない | Stop ending the query in the process list when the top-level row iter is closed. The caller of BeginQuery owns EndQuery. | 整理、テスト、文言だけ |
| #4008 | 2026-10-13 | 効かない | Make Handler.doQuery start and end the root query span. Remove Context.RootSpan and deprecate WithRootSpan. | 整理、テスト、文言だけ |
| #4009 | 2026-10-12 | 効かない | End non-recording spans in NewSpanIter. | 整理、テスト、文言だけ |
| #4015 | 2026-10-14 | 効かない | Support conditional column drops in ALTER TABLE plans | 機能の追加 |
| #4016 | 2026-10-14 | 効かない | [auto-bump] [no-release-notes] dependency by tbantle22 | 依存の上げだけ |
| #4018 | 2026-10-14 | 効かない | [no-release-notes] add test for fixed issue | 変えたファイルがテスト、memory、文書だけ |
| #4019 | 2026-10-15 | 効かない | [no-release-notes] More `DATETIME` test fixes | フォークに該当のコードがない |
| #4022 | 2026-10-15 | 効かない | sql/types: build the JSON_EXTRACT result array only from paths that match | WordPress が使わない機能 |
| #4023 | 2026-10-21 | 効かない | Added AggregateCallResolver for functions that share a name with an aggregate | 整理、テスト、文言だけ |
| #4024 | 2026-10-15 | 効かない | Fix panic for `JSON_PATH`s pointing to null objects and null slices | 依存の上げだけ |
| #4025 | 2026-10-15 | 効かない | [no-release-notes] enginetest: test joins on NULL and NULL-safe equal keys under every join strategy | 変えたファイルがテスト、memory、文書だけ |
| #4026 | 2026-10-16 | 効かない | sql: Add factories for independent registries of SystemVariables and StatusVariables. | 整理、テスト、文言だけ |
| #4027 | 2026-10-16 | 効かない | sql: Engine can carry EngineVariables and they are exposed on the Context and Session. | 整理、テスト、文言だけ |
| #4028 | 2026-10-16 | 効かない | sql: SQL planning and execution access EngineVariables through Context/Session. | 整理、テスト、文言だけ |
| #4029 | 2026-10-16 | 効かない | sql: Server lifecycle, background work and test harnesses support EngineVariables. | 整理、テスト、文言だけ |
| #4033 | 2026-10-20 | 効かない | Build information_schema column defaults without the parser | 性能だけ |
| #4038 | 2026-10-19 | 効かない | [no-release-notes] enginetest: Just load setup scripts at runtime, no need for codegen. | 変えたファイルがテスト、memory、文書だけ |
| #4040 | 2026-10-16 | 効かない | Fix off-by-one error when finding aggregated column during `replaceSortedMinMax` | フォークに該当のコードがない |
| #4044 | 2026-10-20 | 効かない | Assign column IDs to "Unreferenceable" columns | WordPress が使わない機能 |
| #4045 | 2026-10-21 | 効かない | Converted extended type column defaults with assignment casts | Doltgres だけ |
| #4046 | 2026-10-20 | 効かない | [no-release-notes] enginetest: Cleanup a few lingering warts in enginetest. | 変えたファイルがテスト、memory、文書だけ |
| #4048 | 2026-10-22 | 効かない | build(deps): bump github.com/klauspost/compress from 1.17.8 to 1.18.7 in /_integration/go | 変えたファイルがテスト、memory、文書だけ |
| #4049 | 2026-10-21 | 効かない | go.mod: Bump to 1.27.3. Fix some COMPRESS() tests where the behavior changed. | 依存の上げだけ |
| #4051 | 2026-10-21 | 効かない | Count characters instead of bytes in INSERT() | HeimDB が拒否する文 |
| #4052 | 2026-10-21 | 効かない | Use CharSetHandler for character offsets in `INSERT()` | HeimDB が拒否する文 |
| #4057 | 2026-10-23 | 効かない | Skip costed index scan for non `MATCH...AGAINST` filters on fulltext indexes | WordPress が使わない機能 |
| #4058 | 2026-10-23 | 効かない | execute multiple statements in stored procedure exit handlers | WordPress が使わない機能 |
| #4059 | 2026-10-24 | 効かない | Apply sql_select_limit to simple selects and DISTINCT | WordPress が使わない機能 |
| #4062 | 2026-10-24 | 効かない | Do not hoist filters out of the null side of a lateral join | WordPress が使わない機能 |
| #4064 | 2026-10-26 | 効かない | Improve functional dependency analysis when tables don't have a primary key | 性能だけ |
| #4069 | 2026-10-26 | 効かない | Normalize Decimal Hash Value | フォークに該当のコードがない |
| #4073 | 2026-10-28 | 効かない | [no-release-notes] Remove the deprecated `enginetest/scriptgen/setup` reexport package. Use `enginetest/setup` instead. | 変えたファイルがテスト、memory、文書だけ |
| #4074 | 2026-10-28 | 効かない | Reject subqueries, variables, and non-deterministic functions in functional index expressions. | HeimDB が拒否する文 |
| #4075 | 2026-10-30 | 効かない | Fail queries whose generated column expressions recursively reference their own table instead of overflowing the stack. | SQL で確かめられない |
| #4076 | 2026-10-30 | 効かない | Skip resolving virtual columns for DROP and SHOW statements so tables with unresolvable generated expressions can be inspected and repaired. | WordPress が使わない機能 |
| #4078 | 2026-10-27 | 効かない | Add shared regressions for keyless unique writes | 変えたファイルがテスト、memory、文書だけ |
| #4085 | 2026-11-05 | 効かない | enginetest: test index lookups on keyless tables with a virtual column before a stored column | 変えたファイルがテスト、memory、文書だけ |
| #4086 | 2026-10-30 | 効かない | Fixed CHECK constraints on tables with virtual columns | WordPress が使わない機能 |
| #4087 | 2026-10-29 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 依存の上げだけ |
| #4090 | 2026-10-30 | 効かない | Fixed merge join ranges for literals of another extended type | Doltgres だけ |
| #4094 | 2026-10-30 | 効かない | Support unary bitwise complement | 機能の追加 |
| #4098 | 2026-11-03 | 効かない | sql: check generated SET target by table, not name | WordPress が使わない機能 |
| #4099 | 2026-11-03 | 効かない | sql/expression/function: unwrap out-of-band blob arguments at use | フォークに該当のコードがない |
| #4100 | 2026-11-04 | 効かない | Preserve index lookups during join replanning | 回復する panic で、WordPress が出さない文 |
| #4102 | 2026-11-05 | 効かない | sql: Make independent variable state the default for Engine. | 整理、テスト、文言だけ |
| #4103 | 2026-11-05 | 効かない | sql: Remove the global variable registries. | 整理、テスト、文言だけ |
| #4104 | 2026-11-05 | 効かない | sql: Tighten the session and registry interfaces around engine variables. | 整理、テスト、文言だけ |
| #4109 | 2026-11-05 | 効かない | Resolve inherited defaults in indexed CREATE TABLE ... SELECT | HeimDB が拒否する文 |
| #4110 | 2026-11-05 | 効かない | Fix JSON_TYPE SQL NULL propagation | WordPress が使わない機能 |
| #4118 | 2026-11-05 | 効かない | [no-release-notes] enginetest/queries: add chained comparison | 依存の上げだけ |
| #4121 | 2026-11-06 | 効かない | Improve functional dependency analysis for inner joins when tables don't have a primary key | 性能だけ |
| #4122 | 2026-11-05 | 効かない | [auto-bump] [no-release-notes] dependency by elianddb | 依存の上げだけ |
| #4123 | 2026-11-05 | 効かない | Preserve caller transactions when executing stored procedures | WordPress が使わない機能 |
| #4126 | 2026-11-06 | 効かない | Remove unnecessary filter conditions from semijoin plans | 性能だけ |
| #4127 | 2026-11-05 | 効かない | JSON_TYPE(null) evaluates to null, not "NULL" | WordPress が使わない機能 |
| #4129 | 2026-11-06 | 効かない | [auto-bump] [no-release-notes] dependency by fulghum | 依存の上げだけ |
| #4130 | 2026-11-09 | 効かない | Return SQL NULL for JSON_EXTRACT wildcard misses | 依存の上げだけ |
| #4131 | 2026-11-06 | 効かない | Reject transaction control in triggers with ER 1422 | WordPress が使わない機能 |
