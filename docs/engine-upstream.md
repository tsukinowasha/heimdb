# Upstream fixes (stage 110)

A record of every PR merged into upstream main after HeimDB's engine (`internal/sqlengine`, a fork of go-mysql-server v0.20.0 at `5d7248826`), sorted by whether it affects HeimDB. The procedure and the rules for the verdicts are in stage 110 of [`plan.md`](plan.md); the list of engine changes is in [`engine.md`](engine.md).

## Scope

| | |
| --- | --- |
| Upstream main | `c57d5b4c1` (2026-11-09) |
| PRs | 879 merged from 2025-05-13 on (#2103 to #4136). The 4 commits that went in without a PR are 2 pairs that were added and reverted right away |
| vitess | Out of scope. HeimDB stays on `v0.0.0-20250512224608-8fb9c6ea092c` (upstream main is on `v0.0.0-20261106002823-dae4b00ae0a4`). Parser panics become syntax errors since stage 105 |
| Dependencies | Checked the `go.mod` diff. The only ones older in HeimDB are go-icu-regex, jsonpath, and vitess. The over-read in go-icu-regex's `REGEXP_REPLACE` (5448fed) is also in the wasm version HeimDB uses, but it stays inside wasm and the returned length is correct. The process crash in dolt#11183 is in the parallel GROUP BY of the cgo version and does not apply to HeimDB. WordPress does not use `REGEXP_REPLACE` either |
| Reviewed on | 2026-11-10 |

## Categories

- Applied (72): 16 in stage 111, 13 in stage 112 (including #3361, the follow-up to #3211. #3361 was "Not reproduced" in stage 110, but applying #3211 alone drops rows for NOT IN (empty), so the two went in together), 11 in stage 113, 23 in stage 114, and 9 in stage 115 (including #3141, the follow-up to #3110. #3141 was also "Not reproduced" in stage 110, but applying #3110 alone swaps the columns of a three-way UNION, so the two went in together). How each was applied is in rows 111, 112, 113, 114, and 115 of the list of changes in [`engine.md`](engine.md).
- Candidates to apply (0): reproduces on HeimDB, and WordPress or a plugin can issue it. Leaks, locks, wrong writes, privilege gaps, and panics that crash the process count regardless of the statement's shape. The 8 left after stage 114 (#3191, #3960, and #4054, which touch the planner, and #3052, #3060, #3073, #3110, and #3260, which fix SQL shapes) were all applied in stage 115.
- Not applied (62): reproduces on HeimDB, but in a statement WordPress does not issue. Includes PRs with no reproducing statement and those that cannot be checked with SQL.
- Not reproduced (49): the reproducing statements give the same result on HeimDB and real MySQL 8.0.46. The statements of 45 PRs (and those of #3361 and #3141, applied in stages 112 and 115) are in `internal/testdata/stage110_golden.txt` (`TestStage110UpstreamNotReproduced`). The others: already applied (#3116), no statement that gives the same result as MySQL remains (#3045; the difference is the display of DEFAULT NULL, unrelated to the PR), and privilege statements that need a connection as another user (#3941, #4072; checked by hand).
- Not applicable (696): can be ruled out by reading alone. PRs that change only tests, `memory/`, docs, or `go.mod` (181), new features, Doltgres only, performance only, refactoring, features WordPress does not use (window functions, CTEs, JSON, triggers, spatial, foreign keys, generated columns, and so on), code not in the fork, statements HeimDB rejects, and statements MySQL also rejects.

"Reproduces" was decided by running the reproducing statements against a fresh HeimDB (TCP, `serve()`) and a MySQL database and comparing. Reproduction on the memory engine was not used. If a cache stayed in the engine's `MemoryManager` after a statement, it was counted as a leak. For the candidates and the PRs not applied, the last statements run and the results on both sides are in `internal/testdata/stage110_candidates.json` (the input to stage 111).

## Three items from stage 40

- Quoting of ENUM values: #2995. Applied in stage 111 (CREATE TABLE with an ENUM containing `'` failed with 1105, and the table stayed and could not be removed even with DROP TABLE. A value containing `\` became a different value when read back. The escaping of `\` was added by HeimDB).
- LIKE escapes: #3918. Not reproduced (HeimDB does not rewrite to ranges with `keepLikeEscapes` after parsing). #3784, which the title search finds, is a separate fix that only prints ESCAPE in `Like.String()`.
- Zero datetimes: #3305 (function values) and #3476 (precision display) reproduce. #3305 was applied in stage 113 (the internal value of a zero time stays the same; the functions tell it apart). #3476 was applied in stage 114 (it displays with the precision written in the type, and the result type of an expression keeps the value's digits).

## By PR

| PR | Merged | Verdict | Title | Reason |
| --- | --- | --- | --- | --- |
| #2985 | 2025-05-20 | Applied (stage 114) | fix default value for non null enum columns | An INSERT that omits a NOT NULL ENUM column with no default: under the default sql_mode HeimDB returns 1105 `Field 'e' doesn't have a default value`, while MySQL inserts 1 row with 'abc'. Under WordPress's sql_mode too, HeimDB inserts '' with warning 1364, while MySQL inserts 'abc' with no warning. |
| #2995 | 2025-05-28 | Applied (stage 111) | Fixed enum escaping issue | CREATE TABLE with ENUM('it''s','b') fails on HeimDB with 1105 `HeimDB: column t.e: parse type "enum('it's','b') ...": syntax error` (MySQL accepts it). Worse, the table is created and stays, and SHOW CREATE, SELECT, and DROP TABLE all fail with the same 1105. For a value with \ (ENUM('a\\b')), the \ also goes into the type string as is, the value changes when read back, and INSERT 'a\\b' fails with 1105 (MySQL stores 615C62). |
| #2997 | 2025-05-29 | Applied (stage 114) | Properly escape special characters in comments | SHOW CREATE TABLE on HeimDB prints COMMENT 'it's', 'a\b<newline>c', and COMMENT='x\y' raw, while MySQL escapes them as 'it''s', 'a\\b\nc', and 'x\\y'. Reading the output back gives a syntax error. |
| #3000 | 2025-05-30 | Applied (stage 111) | make update join case insensitive | UPDATE MiXeDcAsE SET j = 999 WHERE i IN (SELECT 1), with the table name written the same way, gives affected=0 on HeimDB and changes nothing; MySQL updates 1 row. The same happens with an upper-case prefix such as wp_Posts. |
| #3025 | 2025-06-11 | Applied (stage 114) | Convert if.Eval result to correct type | SELECT IF(c0 = 123, TRUE, c0) AS ref0, MIN(c0) ... GROUP BY ref0 gives 2 groups (1,1),(1,123) on HeimDB and 1 group (1,1) on MySQL (true and 1 are treated as different values). |
| #3030 | 2025-06-13 | Applied (stage 113) | Distinguish nils in firstBuffer | A non-aggregated value in GROUP BY is overwritten by the value of a later row after a NULL in the first row. Under WordPress's sql_mode, SELECT k, a FROM g GROUP BY k (rows (NULL,1,1),(5,2,1)) gives 5 on HeimDB and NULL on MySQL. The upstream example also gives 1 on HeimDB and NULL on MySQL. |
| #3052 | 2025-06-27 | Applied (stage 115) | Use IsJoin flag to check if keyless tables are actually allowed | UPDATE t SET i = 10 WHERE i IN (SELECT 1) on a table without a primary key: HeimDB returns 1105 `unsupported feature: error: keyless tables unsupported for UPDATE JOIN`, MySQL updates 1 row. |
| #3060 | 2025-07-02 | Applied (stage 115) | Merge scopeColumn types in SetOp scope | SELECT * FROM (SELECT e FROM enum_table UNION SELECT v FROM uv) sq: HeimDB returns 1105 `value bug is not valid for this Enum` (it uses the left ENUM type), MySQL returns 5 rows. NULL handling (the count for WHERE e IS NULL) is the same, 1. |
| #3068 | 2025-07-07 | Applied (stage 114) | dolthub/dolt#9426 - Support enum string context in functions | On an ENUM column, e LIKE 'a%' gives 0 on HeimDB (MySQL 1), CONCAT(e,'test') gives '1test' (MySQL 'abctest'), LENGTH(e) gives 1 (MySQL 3), and GROUP_CONCAT(e) gives '1,2,3' (MySQL 'abc,defg,hjikl'). HeimDB uses the internal number. |
| #3073 | 2025-08-14 | Applied (stage 115) | Allow select aliases to be in group by/having | SELECT y AS z FROM xy GROUP BY (y) HAVING AVG(z) > 0: HeimDB returns 1105 `column "z" could not be found in any table in scope`, MySQL returns 1,2,3. |
| #3078 | 2025-07-14 | Applied (stage 114) | dolthub/dolt#8893 - Fix charset validation to match MySQL behavior for issue #8893 | Under WordPress's sql_mode, INSERT ... CONCAT('ab', UNHEX('FF'), 'cd'): HeimDB returns 1105 `invalid string for charset utf8mb4: '[255]'` and inserts nothing, while MySQL truncates to 'ab' with warning 1366 `Incorrect string value: '\xFFcd' for column 's' at row 1`. Under STRICT too, HeimDB returns 1105 and MySQL 1366. |
| #3103 | 2025-08-01 | Applied (stage 111) | dolthub/dolt#9530 - Fix auto-increment overflow handling | INSERT ... SELECT of a BIGINT UNSIGNED 4294967296 into INT UNSIGNED: without STRICT, HeimDB stores 0 and MySQL 4294967295. Under STRICT too, HeimDB inserts 1 row, while MySQL returns error 1264. |
| #3110 | 2025-07-21 | Applied (stage 115) | Use child column id's for union when assigning exec indexes | SELECT * FROM (SELECT 'parent' AS tbl, id, words FROM t UNION SELECT 'child', id, words FROM t2) AS combined WHERE combined.id = 1: HeimDB returns nothing, MySQL returns 2 rows. |
| #3134 | 2025-08-04 | Applied (stage 111) | Properly convert enum/set columns to string columns with alter table | Changing ENUM and SET columns to VARCHAR with ALTER TABLE ... MODIFY: on HeimDB the values turn into numbers ((1,'1','3'),(2,'2','2')); MySQL gives (1,'abc','x,y'),(2,'def','y'). Data is corrupted. |
| #3135 | 2025-08-01 | Applied (stage 112) | include null values when evaluating anti joins created from `WHERE NOT EXISTS` | NOT EXISTS (SELECT 1 FROM ab_hasnull a WHERE a.b = x.y), where the correlated right column has a NULL: HeimDB returns nothing, MySQL returns (1,0),(3,NULL). The plan is LeftOuterHashJoinExcludeNulls with a.b IS NULL, and a single NULL on the right drops every row. The NOT EXISTS form of WP_Tax_Query (NOT NULL columns) matches MySQL. |
| #3174 | 2025-08-25 | Applied (stage 114) | Make `SHOW VARIABLES` display boolean values as ON/OFF to match MySQL | SHOW VARIABLES LIKE 'autocommit' and 'foreign_key_checks' give 1 on HeimDB and ON on MySQL. |
| #3175 | 2025-08-25 | Applied (stage 114) | Support filtering by comment in `show table status` | SHOW TABLE STATUS WHERE Comment = 'hello': HeimDB returns 1105 `column "Comment" could not be found in any table in scope` (the column is named Comments), MySQL returns 1 row. With LIKE, the last column is NULL on HeimDB and 'hello' on MySQL. TABLE_COMMENT in information_schema.TABLES is 'hello' on HeimDB too. |
| #3191 | 2025-09-03 | Applied (stage 115) | Use Decimal.String() for key in hashjoin lookups | A hash join on equality of DECIMAL columns (/*+ HASH_JOIN(t1, t2) */): HeimDB returns nothing, MySQL returns (30.000, 30.00). A hash join on (u1.c0 % 2) = (u2.c0 % 2) also gives 1 row on HeimDB (MySQL 2 rows). Without the hint, HeimDB picks a different join and the results match. |
| #3200 | 2025-09-09 | Applied (stage 112) | implement double truncation and find correct hash comparison type | Equality between a BOOLEAN column and a VARCHAR column (WHERE t1.c2 = t0.c1 and JOIN ... ON): HeimDB returns only (0,'z'), MySQL returns 4 rows: 'g', 'O5', 'Qo9', 'z' (a string that cannot be a number is 0). The hash join converts to the left type. |
| #3211 | 2025-09-11 | Applied (stage 112) | Do not return EOF in existsIter when right iter is empty | NOT EXISTS (SELECT 1 FROM ab_hasnull WHERE b <> xy_hasnull.y): HeimDB returns nothing, MySQL returns 3 (for the row where y is NULL, the condition is NULL and does not match). The plan is AntiJoin, and it treats a pair whose condition is NULL as a match (upstream's nullRej check is inverted). NOT EXISTS with an empty right side matches MySQL on HeimDB too (NULL,NULL). |
| #3212 | 2025-09-11 | Applied (stage 112) | Wrap right side in Distinct when converting semi-joins to inner joins | When a semi-join is turned into an inner join, duplicates on the right are not removed. SELECT p.ID FROM p WHERE p.ID IN (SELECT post_id FROM pm2 WHERE meta_value = 'red' OR meta_value = 'blue') (the wp_postmeta shape, 2 rows for post 1) gives 1,1,2 on HeimDB and 1,2 on MySQL. EXISTS (SELECT 1 FROM pm WHERE pm.post_id = p.ID) and the upstream example also give duplicates. A single equality meta_key = '_color' and the EXISTS form of WP_Tax_Query gave no duplicates (it depends on the plan). |
| #3215 | 2025-09-15 | Applied (stage 114) | Trim strings before converting to bool | '3bxu' OR FALSE, '3bxu' AND TRUE, and ' 1' AND TRUE give 0 on HeimDB and 1 on MySQL. WHERE s (over '1a','0a','a') returns nothing on HeimDB and '1a' on MySQL. |
| #3260 | 2025-10-09 | Applied (stage 115) | dolthub/go-mysql-server#3259: Fix system variable lookup to only happen on no qualifier | A.timestamp, A.version, and UPPER(A.sql_mode), which are columns that do not exist: HeimDB returns the system variable values (1000, 8.0.31, sql_mode), MySQL returns 1054 Unknown column. |
| #3263 | 2025-10-14 | Applied (stage 112) | Do not prune tables in semi-joins | SELECT * FROM (SELECT id, name, description, archived FROM t0 AS test0 WHERE (id IN (SELECT id FROM t0))) AS dummy_alias ORDER BY dummy_alias.name: HeimDB returns 1105 `HeimDB: internal error: runtime error: index out of range [5] with length 4` (panic), MySQL returns 1 row. |
| #3276 | 2025-10-21 | Applied (stage 114) | Truncate strings for datetime conversion | CAST('2020-01-01 a' AS DATETIME) and similar give NULL on HeimDB and 2020-01-01 00:00:00 and similar on MySQL. INSERT '2020-01-01 a' under the default sql_mode: HeimDB returns 1105, MySQL stores 2020-01-01 00:00:00 with warning 1265. Under WordPress's sql_mode, HeimDB stores 0000-00-00 00:00:00 (warning 1265 is the same). |
| #3277 | 2025-10-22 | Applied (stage 111) | dolthub/dolt#9984: Fix `nil` dereference by `Dispose()` in `GROUP BY` iterator when `Next()` is never called | SELECT SUM(value) FROM test_table LIMIT 0 and SELECT COUNT(*) FROM test_table LIMIT 0: HeimDB returns 1105 `HeimDB: internal error: runtime error: invalid memory address or nil pointer dereference` (panic), MySQL returns nothing. LIMIT 0 with GROUP BY is the same on both. No cache is left (0). |
| #3280 | 2025-10-28 | Applied (stage 114) | Allow string truncation when casting to `date` | CAST('2020-01-01 a' AS DATE) gives NULL on HeimDB and 2020-01-01 (with a warning) on MySQL. Under WordPress's sql_mode, inserting '2020-01-01 a' into a DATE column stores 0000-00-00 on HeimDB and 2020-01-01 on MySQL. Under the default sql_mode, the INSERT fails on both (HeimDB 1105, MySQL 1292). |
| #3302 | 2025-11-12 | Applied (stage 113) | Wrap nullable hoisted filter in IsTrue | NOT EXISTS (SELECT 1 FROM (SELECT 1) alias0 WHERE CASE -1 WHEN t0.c1 THEN false END): HeimDB returns nothing, MySQL returns (1,-2). In EXPLAIN PLAN, the condition on outer columns only is hoisted into NOT(EXISTS ... AND CASE ...), and rows drop on NULL. The WEEKDAY form is the same. |
| #3305 | 2025-11-18 | Applied (stage 113) | Fix datetime functions to return correct results for `0` and `false` | Under WordPress's sql_mode, DAY('0000-00-00') gives 1 on HeimDB and 0 on MySQL. DAYNAME, DATEDIFF, DATE_ADD(0, ...), and TIMESTAMPADD give NULL on MySQL, and Saturday, -106752, 0000-01-02, and so on on HeimDB. MONTH/DAY/DAYOFWEEK/DATEDIFF/DATE_FORMAT on a '0000-00-00 00:00:00' column also differ (HeimDB 1, 1, 7, -106752, 0000-01-01; MySQL 0, 0, NULL, NULL, 0000-00-00). |
| #3318 | 2025-11-27 | Applied (stage 112) | dolthub/dolt#10113: Fix DELETE queries with NOT EXISTS uninitialized subqueries | DELETE FROM student_hobby WHERE NOT EXISTS (SELECT 1 FROM student WHERE student.id = student_hobby.student_id): HeimDB returns 1105 (attempted to evaluate uninitialized subquery), MySQL deletes 2 rows. |
| #3320 | 2025-12-04 | Applied (stage 114) | Mark `innodb_lock_wait_timeout` as being in both global and session scopes | SET SESSION innodb_lock_wait_timeout = 50 (and SET innodb_lock_wait_timeout = 50, SET @@session.…): HeimDB returns 1105 (is a GLOBAL variable), MySQL accepts it. @@innodb_lock_wait_timeout is 1 on HeimDB and 50 on MySQL. |
| #3343 | 2025-12-15 | Applied (stage 112) | Check if key is convertible before memoizing lookup join | t0 NATURAL RIGHT JOIN t1 gives (NULL, NULL) on HeimDB and (1, 'i') on MySQL. A JOIN on s.k = n.v (a string primary key and a numeric column) gives only '12' on HeimDB and ' 12', '12', '12abc' on MySQL. HeimDB looks up the string index with a numeric key and drops rows. |
| #3355 | 2025-12-23 | Applied (stage 112) | Do not use nullable columns as null filter when converting antijoins to left joins | NOT EXISTS (SELECT 1 FROM t0 WHERE t0.c0 OR t1.c0) gives 1 on HeimDB and nothing on MySQL. EXPLAIN PLAN shows LeftOuterJoin with t9.c0 IS NULL (a right column that can be NULL is used for the NULL check). |
| #3365 | 2026-01-06 | Applied (stage 114) | Handle more types in `abs` | ABS('hi'), ABS('12.3hi'), ABS('-342.12hi'), and ABS(1 AND TRUE) all give NULL on HeimDB, and 0, 12.3, 342.12, 1 on MySQL. ABS(v) on a LONGTEXT column is also always NULL, and the order of ORDER BY ABS(v) differs too. |
| #3406 | 2026-01-29 | Applied (stage 111) | Set table names to lower when creating table map | UPDATE Items INNER JOIN Items2 ... SET Items.Version = Items2.Version, using table names with upper case and no aliases: 0 rows on HeimDB, 1 row on MySQL. The same with WordPress-like WP_posts / WP_postmeta (0 rows without aliases; it works with lower-case aliases p, pm). Upper-case aliases (P, M) work. |
| #3407 | 2026-01-30 | Applied (stage 114) | Calculate `year` for `timestampdiff` based on date and clock time values | TIMESTAMPDIFF(YEAR, DATE '2011-07-05', DATE '2026-07-04') gives 15 on HeimDB and 14 on MySQL (the reverse direction gives -15 and -14). |
| #3409 | 2026-02-02 | Applied (stage 114) | Calculate `month` and `quarter` for timestampdiff based on date and clock time values | TIMESTAMPDIFF(MONTH, '2026-01-31', '2026-03-01') gives 0 on HeimDB and 1 on MySQL. TIMESTAMPDIFF(QUARTER, '2025-01-01', '2025-12-31') gives 4 on HeimDB and 3 on MySQL. |
| #3423 | 2026-02-10 | Applied (stage 112) | Do not allow sort-based joins between text and number type columns | A JOIN on t0.c0 (VARCHAR primary key) = t1.c0 (INT primary key) gives 2 rows ('123a',123),('223',223) on HeimDB and 4 rows on MySQL. A BETWEEN range join also returns nothing on HeimDB and 1 row on MySQL. |
| #3428 | 2026-02-13 | Applied (stage 111) | Refactor how we manage outer scopes in join iteration | After SELECT id FROM a WHERE id NOT IN (SELECT v FROM b GROUP BY v) (b.v has a NULL), 3 caches stay in MemoryManager (one per left row). The result matches MySQL (empty). The DISTINCT form, the GROUP BY form without NULLs, and the form without GROUP BY leave 0. EXPLAIN PLAN shows AntiJoin (existsIter); when the condition is NULL it goes from esRejectNull to esIncLeft and rebuilds the right iterator (the GroupBy cache) for the next left row without closing it. Close does not close the right side either. |
| #3447 | 2026-02-25 | Applied (stage 114) | Handle Decimal types in `greatest` and `least` functions | GREATEST(a, b, c), LEAST(a, b, c) (DECIMAL columns), and GREATEST(10.5, 9.75): HeimDB returns 1105 (unsupported type for greatest/least argument: decimal.Decimal), MySQL returns 8.80000 \| 2.75000 and 10.50 \| 9.75. |
| #3451 | 2026-03-04 | Applied (stage 111) | Route unknown-user auth failures to `ER_ACCESS_DENIED` for clients | An unknown user gets 2012, a wrong password gets 1045. Whether a user exists can be seen from outside |
| #3455 | 2026-03-03 | Applied (stage 112) | Do not convert AntiJoins to LeftOuterJoin when inside an Update node | UPDATE p SET status = 'closed' WHERE id NOT IN (SELECT id FROM k WHERE status = 'keep'): HeimDB returns 1105 (unable to find field with index 3 in row of 3 columns), MySQL updates 2 rows. UPDATE ... WHERE NOT EXISTS (...) also returns 1105 on HeimDB (uninitialized subquery, same as #3318). |
| #3461 | 2026-03-06 | Applied (stage 111) | Update `ON UPDATE` columns during `INSERT...ON DUPLICATE KEY UPDATE` | On a table whose DEFAULT is '2000-01-01 00:00:00' (the first try with DEFAULT 0 gives 1067 on MySQL), INSERT ... ON DUPLICATE KEY UPDATE v1 = VALUES(v1): for rows whose value changes, MySQL moves dt to the current time (dt = initial value gives 0), HeimDB does not (1). The affected count is 2 on both, and a second run with the same values gives 0 on both. |
| #3468 | 2026-03-13 | Applied (stage 111) | Separate update expressions into explicit and derived update expressions | On a table with an ON UPDATE CURRENT_TIMESTAMP column, UPDATE t SET v = 1 with the same value: HeimDB reports 1 affected row and moves u, MySQL reports 0 affected rows and leaves u as is. An UPDATE that changes the value affects 1 row and moves u on both. |
| #3476 | 2026-03-20 | Applied (stage 114) | Display correct precision for time types | Under WordPress's sql_mode, HeimDB shows '...56.100000' in DATETIME(3) as 56.1, MySQL as 56.100. HeimDB drops .000000 in DATETIME(6). For a zero datetime too, HeimDB shows 0000-00-00 00:00:00, while MySQL adds .0 / .000 / .000000. CAST(... AS DATETIME(3)) also drops .000. |
| #3503 | 2026-04-07 | Applied (stage 112) | Preserve whether left join excludes null when building best plan | SELECT id FROM hasnullid WHERE id NOT IN (SELECT NULL) gives 4 rows 1, 2, 3, NULL on HeimDB and nothing on MySQL. NOT IN (SELECT 1) and NOT IN (SELECT v FROM r) (r has a NULL) are the same on both. |
| #3527 | 2026-04-27 | Applied (stage 111) | Truncate invalid UTF-8 on `INSERT IGNORE` and warn on `LIKE` with bad charset pattern | INSERT IGNORE of invalid UTF-8 (...2E 35 85 20...): HeimDB stores an empty value, MySQL truncates and stores 'SSD 1TB 2.5'. A plain INSERT under WordPress's sql_mode also returns 1105 on HeimDB, while MySQL truncates and stores. LIKE CONCAT('%', CONVERT(UNHEX('85') USING utf8mb4), '%') returns 1105 on HeimDB and 3 rows on MySQL. |
| #3556 | 2026-05-21 | Applied (stage 113) | Fix `CHAR_LENGTH` and `LIKE` failing on the replacement character | For a value with U+FFFD, CHAR_LENGTH(s) returns 1105 on HeimDB (malformed string encountered while checking length) and 5 on MySQL. LIKE '%ab%' and LIKE 'ab_cd' return nothing on HeimDB and 1 on MySQL. With U+FFFD in the pattern, HeimDB returns 1105. |
| #3581 | 2026-06-09 | Applied (stage 113) | Fix `LIKE 'prefix%'` dropping rows with multibyte text | On a utf8mb4_unicode_520_ci column, LIKE '## %' gives 2, 4 on HeimDB and 1 to 5 on MySQL ('## z', '## za', '## 中 body' are dropped). NOT LIKE is wrong the other way. LIKE '山田%' gives nothing on HeimDB and 1 on MySQL. LIKE 'yama%' gives 2 on HeimDB and 2, 3 on MySQL. |
| #3626 | 2026-07-15 | Applied (stage 113) | Prevent index offset when comparing rows in `topRowsIter` | ORDER BY (SELECT MAX(r.approved_on) FROM bar r WHERE r.foo_id = foo.id) ASC LIMIT 1 gives foo-1 on HeimDB and foo-8 on MySQL. DESC LIMIT 2 and ASC LIMIT 4 also come out in insertion order on HeimDB (foo-1, foo-2, ...); the sort key has no effect. |
| #3630 | 2026-09-02 | Applied (stage 111) | server/listener.go: Fix a race on listener shutdown where an Accept goroutine could try to send on a closed conns channel. | A race at shutdown sends on a closed channel and panics (the process crashes). The fork has the same code |
| #3643 | 2026-07-27 | Applied (stage 111) | Use `buildScalar` to get `GetField` expression for `ON UPDATE` columns | WITH r(id) AS (SELECT 'a') UPDATE t2 SET val = 2 WHERE id IN (SELECT id FROM r): HeimDB returns 1105 (HeimDB: table t2: primary key column id: expected string for varchar(64) ..., got time.Time), MySQL updates 1 row. The value for the ON UPDATE column is written to another column (the primary key), and HeimDB's type check in the storage layer stops it (data is not corrupted). |
| #3675 | 2026-09-02 | Applied (stage 114) | sql/types: trim whitespace before parsing date/datetime strings | CAST(' 2024-01-01 ' AS DATE), CAST(CONCAT(' ', '2024-01-01') AS DATE), DATE(' 2024-01-01 '), and CAST(' 2024-01-01 10:20:30 ' AS DATETIME) give NULL on HeimDB and a value on MySQL. Filtering a LONGTEXT ' 2024-05-01' with CAST(v AS DATE) > '2024-04-15' returns nothing on HeimDB and 1 on MySQL. |
| #3723 | 2026-09-08 | Applied (stage 114) | Refactor and improve `DATE` parsing and `SQL_MODE` handling | HOUR('13:04:05') gives NULL on HeimDB and 13 on MySQL. SECOND('2007-12-11T20:21:22Z') gives 22 on HeimDB and 7 on MySQL. DATE(20010203) gives NULL on HeimDB and 2001-02-03 on MySQL. DATE('0000-00-00') and DATE(0) give 0000-00-00 on HeimDB and NULL on MySQL. CAST('2024/01/05 3:4:5' AS DATETIME) gives NULL on HeimDB and 2024-01-05 03:04:05 on MySQL. |
| #3733 | 2026-08-27 | Applied (stage 113) | Evaluate sort key expressions once per row instead of once per comparison | ORDER BY (@i := @i + 1) is a syntax error on HeimDB (1105 near ':='), so this was checked through the bias of ORDER BY RAND(). Running SELECT id FROM t ORDER BY RAND() 400 times on an 8-row table, the counts of each first row were 115, 111, 80, 50, 28, 5, 7, 4 on HeimDB (id 1 to 8) and 56, 54, 43, 48, 54, 57, 44, 44 on MySQL. LIMIT 1 is uniform on HeimDB too (the TopN of stage 96). |
| #3739 | 2026-08-28 | Applied (stage 113) | Fix HAVING aggregate alias resolution | The second upstream statement (customers c JOIN orders p1 ... LEFT JOIN orders p2 ... HAVING COUNT(p2.id) > 0) returns 1146 on HeimDB (table not found: p2) and (1, 2, 2) on MySQL. HeimDB looks up the column of COUNT(p2.id) in HAVING by name only and binds it to p1.id. In a self-join of the same table, forms where the aggregated columns in HAVING do not overlap by name (COUNT(p2.id) only, SUM(p2.total) only) are the same on both. |
| #3760 | 2026-09-02 | Applied (stage 113) | Fix aggregate reuse across scalar subquery scopes | SELECT MAX(d), (SELECT MAX(d) FROM s WHERE tag = 'keep') FROM s gives 2030 and 2030 on HeimDB and 2030 and 2020 on MySQL. COUNT(id) and SUM(id) also reuse the outer value (3 \| 3, 6 \| 6). The MIN(id) form returns 1105 on HeimDB (unable to find field with index 4 in row of 4 columns). The COUNT(*) form matches MySQL. |
| #3761 | 2026-09-02 | Applied (stage 114) | Fix STR_TO_DATE NULL propagation | STR_TO_DATE(NULL, '%Y-%m-%d') and STR_TO_DATE('2024-01-02', NULL) return 1105 on HeimDB (invalid type: <nil> / string) and NULL on MySQL. Applied to a column with NULLs, a single NULL row makes the whole statement fail with 1105 on HeimDB. |
| #3771 | 2026-09-02 | Applied (stage 114) | Apply AM and PM in STR_TO_DATE | STR_TO_DATE('2024-03-05 09:30:17 PM', '%Y-%m-%d %h:%i:%s %p') gives 09:30:17 on HeimDB and 21:30:17 on MySQL. '12:05:00 AM' also gives 12:05:00 on HeimDB and 00:05:00 on MySQL (%p is ignored). |
| #3780 | 2026-09-09 | Applied (stage 112) | Fix EXISTS over ungrouped aggregate subqueries | SELECT id FROM t WHERE EXISTS(SELECT COUNT(*) FROM u WHERE u.k = t.k) gives only 1 on HeimDB and 1,2 on MySQL. NOT EXISTS gives 2 on HeimDB and 0 rows on MySQL. |
| #3810 | 2026-09-09 | Applied (stage 111) | Fix windows over grouped inputs | SELECT g, c, ROW_NUMBER() OVER (ORDER BY g) FROM (SELECT g, COUNT(*) AS c FROM t GROUP BY g) s matches MySQL, but after it runs 1 cache stays in MemoryManager. A GROUP BY derived table without a window function and a window function without GROUP BY leave 0 (controls). |
| #3862 | 2026-09-16 | Applied (stage 114) | Always generate format column `ON UPDATE` clauses in `EXTRA` | SHOW COLUMNS matches MySQL, but extra in information_schema.columns is u: DEFAULT_GENERATED / d: empty on HeimDB and u: DEFAULT_GENERATED on update CURRENT_TIMESTAMP / d: on update CURRENT_TIMESTAMP on MySQL. |
| #3920 | 2026-09-24 | Applied (stage 114) | Fix panic converting strings with a dangling exponent | No panic, but the path that converts a string with no exponent digits to decimal gives an error: ROUND('1E') and ROUND(HEX(age)) return 1366 '1E' is not a valid value for 'decimal(65,30)' on HeimDB (also under WordPress's sql_mode) and 1 on MySQL (warning 1292). A strict INSERT INTO t VALUES (1, '1E', '1E') also returns 1366 on HeimDB, while MySQL stores 1. CAST('1E' AS DECIMAL(10,2)) and '1E' + 0 are the same on both. |
| #3940 | 2026-09-29 | Applied (stage 111) | Require UPDATE privileges for duplicate-key inserts | For a user with only SELECT and INSERT privileges, INSERT INTO test VALUES (0, 9) ON DUPLICATE KEY UPDATE v1 = 9 succeeds on HeimDB and rewrites row 0 to 9, while MySQL returns 1142 UPDATE command denied. (Checked by starting HeimDB without -no-auth on a separate port. The probe tool cannot switch users) |
| #3960 | 2026-10-01 | Applied (stage 115) | Preserve NULL semantics for indexed NOT IN subqueries | `parent NOT IN (SELECT id FROM p)` on an indexed nullable column: HeimDB also returns rows where parent is NULL (MySQL does not). It happens even when the right side has no NULL. A column without an index and DELETE are correct |
| #3980 | 2026-11-06 | Applied (stage 114) | Fix INSTR collation handling and wrapped substrings | On a utf8mb4_unicode_520_ci column, `INSTR(s, 'A')` gives 0 on HeimDB and 4 on MySQL ('xyza'). `LOCATE` and `POSITION` are correct; only INSTR ignores the collation (Instr.Eval in `function/substring.go`). Plugins use this function often |
| #4014 | 2026-10-15 | Applied (stage 113) | Return a NULL row for MAX/MIN without GROUP BY when no row matches | MAX/MIN of the primary key on an empty table: SELECT MAX(i) FROM emptytable gives 0 rows on HeimDB and 1 row of NULL on MySQL. SELECT COALESCE(MAX(i), 0) + 1 FROM emptytable also gives 0 rows on HeimDB and 1 on MySQL. MAX(v) on a non-primary-key column and WHERE i > 5 on a table with rows give the same NULL. |
| #4021 | 2026-10-15 | Applied (stage 113) | Do not remove GroupBy node when replacing sorted MIN/MAX aggregates | Like #4014, SELECT MAX(i) FROM emptytable (primary key) gives 0 rows on HeimDB and NULL on MySQL. MAX(pk3) WHERE pk1=.. AND pk2=.. on a composite primary key and the GROUP BY form are the same on both (v0.20.0 does not have that rewrite). |
| #4054 | 2026-10-23 | Applied (stage 115) | create larger decimal precision during comparisons | For 123.45 in DECIMAL(10,2), d = 123.45000001 gives 1 on HeimDB and 0 on MySQL (d < 123.45000001 also gives 0 and 1). COUNT(*) with WHERE d = 123.451 gives 1 on HeimDB and 0 on MySQL (HeimDB rounds the literal to the column's scale before comparing). |
| #4106 | 2026-11-05 | Applied (stage 111) | Make the quadratic range overlap check opt-in, and check for cancelation during analysis | With 20,000 values in `a IN (...)`, the check for overlapping ranges is quadratic in the number of ranges; HeimDB takes 6.0 s for SELECT and 4.8 s for DELETE (MySQL is instant). DELETE holds the write lock the whole time, and KILL has no effect during analysis |
| #3020 | 2025-06-11 | Not applied | Support OCT() function and fix CONV() mishandling of negative floats and empty string N | CONV('', 10, 2) returns 1105 `HeimDB: internal error: index out of range [0] with length 0` on HeimDB (panic) and NULL on MySQL. CONV(-1.5, 10, 2) gives 1 on HeimDB and 64 ones on MySQL; CONV('-17.5', 10, 16) gives 11 on HeimDB and FFFFFFFFFFFFFFEF on MySQL. |
| #3023 | 2025-06-11 | Not applied | `like` match when collation is unspecified | tag LIKE 'foo' on a TEXT column of JSON_TABLE (no collation) returns nothing on HeimDB and foo on MySQL. |
| #3095 | 2025-07-15 | Not applied | Convert SetType to strings | A SET column in CASE WHEN ... THEN s ELSE 'x' END and CONCAT(s,'') gives 5 (the bits) on HeimDB and 'a,c' on MySQL. |
| #3111 | 2025-07-23 | Not applied | Enum and set conversions in comparison.Compare | e1 = e2 between ENUMs with different definitions (both 'a') gives 0 on HeimDB and 1 on MySQL. s = 'b,a' on a SET gives 1 on HeimDB and 0 on MySQL. |
| #3114 | 2025-07-25 | Not applied | dolthub/dolt#9511 - Fix SET type casting to CHAR and BINARY | CAST(s AS CHAR) and CAST(s AS BINARY) on a SET column give 5 on HeimDB and 'a,c' on MySQL. |
| #3118 | 2025-07-24 | Not applied | Validate time zone before setting it | HeimDB accepts SET time_zone = 'Not/AZone', and a later SELECT NOW() returns 1105 `invalid time zone: Not/AZone`. MySQL returns error 1298 `Unknown or incorrect time zone: 'Not/AZone'` at the SET. |
| #3131 | 2025-07-30 | Not applied | Unwrap ParenExpr when evaluating OrderBy expressions | WITH cte(i) AS (SELECT i FROM t) SELECT * FROM cte ORDER BY (i) gives 2,0,9,4 unsorted on HeimDB and 0,2,4,9 on MySQL. ORDER BY ((i)) on a derived table and ORDER BY (i*10+j) on an expression are the same on both. |
| #3138 | 2025-08-04 | Not applied | dolthub/dolt#9624 - Fix wildcard user authentication for IP patterns | A fix for authentication of hosts with an IP pattern ('u'@'10.0.%'). The only users HeimDB creates are root@'%' and the site user@'%' (`internal/backend/auth.go`); it does not use host patterns |
| #3145 | 2025-08-06 | Not applied | fix panic over `SetOp`s with joins and subqueries | SELECT * FROM t1 UNION (SELECT j FROM t2 WHERE (j > 10 OR j IN (SELECT j FROM t1 JOIN t2))) returns 1105 `HeimDB: internal error: runtime error: slice bounds out of range [1:0]` on HeimDB (panic; the connection stays open) and 0,1,2 on MySQL. |
| #3161 | 2025-08-14 | Not applied | Allow hash functions to work on BINARY columns when the binary is not valid UTF8 | MD5(UNHEX('FF')), SHA1(...), and TO_BASE64(...) return 1105 `invalid string for charset utf8mb4: '[255]'` on HeimDB under both STRICT and WordPress's sql_mode, and 00594fd4..., 85e53271..., '/w==' on MySQL. |
| #3172 | 2025-08-22 | Not applied | Add correlated columns from subqueries to GroupBy select dependencies | GROUP BY the alias x of a correlated subquery in the select list: x is NULL on HeimDB ((2,NULL),(3,NULL)), MySQL gives (2,1),(3,2). It also happens in the v0.20.0 fork. |
| #3193 | 2025-09-03 | Not applied | Left and right joins should not be optimized into cross joins | t LEFT JOIN (SELECT ... WHERE FALSE) AS subq ON TRUE and the same RIGHT JOIN return nothing on HeimDB and 1 on MySQL. HeimDB turns them into a cross join. |
| #3208 | 2025-09-10 | Not applied | dolthub/dolt#9817 - Fix binary operations return type to be uint64 | -1 \| 0 gives 9223372036854775807 on HeimDB and 18446744073709551615 on MySQL. 1 << 63 gives 9223372036854775807 on HeimDB and 9223372036854775808 on MySQL. |
| #3209 | 2025-09-10 | Not applied | Convert values to strings when evaluating `bit_length` | BIT_LENGTH(123), BIT_LENGTH(1.5), BIT_LENGTH(DATE(...)) return 1105 `Invalid argument to bit_length: 1.5` on HeimDB and 24, 24, 80 on MySQL. |
| #3219 | 2025-09-15 | Not applied | dolthub/go-mysql-server#3216: Fix `UNION ALL` with `NULL BLOB SELECT` returning `NULL` for all vals | SELECT name, data FROM a UNION ALL SELECT name, NULL FROM b (data is BLOB): on HeimDB data is NULL for a's rows too, MySQL gives 0xDEADBEEF. HEX(data) and UNION ALL of data with data are the same on both. It also happens in the v0.20.0 fork. |
| #3228 | 2025-09-22 | Not applied | [no-release-notes] refactor truncation tests | HEX(CHAR(0)) gives '' on HeimDB and '00' on MySQL. CHAR(NULL, 65) and CHAR(256) are the same on both. |
| #3256 | 2025-10-09 | Not applied | dolthub/dolt#9927: Fix double negation overflow with Literals | -(-128) gives 127 on HeimDB (MySQL 128), -(-2147483648) gives 2147483647 (MySQL 2147483648), -(-9223372036854775808) gives 1105 `BIGINT out of range` (MySQL 9223372036854775808), and negating a column holding the minimum INT or TINYINT is also 1 too small. |
| #3265 | 2025-10-15 | Not applied | Do not allow inserting NaN and Inf values into numeric type columns | An INSERT of POW(10,308)*10 succeeds on HeimDB and stores +Inf (MySQL returns 1690 DOUBLE value is out of range), and a later SELECT * FROM t WHERE d > 1.5 returns 1105 `HeimDB: internal error: Cannot create a Decimal from +Inf` (panic). CAST(POW(10,308)*10 AS DECIMAL) returns the same 1105 (MySQL 1690). |
| #3308 | 2025-11-18 | Not applied | make `IsNullable` return true for nullable datetime functions | NOT EXISTS (SELECT 1 FROM (SELECT 1) AS subQuery WHERE WEEKDAY(t1.c0)) returns nothing on HeimDB and 1 on MySQL. The direct cause of the dropped rows is the hoisting fixed in #3302 (not wrapping in IsTrue); after #3302, if WEEKDAY's IsNullable is false the condition is not wrapped and the same error remains. |
| #3325 | 2025-12-04 | Not applied | Make `IsNullable` return `true` for log and math functions where applicable | NOT EXISTS (SELECT 1 FROM (SELECT 1) AS sq WHERE LOG(t0.c0)) and SQRT(t0.c0) return nothing on HeimDB and -1 on MySQL. As with #3308, the direct cause is the hoisting fixed in #3302, and unless the IsNullable of LOG/SQRT is fixed too it remains after #3302. |
| #3350 | 2025-12-18 | Not applied | Updated `IsNullable` for all functions to match `Eval` output | No reproducing statement. A mechanical change to 46 files |
| #3382 | 2026-01-15 | Not applied | Ensure join correctness for enums of different types | A JOIN between ENUMs with different value orders (a.e = b.e) gives ('x','z'),('z','x') on HeimDB and ('x','x'),('z','z') on MySQL. HeimDB compares the internal numbers. |
| #3388 | 2026-01-27 | Not applied | Apply filter simplifications to Join condition | t1 INNER JOIN t0 ON (t1.c0 BETWEEN t0.c2 AND t0.c2) returns nothing on HeimDB and (0, NULL, NULL, NULL, 'KZ') on MySQL. Same statement as #3415. |
| #3410 | 2026-02-03 | Not applied | Replace `Time.Sub` call in `TIMESTAMPDIFF` with `microsecondsDiff` | TIMESTAMPDIFF(MICROSECOND, '0001-01-01 00:00:00', '9999-12-31 23:59:59') gives 9223372036854775 on HeimDB and 315537897599000000 on MySQL. SECOND and DAY from '1000-01-01' also overflow (HeimDB 9223372036, 106751; MySQL 32377449600, 374739). |
| #3415 | 2026-02-04 | Not applied | Simplify Between expressions for GetField arguments | Same statement as #3388: nothing on HeimDB, (0, NULL, NULL, NULL, 'KZ') on MySQL. |
| #3417 | 2026-02-05 | Not applied | Do not join remaining tables with CrossJoins during buildSingleLookupPlan | A true cross join that includes a table without a primary key (`FROM t6, t1 JOIN t0 ON ...` with no condition linking t6): `COUNT(*)` gives 12 on HeimDB and 4 on MySQL. With a linking condition it is correct. Every WordPress join is linked by a condition |
| #3433 | 2026-02-18 | Not applied | Correctly compute scopeLen for joins | A statement with two levels of JOIN LATERAL returns 1105 on HeimDB (internal error: slice bounds out of range [8:7], a recovered panic) and 8 rows on MySQL. Nesting without LATERAL (three forms: a JOIN inside EXISTS inside EXISTS, a JOIN inside a scalar subquery, and so on) matches MySQL. |
| #3435 | 2026-07-22 | Not applied | [no-release-notes] sql/types: fix dropped errors | No reproducing statement. Three lines that discard an error |
| #3618 | 2026-07-22 | Not applied | Error on SELECT @@SESSION.<global-only-var>, matching MySQL | SELECT @@SESSION.innodb_autoinc_lock_mode gives 2 on HeimDB and 1238 on MySQL (a GLOBAL-only variable). |
| #3651 | 2026-07-29 | Not applied | Correctly do NullUnsafe comparisons with tuples. | (0, NULL) = (0, NULL) and (1, NULL) IN ((1, NULL)) are true on HeimDB (1 row) and NULL on MySQL (0 rows). (1, NULL) <=> (1, 2) and (2, NULL) > (1.5, 2) return 1105 on HeimDB (value not nil: 2) and 0 and 1 on MySQL. (a, b) NOT IN ((1, 5)) gives 1, 2 on HeimDB and 2 on MySQL. |
| #3658 | 2026-07-29 | Not applied | When comparing tuples for equality, widen each pair of input values. | (1, 1) = (1.1, 1.1) gives 1 on HeimDB and 0 on MySQL. On a table with primary key (a, b), WHERE (a, b) = (1.1, 1.1) gives 1 row on HeimDB and nothing on MySQL. |
| #3702 | 2026-08-14 | Not applied | fix panic in `format()` | FORMAT(123456, 3, 1) returns 1105 on HeimDB (internal error: interface conversion: interface {} is int8, not string, a recovered panic) and '123,456.000' on MySQL. FORMAT('notanumber', 2), FORMAT('12abc', 2), and FORMAT(123.456, 'notanumber') give NULL on HeimDB and 0.00, 12.00, 123 on MySQL. |
| #3717 | 2026-08-19 | Not applied | bug fix: outer scope visibility in multiple levels of nested derived tables | SELECT a, (SELECT colid FROM (SELECT x AS colid FROM (SELECT outer_t.a AS x FROM inner_t) inner1) outer1) FROM outer_t returns 1105 on HeimDB (unable to find field with index 1 in row of 0 columns) and (1,1),(2,2) on MySQL. The three-level form is the same. |
| #3770 | 2026-09-03 | Not applied | Fix INET_NTOA for unsigned IPv4 values | INET_NTOA(3232235777), INET_NTOA(4294967295), and INET_NTOA(INET_ATON('203.0.113.9')) all give 127.255.255.255 on HeimDB and 192.168.1.1, 255.255.255.255, 203.0.113.9 on MySQL. |
| #3777 | 2026-09-03 | Not applied | String inputs to integer division should be treated as decimals to match MySQL | '1.2' DIV '0.2' gives 5 on HeimDB and 6 on MySQL. '1.2' DIV '0.4' gives 2 and 3 (HeimDB divides in float). |
| #3789 | 2026-09-03 | Not applied | Fix tuple IN comparisons containing NULL | (c0, c1) IN ((20, NULL), (30, 5)) for row (20, NULL) gives 1 on HeimDB and NULL on MySQL. In WHERE, HeimDB returns id=2 and MySQL 0 rows. |
| #3797 | 2026-10-05 | Not applied | Resolve correlated aggregates at their outer scope | SELECT grp, (SELECT GROUP_CONCAT(a.val ...) FROM concat_probe p) FROM concat_scope a GROUP BY grp returns 1105 unable to find field with index 2 in row of 1 columns on HeimDB and (1,'a\|b'),(2,'c') on MySQL. The HAVING form returns 1146 table not found: a on HeimDB. MAX(a.val) and COUNT(a.val) give the same 1105. |
| #3823 | 2026-09-10 | Not applied | Fix BIGINT arithmetic overflow handling | For 18446744073709551615 in BIGINT UNSIGNED, u + 0 gives 9223372036854775807 on HeimDB and 18446744073709551615 on MySQL (the value is garbled even away from the boundary). u + 1 and s + 1 give -9223372036854775808 on HeimDB (wraparound) and 1690 on MySQL. |
| #3824 | 2026-09-09 | Not applied | Fix collation coercibility resolution and binary collation detection | COLLATION(CONCAT(a, b)) (a is utf8mb4_unicode_520_ci, b is VARBINARY) gives utf8mb4_unicode_520_ci on HeimDB and binary on MySQL. COLLATION(CONCAT(a, 'x' COLLATE utf8mb4_bin)) gives utf8mb4_unicode_520_ci on HeimDB and utf8mb4_bin on MySQL. The comparison result (CONCAT(a, b) = 'aA') is the same, 1. |
| #3845 | 2026-09-09 | Not applied | Fix `CHAR` values differing only in trailing space to be equal | Inserting 'a ' into CHAR(3): HeimDB keeps the trailing space (LENGTH 2), MySQL drops it (LENGTH 1). So GROUP BY c gives 3 groups on HeimDB and 2 on MySQL, and DISTINCT, WHERE c = 'a' (only 1 on HeimDB, 1,2 on MySQL), and PARTITION BY c differ too. |
| #3864 | 2026-09-15 | Not applied | Implement conversions from `DATE`, `DATETIME`, and `TIMESTAMP` to numbers | d + 0 gives 20250331000000 on HeimDB and 20250331 on MySQL. ts + 0 gives 2.0250331123456e+13 and 20250331123456, CAST(d AS SIGNED) gives 1743379200 (Unix time) and 20250331, SUM(d) gives 1.7433792e+09 and 20250331, and ABS(dt) gives NULL and 2.0250331123456e+13. |
| #3901 | 2026-09-22 | Not applied | handle no delimiter datetime string parsing | cast('200101' as date) gives NULL on HeimDB and 2020-01-01 on MySQL. cast('00200101.0203' as date) gives NULL and 0020-01-01, cast('20101' as datetime(6)) gives NULL and 2020-10-01 00:00:00.000000. |
| #3923 | 2026-09-25 | Not applied | Resolve untyped `NULL` columns in CTAS to `VARBINARY(0)` | CREATE TABLE t AS SELECT NULL AS a, 1 AS b returns 1105 HeimDB: column t.a: parse type "null" on HeimDB (the storage layer cannot read the type null); MySQL creates it with a binary(0). |
| #3950 | 2026-09-30 | Not applied | Fix correlated subquery dependencies in grouped joins | select mt.s, (select count(*) from othertable where i2 <= mt.i) from mytable mt left join othertable ot on mt.i = ot.i2 where mt.i = 2 group by mt.i, mt.s gives ('second row', 3) on HeimDB and ('second row', 2) on MySQL. |
| #3955 | 2026-09-30 | Not applied | Convert STR_TO_DATE arguments to strings | STR_TO_DATE(20250331, '%Y%m%d') returns 1105 invalid type: int32 on HeimDB, and STR_TO_DATE(BINARY '2025-03-31', ...) returns 1105 invalid type: []uint8; MySQL gives 2025-03-31 for both. |
| #3956 | 2026-09-30 | Not applied | Preserve NOT IN predicates that cannot be unnested | id NOT IN (SELECT ... tags) AND id NOT IN (SELECT ... UNION SELECT ...) gives leaf, root on HeimDB (the NOT on the UNION side is lost and it becomes IN) and b on MySQL. |
| #3957 | 2026-09-30 | Not applied | Preserve unsigned bits when casting to SIGNED | CAST(u AS SIGNED) for 18446744073709551615 gives 9223372036854775807 on HeimDB and -1 on MySQL. For 9223372036854775808 it gives 9223372036854775807 and -9223372036854775808 (HeimDB saturates). |
| #3958 | 2026-09-30 | Not applied | Reject malformed UTF-8 in CONVERT USING utf8mb4 | CONVERT(0xC328 USING utf8mb4) gives a broken string (�() on HeimDB, and NULL with warning 1300 on MySQL. HEX(CONVERT(0x61FF62 USING utf8mb4)) gives 61FF62 and NULL. |
| #3964 | 2026-09-30 | Not applied | Validate twelve-hour STR_TO_DATE inputs | STR_TO_DATE('12:00:00 AM', '%h:%i:%s %p') gives 12:00:00 on HeimDB and 00:00:00 on MySQL. STR_TO_DATE('13:00 PM', '%h:%i %p') gives 13:00:00 on HeimDB and NULL on MySQL. |
| #3971 | 2026-10-02 | Not applied | Delegate `IN` element evaluation to comparison expressions and apply coercibility to HashInTuple | label IN ('cafe' COLLATE utf8mb4_bin, 'other') (the column is utf8mb4_unicode_ci) gives 1,2,4,5,6 on HeimDB (it compares with the column's collation) and 2,4 on MySQL. LOWER(TRIM(label)) IN, tuple IN, and column IN are the same on both. |
| #3977 | 2026-10-08 | Not applied | Sort set operation ORDER BY by the output column | select 1000 union select 1 order by 1 gives 1000, 1 on HeimDB and 1, 1000 on MySQL. 'b' union 'a' order by 1 and abs(-1000) union 1 order by 1 also come out in a different order (ORDER BY a constant has no effect). |
| #3979 | 2026-10-01 | Not applied | Preserve decimal precision in GREATEST and LEAST | greatest(a, b, c) (DECIMAL columns) returns 1105 unsupported type for greatest/least argument: decimal.Decimal on HeimDB and 8.80000 on MySQL. A DECIMAL literal such as greatest(1.5, 2.25) gives the same 1105. |
| #3995 | 2026-10-08 | Not applied | Fix `SIGN` to handle signed floating points < 0.5 and string truncation | SIGN(0.3), SIGN(-0.3), SIGN(0.49), SIGN(-0.0001), SIGN(0.3e0), and SIGN(-1e-5) all give 0 on HeimDB and 1/-1 on MySQL. SIGN(' 0.3') and SIGN('+.5') also give 0 on HeimDB and 1 on MySQL. In stage 119, the same difference was fixed by a HeimDB fix without taking the PR (decide in DOUBLE, as MySQL does; the 119 row of `docs/engine.md`). |
| #4002 | 2026-10-22 | Not applied | Preserve column metadata for parenthesized GROUP BY references | SELECT COUNT(*) FROM tab0 GROUP BY (col1), col2 HAVING col1 >= COUNT(*) returns 1105 column "col1" could not be found in any table in scope on HeimDB and 0 rows on MySQL. The two statements after adding rows also give 1105 and (1),(2) / (1). |
| #4011 | 2026-10-19 | Not applied | Prune unselected columns from `Columns()` | No reproducing statement. It changes the expected plans a lot |
| #4012 | 2026-10-14 | Not applied | Match MySQL Punctuation for `DATETIME` string parsing | CAST('2025/03/31 12.34.56' AS DATETIME) gives NULL on HeimDB and 2025-03-31 12:34:56 on MySQL (warning 4095). CAST('2025-03-31 12:34:56xyz' AS DATETIME) gives NULL and 2025-03-31 12:34:56 (warning 1292 Truncated). |
| #4020 | 2026-10-15 | Not applied | Handle `locate` case when `str` length is 0 and `position` is greater than 0 | locate('', 'abc', 4) gives 0 on HeimDB and 4 on MySQL (empty search string at position length + 1). Other combinations are the same. |
| #4032 | 2026-10-16 | Not applied | Fix nullsafe logic for tuples containing null values | SELECT (CAST(NULL AS DATETIME), 5) <=> (1, 5) returns 1105 Incorrect datetime value: '1' on HeimDB and 0 on MySQL. The other two are the same. |
| #4034 | 2026-10-16 | Not applied | Set `orderByScope.cols` properties for SetOps using that of the merged columns | Same statements as #3977; the order of select 1000 union select 1 order by 1 and the others is reversed between HeimDB and MySQL. |
| #4047 | 2026-10-23 | Not applied | Fix type coercision for `GREATEST` and `LEAST` functions | GREATEST(1, 'a') gives 1 on HeimDB and 'a' on MySQL. GREATEST('2025-01-01', CAST('2024-12-31' AS DATE)) gives 0 and 2025-01-01, LEAST(10, '9') gives 9 and 10 (MySQL compares as strings), and GREATEST(1, 2.5, '3') gives 1105 and 3. |
| #4070 | 2026-10-26 | Not applied | Allow `COUNT(1)` and `COUNT(*)` in the same `SELECT` | SELECT COUNT(1), COUNT(*) FROM count_t returns 1105 column "COUNT(*)" does not exist in scope, but there is an alias ... on HeimDB and (3,3) on MySQL. The GROUP BY form also gives 1105. With the order COUNT(*), COUNT(1) it works. |
| #4112 | 2026-11-05 | Not applied | Preserve datetime precision in INTERVAL arithmetic | CAST('2026-01-02 12:34:56.527248' AS DATETIME(6)) + INTERVAL 1 MICROSECOND gives 2026-01-02 12:34:56 on HeimDB and 12:34:56.527249 on MySQL. A DATETIME(3) column + INTERVAL 1 HOUR also loses the fractional seconds. |
| #2983 | 2025-05-20 | Not reproduced | fix explain plan on server engine | EXPLAIN SELECT COUNT(*) FROM t and EXPLAIN with a primary key equality do not fail on HeimDB or MySQL (no max1Row error). Already covered by the analyzeNode fix in stage 48. |
| #3027 | 2025-06-18 | Not reproduced | make `sql.HashOf()` collation aware | Makes sql.HashOf look at the type and collation, so DISTINCT, UNION, INTERSECT, EXCEPT, hash joins, IN (SELECT), and so on group values that are equal under the collation. |
| #3032 | 2025-06-16 | Not reproduced | Generalize types in `CASE`, `IF`, and `IFNULL` | Decides the result type of CASE, IF, and IFNULL by MySQL's rules (GeneralizeTypes) and converts the values to that type. |
| #3041 | 2025-06-27 | Not reproduced | fix `group_concat` with `order by` subquery clauses | Wrong index assignment in GROUP_CONCAT(... ORDER BY (correlated subquery)) (wrong result or error). |
| #3043 | 2025-06-25 | Not reproduced | fix missed case for nullable enums | Follow-up to #2985. A nullable ENUM column does not get the first value (it stays NULL). |
| #3045 | 2025-06-26 | Not reproduced | Fix 9280 escape ctrl z comment strs | MySQL 8.0.46 also prints Ctrl+Z (0x1A) in a SHOW CREATE TABLE comment unescaped, and HeimDB gives the same 'a\x1ab'. The only difference is whether DEFAULT NULL is shown. |
| #3049 | 2025-06-30 | Not reproduced | Fix 7300 date time overflow underflow | Rewrites date/time and INTERVAL arithmetic (TimeDelta.apply) with Go's time, matching MySQL for NULL on out-of-range results, the DATETIME maximum, and rounding of sub-second digits. |
| #3113 | 2025-07-23 | Not reproduced | Fix double counted time delta when converting time zones | HeimDB sets time.Local to UTC at startup (cmd/heimdb/main.go:43), so the time-zone offset is not applied twice. TIMESTAMPDIFF(…, UTC_TIMESTAMP(6), NOW(6)) and CONVERT_TZ under time_zone '+09:00', '-05:00', '+00:00' match MySQL (also on a test server whose process time zone was JST). |
| #3116 | 2025-07-24 | Not reproduced | fix panic in stats histogram union and intersect | Already taken in stage 46 |
| #3122 | 2025-07-25 | Not reproduced | Allow SYSTEM as valid time zone | Follow-up to #3118. The time_zone check accepts SYSTEM as a valid value. |
| #3141 | 2025-08-06 | Applied (stage 115) | dolthub/dolt#9628 - Fix UNION column mapping bug in nested multi-way operations | With three or more UNIONs ((A UNION B) UNION C), the column IDs of the nested SetOp are taken from the reordered set, and columns get swapped (duplicate rows and mixed-up columns). |
| #3148 | 2025-08-18 | Not reproduced | dolthub/dolt#9641 - Fix BIT Overflow | Decides the column types of a UNION result with GeneralizeTypes, and fixes values overflowing in a UNION ALL of a BIT(1) column and a number (48 does not fit). It also removes the Max1Row mark from UNION subqueries. |
| #3176 | 2025-08-27 | Not reproduced | Trim strings to number prefix when cast | CAST('12abc' AS SIGNED)=12, CAST('1e3' AS SIGNED)=1, CAST('12.345x' AS DECIMAL(10,2))=12.35, CAST('7abc' AS DECIMAL(10,2))=7.00 with warning 1292, and sorting by CAST(meta_value AS SIGNED) match MySQL (fixed in stage 85). |
| #3204 | 2025-09-12 | Not reproduced | dolthub/dolt#9812: Coalesce `IN` and `=` operator logic | Makes IN and = behave the same for strings and numbers (SELECT 'A' IN (0) is 1). |
| #3206 | 2025-09-09 | Not reproduced | fix equality check in `buildSingleLookupPlan()` | buildSingleLookupPlan builds an index join plan even for join conditions that are not simple equalities (such as >=), drops the condition, and gives a wrong result. |
| #3230 | 2025-09-29 | Not reproduced | truncation refactoring and partial decimal truncation implementation | A rework of truncation in type conversion (truncating the DECIMAL part, reworking IN comparisons, a warning for CAST of a negative value to unsigned, ignoring conversion errors in index comparisons, and the JSON_TABLE error message). |
| #3237 | 2025-09-30 | Not reproduced | fix string to boolean comparison for `HashInTuple` expressions | Fixes truncation when comparing strings and numbers in HashInTuple (a hash of the constant tuple in IN) ('123abc' IN ('string', 1, 2, 123) is 1, '123.45a' IN (123.5) is 0). |
| #3242 | 2025-10-01 | Not reproduced | Added collation ignoring for information_schema | COLUMN_NAME COLLATE utf8mb4_0900_ai_ci on information_schema.COLUMNS does not fail on HeimDB (the bug upstream fixes does not occur). MySQL 8.0.46 returns error 1253 `COLLATION 'utf8mb4_0900_ai_ci' is not valid for CHARACTER SET 'utf8mb3'`. COLLATE utf8mb3_general_ci works on both. |
| #3258 | 2025-10-09 | Not reproduced | dolthub/dolt#9935: Add fix for boolean evaluation in analyzer for EXISTS | Simplification goes wrong for OR/AND containing a literal boolean under NOT in WHERE (!(1 \|\| EXISTS(...))). |
| #3261 | 2025-10-10 | Not reproduced | Do not convert keys if key type is incompatible with column type | When building an index range on a string column from a numeric constant, the number is converted to the column type (string) for the range, and the result differs from MySQL, which compares as numbers (1630944823 >= c0). |
| #3278 | 2025-10-22 | Not reproduced | Respect precision when casting to datetime | CAST(... AS DATETIME) ignores the type's precision and always converts with 6 digits (MySQL rounds the sub-second part for DATETIME). Follow-up to #3276. |
| #3300 | 2025-11-11 | Not reproduced | Move `hoistOutOfScopeFilters` rule to `DefaultRules` | hoistOutOfScopeFilters does not run on each side of a UNION (finalizeUnions), so a NOT EXISTS subquery with a condition on outer columns only goes wrong inside a UNION. |
| #3333 | 2025-12-10 | Not reproduced | fix overflow indexed table access | A constant that does not fit the index column's type (999 for a TINYINT column, a negative number for an UNSIGNED column) is clamped to the type's end value for the index lookup, so = 999 returns the row with 127 and > -1 on an UNSIGNED column returns nothing. |
| #3356 | 2025-12-26 | Not reproduced | Return boolean literal when simplifying `AND` and `OR` filters | When simplifying AND / OR in WHERE, the child that decides the truth value (19 or 's') is returned as is and compared without being a boolean type, giving a wrong result. |
| #3361 | 2026-01-05 | Applied (stage 112) | Handle empty right iterators in exists iterator as an EOF | In an existence join (semi / anti join for EXISTS / NOT EXISTS), when the right iterator is the empty marker (EmptyIter), the fork turns the whole iterator into EOF even for an anti join and drops every left row. |
| #3419 | 2026-02-06 | Not reproduced | Use placeholder `ColumnId`s for `EmptyTable` | EmptyTable has no column IDs, so the mapping from columns to execution positions is off, and LEFT JOIN ... ON FALSE and similar read the wrong columns. |
| #3438 | 2026-02-19 | Not reproduced | Implement `sql.StringType` interface for `systemStringType` | Statements that return @@session.time_zone and @@system_time_zone in CASE / IF do not panic and return a value. The value is 'JST' on HeimDB and 'UTC' on MySQL, a difference in system_time_zone (HeimDB uses the host's time zone, the MySQL container is UTC). COALESCE(@@session.time_zone, 'x') is the same. |
| #3442 | 2026-02-24 | Not reproduced | sql/types: implement NumberType for system numeric types | The numeric types of system variables (bool, int, uint, double) do not satisfy sql.NumberType and are left out of numeric handling (statistics buckets, joins, the index key check). |
| #3561 | 2026-05-26 | Not reproduced | fix(rowexec): dispose CachedResults child when closing hashLookupGeneratingIter (#3560) | Fixes a leak where hashLookupGeneratingIter.Close does not close its child and the cache of intermediate results on the right side of a hash join stays in MemoryManager. |
| #3572 | 2026-06-08 | Not reproduced | Rewrite `CachedResults` node and `cachedResultsIter` to not use `MemoryManager` | Follow-up to #3561. Concluding that the real cause was a cachedResultsIter that is neither fully read nor closed leaving its cache in MemoryManager, it rewrote CachedResults so it does not use MemoryManager. |
| #3591 | 2026-07-09 | Not reproduced | Combine `pushFilters` rule with `moveJoinConditionsToFilter` | A rewrite that merges pushFilters and moveJoinConditionsToFilter. With the old way of wrapping a join condition that references only one side in a Filter, a condition left in the middle of nested joins is dropped by reorderJoin (dolthub/dolt#9868). #3231 (2025-09, not in the fork) partly fixed this by pushing it into the join condition but caused another error (dolt#10899); this PR fixes both. |
| #3667 | 2026-08-05 | Not reproduced | Clear autocommit transactions on err on all paths | Ran a SELECT reading a missing table / column, an UPDATE of a missing column, an INSERT with a duplicate-key error, and an UPDATE reading a missing table on one connection and left it open; an UPDATE t SET v = 2 on another connection went through without waiting (0.03 s), and the first connection also saw v = 2. HeimDB's CommandEnd cleans up what autocommit leaves behind. |
| #3798 | 2026-09-04 | Not reproduced | Fix count distinct tuple hashing | COUNT(DISTINCT a, b) hashed a string made by joining the values with ',', so ('a,b','c') and ('a','b,c') were counted as the same. It now uses a per-type hash (sql/hash). |
| #3825 | 2026-09-09 | Not reproduced | Support multi-byte strings and collation derivation in `LPAD` and `RPAD` | LPAD/RPAD count multi-byte characters as characters, and the result's collation is taken from the string argument. |
| #3863 | 2026-09-14 | Not reproduced | Decorrelate inner join subqueries in `EXISTS` subqueries | When decorrelating a correlated condition in the ON of an INNER JOIN inside an EXISTS subquery, a condition on the NULL side of an outer join was hoisted and matching rows were dropped. |
| #3899 | 2026-09-18 | Not reproduced | Fix dropped residual filters and false branch matching in concat lookup join | In an index join with OR in ON (concat lookup join), an index matched only by another condition on the right table (extraFilters) was used for an OR branch's lookup, and the remaining conditions were dropped. |
| #3918 | 2026-10-01 | Not reproduced | Support escaped `LIKE` wildcard in analyzer prefix extraction and pattern matching | Handles the \_ and \% escapes in LIKE patterns correctly in the rewrite to equality or prefix ranges (simplifyFilters) and in matching. Paired with the vitess fix that keeps \_ and \% in strings (vitess#497). |
| #3941 | 2026-09-30 | Not reproduced | Resolved table and CTE authorization | For a user with only SELECT privileges, INSERT INTO test2 WITH test2 AS (SELECT pk + 10, v1 FROM test) SELECT * FROM test2 is refused on HeimDB too (nothing is written). WITH cte AS (...) SELECT works on both. The privilege gap from mixing up a CTE and a real table does not occur. The WITH ... INSERT form is 1064 on MySQL. |
| #3967 | 2026-10-01 | Not reproduced | `IN` expr: compare every element of the right tuple | When the elements of the tuple on the right of IN have mixed types (integer and decimal), the comparison used the first element's type, rounded 0.49 to 0, and made 0 IN (9, 0.49) true. It now compares element by element. |
| #3973 | 2026-10-01 | Not reproduced | Preserve literal defaults during ALTER table rewrites | An ALTER that rewrites the table (MODIFY and so on), starting from the saved table definition, reclassified a datetime literal default as an expression, and COLUMN_DEFAULT later got quotes. |
| #4001 | 2026-10-09 | Not reproduced | Preserve aggregate input column IDs for HAVING | When adding input columns for an aggregation, the column ID of GetField was not kept, and HAVING saw the wrong value of a group column. |
| #4005 | 2026-10-13 | Not reproduced | Generate correct GetField expressions when optimizing joins on table aliases with pruned columns. | A join optimization over an alias of a table with pruned columns panicked (convertSemiToInnerJoin built GetField with the original table name instead of the alias, and convertAntiToLeftJoin used the pruned table definition). |
| #4039 | 2026-10-22 | Not reproduced | GROUP_CONCAT: concatenate BIT values as raw bytes | GROUP_CONCAT joins BIT values as raw bytes instead of decimal characters (HEX(GROUP_CONCAT(v)) becomes 0305, as on MySQL). |
| #4041 | 2026-10-16 | Not reproduced | Clear sign on negative zero during `DecimalMod` | Drops the sign of a negative zero in a DECIMAL MOD result (-5 MOD 5 is '0', not '-0'). |
| #4056 | 2026-10-23 | Not reproduced | Fix accidentally dropping filters in complex joins | The join order planner did not check that the new plan includes every join condition, and dropped inner join conditions in joins of many tables. |
| #4061 | 2026-10-26 | Not reproduced | Do not apply outer scope index lookups below windows and limits | When turning a correlated condition into an index lookup (applyIndexesFromOuterScope), the lookup was pushed even into tables under a window or LIMIT/OFFSET/TopN, and LIMIT saw only the 1 row that was looked up. |
| #4072 | 2026-10-28 | Not reproduced | Require SUPER to SET global and persisted variables | For a user with only SELECT privileges (HeimDB with authentication started on a separate port), SET GLOBAL max_connections, SET PERSIST, SET @@global.sql_mode, SET @@persist.x, and a SET mixing SESSION and GLOBAL are all refused with 1227 Access denied ...SUPER or SYSTEM_VARIABLES_ADMIN, and the values do not change (analyzer/global_privileges.go from stage 42). |
| #4084 | 2026-11-09 | Not reproduced | Truncate strings to the column length under `INSERT IGNORE` and `UPDATE IGNORE` | Truncates strings longer than the column to the column length correctly by character set in INSERT IGNORE and UPDATE IGNORE (and ON DUPLICATE KEY UPDATE). The fork's convertDataAndWarn (line 343 of sql/rowexec/insert.go) cuts by byte position with row[idx].(string)[:maxLength], so it cuts in the middle of a multi-byte character and writes invalid UTF-8 (or fewer characters), and for non-string values (numbers, datetimes, []byte) it panics on the type assertion. |
| #4117 | 2026-11-05 | Not reproduced | Report actual column collations in SHOW FULL COLUMNS | SHOW FULL COLUMNS shows the column's actual collation in Collation instead of the default (utf8mb4_0900_bin) (ENUM and SET too). |
| #4132 | 2026-11-08 | Not reproduced | Open foreign key editors when a plan runs, not when it's analyzed | After UPDATE a JOIN b ... SET a.v = b.v, b.v = a.v, an INSERT ... SELECT that ends in an error, and EXPLAIN UPDATE JOIN, the cache count is 0 and the results match MySQL. HeimDB's Session.CommandEnd closes the rewrites and scans at the end of the statement. The number of open editors in the storage layer was not measured with this tool. |
| #4136 | 2026-11-09 | Not reproduced | Leave a statement's autocommit transaction to that statement | When a statement run from an interpreted function (Doltgres's PL/pgSQL) fails, clearAutocommitTransaction released the outer statement's autocommit transaction; this stops that and instead rolls back the transaction of the failed autocommit statement after the iterator is closed (TransactionCommittingIter.Close). It also fixes a statement that failed in Close leaving its transaction for the next statement. |
| #2103 | 2026-01-28 | Not applicable | Bump google.golang.org/grpc from 1.53.0 to 1.56.3 | Dependency bump only |
| #2894 | 2025-05-28 | Not applicable | [planbuilder] nil database doesn't panic building procedure call | Feature WordPress does not use |
| #2976 | 2025-05-13 | Not applicable | [auto-bump] [no-release-notes] dependency by jycor | Dependency bump only |
| #2977 | 2025-05-14 | Not applicable | Bug fix for offset without limit | Doltgres only |
| #2978 | 2025-05-14 | Not applicable | Bug fix: ensure an active transaction is set before preparing a query | Server-side prepared statements. WordPress does not use them |
| #2979 | 2025-05-16 | Not applicable | fix indexing for `GROUP BY`s and `WINDOW`s in `INSERT` and `REPLACE` statements in `TRIGGERS` | Feature WordPress does not use |
| #2980 | 2025-05-15 | Not applicable | Unwrap inputs to JSON_OBJECT. | Feature WordPress does not use |
| #2981 | 2025-05-15 | Not applicable | Unwrap wrapper values used in JSON aggregation functions, and un-skip accidentally-skipped tests for this behavior. | Feature WordPress does not use |
| #2982 | 2025-05-20 | Not applicable | Bug fix: use ExtendedType serialization if present | Doltgres only |
| #2984 | 2025-05-20 | Not applicable | Add lock_warnings system variable. | New feature |
| #2986 | 2025-05-21 | Not applicable | add returning clause case for assign exec index rule | Doltgres only |
| #2988 | 2025-05-23 | Not applicable | Implement `REVOKE ALL PRIVILEGES, GRANT OPTION ...` | New feature |
| #2989 | 2025-05-22 | Not applicable | fix: Fix create new server in example. | Refactoring, tests, or wording only |
| #2991 | 2025-08-26 | Not applicable | test: optimize database connection test logic | Only tests, memory, or docs changed |
| #2992 | 2025-05-27 | Not applicable | Hacky extension point for aggregate function determination | Doltgres only |
| #2993 | 2025-05-27 | Not applicable | Added undeclared variable error message | Refactoring, tests, or wording only |
| #2994 | 2025-06-13 | Not applicable | fix NewServer call | Only tests, memory, or docs changed |
| #2996 | 2025-05-28 | Not applicable | Revert "Merge pull request #2894 from dolthub/max/nil-proc-db" | Feature WordPress does not use |
| #3001 | 2025-05-31 | Not applicable | Support for injected expressions with ORDER BY clauses | Doltgres only |
| #3002 | 2025-05-30 | Not applicable | fix table functions in stored procedures | Feature WordPress does not use |
| #3004 | 2025-06-03 | Not applicable | Add support for `IS NULL` expressions with records | Doltgres only |
| #3005 | 2025-08-14 | Not applicable | Added support for `@@port`, `@@hostname`, and other system variables | New feature |
| #3006 | 2025-06-05 | Not applicable | implement `if [not] exists` logic for DDL around `view`s and `index`es | New feature |
| #3009 | 2025-06-03 | Not applicable | New skipped tests for group_concat | Only tests, memory, or docs changed |
| #3010 | 2025-06-03 | Not applicable | Add query time to logger | New feature |
| #3011 | 2025-06-05 | Not applicable | [auto-bump] [no-release-notes] dependency by angelamayxie | Dependency bump only |
| #3012 | 2025-06-06 | Not applicable | Handle `insert...returning...` queries | New feature |
| #3013 | 2025-06-06 | Not applicable | handle insert returning for server context | Only tests, memory, or docs changed |
| #3014 | 2025-06-06 | Not applicable | Simplify implementation of `hasSingleOutput` | Refactoring, tests, or wording only |
| #3015 | 2025-06-09 | Not applicable | server var tests | Refactoring, tests, or wording only |
| #3016 | 2025-06-09 | Not applicable | Minor updates to support `UPDATE ... FROM` in Doltgres | Doltgres only |
| #3017 | 2025-06-09 | Not applicable | Support `*` in `returning` clauses | New feature |
| #3018 | 2025-06-09 | Not applicable | fix text storage for `left` and `instr` function | Feature WordPress does not use |
| #3019 | 2025-06-09 | Not applicable | [auto-bump] [no-release-notes] dependency by angelamayxie | Dependency bump only |
| #3021 | 2025-06-10 | Not applicable | Added test for `insert...select...returning` | Only tests, memory, or docs changed |
| #3022 | 2025-06-11 | Not applicable | [no-release-notes] integration test for `go-mysql-org` | Dependency bump only |
| #3024 | 2025-06-16 | Not applicable | Adding skipped tests for `UPDATE ... JOIN` bugs | Only tests, memory, or docs changed |
| #3028 | 2025-06-12 | Not applicable | add mysql dialect tag to new `group by` test | Only tests, memory, or docs changed |
| #3029 | 2025-06-13 | Not applicable | Fix 9347 panic case statements info schema | Recovered panic, in a statement WordPress does not issue |
| #3031 | 2025-06-13 | Not applicable | Fix 7998 json escape double quotes | Feature WordPress does not use |
| #3033 | 2025-06-16 | Not applicable | [no-release-notes] make test less flakey | Only tests, memory, or docs changed |
| #3034 | 2025-06-17 | Not applicable | Allow `drop trigger ...` when trigger is invalid | Feature WordPress does not use |
| #3036 | 2025-06-18 | Not applicable | Cache `REGEX` | Performance only |
| #3037 | 2025-06-21 | Not applicable | Apply foreign key constraints to `UPDATE JOIN` | Feature WordPress does not use |
| #3038 | 2025-06-21 | Not applicable | Fix schema for call to `hash.HashOf()` in `HashLookups` | The code is not in the fork |
| #3040 | 2025-06-24 | Not applicable | backtick column names in check constraints | Feature WordPress does not use |
| #3042 | 2025-06-27 | Not applicable | Apply multiple triggers to `UPDATE JOIN` | Feature WordPress does not use |
| #3046 | 2025-06-27 | Not applicable | Fix SET statements to return OkResult instead of empty rows | Refactoring, tests, or wording only |
| #3047 | 2025-07-01 | Not applicable | Detect when one index is always better than another. | Performance only |
| #3048 | 2025-06-27 | Not applicable | Added field to ScriptTest to skip test | Only tests, memory, or docs changed |
| #3050 | 2025-06-27 | Not applicable | fix exists subqueries in stored procedures | Feature WordPress does not use |
| #3051 | 2025-06-27 | Not applicable | Fixes 7201 non json data err comparison errs | Only tests, memory, or docs changed |
| #3053 | 2025-06-30 | Not applicable | moved keyless subquery test to correct place | Only tests, memory, or docs changed |
| #3054 | 2025-07-03 | Not applicable | Support for set-returning functions | Doltgres only |
| #3056 | 2025-06-28 | Not applicable | Revert "Merge pull request #3046 from dolthub/macneale4-claude/query-ok" | Refactoring, tests, or wording only |
| #3057 | 2025-07-01 | Not applicable | Changing how `NO_MERGE_JOIN` hint is applied to fix a panic | Recovered panic, in a statement WordPress does not issue |
| #3058 | 2025-07-01 | Not applicable | Add variety of Enum tests | Only tests, memory, or docs changed |
| #3059 | 2025-07-05 | Not applicable | Fix SET statements to return OkResult instead of empty rows | Refactoring, tests, or wording only |
| #3061 | 2025-07-02 | Not applicable | Fix #9428: Allow default keyword as value for generated columns. | Feature WordPress does not use |
| #3062 | 2025-07-03 | Not applicable | Allow DEFAULT keyword in UPDATE for generated columns (issue #9438) | Feature WordPress does not use |
| #3063 | 2025-07-03 | Not applicable | Fix `count(*)` for added generated columns | Feature WordPress does not use |
| #3064 | 2025-07-09 | Not applicable | Abstract `IsNull` and `IsNotNull` expression logic | Doltgres only |
| #3065 | 2025-07-03 | Not applicable | Fix enum columns cannot have auto_increment | Refactoring, tests, or wording only |
| #3067 | 2025-07-03 | Not applicable | dolthub/dolt#9427 - Prevent user and system variables in column defaults and generated values | Refactoring, tests, or wording only |
| #3070 | 2025-07-07 | Not applicable | Don't prune VirtualColumnTable tables | Feature WordPress does not use |
| #3074 | 2025-07-08 | Not applicable | dolthub/dolt#9424 - Fix enum foreign key constraints to match MySQL behavior | Feature WordPress does not use |
| #3076 | 2025-07-10 | Not applicable | dolthub/dolt#9425 - Fix enum zero validation in strict mode | New feature |
| #3077 | 2025-07-08 | Not applicable | Add various `SET` type tests | Only tests, memory, or docs changed |
| #3079 | 2025-07-09 | Not applicable | INSERT string function | New feature |
| #3080 | 2025-07-09 | Not applicable | add `auto_increment` tests with various types | Only tests, memory, or docs changed |
| #3081 | 2025-07-09 | Not applicable | make_set and export_set functions | New feature |
| #3082 | 2025-07-10 | Not applicable | Allow GroupBy primary key (and some test clean up) | New feature |
| #3083 | 2025-07-10 | Not applicable | Split function-specific engine test queries into their own file | Refactoring, tests, or wording only |
| #3084 | 2025-07-11 | Not applicable | dolthub/dolt#9481 - Move auto_increment validation to GMS | Refactoring, tests, or wording only |
| #3085 | 2025-07-10 | Not applicable | Fix enum DEFAULT NULL validation in analyzer | The code is not in the fork |
| #3086 | 2025-07-11 | Not applicable | [no-release-notes] Moved more function tests into their own file | Only tests, memory, or docs changed |
| #3087 | 2025-07-11 | Not applicable | Mixed Type Foreign Key | Only tests, memory, or docs changed |
| #3088 | 2025-07-11 | Not applicable | [no-release-notes] Refactored some shared functions into common.go | Refactoring, tests, or wording only |
| #3089 | 2025-07-11 | Not applicable | `DECIMALS` with foreign keys tests | Only tests, memory, or docs changed |
| #3090 | 2025-07-11 | Not applicable | pull from main | Refactoring, tests, or wording only |
| #3091 | 2025-07-14 | Not applicable | sql/types: JSONWrapper: Thread Context on some methods on JSON-related types, such as ToInterface and Compare. | Feature WordPress does not use |
| #3092 | 2025-07-11 | Not applicable | add UTC() to unix time min and max Timestamp | Refactoring, tests, or wording only |
| #3093 | 2025-07-14 | Not applicable | go.mod: Bump to Go 1.24.0. | Refactoring, tests, or wording only |
| #3094 | 2025-07-16 | Not applicable | dolthub/dolt#9496 - Fix DECIMAL foreign key constraint validation to match MySQL behavior | Feature WordPress does not use |
| #3096 | 2025-07-16 | Not applicable | Allow empty strings in set string conversions | Feature WordPress does not use |
| #3097 | 2025-07-15 | Not applicable | test aggregates over indexes with false filter | Only tests, memory, or docs changed |
| #3100 | 2025-07-16 | Not applicable | add skipped tests for auto_increment and max integer | Only tests, memory, or docs changed |
| #3101 | 2025-07-16 | Not applicable | dolthub/dolt#9472 - Fix SET column foreign key constraintsfix specific errs | Feature WordPress does not use |
| #3102 | 2025-07-16 | Not applicable | dolthub/dolt#9494 - Clean up mixed string foreign key logic | Feature WordPress does not use |
| #3104 | 2025-07-17 | Not applicable | dolthub/dolt#9519 - Fix SQL syntax error for mixed named columns and * in SELECT | Refactoring, tests, or wording only |
| #3105 | 2025-07-17 | Not applicable | support set returning function cases in table function wrapper | Doltgres only |
| #3106 | 2025-07-17 | Not applicable | adding skipped tests for time types and foreign keys | Only tests, memory, or docs changed |
| #3107 | 2025-07-17 | Not applicable | change Compute method signature to return error separately for window functions | Feature WordPress does not use |
| #3108 | 2025-07-23 | Not applicable | remove unnecessary sort over merge joins | Performance only |
| #3109 | 2025-07-25 | Not applicable | dolthub/dolt#9544 - Fix time type foreign key constraints | Feature WordPress does not use |
| #3115 | 2025-07-23 | Not applicable | Bug fix: correctly parse/resolve trigger definitions across multiple databases | Feature WordPress does not use |
| #3117 | 2025-07-30 | Not applicable | drop sorts in cross joins and inner joins | Performance only |
| #3119 | 2025-07-24 | Not applicable | Add test for setting time zone from table value | Only tests, memory, or docs changed |
| #3120 | 2025-07-25 | Not applicable | [no-release-notes] generate query script plans | Only tests, memory, or docs changed |
| #3121 | 2025-07-25 | Not applicable | [no-release-notes] remove dialect option from merge join tests | Only tests, memory, or docs changed |
| #3123 | 2025-07-28 | Not applicable | Support decimal unique keys | Only tests, memory, or docs changed |
| #3126 | 2025-07-29 | Not applicable | Added test for adding check constraint when modifying column | Feature WordPress does not use |
| #3127 | 2025-07-30 | Not applicable | [auto-bump] [no-release-notes] dependency by angelamayxie | Dependency bump only |
| #3128 | 2025-07-30 | Not applicable | [auto-bump] [no-release-notes] dependency by jycor | Dependency bump only |
| #3137 | 2025-08-05 | Not applicable | Bug fix: ordered aggregates in triggers | Feature WordPress does not use |
| #3139 | 2025-08-08 | Not applicable | Remove the go-kit dependency | Refactoring, tests, or wording only |
| #3142 | 2025-08-07 | Not applicable | Add support for `DELETE ... RETURNING` | Doltgres only |
| #3143 | 2025-08-06 | Not applicable | Add stack trace in error from panic handler | Refactoring, tests, or wording only |
| #3146 | 2025-08-11 | Not applicable | allocate fewer `sql.Row`s | Performance only |
| #3147 | 2025-08-07 | Not applicable | dolthub/go-mysql-server#3144 - Fix parseErr tracing to show error messages instead of memory addresses | Refactoring, tests, or wording only |
| #3150 | 2025-08-11 | Not applicable | Misc code clean up | Refactoring, tests, or wording only |
| #3152 | 2025-08-11 | Not applicable | Validate trigger name length is not over 96 chars | Feature WordPress does not use |
| #3154 | 2025-08-12 | Not applicable | avoid using `sql.Row.Append()` | Performance only |
| #3155 | 2025-08-12 | Not applicable | Made LOAD DATA handle NULL values and defaults correctly | Feature WordPress does not use |
| #3156 | 2025-08-13 | Not applicable | Add support for altering table comments | New feature |
| #3157 | 2025-08-13 | Not applicable | [auto-bump] [no-release-notes] dependency by angelamayxie | Dependency bump only |
| #3158 | 2025-08-19 | Not applicable | cache `sql.Schema` in `plan.Project` and `plan.TableAlias` | Performance only |
| #3160 | 2025-08-14 | Not applicable | [auto-bump] [no-release-notes] dependency by nicktobey | Dependency bump only |
| #3162 | 2025-09-03 | Not applicable | Add support for VECTOR type | Feature WordPress does not use |
| #3163 | 2025-08-15 | Not applicable | [no-release-notes] Refactor repeated getEmptyPort functions into shared util | Refactoring, tests, or wording only |
| #3164 | 2025-08-15 | Not applicable | handle RowIter as result from Eval function in TableFunctionWrapper | Doltgres only |
| #3166 | 2025-08-20 | Not applicable | Unskip GroupBy validation on Project, Having, and Sort nodes | New feature |
| #3167 | 2025-09-02 | Not applicable | Bump go-sql-driver/mysql | Dependency bump only |
| #3168 | 2025-08-28 | Not applicable | align struct fields to reduce memory usage | Performance only |
| #3169 | 2025-08-20 | Not applicable | go.mod: Bump go-icu-regex. Picks up a wazero bump which improves amd64 behavior on builds with go 1.24.0+. | Dependency bump only |
| #3170 | 2025-08-20 | Not applicable | [no-release-notes] Implement Describable for Alias and Subquery | Refactoring, tests, or wording only |
| #3173 | 2025-08-22 | Not applicable | Mask column names from non-triggered tables in UpdateJoins | Feature WordPress does not use |
| #3178 | 2025-08-28 | Not applicable | Do not materialize any output for WindowPartitionIter if input is empty | Feature WordPress does not use |
| #3180 | 2025-08-29 | Not applicable | dolthub/dolt#9738 - Add CREATE VIEW parentheses tests for MySQL compatibility | Dependency bump only |
| #3182 | 2025-08-29 | Not applicable | [no-release-notes] Added test for create table from aggregate function | Dependency bump only |
| #3183 | 2025-08-29 | Not applicable | [auto-bump] [no-release-notes] dependency by angelamayxie | Dependency bump only |
| #3185 | 2025-09-11 | Not applicable | [no-release-notes] .github/workflows/test.yml: Prepare for go-icu-regex dependency on ICU4C by ensuring it is available to Cgo in all test environments. | Only tests, memory, or docs changed |
| #3186 | 2025-09-02 | Not applicable | /go.mod: bump go | Dependency bump only |
| #3187 | 2025-09-02 | Not applicable | Allow aggregate/window functions with match expressions | Feature WordPress does not use |
| #3188 | 2025-09-02 | Not applicable | [auto-bump] [no-release-notes] dependency by coffeegoddd | Dependency bump only |
| #3189 | 2025-09-03 | Not applicable | [auto-bump] [no-release-notes] dependency by nicktobey | Dependency bump only |
| #3190 | 2025-09-08 | Not applicable | Refactorings to support index scans for pg catalog tables | Doltgres only |
| #3192 | 2025-09-03 | Not applicable | fix update join with conflicting aliases | Feature WordPress does not use |
| #3194 | 2025-09-04 | Not applicable | Group by validation for aggregated queries | New feature |
| #3195 | 2025-09-05 | Not applicable | Initialize newChildren with node.Children to avoid nil child during replaceIdxSort | The code is not in the fork |
| #3196 | 2025-09-05 | Not applicable | Do not push down filters for Full Outer joins | Feature WordPress does not use |
| #3197 | 2025-09-06 | Not applicable | dolthub/dolt#9794 - Fix string function panics with Dolt TextStorage | Feature WordPress does not use |
| #3199 | 2025-09-05 | Not applicable | Add added column to CreateConstraint scope | Feature WordPress does not use |
| #3201 | 2025-09-08 | Not applicable | dolthub/dolt#9807 - FULL OUTER JOIN with empty left subquery rets unmatched right rows | Feature WordPress does not use |
| #3203 | 2025-09-16 | Not applicable | allow setting session variable default value | New feature |
| #3205 | 2025-09-09 | Not applicable | Relax restriction that was preventing us from caching the result of subqueries. | Performance only |
| #3210 | 2025-09-10 | Not applicable | Copy parent row in lateralJoinIterator.buildRow | Feature WordPress does not use |
| #3213 | 2025-09-12 | Not applicable | Copy parent row in fullJoinIter | Feature WordPress does not use |
| #3214 | 2025-09-15 | Not applicable | Implement `PIPES_AS_CONCAT` mode | New feature |
| #3217 | 2025-09-17 | Not applicable | Add `CREATE DATABASE` fall back on `CREATE SCHEMA` to mirror MySQL | New feature |
| #3218 | 2025-09-15 | Not applicable | [auto-bump] [no-release-notes] dependency by angelamayxie | Refactoring, tests, or wording only |
| #3220 | 2025-09-16 | Not applicable | go.mod: Bump go-icu-regex. Pick up ICU4C Cgo dependency. | Refactoring, tests, or wording only |
| #3222 | 2025-09-16 | Not applicable | [no-release-notes] Clean up SQL modes | Refactoring, tests, or wording only |
| #3223 | 2025-10-08 | Not applicable | README.md: Add notes to the readme indicating cgo dependency. | Refactoring, tests, or wording only |
| #3224 | 2025-09-18 | Not applicable | Validate expressions in `ORDER BY` clause during `GROUP BY` validation | New feature |
| #3225 | 2025-09-18 | Not applicable | [no-release-notes] bump golang version | Dependency bump only |
| #3226 | 2025-09-23 | Not applicable | fix Expressions() of TableFunctionWrapper to make the function expression visible | Doltgres only |
| #3227 | 2025-09-19 | Not applicable | dolthub/dolt#9857: Add UUID_SHORT() support | New feature |
| #3231 | 2025-09-24 | Not applicable | Push filters down into join condition | Performance only |
| #3232 | 2025-09-24 | Not applicable | Add `TEXT(m)` support | Only tests, memory, or docs changed |
| #3233 | 2025-09-24 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3234 | 2025-09-25 | Not applicable | dolthub/dolt#9873: Add tests for `FOR UPDATE OF` | New feature |
| #3235 | 2025-09-25 | Not applicable | Allow caching subqueries in a tree of joins. | Performance only |
| #3236 | 2025-09-25 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3238 | 2025-09-28 | Not applicable | fix load data when escaped and enclosed are the same | Feature WordPress does not use |
| #3241 | 2025-09-30 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3243 | 2025-10-01 | Not applicable | Added armscii charset and collations | New feature |
| #3244 | 2025-10-01 | Not applicable | /go.{mod,sum}: add patch version | Dependency bump only |
| #3245 | 2025-10-01 | Not applicable | Allow triggers to fire with `INSERT...RETURNING` statements | Feature WordPress does not use |
| #3246 | 2025-10-03 | Not applicable | sql/rowexec: Add a SessionCommandSafepoint session callback for long-running write commands. Call it in some rowexec implementations. | New feature |
| #3247 | 2025-10-02 | Not applicable | dolthub/dolt#9865: Fix EOF on procedure | Feature WordPress does not use |
| #3248 | 2025-11-03 | Not applicable | Implement `sql.ValueRow` | Performance only |
| #3249 | 2025-10-03 | Not applicable | dolthub/dolt#9916: Add TRUNCATE(X,D) Support | New feature |
| #3250 | 2025-10-03 | Not applicable | Replace SubqueryAlias that selects an entire table with TableAlias | Performance only |
| #3251 | 2025-10-08 | Not applicable | Condense nested `select * from` SubqueryAliases into the innermost SubqueryAlias | Performance only |
| #3252 | 2025-10-07 | Not applicable | Add pure Go regex implementation for non-CGO builds | New feature |
| #3253 | 2025-10-06 | Not applicable | [no-release-notes] Add default VarChar and VarBinary types to `types/strings.go` | Refactoring, tests, or wording only |
| #3254 | 2025-10-07 | Not applicable | [no-release-notes] Tweak ComparableJSON interface to take context parameter and have mor… | Feature WordPress does not use |
| #3262 | 2025-10-10 | Not applicable | dolthub/dolt#9927: Fix SQL regression on UnaryMinus NULL CAST | The code is not in the fork |
| #3267 | 2025-10-16 | Not applicable | Fix GroupBy validation for queries with Having | The code is not in the fork |
| #3268 | 2025-10-16 | Not applicable | [no-release-notes] Refactor NaN and infinity checks into separate function | Refactoring, tests, or wording only |
| #3269 | 2025-10-16 | Not applicable | Add Grafana to listed Backends | Only tests, memory, or docs changed |
| #3270 | 2025-10-20 | Not applicable | dolthub/dolt#9969: Fix `ENCLOSED BY` ignoring terminator inside of field when using `LOAD DATA INFILE` | Feature WordPress does not use |
| #3274 | 2025-10-20 | Not applicable | Do not convert full outer joins to cross joins | Feature WordPress does not use |
| #3275 | 2025-10-21 | Not applicable | dolthub/dolt#9977: Prevent filter pushdown for Anti joins for `NOT IN` | The code is not in the fork |
| #3279 | 2025-11-05 | Not applicable | dolthub/dolt#9887: Add `BINLOG` and `mariadb-binlog` support | Feature WordPress does not use |
| #3282 | 2025-10-29 | Not applicable | /.github/workflows/bump-dependency.yaml: sanatize stuff | Only tests, memory, or docs changed |
| #3283 | 2025-11-03 | Not applicable | dolthub/dolt#9316: Fix PK setting and add new fields for `CREATE TABLE ... SELECT` | Statement HeimDB rejects |
| #3284 | 2025-10-30 | Not applicable | Use IndexedTableAccess for Sorts over a SubqueryAlias | Performance only |
| #3285 | 2025-10-30 | Not applicable | [no-release-notes] /.github/workflows/bump-dependency.yaml: fix command | Only tests, memory, or docs changed |
| #3287 | 2025-10-31 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3289 | 2025-11-06 | Not applicable | [no-release-notes] Trace debugger for join planner, bug fix for Doltgres indexed joins | Doltgres only |
| #3290 | 2025-11-05 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3291 | 2025-11-10 | Not applicable | Add support for configuring a user's TLS connection requirements | New feature |
| #3292 | 2025-11-07 | Not applicable | Updating auth interfaces to pass connection | New feature |
| #3293 | 2025-11-07 | Not applicable | Don't make string unless in Debug mode | Performance only |
| #3294 | 2025-11-07 | Not applicable | [no-release-notes] Removed unnecessary RowIter implementations (handled by row exec builder | Refactoring, tests, or wording only |
| #3295 | 2025-11-07 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Refactoring, tests, or wording only |
| #3296 | 2025-11-11 | Not applicable | use type switch instead of `Fprintf` for grouping key | Performance only |
| #3297 | 2025-11-11 | Not applicable | dolthub/dolt#10050: Fix JSON conversion for `dolt` when using `TextStorage` | Feature WordPress does not use |
| #3298 | 2025-11-14 | Not applicable | Validate connection security properties | New feature |
| #3299 | 2025-11-11 | Not applicable | Prevent panic when using table functions in joins and subqueries | Recovered panic, in a statement WordPress does not issue |
| #3303 | 2025-12-12 | Not applicable | Explicit Analyzer/Builder Overrides | Refactoring, tests, or wording only |
| #3304 | 2025-11-14 | Not applicable | Support table functions with non-literal arguments in subqueries and lateral joins | Feature WordPress does not use |
| #3306 | 2025-11-18 | Not applicable | dolthub/dolt#10083: Honor *definer privileges* when rebinding views | Feature WordPress does not use |
| #3309 | 2025-11-19 | Not applicable | unsafe methods in `SQL` and `SQLValue` | Performance only |
| #3310 | 2025-11-19 | Not applicable | Split `Iter.Next()`, `RowToSQL`, and `callback` into separate threads | Performance only |
| #3311 | 2026-01-06 | Not applicable | Bump golang.org/x/crypto from 0.0.0-20191011191535-87dc89f01550 to 0.45.0 | Dependency bump only |
| #3314 | 2025-11-24 | Not applicable | server/handler: Add ConnectionAuthenticated callback which Vitess can call once the connection is authenticated. | New feature |
| #3315 | 2025-11-24 | Not applicable | cache static groupby schema | Performance only |
| #3316 | 2025-11-24 | Not applicable | [auto-bump] [no-release-notes] dependency by reltuk | Refactoring, tests, or wording only |
| #3319 | 2025-12-02 | Not applicable | Fix `TimestampFuncExpr` and `SetOp` in stored procedures | Feature WordPress does not use |
| #3321 | 2025-12-02 | Not applicable | rewrite last query info | Performance only |
| #3322 | 2025-12-02 | Not applicable | custom `AppendDateFormat` | Performance only |
| #3323 | 2025-12-04 | Not applicable | [no-release-notes] Create Datetime3 type | New feature |
| #3324 | 2025-12-04 | Not applicable | [no-release-notes] Add cost estimates to GroupBy, Sort, HashLookup, SubqueryAlias, and Case nodes in plan tests | Refactoring, tests, or wording only |
| #3326 | 2025-12-04 | Not applicable | Skip expected estimates and analysis for keyless table plan tests | Only tests, memory, or docs changed |
| #3327 | 2025-12-04 | Not applicable | [no-release-notes] Fix plangen to correctly generate test files when explicitly skipping the row estimate | Only tests, memory, or docs changed |
| #3328 | 2025-12-05 | Not applicable | Avoid underestimating rows in outer joins. | Performance only |
| #3329 | 2026-01-06 | Not applicable | Bump github.com/sirupsen/logrus from 1.8.1 to 1.8.3 | Dependency bump only |
| #3330 | 2025-12-12 | Not applicable | Add fast path for simple `IN` filters | Performance only |
| #3331 | 2025-12-09 | Not applicable | Introduce notion of conditional equivalence sets in FDS for optimizing outer joins. | Performance only |
| #3332 | 2025-12-09 | Not applicable | Fix create view error message | Refactoring, tests, or wording only |
| #3334 | 2025-12-11 | Not applicable | Index lookup type conversion issues | Doltgres only |
| #3335 | 2025-12-12 | Not applicable | For large joins, prefer sticking to the user-supplied join order over full enumeration. | Performance only |
| #3336 | 2025-12-10 | Not applicable | Return a helpful error message when attempting to use a table function where a non-table function is expected. | Refactoring, tests, or wording only |
| #3337 | 2025-12-10 | Not applicable | [auto-bump] [no-release-notes] dependency by coffeegoddd | Dependency bump only |
| #3339 | 2025-12-12 | Not applicable | Do not erase projections for insert sources inside triggers | Feature WordPress does not use |
| #3340 | 2025-12-12 | Not applicable | enable native indexes on all in-memory tables by default | Refactoring, tests, or wording only |
| #3342 | 2025-12-12 | Not applicable | Set `SubqueryAlias.OuterScopeVisibility` to true if inside trigger scope | Feature WordPress does not use |
| #3345 | 2025-12-16 | Not applicable | various `analyzer` and `planbuilder` optimizations | Performance only |
| #3347 | 2025-12-16 | Not applicable | Customizable filter walk for costed index scan | Doltgres only |
| #3348 | 2025-12-17 | Not applicable | avoid heap allocs in `transform.Inspect` and `transform.InspectExpr` | Performance only |
| #3349 | 2025-12-19 | Not applicable | Add User Variable discards and fix per-row field to column indices for `LOAD DATA` | Feature WordPress does not use |
| #3352 | 2025-12-22 | Not applicable | Use concurrency in groupby `compute` | Performance only |
| #3354 | 2026-01-06 | Not applicable | Add required attribution for mascot image to README | Only tests, memory, or docs changed |
| #3357 | 2025-12-30 | Not applicable | [no-release-notes] Add test for lookup join with out of range key | Only tests, memory, or docs changed |
| #3358 | 2025-12-30 | Not applicable | condense `time.Time` library calls | Performance only |
| #3360 | 2026-01-03 | Not applicable | [no-release-notes] Add test for indexed varchar column filtered by boolean | Only tests, memory, or docs changed |
| #3362 | 2026-01-05 | Not applicable | [no-release-notes] Join cleanups | Refactoring, tests, or wording only |
| #3363 | 2026-01-06 | Not applicable | [no-release-notes] More cleanup | Refactoring, tests, or wording only |
| #3364 | 2026-01-06 | Not applicable | Add panic handling to spawned goroutine | The code is not in the fork |
| #3366 | 2026-01-07 | Not applicable | Fix query ok for `drop view` | Feature WordPress does not use |
| #3367 | 2026-01-08 | Not applicable | drop schema support | Doltgres only |
| #3368 | 2026-01-08 | Not applicable | Do not parse trigger bodies in `LoadOnly` context | Feature WordPress does not use |
| #3369 | 2026-01-10 | Not applicable | Added DISTINCT ON handling | Doltgres only |
| #3370 | 2026-01-21 | Not applicable | Wrap `errgroup.Group.Go()` calls for consistent panic recovery | Refactoring, tests, or wording only |
| #3371 | 2026-01-08 | Not applicable | Error out for `NATURAL FULL JOIN` | Feature WordPress does not use |
| #3372 | 2026-01-08 | Not applicable | [auto-bump] [no-release-notes] dependency by angelamayxie | Dependency bump only |
| #3376 | 2026-01-09 | Not applicable | Add `IsExpr` case to `replaceVariablesInExpr` | Feature WordPress does not use |
| #3379 | 2026-01-23 | Not applicable | Create scope mapping for views | Performance only |
| #3380 | 2026-01-15 | Not applicable | Generate index lookups for filters that appear in lateral joins | Performance only |
| #3381 | 2026-01-15 | Not applicable | Use key type for memory table index filter range | Refactoring, tests, or wording only |
| #3383 | 2026-01-15 | Not applicable | When applying indexes from outer scopes, resolve references to table aliases | Performance only |
| #3384 | 2026-01-21 | Not applicable | Look at every join node parent when computing outer scopes. | Performance only |
| #3385 | 2026-01-20 | Not applicable | [no-release-notes] Add skipped JoinOp test for enums of different types | Refactoring, tests, or wording only |
| #3386 | 2026-01-23 | Not applicable | Push filters that contain references to outer/lateral scopes. | Performance only |
| #3387 | 2026-01-27 | Not applicable | add TODOs and update simplified Not type (wasn't expecting this branch to merge) | Refactoring, tests, or wording only |
| #3389 | 2026-01-21 | Not applicable | remove `convertLeftAndRight` | Performance only |
| #3391 | 2026-01-21 | Not applicable | [no-release-notes] Add test to make sure serial is the right type | Dependency bump only |
| #3392 | 2026-01-21 | Not applicable | [auto-bump] [no-release-notes] dependency by angelamayxie | Dependency bump only |
| #3393 | 2026-01-22 | Not applicable | add `transform.InspectWithOpaque` function | Refactoring, tests, or wording only |
| #3395 | 2026-01-22 | Not applicable | fewer `strings.ToLower` calls in `gatherTableAlias` | Performance only |
| #3396 | 2026-01-26 | Not applicable | avoid converting `float64` to `float64` | Performance only |
| #3397 | 2026-01-26 | Not applicable | [auto-bump] [no-release-notes] dependency by NathanGabrielson | Dependency bump only |
| #3399 | 2026-01-27 | Not applicable | GMS warning on functional indices | New feature |
| #3400 | 2026-02-02 | Not applicable | Allow pushing filters past Project nodes | Performance only |
| #3403 | 2026-01-28 | Not applicable | /go.{mod,sum}: bump go version | Dependency bump only |
| #3404 | 2026-01-28 | Not applicable | Bump google.golang.org/protobuf from 1.28.1 to 1.33.0 | Dependency bump only |
| #3405 | 2026-01-28 | Not applicable | [auto-bump] [no-release-notes] dependency by coffeegoddd | Dependency bump only |
| #3408 | 2026-01-31 | Not applicable | Added runner to hooks | New feature |
| #3414 | 2026-02-02 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Dependency bump only |
| #3416 | 2026-02-05 | Not applicable | allow Doltgres to add more information schema tables | Doltgres only |
| #3424 | 2026-02-10 | Not applicable | [no-release-notes] Update `left join` join op tests | Only tests, memory, or docs changed |
| #3425 | 2026-02-11 | Not applicable | Do not set scope length during join planning | Recovered panic, in a statement WordPress does not issue |
| #3426 | 2026-02-12 | Not applicable | Do not push filters into SubqueryAliases that wrap RecursiveCTEs | Feature WordPress does not use |
| #3427 | 2026-02-12 | Not applicable | Do not wrap hoisted subquery in Limit node until AFTER checking if it's a SubqueryAlias | Performance only |
| #3429 | 2026-02-16 | Not applicable | Disallow creating VECTOR indexes on nullable columns | Feature WordPress does not use |
| #3432 | 2026-02-21 | Not applicable | `TopN` optimizations | Performance only |
| #3434 | 2026-02-18 | Not applicable | /_integration/php: fix cve | Only tests, memory, or docs changed |
| #3436 | 2026-02-19 | Not applicable | Get index column name using offset from table name prefix length | Feature WordPress does not use |
| #3437 | 2026-03-03 | Not applicable | Bump filippo.io/edwards25519 from 1.1.0 to 1.1.1 | Dependency bump only |
| #3439 | 2026-03-02 | Not applicable | Bump filippo.io/edwards25519 from 1.1.0 to 1.1.1 in /_integration/go | Only tests, memory, or docs changed |
| #3440 | 2026-02-20 | Not applicable | [no-release-notes] Rename variable names from date to time | Refactoring, tests, or wording only |
| #3441 | 2026-02-20 | Not applicable | bug fix: allow `ALTER TABLE DROP CONSTRAINT` to remove unique indexes | New feature |
| #3443 | 2026-02-23 | Not applicable | [auto-bump] [no-release-notes] dependency by reltuk | Dependency bump only |
| #3445 | 2026-02-24 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Dependency bump only |
| #3446 | 2026-02-25 | Not applicable | [auto-bump] [no-release-notes] dependency by jennifersp | Dependency bump only |
| #3448 | 2026-02-26 | Not applicable | Move `/` guard on new databases to Dolt and add `sql.ErrWrongDBName` and `sql.SQLError` to automatically add other error metadata | Refactoring, tests, or wording only |
| #3449 | 2026-02-25 | Not applicable | keep top-level `plan.Limit` for doltgres `generate_series` | Doltgres only |
| #3450 | 2026-03-10 | Not applicable | [no-release-notes] handle authorization for functions (doltgres only) | Doltgres only |
| #3452 | 2026-03-02 | Not applicable | lazy sql-mode string | Performance only |
| #3457 | 2026-03-04 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3458 | 2026-03-11 | Not applicable | Move prepared queries to `sql.Session` and add parsed query cache | Refactoring, tests, or wording only |
| #3460 | 2026-03-09 | Not applicable | [no-release-notes] .github/workflows/dependency-test.yml: Do not trigger this workflow on issue_comment. | Only tests, memory, or docs changed |
| #3463 | 2026-03-09 | Not applicable | remove hacky handling of trigger parsing | Feature WordPress does not use |
| #3465 | 2026-03-10 | Not applicable | support IF EXISTS in ALTER TABLE | New feature |
| #3466 | 2026-03-11 | Not applicable | fix create db if not exists and drop db if exists to not return errors under concurrency | HeimDB rejects CREATE DATABASE |
| #3467 | 2026-03-12 | Not applicable | add sql.Databaser to alter default and alter index | Refactoring, tests, or wording only |
| #3470 | 2026-03-16 | Not applicable | [no-release-notes] Added timezone to integration tests | Only tests, memory, or docs changed |
| #3471 | 2026-03-19 | Not applicable | Changed some reflection checks to use type equality | Refactoring, tests, or wording only |
| #3472 | 2026-03-19 | Not applicable | Match MySQL output when marshalling datetimes in JSON objects. | Feature WordPress does not use |
| #3473 | 2026-03-18 | Not applicable | fix missing check expressions in child expressions of update | Feature WordPress does not use |
| #3474 | 2026-03-26 | Not applicable | Bump google.golang.org/grpc from 1.56.3 to 1.79.3 | Dependency bump only |
| #3479 | 2026-03-24 | Not applicable | sql: background_threads.go: Have Shutdown return nil error on success. | Refactoring, tests, or wording only |
| #3480 | 2026-03-25 | Not applicable | Improve performance when displaying timestamps with precision. | Performance only |
| #3481 | 2026-03-25 | Not applicable | Improve performance when displaying zero timestamps. | Performance only |
| #3482 | 2026-03-27 | Not applicable | Allow infinity and NaN float64 values | Doltgres only |
| #3483 | 2026-03-25 | Not applicable | Fix `MATCH AGAINST` fails when fulltext-indexed table is accessed via `JOIN` alias | Feature WordPress does not use |
| #3484 | 2026-03-30 | Not applicable | sql/plan: cached_results.go: Remove the global CachedResultsManager. The Analyzer now owns and threads it. | Refactoring, tests, or wording only |
| #3485 | 2026-03-25 | Not applicable | Fix prefix index validation for non-utf8mb4 charsets | Statement HeimDB rejects |
| #3487 | 2026-03-25 | Not applicable | /.github/workflows: bump checkout action | Only tests, memory, or docs changed |
| #3488 | 2026-03-27 | Not applicable | Fix `WHERE` filter on view columns backed by string literals fails with index out of bounds | Feature WordPress does not use |
| #3489 | 2026-03-30 | Not applicable | [no-release-notes] go.mod: Bump dolthub/sqllogictest version to match doltgresql. Makes multi-repo refactoring easier. | Dependency bump only |
| #3490 | 2026-03-26 | Not applicable | bug fix: add check for unmatched column | Performance only |
| #3491 | 2026-04-22 | Not applicable | Feature: Indexed functional expressions | New feature |
| #3492 | 2026-03-30 | Not applicable | eventscheduler: Add the ability for eventscheduler to quiesce if there are no registered events and all registered databases promise to notify the scheduler anytime events change. | Feature WordPress does not use |
| #3493 | 2026-03-31 | Not applicable | Make QuiescableEvents discoverable on the PrivilegedDatabase wrapper. | Feature WordPress does not use |
| #3494 | 2026-06-04 | Not applicable | [no-release-notes] Unskip and extend test-suite for unquoted identifiers | Only tests, memory, or docs changed |
| #3496 | 2026-03-31 | Not applicable | Revert "Make QuiescableEvents discoverable on the PrivilegedDatabase wrapper." | Feature WordPress does not use |
| #3497 | 2026-03-31 | Not applicable | sql: Change from a marker method on QuiescableEventDatabase to something that returns a bool. | Feature WordPress does not use |
| #3498 | 2026-04-01 | Not applicable | eventscheduler: Block event scheduler's Close() on finishing its spawned goroutines. | Feature WordPress does not use |
| #3499 | 2026-04-01 | Not applicable | Support for unwrapping geometry values, which may be stored inline or out of band | Feature WordPress does not use |
| #3501 | 2026-04-13 | Not applicable | Several new spatial funcs | Feature WordPress does not use |
| #3505 | 2026-04-08 | Not applicable | trim analyzer rules for simple selects | Performance only |
| #3508 | 2026-04-12 | Not applicable | go.mod: Bump go-icu-regex. | Dependency bump only |
| #3510 | 2026-04-15 | Not applicable | Ignore unique key errors when inserting into fulltext position table | Feature WordPress does not use |
| #3511 | 2026-04-16 | Not applicable | cache arithmetic type | Performance only |
| #3512 | 2026-04-23 | Not applicable | new signal error to skip modifying rows during DML operation | Doltgres only |
| #3513 | 2026-04-22 | Not applicable | Threaded context everywhere | Refactoring, tests, or wording only |
| #3514 | 2026-04-21 | Not applicable | handle foreign key errors for Doltgres | Doltgres only |
| #3516 | 2026-04-21 | Not applicable | Fix `SHOW CREATE VIEW` fail when underlying table is dropped | Feature WordPress does not use |
| #3517 | 2026-04-21 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3519 | 2026-04-21 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Dependency bump only |
| #3522 | 2026-04-23 | Not applicable | go.mod: Bump to golang 1.26.2. | Dependency bump only |
| #3523 | 2026-05-04 | Not applicable | Add handling of TargetRowSize table attribute to GMS interfaces | New feature |
| #3524 | 2026-04-24 | Not applicable | [no-release-notes] don't create new context in `expression.Alias.String()` | Performance only |
| #3526 | 2026-04-24 | Not applicable | Update for functional expression index parser changes | New feature |
| #3528 | 2026-04-30 | Not applicable | Bump go.opentelemetry.io/otel from 1.39.0 to 1.41.0 | Dependency bump only |
| #3529 | 2026-04-24 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Refactoring, tests, or wording only |
| #3532 | 2026-05-13 | Not applicable | replace all `decimal.Decimal` with `*apd.Decimal` | Refactoring, tests, or wording only |
| #3533 | 2026-05-01 | Not applicable | fixed context reassignment error, which causes a panic in a running d… | Feature WordPress does not use |
| #3534 | 2026-05-01 | Not applicable | [no-release-notes] Remove uses of context.Background() by threading in context from callers. | Refactoring, tests, or wording only |
| #3535 | 2026-05-01 | Not applicable | Enforce inline column references as foreign key constraints | Feature WordPress does not use |
| #3537 | 2026-05-28 | Not applicable | sql: opt-in SQL trace redaction | New feature |
| #3538 | 2026-05-05 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3539 | 2026-05-07 | Not applicable | Bug fix for dropping sort nodes | Feature WordPress does not use |
| #3542 | 2026-05-11 | Not applicable | [no-release-notes] Mark test with MySQL dialect to prevent it from failing on Doltgres | Only tests, memory, or docs changed |
| #3543 | 2026-05-20 | Not applicable | Improve costed index scans for tuples by destructuring tuple operations when possible. | Performance only |
| #3544 | 2026-05-15 | Not applicable | [no-release-notes] Remove unused Makefile. | Only tests, memory, or docs changed |
| #3545 | 2026-05-15 | Not applicable | [no-release-notes] Update MAINTAINERS. | Only tests, memory, or docs changed |
| #3546 | 2026-05-15 | Not applicable | Cache context to use for `String()` methods | Refactoring, tests, or wording only |
| #3548 | 2026-05-15 | Not applicable | [no-release-notes] Slight context fixes | Doltgres only |
| #3549 | 2026-05-15 | Not applicable | [no-release-notes] Misc cleanups related to filter pushdown | Refactoring, tests, or wording only |
| #3550 | 2026-05-19 | Not applicable | modify CompareJSON to use sorted object keys | Feature WordPress does not use |
| #3551 | 2026-05-19 | Not applicable | star expression used in function | Doltgres only |
| #3552 | 2026-05-21 | Not applicable | Do not allow `CASCADE` and `SET NULL` foreign keys on columns that are referenced by `STORED` generated column expressions | Feature WordPress does not use |
| #3553 | 2026-05-27 | Not applicable | add partial index on WHERE predicate | New feature |
| #3554 | 2026-05-21 | Not applicable | [auto-bump] [no-release-notes] dependency by jennifersp | Dependency bump only |
| #3555 | 2026-05-21 | Not applicable | Lookup table when not specified in `DROP INDEX` statements | Doltgres only |
| #3557 | 2026-05-21 | Not applicable | Include `CHAR` and `VARCHAR` in `IN` expression optimization | Performance only |
| #3558 | 2026-05-22 | Not applicable | Unwrap wrapped strings before casting to string during `RegexpReplace.Eval` | Feature WordPress does not use |
| #3559 | 2026-07-22 | Not applicable | [no-release-notes] docs: mention MariaDB client compatibility in README | Only tests, memory, or docs changed |
| #3562 | 2026-05-29 | Not applicable | pass schema name when retreiving function | Doltgres only |
| #3563 | 2026-06-01 | Not applicable | Introduce `SchemaObjectNameValidator` interface | Doltgres only |
| #3565 | 2026-05-28 | Not applicable | Allow unparenthesized function expressions in column defaults. | New feature |
| #3566 | 2026-05-28 | Not applicable | analyzer: export BuildForeignKeyEditor for callers outside DML analysis | Refactoring, tests, or wording only |
| #3567 | 2026-05-28 | Not applicable | fix for division on decimals | The code is not in the fork |
| #3568 | 2026-06-04 | Not applicable | bug fix for non-found search paths | Feature WordPress does not use |
| #3569 | 2026-06-03 | Not applicable | [no-release-notes] Pass context to extended type deserialization | Doltgres only |
| #3570 | 2026-06-02 | Not applicable | improvements to time to wire format | Performance only |
| #3571 | 2026-06-03 | Not applicable | Add `IndexNameGenerator` interface | Doltgres only |
| #3573 | 2026-06-04 | Not applicable | support NOT VALID on foreign key and check constraints | Doltgres only |
| #3574 | 2026-06-03 | Not applicable | Ensure SchemaObjectNameValidator is used when a table is renamed | Doltgres only |
| #3575 | 2026-06-04 | Not applicable | decompose tuple inequalities | Performance only |
| #3576 | 2026-06-04 | Not applicable | [auto-bump] [no-release-notes] dependency by jennifersp | Dependency bump only |
| #3577 | 2026-06-05 | Not applicable | fix: return ok result schema for `ALTER TABLE` comment | The code is not in the fork |
| #3578 | 2026-06-05 | Not applicable | fix: retain table comment when primary key is a separate clause | Feature WordPress does not use |
| #3579 | 2026-06-04 | Not applicable | make hashjoin compatible with doltgres | Doltgres only |
| #3582 | 2026-06-10 | Not applicable | fix: panic in regexp functions over large text in group by | Dependency bump only |
| #3584 | 2026-07-22 | Not applicable | Add LockSubsystem.TryLock for non-blocking lock acquisition | Refactoring, tests, or wording only |
| #3585 | 2026-06-15 | Not applicable | Add right side lookup overhead for LookupJoins | Performance only |
| #3587 | 2026-06-12 | Not applicable | go.mod: Bump github.com/lestrrat-go/strftime to v1.2.0. Fixes a lock leak which can deadlock queries in Dolt. | Dependency bump only |
| #3589 | 2026-06-16 | Not applicable | fix JSON control-character escaping and out-of-band text reads | Feature WordPress does not use |
| #3590 | 2026-06-16 | Not applicable | server: Use pollForClosedConnection on all query paths, including DML, DDL, OkResults, empty result schemas, etc. | Refactoring, tests, or wording only |
| #3592 | 2026-06-16 | Not applicable | [auto-bump] [no-release-notes] dependency by reltuk | Dependency bump only |
| #3593 | 2026-06-16 | Not applicable | server/handler.go: Use event-driven notification to quickly cancel a running query after a client-disconnect, instead of polling socket state in a platform-dependent way. | New feature |
| #3594 | 2026-06-17 | Not applicable | allow using star expr after sql value for Doltgres | Doltgres only |
| #3595 | 2026-06-17 | Not applicable | Rollback event-driven stale-client detection. | Refactoring, tests, or wording only |
| #3596 | 2026-06-17 | Not applicable | [auto-bump] [no-release-notes] dependency by reltuk | Dependency bump only |
| #3597 | 2026-06-17 | Not applicable | Revert "Merge pull request #3590 from dolthub/aaron/poll-for-closed-connection-more-cases" | Refactoring, tests, or wording only |
| #3598 | 2026-06-23 | Not applicable | Fix `JSON_LENGTH` and member-access lookups to match MySQL | Feature WordPress does not use |
| #3600 | 2026-07-08 | Not applicable | Allow filter pushdown in subqueries when analyzing insert sources | Performance only |
| #3601 | 2026-06-22 | Not applicable | cache `GeneralizeTypes` output and avoid `fmt.Sprintf` in `HashOfSimple` | Performance only |
| #3602 | 2026-06-23 | Not applicable | Create single override point for `SplitConjunction` | Doltgres only |
| #3603 | 2026-06-23 | Not applicable | Only get projection expressions once when creating filterSet during filter pushdown | Refactoring, tests, or wording only |
| #3605 | 2026-06-24 | Not applicable | Use `TableId`s instead of names when mapping filters to tables | Joins two tables of the same name without aliases; MySQL also rejects it with 1066 (Not unique table/alias). With aliases, HeimDB is correct even between tables on a branch |
| #3606 | 2026-06-25 | Not applicable | [auto-bump] [no-release-notes] dependency by reltuk | Refactoring, tests, or wording only |
| #3607 | 2026-06-25 | Not applicable | [no-release-notes] Move `t.Skip()` to correct run for `TestQueryPlan` | Only tests, memory, or docs changed |
| #3608 | 2026-07-08 | Not applicable | Rewrite `pushFilters` to use single top-down node traversal | Refactoring, tests, or wording only |
| #3609 | 2026-06-26 | Not applicable | server: Add client-disconnect watching to more platforms and more query types. | New feature |
| #3610 | 2026-06-26 | Not applicable | pushdown inequalities to merge joins | Performance only |
| #3611 | 2026-08-05 | Not applicable | avoid `fmt.Sprintf` in groupby hash | Doltgres only |
| #3612 | 2026-07-06 | Not applicable | Bug fix: allow running analysis on indexed join queries with bindvars | The code is not in the fork |
| #3613 | 2026-07-09 | Not applicable | Apply `IndexSearchable` optimization to `sql.Equality` expressions | Doltgres only |
| #3614 | 2026-07-13 | Not applicable | Report Sum/Avg's true float64 type; add window-func override hook | Doltgres only |
| #3615 | 2026-07-13 | Not applicable | Bump golang.org/x/crypto from 0.46.0 to 0.52.0 | Dependency bump only |
| #3616 | 2026-07-10 | Not applicable | [no-release-notes] Refactor set types into new `sets` package | Refactoring, tests, or wording only |
| #3617 | 2026-07-16 | Not applicable | special case on extended type for getting compare type | Doltgres only |
| #3619 | 2026-07-13 | Not applicable | sql: Fix ALTER USER to allow the Identity field of the User record to be updated. | Feature WordPress does not use |
| #3621 | 2026-07-15 | Not applicable | bug fix: honor named window reference | Feature WordPress does not use |
| #3623 | 2026-07-22 | Not applicable | memory: use stable sort for secondary index ordering | Only tests, memory, or docs changed |
| #3625 | 2026-09-02 | Not applicable | Advertise binary charset for `BIT` result columns | Feature WordPress does not use |
| #3627 | 2026-07-17 | Not applicable | fix stats functions to include schema | Doltgres only |
| #3628 | 2026-07-20 | Not applicable | [no-release-notes] Refactor: new `sorters` package | Refactoring, tests, or wording only |
| #3629 | 2026-07-17 | Not applicable | [no-release-notes] Rename SortField(s) to SortCondition(s) | Refactoring, tests, or wording only |
| #3633 | 2026-07-28 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Dependency bump only |
| #3634 | 2026-07-20 | Not applicable | Add support for multiple expressions in functional indexes | New feature |
| #3637 | 2026-07-21 | Not applicable | [no-release-notes] Move heap sort to `sorters` package and make `RowSorter` fields private | Refactoring, tests, or wording only |
| #3638 | 2026-07-22 | Not applicable | Bug fixes for secondary indexes using virtual columns | Feature WordPress does not use |
| #3639 | 2026-07-28 | Not applicable | have coster consider not picking secondary indexes | Performance only |
| #3641 | 2026-07-23 | Not applicable | Bug fix for a panic when calling a stored procedure without a database selected | Recovered panic, in a statement WordPress does not issue |
| #3644 | 2026-07-27 | Not applicable | add ExtendedTableFunction interface to support table function schema … | Doltgres only |
| #3646 | 2026-07-27 | Not applicable | [no-release-notes] Reorganize query script tests | Only tests, memory, or docs changed |
| #3647 | 2026-07-27 | Not applicable | [no-release-notes] Revert "[no-release-notes] Reorganize query script tests" | Only tests, memory, or docs changed |
| #3648 | 2026-07-27 | Not applicable | [no-release-notes] Reorganize query script tests | Only tests, memory, or docs changed |
| #3652 | 2026-07-28 | Not applicable | Allow aliasing column names in a table function | New feature |
| #3653 | 2026-07-28 | Not applicable | adding prepare tests for insert and update | Only tests, memory, or docs changed |
| #3654 | 2026-07-28 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Dependency bump only |
| #3655 | 2026-07-29 | Not applicable | [no-release-notes] Add test for left outer join with duplicate join keys and non-merge filter | Only tests, memory, or docs changed |
| #3656 | 2026-07-29 | Not applicable | Bug fix for `CASE` expressions with `ExtendedType` instances | Doltgres only |
| #3657 | 2026-07-29 | Not applicable | check stats prov for empty stat | The code is not in the fork |
| #3659 | 2026-07-30 | Not applicable | fallback to gms types for extended type histograms | Doltgres only |
| #3660 | 2026-07-30 | Not applicable | bug fixes for schema in table statistics | Doltgres only |
| #3661 | 2026-07-31 | Not applicable | create schema from inscope columns of the table being parsed as column | Doltgres only |
| #3662 | 2026-08-04 | Not applicable | add case for injected exprs in node batch | Doltgres only |
| #3663 | 2026-08-03 | Not applicable | bug fix: infer a bindvar's column type | Doltgres only |
| #3664 | 2026-08-03 | Not applicable | Check if SubqueryAlias itself is lateral when prepending rows | HeimDB rejects views. The derived-table form without a view (EXISTS over nested derived tables) matches MySQL |
| #3665 | 2026-08-05 | Not applicable | Adding a new `TimeDeltaExpression` interface | Doltgres only |
| #3666 | 2026-08-05 | Not applicable | Include `orderBy` in various window functions to return correct default framer | Feature WordPress does not use |
| #3669 | 2026-08-06 | Not applicable | Combine `assignmentExprsToUpdateExprs` and `buildOnDupUpdateExprs` | Recovered panic, in a statement WordPress does not issue |
| #3670 | 2026-08-10 | Not applicable | Fix panic when a RANGE window frame offset pushes a SET/ENUM order-by value outside its valid domain | Feature WordPress does not use |
| #3671 | 2026-08-06 | Not applicable | Handle negative start positions in `LOCATE` function | Recovered panic, in a statement WordPress does not issue |
| #3672 | 2026-08-06 | Not applicable | Pass context through aggregate/window check funcs | Doltgres only |
| #3674 | 2026-08-07 | Not applicable | Error out for undefined window names | Feature WordPress does not use |
| #3676 | 2026-08-07 | Not applicable | Update various window aggregation functions to expect the correct number of children | Feature WordPress does not use |
| #3677 | 2026-08-08 | Not applicable | Bump jsonpath and add new test for parsing JSON null. | Dependency bump only |
| #3687 | 2026-08-10 | Not applicable | SECURITY: Document security stance on panics generated by GMS. | Refactoring, tests, or wording only |
| #3689 | 2026-08-10 | Not applicable | Refactor window function tests into `ScriptTests` | Only tests, memory, or docs changed |
| #3690 | 2026-08-11 | Not applicable | Check for null before casting `LAST_INSERT_ID` input | Recovered panic, in a statement WordPress does not issue |
| #3691 | 2026-08-11 | Not applicable | block certain statements from `PREPARE` | Feature WordPress does not use |
| #3692 | 2026-08-12 | Not applicable | When serializing bytes for a MySQL client, correctly handle wrapped values, such as adaptive encoded columns | Feature WordPress does not use |
| #3693 | 2026-08-11 | Not applicable | fix panic for `GeometryFromWKT` and various GeoSpatial bugs | Feature WordPress does not use |
| #3694 | 2026-08-12 | Not applicable | [no-release-notes] Unskip tests that now pass in Doltgres | Only tests, memory, or docs changed |
| #3695 | 2026-08-12 | Not applicable | Allow `NULL` and other numerical swap flags for `BIN_TO_UUID` | Recovered panic, in a statement WordPress does not issue |
| #3696 | 2026-08-12 | Not applicable | cap initial heapsize | The code is not in the fork |
| #3697 | 2026-08-13 | Not applicable | fix inet6 type assertions | Recovered panic, in a statement WordPress does not issue |
| #3699 | 2026-08-19 | Not applicable | Correctly parse and compare JSON docs with numbers that are too large to fit in a float64 without losing precision. | Feature WordPress does not use |
| #3700 | 2026-08-13 | Not applicable | fix panic on `rand(null)` | Recovered panic, in a statement WordPress does not issue |
| #3701 | 2026-08-13 | Not applicable | [no-release-notes] skip large limit test for non mysql dialects | Only tests, memory, or docs changed |
| #3703 | 2026-08-17 | Not applicable | Fix panic for `LIKE` expression with empty `ESCAPE` character | Recovered panic, in a statement WordPress does not issue |
| #3704 | 2026-09-01 | Not applicable | transaction-scoped session var support | Doltgres only |
| #3705 | 2026-08-27 | Not applicable | Actually resolve column `DEFAULT` expression when building scalar expressions | New feature |
| #3710 | 2026-08-18 | Not applicable | fix panic in TRIM functions | Recovered panic, in a statement WordPress does not issue |
| #3714 | 2026-08-19 | Not applicable | fix panic on invalid `N` argument to `NTILE` | Feature WordPress does not use |
| #3716 | 2026-08-19 | Not applicable | fix panic for `UNION`, `INTERSECT` and `EXCEPT` with unequal schema lengths | Recovered panic, in a statement WordPress does not issue |
| #3718 | 2026-08-20 | Not applicable | Fix functional index display in `SHOW INDEX` and `information_schema.statistics` | Feature WordPress does not use |
| #3719 | 2026-08-21 | Not applicable | fix panic on date type in `Round()` | Recovered panic, in a statement WordPress does not issue |
| #3724 | 2026-08-28 | Not applicable | Added extended vector support | Feature WordPress does not use |
| #3725 | 2026-08-25 | Not applicable | Support PostgreSQL scalar function aliases in FROM | Doltgres only |
| #3726 | 2026-08-25 | Not applicable | Handle empty set results in table function wrappers | Doltgres only |
| #3727 | 2026-08-25 | Not applicable | Support extended types in COUNT DISTINCT | Doltgres only |
| #3728 | 2026-08-26 | Not applicable | Expose DISTINCT aggregate retention | Doltgres only |
| #3730 | 2026-08-28 | Not applicable | Return an error for a bind variable in an AS OF clause | Feature WordPress does not use |
| #3731 | 2026-08-27 | Not applicable | Reject unsafe partial index join scans | The code is not in the fork |
| #3732 | 2026-09-01 | Not applicable | Validate DISTINCT windows after function resolution | Doltgres only |
| #3735 | 2026-08-28 | Not applicable | handle parenthesized select with trailing order by and limit | New feature |
| #3736 | 2026-08-28 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3737 | 2026-08-28 | Not applicable | Preserve LEFT JOIN semantics for lateral joins with no join filters | Feature WordPress does not use |
| #3738 | 2026-08-28 | Not applicable | Don't set the max1Row query flag for plans containing RowIter exprs | Doltgres only |
| #3740 | 2026-08-28 | Not applicable | Convert `LOAD_FILE` filename to text type | The code is not in the fork |
| #3741 | 2026-08-31 | Not applicable | Don't re-evaluate set-returning expressions in the final projection | Doltgres only |
| #3742 | 2026-08-31 | Not applicable | Avoid redundant window output sort | Performance only |
| #3744 | 2026-08-31 | Not applicable | sql/mysql_db: Make OverwriteUsersAndGrantsData leave ephemeral users in place. | Feature WordPress does not use |
| #3745 | 2026-09-01 | Not applicable | Plumb DROP TABLE ... CASCADE through to plan.DropTable | Doltgres only |
| #3746 | 2026-09-01 | Not applicable | Support conditional duplicate updates | Doltgres only |
| #3747 | 2026-09-01 | Not applicable | sql.ValueRows: Correctly handle filter expressions that aren't bools | The code is not in the fork |
| #3748 | 2026-09-01 | Not applicable | sql.ValueRows: Correctly handle NULLs in comparisons. | The code is not in the fork |
| #3750 | 2026-09-01 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Dependency bump only |
| #3751 | 2026-09-02 | Not applicable | [no-release-notes] add dolthub/dolt#11469 regression tests | Only tests, memory, or docs changed |
| #3753 | 2026-09-03 | Not applicable | Prevent insert expression slice aliasing | Feature WordPress does not use |
| #3756 | 2026-09-03 | Not applicable | [no-release-notes] add #11468 regressions | Only tests, memory, or docs changed |
| #3758 | 2026-09-03 | Not applicable | Limit PostgreSQL conflict ignores to duplicate keys | Doltgres only |
| #3759 | 2026-09-02 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Dependency bump only |
| #3762 | 2026-09-02 | Not applicable | Allow REGEXP_REPLACE position after source | Feature WordPress does not use |
| #3763 | 2026-09-02 | Not applicable | Support binary values in CRC32 | Feature WordPress does not use |
| #3764 | 2026-09-02 | Not applicable | Preserve non-ASCII Soundex initial case | Feature WordPress does not use |
| #3765 | 2026-09-02 | Not applicable | Reject ABS of minimum BIGINT | Feature WordPress does not use |
| #3766 | 2026-09-03 | Not applicable | Fix recursive CTE result type coercion | Feature WordPress does not use |
| #3768 | 2026-09-03 | Not applicable | Match MySQL default `SQL_MODE` | Refactoring, tests, or wording only |
| #3769 | 2026-09-02 | Not applicable | Fix decimal CEIL and FLOOR input mutation | The code is not in the fork |
| #3772 | 2026-09-02 | Not applicable | Support extended hours in TIME_TO_SEC | Feature WordPress does not use |
| #3773 | 2026-09-03 | Not applicable | Fix TIME_FORMAT hour specifiers | Feature WordPress does not use |
| #3774 | 2026-09-03 | Not applicable | Preserve empty strings in GROUP_CONCAT | Feature WordPress does not use |
| #3775 | 2026-09-03 | Not applicable | fix: change INFORMATION_SCHEMA default collation from utf8mb3 to utf8mb4 | Refactoring, tests, or wording only |
| #3776 | 2026-09-03 | Not applicable | Warn when removing `NO_ZERO_IN_DATE` from `@@SQL_MODE` | New feature |
| #3778 | 2026-09-03 | Not applicable | Add `MAKEDATE` function | New feature |
| #3779 | 2026-09-03 | Not applicable | enginetest: cover keyless count column projection | Only tests, memory, or docs changed |
| #3782 | 2026-09-03 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Dependency bump only |
| #3783 | 2026-09-03 | Not applicable | Fix CURRENT_TIME precision in window expressions | Feature WordPress does not use |
| #3784 | 2026-09-03 | Not applicable | Preserve LIKE escape clauses in window expressions | Feature WordPress does not use |
| #3785 | 2026-09-04 | Not applicable | Preserve literal case in window expression identity | Feature WordPress does not use |
| #3786 | 2026-09-03 | Not applicable | Include bucket count in NTILE identity | Feature WordPress does not use |
| #3788 | 2026-09-04 | Not applicable | Return NULL for all-NULL window sums | Feature WordPress does not use |
| #3790 | 2026-09-03 | Not applicable | Enable bitwise window aggregates | Feature WordPress does not use |
| #3791 | 2026-09-04 | Not applicable | Distinguish window frame ending bounds | Feature WordPress does not use |
| #3792 | 2026-09-04 | Not applicable | Mark FIRST_VALUE results nullable | Feature WordPress does not use |
| #3793 | 2026-09-04 | Not applicable | Support TIME values in HOUR | New feature |
| #3794 | 2026-09-04 | Not applicable | Return ROW_NUMBER values as INT64 | Feature WordPress does not use |
| #3795 | 2026-09-03 | Not applicable | Fix correlated subqueries alongside window functions | Feature WordPress does not use |
| #3796 | 2026-09-03 | Not applicable | Fix constant window expression partitions | Feature WordPress does not use |
| #3800 | 2026-09-03 | Not applicable | Validate set ops for aggregations in `ORDER BY` clause | New feature |
| #3802 | 2026-09-04 | Not applicable | Add non-window CURRENT_TIME precision regression test | Only tests, memory, or docs changed |
| #3804 | 2026-09-04 | Not applicable | Omit `INVISIBLE` columns projections from wildcard projections and default inserts | New feature |
| #3805 | 2026-09-04 | Not applicable | [no-release-notes] enginetest: add tests for directed TRIM without removal pattern | Dependency bump only |
| #3806 | 2026-09-05 | Not applicable | [no-release-notes] enginetest: add regression tests for nested parentheses subqueries | Only tests, memory, or docs changed |
| #3807 | 2026-09-09 | Not applicable | Fix `SELECT` list alias resolution in window clauses | Feature WordPress does not use |
| #3808 | 2026-09-04 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3809 | 2026-09-08 | Not applicable | Count nonnumeric strings as non-null in window AVG and SUM | Feature WordPress does not use |
| #3811 | 2026-09-04 | Not applicable | Return error for standalone interval expressions | Recovered panic, in a statement WordPress does not issue |
| #3812 | 2026-09-04 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3813 | 2026-09-04 | Not applicable | Fix merging nondeterministic window expressions | Feature WordPress does not use |
| #3815 | 2026-09-05 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3816 | 2026-09-08 | Not applicable | Make sure window framer end is never after the partition start | Feature WordPress does not use |
| #3818 | 2026-09-09 | Not applicable | [no-release-notes] ci: optimize dolt and doltgresql dependency integration tests | Only tests, memory, or docs changed |
| #3820 | 2026-09-09 | Not applicable | Added DESC indexes and NULL ordering | New feature |
| #3821 | 2026-09-08 | Not applicable | Fix correlated subqueries in window sorting | Feature WordPress does not use |
| #3822 | 2026-09-10 | Not applicable | Fix empty inserts with generated columns | Feature WordPress does not use |
| #3832 | 2026-09-09 | Not applicable | Always return `transform.NewTree` when replacing `GetField` with aliased projection expression | HeimDB rejects views. The derived-table form without a view matches MySQL |
| #3833 | 2026-09-10 | Not applicable | [1/6] Fix independent expression strings and descriptions | Refactoring, tests, or wording only |
| #3834 | 2026-09-10 | Not applicable | [2/6] Fix subquery strings and scalar alias resolution | Refactoring, tests, or wording only |
| #3835 | 2026-09-10 | Not applicable | [3/6] Fix user-variable strings and prepared bindings | Feature WordPress does not use |
| #3836 | 2026-09-10 | Not applicable | [4/6] Test grouped windows and iterator cleanup | Only tests, memory, or docs changed |
| #3840 | 2026-09-09 | Not applicable | Support pluggable UPDATE assignment evaluation | Doltgres only |
| #3841 | 2026-09-09 | Not applicable | Delimit window partition and order expressions in PartitionId | Feature WordPress does not use |
| #3842 | 2026-09-15 | Not applicable | Automate dependency bump approval and merge | Only tests, memory, or docs changed |
| #3847 | 2026-09-10 | Not applicable | Remove schema length check when analyzing subquery aliases | Feature WordPress does not use |
| #3848 | 2026-09-10 | Not applicable | [no-release-notes] Remove `resolve_subqueries_test.go` | Only tests, memory, or docs changed |
| #3851 | 2026-09-11 | Not applicable | [no-release-notes] Fixes for integrator functionality | Refactoring, tests, or wording only |
| #3852 | 2026-09-10 | Not applicable | Reject unsupported DISTINCT window aggregates | Feature WordPress does not use |
| #3855 | 2026-09-10 | Not applicable | Implement Numeric to Date conversions | New feature |
| #3857 | 2026-09-10 | Not applicable | Set return type for population statistic aggregate functions | Feature WordPress does not use |
| #3858 | 2026-09-10 | Not applicable | Cover vector index nullability across DDL paths | Only tests, memory, or docs changed |
| #3860 | 2026-09-11 | Not applicable | [no-release-notes] Remove aggregates | Refactoring, tests, or wording only |
| #3866 | 2026-09-14 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Dependency bump only |
| #3867 | 2026-09-29 | Not applicable | Resolve `order by` columns using column ID instead of name to avoid ambiguity | Feature WordPress does not use |
| #3868 | 2026-09-14 | Not applicable | [no-release-notes] Rename `test` package to `testutils` | Refactoring, tests, or wording only |
| #3869 | 2026-09-15 | Not applicable | [no-release-notes] Test foreign keys with spatial parent columns | Only tests, memory, or docs changed |
| #3870 | 2026-09-16 | Not applicable | [no-release-notes] Test CTE definition ordering | Only tests, memory, or docs changed |
| #3871 | 2026-09-15 | Not applicable | [no-release-notes] Test invalid binary decimal parameters | Only tests, memory, or docs changed |
| #3872 | 2026-09-15 | Not applicable | [no-release-notes] Test oversized primary-key lookups | Only tests, memory, or docs changed |
| #3873 | 2026-09-16 | Not applicable | [no-release-notes] Test INSERT aliases in duplicate-key updates | Only tests, memory, or docs changed |
| #3874 | 2026-09-16 | Not applicable | [no-release-notes] Test cursor continue-handler checksum loops | Only tests, memory, or docs changed |
| #3875 | 2026-09-15 | Not applicable | [no-release-notes] Test large DOUBLE arithmetic | Only tests, memory, or docs changed |
| #3876 | 2026-09-16 | Not applicable | [no-release-notes] Test consistent JSON object comparisons | Only tests, memory, or docs changed |
| #3877 | 2026-09-16 | Not applicable | [no-release-notes] Test virtual-column filtering and sorting | Only tests, memory, or docs changed |
| #3878 | 2026-09-30 | Not applicable | [no-release-notes] Fix column projection in memory vector scans | Only tests, memory, or docs changed |
| #3879 | 2026-10-01 | Not applicable | [no-release-notes] Fix memory vector scan projections with generated columns | Only tests, memory, or docs changed |
| #3880 | 2026-09-16 | Not applicable | [no-release-notes] Test JSON object paths on arrays | Only tests, memory, or docs changed |
| #3881 | 2026-09-15 | Not applicable | [no-release-notes] Test prepared decimal fractional digits | Only tests, memory, or docs changed |
| #3882 | 2026-09-15 | Not applicable | [no-release-notes] Test unique indexes on virtual columns | Only tests, memory, or docs changed |
| #3883 | 2026-09-16 | Not applicable | [no-release-notes] Test stored-procedure IF results over the wire | Only tests, memory, or docs changed |
| #3884 | 2026-09-29 | Not applicable | [no-release-notes] Migrate `CustomValueValidator` to `testutils` | Refactoring, tests, or wording only |
| #3885 | 2026-09-15 | Not applicable | Support wildcard replication filter values | Feature WordPress does not use |
| #3886 | 2026-09-15 | Not applicable | Simplify partial join parent for window functions in `exists` subqueries | Feature WordPress does not use |
| #3888 | 2026-09-15 | Not applicable | Fix LOAD_FILE directory handle leaks | The code is not in the fork |
| #3889 | 2026-09-16 | Not applicable | Filled generation_expression and extra for generated columns in information_schema.columns | Feature WordPress does not use |
| #3890 | 2026-10-07 | Not applicable | Fix nullable RANGE frame boundaries | Feature WordPress does not use |
| #3891 | 2026-10-06 | Not applicable | Fix implicit peer frames for window functions | Feature WordPress does not use |
| #3893 | 2026-10-06 | Not applicable | Fix window expression identity collisions | Feature WordPress does not use |
| #3894 | 2026-09-16 | Not applicable | [auto-bump] [no-release-notes] dependency by zachmu | Dependency bump only |
| #3895 | 2026-09-16 | Not applicable | [no-release-notes] refactor time functions | Refactoring, tests, or wording only |
| #3896 | 2026-09-16 | Not applicable | [no-release-notes] test CTAS and INSERT with window functions and correlated subqueries | Only tests, memory, or docs changed |
| #3897 | 2026-09-16 | Not applicable | Optimize `SPACE` with `strings.Repeat` | Performance only |
| #3898 | 2026-09-17 | Not applicable | Use common extended type for VALUES rows | Doltgres only |
| #3900 | 2026-09-21 | Not applicable | Fixed duplicate window columns | Feature WordPress does not use |
| #3904 | 2026-09-21 | Not applicable | Fix `ONLY_FULL_GROUP_BY` validation for window functions | Feature WordPress does not use |
| #3907 | 2026-09-23 | Not applicable | Optimize aggregate functions and remove sort nodes on multi-column indexes provided that any columns before the aggregated/sorted column are constant | Performance only |
| #3908 | 2026-09-23 | Not applicable | Unwrap `ANY_VALUE` for aggregates and window functions | Feature WordPress does not use |
| #3909 | 2026-09-23 | Not applicable | [no-release-notes] server: Expose the sql.Session for a *mysql.Conn through a GetSession accessor. | Refactoring, tests, or wording only |
| #3911 | 2026-09-25 | Not applicable | Add delimited time regex | The code is not in the fork |
| #3912 | 2026-09-23 | Not applicable | [no-release-notes] Fix failing `TestWindowFunctions` in Doltgres CI tests | Doltgres only |
| #3914 | 2026-09-23 | Not applicable | [no-release-notes] set the system timezone for DoltgreSQL CI tests | Only tests, memory, or docs changed |
| #3915 | 2026-09-23 | Not applicable | [no-release-notes] Mark any_value to mysql dialect | Only tests, memory, or docs changed |
| #3916 | 2026-10-15 | Not applicable | Prune table columns beneath HAVING | Performance only |
| #3919 | 2026-09-25 | Not applicable | implement precision for TIME types | New feature |
| #3921 | 2026-09-25 | Not applicable | Fix non-literal evaluation in `unix_timestamp` | Recovered panic, in a statement WordPress does not issue |
| #3924 | 2026-09-28 | Not applicable | Fix panic on nil decimal values in `VALUES` derived tables | Recovered panic, in a statement WordPress does not issue |
| #3932 | 2026-10-20 | Not applicable | When replacing a subquery with a table alias, push the subquery columnIds into the underlying table. | Feature WordPress does not use |
| #3934 | 2026-09-28 | Not applicable | [no-release-notes] Make `UNIX_TIMESTAMP` subquery string test timezone-independent | Only tests, memory, or docs changed |
| #3935 | 2026-09-28 | Not applicable | [no-release-notes] Fail when Dolt/DoltgreSQL integration tests also fail against main | Only tests, memory, or docs changed |
| #3936 | 2026-10-02 | Not applicable | Convert `ANY_VALUE` from an aggregate to a scalar function | Feature WordPress does not use |
| #3937 | 2026-09-28 | Not applicable | [no-release-notes] Accept `dolthub/dolt#N` and `dolthub/doltgresql#N` PR references in integration test workflow | Only tests, memory, or docs changed |
| #3938 | 2026-09-29 | Not applicable | [no-release-notes] Always run integration tests; gate merging on separate jobs | Only tests, memory, or docs changed |
| #3939 | 2026-09-29 | Not applicable | fix panic for `CAST` and `CONVERT` to types with invalid precision/scale | Recovered panic, in a statement WordPress does not issue |
| #3946 | 2026-09-29 | Not applicable | Default to `InternalDecimalType` for `CONVERT` with decimal arguments | The code is not in the fork |
| #3947 | 2026-10-01 | Not applicable | Execute duplicate-key update triggers | Feature WordPress does not use |
| #3948 | 2026-09-30 | Not applicable | Implement numeric converts to `TIME` with precision | New feature |
| #3949 | 2026-09-30 | Not applicable | Fix column statistics queries without initialized privileges | Feature WordPress does not use |
| #3951 | 2026-09-30 | Not applicable | Validate temporal cast precision | Recovered panic, in a statement WordPress does not issue |
| #3952 | 2026-09-30 | Not applicable | Account for COUNT DISTINCT memory usage | Performance only |
| #3953 | 2026-09-30 | Not applicable | Allow grouped expressions without column dependencies | Feature WordPress does not use |
| #3954 | 2026-09-30 | Not applicable | Respect context cancellation errors during group by aggregation | The code is not in the fork |
| #3959 | 2026-09-30 | Not applicable | Recompute stored generated columns after definition changes | Feature WordPress does not use |
| #3961 | 2026-09-30 | Not applicable | Store binary collation metadata explicitly | Performance only |
| #3962 | 2026-09-30 | Not applicable | Clamp unsigned MEDIUMINT serialization to its maximum value | Refactoring, tests, or wording only |
| #3963 | 2026-09-30 | Not applicable | Reset regular expressions between window partitions | Feature WordPress does not use |
| #3965 | 2026-09-30 | Not applicable | Reject out-of-range INET_NTOA arguments | Feature WordPress does not use |
| #3970 | 2026-09-30 | Not applicable | Support PostgreSQL UPDATE row counts | Doltgres only |
| #3972 | 2026-10-01 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #3974 | 2026-10-01 | Not applicable | Test listener shutdown with pending connections | Only tests, memory, or docs changed |
| #3975 | 2026-10-01 | Not applicable | Test EXISTS over empty scalar aggregate inputs | Only tests, memory, or docs changed |
| #3976 | 2026-10-01 | Not applicable | Test EXISTS and NOT EXISTS with window functions | Only tests, memory, or docs changed |
| #3982 | 2026-10-02 | Not applicable | [no-release-notes] enginetest: add regression test for dolthub/dolt#11558 | Only tests, memory, or docs changed |
| #3985 | 2026-10-13 | Not applicable | Add regression test for ValueRowIter | Only tests, memory, or docs changed |
| #3987 | 2026-10-06 | Not applicable | [no-release-notes] Cleanup QueryTest functions | Only tests, memory, or docs changed |
| #3989 | 2026-10-06 | Not applicable | Infer precision for `TIME` function | New feature |
| #3990 | 2026-10-10 | Not applicable | [no-release-notes] organize test queries scripts, breaking up script_queries.go | Only tests, memory, or docs changed |
| #3991 | 2026-10-12 | Not applicable | Optimize wildcard-only LIKE filters | Performance only |
| #3992 | 2026-10-08 | Not applicable | Added view recursion detection | Statement HeimDB rejects |
| #3993 | 2026-10-08 | Not applicable | Moved the external function provider into the builder overrides | Refactoring, tests, or wording only |
| #3994 | 2026-10-08 | Not applicable | Improve `TIME` parsing | Feature WordPress does not use |
| #3996 | 2026-10-07 | Not applicable | [auto-bump] [no-release-notes] dependency by nicktobey | Dependency bump only |
| #3997 | 2026-10-08 | Not applicable | Fix extended-type hash key conversion | Doltgres only |
| #3998 | 2026-10-09 | Not applicable | Prevent subquery filter pushdown from substituting set-returning functions into scalar predicates | Doltgres only |
| #4000 | 2026-10-09 | Not applicable | Have nested stored procedures reuse existing transaction | Feature WordPress does not use |
| #4003 | 2026-10-12 | Not applicable | Estimate a join of two empty estimates as empty on every architecture | Performance only |
| #4006 | 2026-10-12 | Not applicable | [no-release-notes] Initialize status variables in process list tests so they pass when run alone. | Only tests, memory, or docs changed |
| #4007 | 2026-10-12 | Not applicable | Stop ending the query in the process list when the top-level row iter is closed. The caller of BeginQuery owns EndQuery. | Refactoring, tests, or wording only |
| #4008 | 2026-10-13 | Not applicable | Make Handler.doQuery start and end the root query span. Remove Context.RootSpan and deprecate WithRootSpan. | Refactoring, tests, or wording only |
| #4009 | 2026-10-12 | Not applicable | End non-recording spans in NewSpanIter. | Refactoring, tests, or wording only |
| #4015 | 2026-10-14 | Not applicable | Support conditional column drops in ALTER TABLE plans | New feature |
| #4016 | 2026-10-14 | Not applicable | [auto-bump] [no-release-notes] dependency by tbantle22 | Dependency bump only |
| #4018 | 2026-10-14 | Not applicable | [no-release-notes] add test for fixed issue | Only tests, memory, or docs changed |
| #4019 | 2026-10-15 | Not applicable | [no-release-notes] More `DATETIME` test fixes | The code is not in the fork |
| #4022 | 2026-10-15 | Not applicable | sql/types: build the JSON_EXTRACT result array only from paths that match | Feature WordPress does not use |
| #4023 | 2026-10-21 | Not applicable | Added AggregateCallResolver for functions that share a name with an aggregate | Refactoring, tests, or wording only |
| #4024 | 2026-10-15 | Not applicable | Fix panic for `JSON_PATH`s pointing to null objects and null slices | Dependency bump only |
| #4025 | 2026-10-15 | Not applicable | [no-release-notes] enginetest: test joins on NULL and NULL-safe equal keys under every join strategy | Only tests, memory, or docs changed |
| #4026 | 2026-10-16 | Not applicable | sql: Add factories for independent registries of SystemVariables and StatusVariables. | Refactoring, tests, or wording only |
| #4027 | 2026-10-16 | Not applicable | sql: Engine can carry EngineVariables and they are exposed on the Context and Session. | Refactoring, tests, or wording only |
| #4028 | 2026-10-16 | Not applicable | sql: SQL planning and execution access EngineVariables through Context/Session. | Refactoring, tests, or wording only |
| #4029 | 2026-10-16 | Not applicable | sql: Server lifecycle, background work and test harnesses support EngineVariables. | Refactoring, tests, or wording only |
| #4033 | 2026-10-20 | Not applicable | Build information_schema column defaults without the parser | Performance only |
| #4038 | 2026-10-19 | Not applicable | [no-release-notes] enginetest: Just load setup scripts at runtime, no need for codegen. | Only tests, memory, or docs changed |
| #4040 | 2026-10-16 | Not applicable | Fix off-by-one error when finding aggregated column during `replaceSortedMinMax` | The code is not in the fork |
| #4044 | 2026-10-20 | Not applicable | Assign column IDs to "Unreferenceable" columns | Feature WordPress does not use |
| #4045 | 2026-10-21 | Not applicable | Converted extended type column defaults with assignment casts | Doltgres only |
| #4046 | 2026-10-20 | Not applicable | [no-release-notes] enginetest: Cleanup a few lingering warts in enginetest. | Only tests, memory, or docs changed |
| #4048 | 2026-10-22 | Not applicable | build(deps): bump github.com/klauspost/compress from 1.17.8 to 1.18.7 in /_integration/go | Only tests, memory, or docs changed |
| #4049 | 2026-10-21 | Not applicable | go.mod: Bump to 1.27.3. Fix some COMPRESS() tests where the behavior changed. | Dependency bump only |
| #4051 | 2026-10-21 | Not applicable | Count characters instead of bytes in INSERT() | Statement HeimDB rejects |
| #4052 | 2026-10-21 | Not applicable | Use CharSetHandler for character offsets in `INSERT()` | Statement HeimDB rejects |
| #4057 | 2026-10-23 | Not applicable | Skip costed index scan for non `MATCH...AGAINST` filters on fulltext indexes | Feature WordPress does not use |
| #4058 | 2026-10-23 | Not applicable | execute multiple statements in stored procedure exit handlers | Feature WordPress does not use |
| #4059 | 2026-10-24 | Not applicable | Apply sql_select_limit to simple selects and DISTINCT | Feature WordPress does not use |
| #4062 | 2026-10-24 | Not applicable | Do not hoist filters out of the null side of a lateral join | Feature WordPress does not use |
| #4064 | 2026-10-26 | Not applicable | Improve functional dependency analysis when tables don't have a primary key | Performance only |
| #4069 | 2026-10-26 | Not applicable | Normalize Decimal Hash Value | The code is not in the fork |
| #4073 | 2026-10-28 | Not applicable | [no-release-notes] Remove the deprecated `enginetest/scriptgen/setup` reexport package. Use `enginetest/setup` instead. | Only tests, memory, or docs changed |
| #4074 | 2026-10-28 | Not applicable | Reject subqueries, variables, and non-deterministic functions in functional index expressions. | Statement HeimDB rejects |
| #4075 | 2026-10-30 | Not applicable | Fail queries whose generated column expressions recursively reference their own table instead of overflowing the stack. | Cannot be checked with SQL |
| #4076 | 2026-10-30 | Not applicable | Skip resolving virtual columns for DROP and SHOW statements so tables with unresolvable generated expressions can be inspected and repaired. | Feature WordPress does not use |
| #4078 | 2026-10-27 | Not applicable | Add shared regressions for keyless unique writes | Only tests, memory, or docs changed |
| #4085 | 2026-11-05 | Not applicable | enginetest: test index lookups on keyless tables with a virtual column before a stored column | Only tests, memory, or docs changed |
| #4086 | 2026-10-30 | Not applicable | Fixed CHECK constraints on tables with virtual columns | Feature WordPress does not use |
| #4087 | 2026-10-29 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Dependency bump only |
| #4090 | 2026-10-30 | Not applicable | Fixed merge join ranges for literals of another extended type | Doltgres only |
| #4094 | 2026-10-30 | Not applicable | Support unary bitwise complement | New feature |
| #4098 | 2026-11-03 | Not applicable | sql: check generated SET target by table, not name | Feature WordPress does not use |
| #4099 | 2026-11-03 | Not applicable | sql/expression/function: unwrap out-of-band blob arguments at use | The code is not in the fork |
| #4100 | 2026-11-04 | Not applicable | Preserve index lookups during join replanning | Recovered panic, in a statement WordPress does not issue |
| #4102 | 2026-11-05 | Not applicable | sql: Make independent variable state the default for Engine. | Refactoring, tests, or wording only |
| #4103 | 2026-11-05 | Not applicable | sql: Remove the global variable registries. | Refactoring, tests, or wording only |
| #4104 | 2026-11-05 | Not applicable | sql: Tighten the session and registry interfaces around engine variables. | Refactoring, tests, or wording only |
| #4109 | 2026-11-05 | Not applicable | Resolve inherited defaults in indexed CREATE TABLE ... SELECT | Statement HeimDB rejects |
| #4110 | 2026-11-05 | Not applicable | Fix JSON_TYPE SQL NULL propagation | Feature WordPress does not use |
| #4118 | 2026-11-05 | Not applicable | [no-release-notes] enginetest/queries: add chained comparison | Dependency bump only |
| #4121 | 2026-11-06 | Not applicable | Improve functional dependency analysis for inner joins when tables don't have a primary key | Performance only |
| #4122 | 2026-11-05 | Not applicable | [auto-bump] [no-release-notes] dependency by elianddb | Dependency bump only |
| #4123 | 2026-11-05 | Not applicable | Preserve caller transactions when executing stored procedures | Feature WordPress does not use |
| #4126 | 2026-11-06 | Not applicable | Remove unnecessary filter conditions from semijoin plans | Performance only |
| #4127 | 2026-11-05 | Not applicable | JSON_TYPE(null) evaluates to null, not "NULL" | Feature WordPress does not use |
| #4129 | 2026-11-06 | Not applicable | [auto-bump] [no-release-notes] dependency by fulghum | Dependency bump only |
| #4130 | 2026-11-09 | Not applicable | Return SQL NULL for JSON_EXTRACT wildcard misses | Dependency bump only |
| #4131 | 2026-11-06 | Not applicable | Reject transaction control in triggers with ER 1422 | Feature WordPress does not use |
