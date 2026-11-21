# Development plan for a WordPress-only branching database

Oct 2, 2026

## Overview and goals

Build a database that speaks the MySQL-compatible protocol and lets you "roll WordPress data back to a point in time" and "create staging with a branch". It is written in Go, uses go-mysql-server for the SQL layer and Pebble for storage.

The value of this database lies in features plain MySQL cannot offer, so beating MySQL on speed is not a goal. Building what does not exist yet takes priority over practicality.

The two headline features are:

- **Time travel**: `AS OF` reads data at any past point in time. If a plugin update breaks the site, you can go back to just before the update.
- **Branches**: create staging from production without copying. WordPress only needs `DB_NAME` set to something like `wordpress@staging`. You can review the diff and merge it into production.

What sets it apart from other databases with branches is that it "knows WordPress": URL overrides, avoiding ID conflicts, and merges that follow the meaning of each table. The details are in "WordPress-specific problems".

Versions: the source was published in 2026-11 as 0.3.0. While in 0.x, the minor version is raised when backward compatibility of the data format is dropped ("Upgrading HeimDB" in [`operations.md`](operations.md)). The target for 1.0.0 is one year after publication (around the end of 2027).

Not planned for now: distributed placement at the edge, performance optimization, full compatibility as a general-purpose MySQL, SQL features WordPress does not use, full-text search (`MATCH ... AGAINST`; FULLTEXT indexes are handled as definitions only).

## Results of early validation

With only go-mysql-server's in-memory backend (the `memory` package), the WordPress installer ran to the end without changes. Comparing the wordpress-develop PHPUnit test suite against real MySQL 8.0 gave the following.

| Group | Tests | MySQL 8.0 | Fake MySQL (in-memory) |
| --- | --- | --- | --- |
| option | 413 | all pass | all pass |
| meta | 481 | all pass | all pass |
| query | 1,896 | all pass | 20 failures, 1 error |
| wpdb | 651 | all pass | 66 failures |

The known causes are:

- **Order of ties**: for `ORDER BY post_date DESC`, MySQL returns rows with the same date in descending ID order, while the fake MySQL returns them in ascending ID order. This is likely because MySQL scans the `type_status_date (post_type, post_status, post_date, ID)` index backwards (not yet confirmed with `EXPLAIN`). `test_query_sticky_posts_change` reproduces this failure when run alone.
- **Duplicate IDs**: only when the group runs, `754` is returned 5 times where `758..754` is expected. It does not reproduce alone; state carried over from earlier tests is suspected. A primary key `IN` lookup alone, and an `IN` lookup inside a transaction, did not reproduce it. Not explained. (In stage 20, duplicate rows caused by overlapping index ranges were found and fixed. This is probably it.)
- **Character sets**: `CONVERT ... USING` to koi8r, hebrew, big5 and others drops the connection (MySQL server has gone away). Getting a table's character set and `SET NAMES ... COLLATE` also differ from the real thing.
- **dbDelta**: FULLTEXT indexes are not reported by `SHOW INDEX FROM`, so they are judged as "needs to be added" every time.

These are results on the in-memory implementation. In the Pebble implementation, the ordering problem is to be solved structurally by scanning indexes backwards.

## Architecture and design

MVCC is used from the start: rows are never overwritten, and each change appends a new version. Time travel is "reading the latest version at or before a given time", and a branch is "reading the parent up to the fork time and layering its own changes after that". Adding versions later would mean redesigning the keys, so versions and times are in the keys even while `main` is the only branch.

There are 3 layers.

1. **Protocol and SQL**: go-mysql-server handles the MySQL wire protocol, parsing and query plans.
2. **Storage adapter**: our own code. It implements `sql.DatabaseProvider`, `sql.Database`, `sql.TableCreator`, `sql.Table`, `sql.InsertableTable`, `sql.UpdatableTable` and `sql.DeletableTable`, and maps rows and indexes to Pebble keys. The interfaces change between versions, so the `memory` package of the version in use is the reference.
3. **Pebble**: an ordered key-value store (`github.com/cockroachdb/pebble/v2`). It provides range scans, atomic batch writes and snapshots.

### Key design

```
m/clock                                   → last commit time issued
m/seq/<name>                              → ID sequences ("branch", "table")
m/branch/<branch ID>                      → branch info (name, parent branch, fork time)
m/gc                                      → time of the boundary of the last finished GC (nothing before it can be read)
m/merge/<branch ID>/<inverted time>       → merge base (stage 7)
m/resolve/<branch ID>/<table name>/<id>   → record of a conflict resolution (stage 7b)
m/sync/<branch ID>/<inverted time>        → record of a sync of the parent's changes into the branch (the time in the key is the sync's commit time, the value is the parent time that was read. Stage 94)
m/users                                   → users and privileges (go-mysql-server's mysql_db serialization. Not versioned. Stage 14)
m/format                                  → data format version number (stage 18)
m/logtrim                                 → time of the last deleted commit record (stage 24)
m/rewrite/<branch ID>/<table ID>          → marker of the new table ID during a rewrite (stage 53. GC does not delete it. Cleaned up at startup)
m/mergeexclude                            → list of table patterns excluded from merges (JSON. Not versioned. Stage 60)
l/<time>                                  → commit record (the contents of that commit's Pebble batch. Stage 24)
l/<time>/<number>                         → continuation of a record over 1 MiB (numbered from 1. Stage 59)
b/<branch ID>/c/<table name>/<inverted time>
                                          → table definition (schema), or a deletion marker
b/<branch ID>/t/<table ID>/ai             → next AUTO_INCREMENT value
b/<branch ID>/t/<table ID>/r/<primary key...>/<inverted time>
                                          → row data, or a deletion marker
b/<branch ID>/t/<table ID>/i/<index ID>/<columns...>/<primary key...>/<inverted time>
                                          → original values of the index columns and the primary key (stage 27), or a deletion marker
```

- The `/` in this notation only marks separations. A real key is a 1-byte tag (`m`, `b`, `c` and so on) joined with values in an order-preserving encoding. IDs and times are 8-byte big-endian; names use the string encoding described below.
- Times are UNIX times in microseconds, issued monotonically through `m/clock` as "the larger of the current time and the previous time + 1". They go into keys inverted (`math.MaxUint64 - ts`). Versions of the same row then sort newest first, and "the latest version at or before time T" takes one seek.
- A delete does not remove keys; it appends a "deletion marker". Removing keys would make a row deleted in a branch show through from the parent. The first byte of the value is `0x00` (deletion marker) or `0x01` (data).
- The `main` branch has ID 1. Table IDs and branch IDs come from the `m/seq/` sequences and are never reused.
- Table definitions are versioned too, so that `AS OF` and branch layering can look up "the schema at that point" even after `ALTER TABLE`. Each row value holds the version (time) of the table definition it was written with, and is decoded with that schema.
- Table definition keys use the lowercased name; row and index keys use the table ID. A rename only writes "a deletion marker on the old name and a definition with the same table ID on the new name" at the same time, without rewriting rows. Recreating a dropped table with the same name gives it a new table ID.
- Table definitions are stored as JSON. They hold the columns (name, type string, DEFAULT and ON UPDATE expression strings, nullability, AUTO_INCREMENT, primary key, comment), the primary key column order, index definitions (ID, name, columns and prefix lengths, unique or not, key level), the collation and the comment. Types and expressions are stored in go-mysql-server's string form (from stage 120, FLOAT and DOUBLE type strings carry the digits and unsigned, as in MySQL: `float(7,2)`, `double(10,3) unsigned`, `float unsigned`. Definitions up to stage 119 have only `float` and `double`, and are read as types without digits), and restored with `planbuilder.ParseColumnTypeString` and `planbuilder.StringToColumnDefaultValue`. The `CREATE TABLE` statement itself is not stored, because go-mysql-server does not hand it over (`CreateTable` receives a structured schema).
- The value of an index entry holds the original values of the index columns and of the primary key columns not in the index, in the same encoding as row values (without the version number) (stage 27). String keys cannot be turned back into the original value from collation weights (`'Post'` and `'post'` are the same key), so the value is kept so that a statement using only these columns can be answered without fetching the row itself. Even an update to a value that is equal under the collation (same key) rewrites the entry if the value changed. Entries written before stage 27 have no value (empty); reads fetch the row itself for them (the data format version was not changed).
- An index definition has an integer level (`key_level`, stage 117) that says which types can go into its entries. 0 (no marker; definitions created before stage 117) allows only integers, date/time, strings and byte strings; 1 also allows DECIMAL, FLOAT, DOUBLE and ENUM; 2 (stage 120) also allows SET and BIT. An index with any column of a type outside its level has no entries and is emulated by reading all rows (like DECIMAL and similar indexes before stage 117). New tables, `CREATE INDEX`, indexes added by a merge, and an `ALTER TABLE` that rewrites the table get the current level, and their entries are built from the rows at that point. Existing indexes stay at level 0 when read (past points and branches are read at the level of the definition at that point, so a point without entries is never read), and the user rebuilds them at the current level with `CALL heimdb_rebuild_indexes()` (it writes the branch's current definition and builds entries the same way as `CREATE INDEX`). From stage 121, production rebuilds the ones in main one at a time in the background after startup, the same way (`-rebuild-indexes`; the data format version and the record level do not change). The level is not part of definition equality (index comparison in restore, sync and merge).
- Secondary indexes include the primary key at the end, as in InnoDB. Scanning backwards then returns ties in descending primary key order, matching MySQL's order. However, go-mysql-server v0.20.0 picks a backward scan only in limited cases (it replaces `ORDER BY` with an index scan only when the sort columns match the leading columns of the index; `isSortFieldsValidPrefix` in `internal/sqlengine/sql/analyzer/replace_sort.go`).
- Tables without a primary key (stage 72) are shown to the engine as tables without a primary key, and stored with one of the following storage keys (`keyless` in the table definition). Multi-column primary keys were supported from the start, for `wp_term_relationships`.
  - Tables ordered by a unique key: if there is a unique index whose columns are all `NOT NULL`, include no prefix-length columns, and are all of types that can be a primary key (`rowenc.PKType`), the first such index (the one MySQL's InnoDB makes the implicit primary key) gives the storage key (the definition's `pk_ordinals` are those columns). The key has the same form as a primary key. The index definition and entries are kept as a normal unique index.
  - Tables ordered by row ID: otherwise, two hidden columns are added at the end, `__heimdb_row_id` (`bigint unsigned`; the storage key) and `__heimdb_row_hash` (`varbinary(16)`; the first 16 bytes of the SHA-256 of the encoded visible column values), and a hidden index is added on `__heimdb_row_hash` (`hidden` in the column and index definitions). `__heimdb_row_id` is a hidden AUTO_INCREMENT column, numbered from the same per-branch range as 64-bit columns by the AUTO_INCREMENT counter (`ai`) (the engine sees a table without an AUTO_INCREMENT column; for this reason, a table with an AUTO_INCREMENT column and no primary key is rejected). A full read returns rows in insertion order (like MySQL's hidden row ID). The engine passes the old row values when deleting or updating a row, so the row ID is looked up through the hash index (if several rows have the same contents, the smallest ID). `UPDATE`, merge, restore and replicas do not change row IDs.
  - Each rewrite (`ALTER TABLE`), addition or removal of a unique index, `ADD PRIMARY KEY` and `DROP PRIMARY KEY` decides again which of the two to use, from the definition at that point. If it changes, the table is rewritten (an empty table only changes its definition). FULLTEXT indexes keep only their definition and get no search entries (stage 10).
- A branch's diff is obtained just by scanning that branch's key space.
- A commit record (`l/<time>`) holds the contents of that commit's batch (including `m/clock`) and is written in the same batch. The time is the commit time, not inverted (read oldest first). Replicas apply the same records in the same order and keep the same records themselves. A record over 1 MiB is split into 1 MiB pieces written to `l/<time>` and `l/<time>/<number>` (4-byte big-endian, from 1), and joined when read (stage 59; for one large value, Pebble keeps both the whole value and a compressed copy in memory when writing it out to a file). Not recorded is maintenance local to the machine (GC and its boundary `m/gc`, the data format version `m/format`, deleting records and its time `m/logtrim`, cleaning up temporary tables at startup). Machine-local keys are written directly to Pebble without going through a commit.

### Data format version

The data format version is kept in `m/format` (stage 18). Since it is now used in production, from stage 36 existing data is guaranteed to stay readable. 0.1.0 to 0.1.5 are version 1; from 0.1.6, version 2 (stage 38: single-column string primary keys. Version 1 data is read as is, and only the number is rewritten at startup); from 0.2.11, version 3 (stage 59: large commit records written in pieces. Version 2 records are read as is as unsplit records, and only the number is rewritten at startup); from 0.2.19, version 4 (stage 70: primary keys containing byte strings or dates, and multi-column primary keys containing strings. Version 3 data is read as is, and only the number is rewritten at startup); from stage 72, version 5 (tables without a primary key. Version 4 data is read as is, and only the number is rewritten at startup. The record content format stays 2, and the replica record level becomes 3); from stage 94, version 6 (the record of syncing the parent's changes into a branch, `m/sync`. Version 5 data is read as is, and only the number is rewritten at startup. The record content format stays 2, and the replica record level becomes 4. An older HeimDB does not know sync records and would misread the merge base, so it is not allowed to open the data); from stage 117, version 7 (index entries for indexes containing DECIMAL, FLOAT, DOUBLE and ENUM columns, and the key level in index definitions. Version 6 data is read as is, and an index without a level is read as having no entries, as before. Only the number is rewritten at startup. The record content format stays 2, and the replica record level becomes 5. An older HeimDB does not know levels and would not maintain the entries of those indexes, so it is not allowed to open the data); from stage 120, version 8 (type strings for FLOAT(M,D), DOUBLE(M,D) and UNSIGNED, and key level 2 for index entries of indexes containing SET and BIT columns. Version 7 data is read as is: FLOAT and DOUBLE columns of existing tables stay types without digits, and level 0 and 1 indexes containing SET or BIT columns are read as having no entries, as before. Only the number is rewritten at startup. The record content format stays 2, and the replica record level becomes 6. 0.2.39 and 0.2.40 would silently read `float(7,2)` as `float` without digits, not round on write, and not maintain level 2 index entries, so they are not allowed to open the data).
- The version is raised even when the existing key forms stay the same and a new form is only added (stage 70). This keeps an older HeimDB from opening data that contains tables in the new form (an older version would reject the table only when it opens it, or misread it). Existing data is not migrated; only the number is rewritten.

- To change it, raise `store.FormatVersion` and add code that migrates data from the old version to the new one at startup. If only indexes change, rebuilding them from rows is enough (indexes can be derived from rows). There are no plans to change the foundations: rows, table definitions and key order.
- Things that could force a change: string keys in indexes are ordered by go-mysql-server's collation weights (per character) (`rowenc/index.go`), so if a go-mysql-server update changes the weights, or a difference from MySQL (trailing space comparison, multi-character mappings) is fixed, the keys must be rebuilt. A new column type (such as `JSON`) only adds a type tag and does not change existing data. Upgrading go-mysql-server could change the serialization of users and privileges (`m/users`) and how type names are parsed.
- The column type strings in table definitions (`Column.Type` in `internal/schema`) were fixed to escape `'` and `\` in ENUM and SET values (stage 111). The reading side (vitess's parser) is unchanged, and an older HeimDB reads the new definitions to the same values, so the version was not changed. Definitions saved before are read as they are (values containing `\` are read as escape characters and so on, as before).
- An older HeimDB refuses to start on data of a newer version (stage 18). Replication carries records (keys and values) as they are, so the versions of production and replica are checked, and they are not connected if they differ (stage 36). What is checked is the version of the record content format (`store.RecordFormat`), kept separate from the data format version (stage 59). Version 3 only changed how commit records are laid out; the records carried (joined) are the same as in version 2, so the record content format stays 2 (it connects with 0.2.10 and earlier). When row or index keys and values change, both are raised. Version 4 does not change existing key and value forms and only adds tables with primary keys in new forms, so the record content format stays 2 (while replicas are upgraded first, they still connect to production at 0.2.18 or earlier). In exchange, a replica at 0.2.18 or earlier must not be connected to production at 0.2.19 or later (it would also receive records for tables with primary keys in the new forms. docs/operations.md).

### Time travel times and the retention period

The time passed to `AS OF` is interpreted as a date and time in the session's `time_zone` (the same feel as MySQL's TIMESTAMP type). go-mysql-server passes string literals as strings, and `TIMESTAMP(...)` and `NOW()` as "a `time.Time` holding the date and time in the session time zone but labeled UTC", so in both cases the date and time fields are reread in the session time zone and converted to the internal time (UTC microseconds). Strings are accepted as `YYYY-MM-DD`, `YYYY-MM-DD HH:MM:SS`, or with fractional seconds; anything else is an explicit error. Tables at a past point are read-only and do not include your own uncommitted changes.

Policy for the retention period after which old versions are deleted:

- The retention period is a startup setting (`-retention`, default 30 days, 0 means never delete). The GC interval is also a setting (`-gc-interval`, default 1 hour).
- What may be deleted: versions older than the retention boundary for which the same row has a newer version at or before the boundary (the version visible at the boundary is kept). If all versions at or before the boundary are deletion markers, all keys of that row may be deleted.
- For versions at or before a branch's fork time, the version visible at the fork time is kept, because the child branch reads it through the parent.
- An `AS OF` before the retention boundary is an explicit error.
- The version visible at a merge base (stage 7) is also kept (it is read as the base of the next merge).
- Deletion of old versions (GC) is implemented after stage 6, once the branch design is in place.

### Order-preserving encoding

Values are encoded so that "their order when compared as byte strings matches the order of the original values". Pebble's range scans then directly serve as `ORDER BY` and range lookups.

```go
// Unsigned integer: big-endian
func appendUint64(b []byte, v uint64) []byte {
	return binary.BigEndian.AppendUint64(b, v)
}

// Signed integer: flip the sign bit
func appendInt64(b []byte, v int64) []byte {
	return binary.BigEndian.AppendUint64(b, uint64(v)^(1<<63))
}

// String: escape 0x00 as 0x00 0xFF, terminate with 0x00 0x01
func appendBytes(b []byte, s []byte) []byte {
	for _, c := range s {
		if c == 0x00 {
			b = append(b, 0x00, 0xFF)
		} else {
			b = append(b, c)
		}
	}
	return append(b, 0x00, 0x01)
}
```

Without the string terminator, the order of `("ab", "c")` and `("abc", ...)` breaks in multi-column keys (the same idea as CockroachDB).

When a column value goes into a key, it is prefixed with a NULL marker (`0x00` for NULL, or `0x01` followed by the value). MySQL's ascending order puts NULL first, and this matches it. Strings need the marker too; without it, the order of the empty string (`0x00 0x01`) and NULL is not defined.

Primary key columns go in without a NULL marker, per type as follows (a primary key cannot hold NULL). Ascending primary key order is exactly the key order.

- Integers: "`0x00` followed by the 8 bytes of `appendInt64`". Only `uint64` values that do not fit in `int64` go in as "`0x01` followed by the 8 bytes of `appendUint64`". Signed and unsigned values compare in the same order.
- Strings with a collation (`CHAR`, `VARCHAR`; stage 38): the sequence of collation weights, the same as in secondary indexes.
- Binary strings (`BINARY`, `VARBINARY`; stage 70): `appendBytes`. The engine pads `BINARY(n)` values with trailing `0x00` to n bytes when writing, so the padded value goes in.
- Date and time (`DATE`, `DATETIME`, `TIMESTAMP`; stage 70): the same 12 bytes as in secondary indexes.
- `ENUM` (stage 70): the number (uint16) the engine puts in the row, in the same form as integers. MySQL also orders `ENUM` by number.
- In row names in diffs and duplicate errors, byte strings that cannot be read as UTF-8 or contain control characters are shown as `0x` hex (the result column is a utf8mb4 string).

Types can be combined in any way (stage 70; before that, only integer columns and a single string column). Primary keys of other types (`TEXT`, `BLOB`, `SET`, `DECIMAL` and so on) are rejected with an explicit error. WordPress core primary keys are all integers; strings, byte strings, dates and `ENUM` are used by plugin tables (WooCommerce, Wordfence and others). For every type, a value's length is self-delimiting (no key is a prefix of another key), so joining columns does not break the order.

A row value is laid out as a 1-byte marker (`0x00` deletion marker, `0x01` data), the table definition version (8 bytes), the number of columns, and each column's value. Column values store the Go values go-mysql-server puts in the row as "type tag + contents" as they are, without converting through the column type (`internal/rowenc`).

Secondary index key columns are prefixed with a NULL marker and then encoded per type as follows; the original values are read from the row.

- Integers: same format as the primary key.
- Strings with a collation: the collation weight of each character (go-mysql-server's `CollationID.Sorter()`), each as "`0x01` + 4 bytes with the sign flipped", terminated with `0x00`. This gives the same order as go-mysql-server's string comparison (compare weights in order; if equal, the shorter one first), and with a case-insensitive collation `'home'` and `'HOME'` become the same key. With a prefix length, the value is cut to that many characters before computing weights.
- Binary strings: `appendBytes`.
- Date and time (DATETIME, TIMESTAMP, DATE): UNIX seconds with `appendInt64`, followed by nanoseconds in 4 bytes. `0000-00-00 00:00:00` (Go's zero time) also sorts first.
- The following apply only to indexes at key level 1 (stage 117).
  - DECIMAL: a form independent of the column's scale. One sign byte (`0x01` negative, `0x02` zero, `0x03` positive; `-0` is 0). Zero is just this. A positive value is one byte for the number of integer digits (0 if the integer part is 0), each integer digit, each fractional digit with trailing zeros removed (one byte per digit, the digit plus 1, `0x01` to `0x0A`), and the terminator `0x00`. A negative value inverts every byte of the positive form of its absolute value (from the digit count through the terminator; the terminator becomes `0xFF`). `1.5` and `1.50` become the same key. The terminator appears nowhere else, so no key is a prefix of another, and inverting only reverses the order without breaking it.
  - FLOAT, DOUBLE: the IEEE 754 bits of the value as a double, 8 bytes, with the sign bit set for positive values (including `+0`) and all bits inverted for negative values. `-0` becomes `+0` (MySQL also treats 0 and -0 as duplicates in a unique index). NaN and infinity cannot be in a column, so they are an explicit error if they arrive. FLOAT (single-precision) values are widened to double (widening does not change the order).
  - ENUM: the number (uint16) the engine puts in the row, in the same form as integers (same as an ENUM primary key; MySQL also orders by number).
- The following apply only to indexes at key level 2 (stage 120).
  - SET: the bits of the set of members the engine puts in the row (uint64; the first value in the definition is the lowest bit), in the same form as integers (9 bytes). MySQL also orders SET indexes by this number. As in MySQL, only `=` and `IN` use a range lookup; comparisons with `<` and `>` (MySQL compares them as strings) read all rows.
  - BIT: the value (uint64), in the same form as integers (9 bytes). `appendKeyInt` also orders uint64 values of 2^63 or more as unsigned numbers, so the order of BIT(64) matches MySQL too.
- An index containing a type not listed above (such as JSON), or a type outside its level, has no keys and is emulated with a full scan and a sort. MySQL cannot index JSON directly either (3152).

For indexes with a prefix length, the key order does not match the value order, so rows found by a lookup are re-sorted by value. Encodings for other types follow the same principle and are added, with a higher level, in the stage that needs them.

## Development steps

The goal of the first demo is "write a post in WordPress, delete it, and read the post as it was before deletion with `AS OF`". Each stage moves on only after its completion criteria are met.

The detailed per-stage records (background, method, results, fixes from review, differences only recorded) were kept in the pre-publication history and are not in this repository. From stage 130 on, each stage's record (background, method, results, review fixes, and an "Engine changes" section) is written in English under `docs/stages/`. Code comments that point to stage N in docs/plan.md refer to stage N of this table. The differences from MySQL that each stage only recorded (stages 110 to 120) are collected in [`docs/differences.md`](differences.md).

Commit IDs cited in the documents for stages up to 129 (such as `65d845f`) are from the pre-publication history and are not in this repository's history (it was squashed into one commit when it was published).

| Stage | Summary |
| --- | --- |
| 0 | Understand the go-mysql-server interfaces |
| 1 | Encoding and schema persistence |
| 2 | Reading and writing versioned rows, and minimal transactions |
| 3 | Time travel |
| 4 | Secondary indexes |
| 5 | Transaction isolation |
| 5.5 | Column changes, TRUNCATE, temporary tables |
| 6 | Branches |
| 7 | Diff and merge |
| 8 | Restore to a past point in time |
| 9 | Deletion of old versions (GC) |
| 10 | FULLTEXT indexes |
| 11 | Small compatibility differences |
| 12 | Merging index differences |
| 13 | Merging column changes |
| 14 | Authentication |
| 15 | `ALTER` to NOT NULL under a non-STRICT sql_mode (item F postponed from stage 11) |
| 16 | Backup |
| 17 | Export and import with mysqldump |
| 18 | Durability (crash recovery, shutdown procedure, data format version) |
| 19 | Observability |
| 20 | Compatibility on the full PHPUnit test suite |
| 21 | Value handling under a non-STRICT sql_mode (11 cases found in stage 20) |
| 22 | JOIN, and read optimizations tuned to WordPress queries |
| 23 | Index and JOIN choice from statistics |
| 24 | Commit records and asynchronous replicas |
| 25 | Synchronous replicas with a timeout, and a manual failover procedure |
| 26 | Encrypted connections (TLS) |
| 27 | Index-only reads |
| 28 | Small speed improvements |
| 29 | Sampling for statistics |
| 30 | Advance a replica's position only after rebuilding its in-memory state (branches, users, row estimates) |
| 31 | Prevent SQL from reading or writing server files |
| 32 | Do not run WordPress as root (least-privilege default) |
| 33 | Change watch (watch for risky changes) |
| 34 | List of changes, and warnings on restore |
| 35 | Block global operations WordPress does not use (`SET GLOBAL` and processlist) |
| 36 | Fix the data format version, and check versions in replication |
| 37 | Accept utf8mb3 tables as utf8mb4 |
| 38 | Single-column string primary keys |
| 39 | Tests with schemas shaped like long-lived sites, and operating notes |
| 40 | Bring go-mysql-server v0.20.0 into HeimDB (fork; no behavior change) |
| 41 | Clean up open transactions on shutdown |
| 42 | Move the security workarounds (A1 to A4: server files and similar) into the engine, and fix `go vet` findings |
| 43 | Fix findings from the review (Fable) of stages 34 to 42 |
| 44 | Around merges (statements to undo, `db_version` state, dry runs, cost of the diff summary) |
| 45 | Guardrails for test environments (branch size limit, statement time limit on branches, `MAX_USER_CONNECTIONS`) |
| 46 | Fix engine panics |
| 47 | Measure current performance on Linux (GCE) and pick the largest gaps |
| 48 | Performance fixes, round 1 (primary key checks, collation weight table, EXPLAIN PLAN) |
| 49 | Add known-vulnerability checks to CI (govulncheck) |
| 50 | Performance fixes, round 2 (REST API responses, term counts, category list) |
| 51 | LEFT JOIN in category and tag lists (outer join simplification) |
| 52 | Make integer type checks cheaper |
| 53 | Memory of ALTER TABLE on large tables |
| 54 | Faster LIKE matching for search |
| 55 | Do not exhaust memory on large tables |
| 56 | Catalog of plugins that work with HeimDB |
| 57 | Accept ascii columns and quoted engine names, as MySQL does |
| 58 | Keep the site running at the memory limit |
| 59 | Reduce memory of large writes, merges and restores |
| 60 | Tables excluded from merges, and machine-readable merge results |
| 61 | Separate plugins' `int` AUTO_INCREMENT ranges per branch |
| 62 | Notify when only one of a pair of tables is restored |
| 63 | Let branch users call the procedures for their own branch |
| 64 | Do not treat automatic duplicates with identical contents as conflicts (Issue #5) |
| 65 | Remove unneeded code and tests outside the engine (A) |
| 66 | Remove unused tests and tools from the engine fork (C) |
| 67 | Remove record-only tests, unused benchmarks and documents (B) |
| 68 | Survey the top 100 popular plugins on wordpress.org |
| 69 | Match MySQL for `SHOW INDEX ... WHERE` and `DO` |
| 70 | Primary keys containing strings, byte strings and dates |
| 71 | `UNION` containing primary key equality statements |
| 72 | Tables without a primary key |
| 73 | Metrics endpoint (Prometheus `/metrics`) |
| 74 | Accept `OPTIMIZE TABLE`, `CHECK TABLE` and `REPAIR TABLE` |
| 75 | Measure current performance on Linux (GCE) and decide what to fix next |
| 76 | Extend crash tests to branch operations and the new kinds of tables |
| 77 | Performance fixes (integer comparison, reading near-full ranges, JOINs looked up by primary key) |
| 78 | Performance fixes (per-statement and per-row costs) |
| 79 | Performance fixes (bad plans for light statements, and per-statement costs) |
| 80 | Performance fixes (JOINs looked up by constant keys, and the first statement per table after a restart) |
| 81 | Observing slow statements, and a way to stop statements |
| 82 | Find and remove connections stuck while holding the write lock (an outage in production) |
| 83 | meta_query JOIN plans and per-row speed (the headless API) |
| 84 | REST statements whose plans differ from MySQL (the headless API) |
| 85 | Match MySQL for string-to-number conversion |
| 86 | Use equality with expressions between comma-joined tables as join conditions (a cron job in production) |
| 87 | Open backups in environments with different privileges, and encrypted backups |
| 88 | Issues found while checking 0.2.28 (`/backups` ownership in the image, log text), and small differences from MySQL (double formatting, DECIMAL range, string-number `IN`) |
| 89 | Dependencies and the Go version (2 High findings from Docker Scout) |
| 90 | Connection collation and collation mix errors |
| 91 | Test suite that checks MySQL compatibility from PHP (wpdb) |
| 92 | In non-strict writes, adjust values that do not fit the column and store them, as MySQL does |
| 93 | `SHOW WARNINGS` after a failed statement, handling of values equal under the collation, `sql_mode` default |
| 94 | Sync the parent's changes into a branch (`heimdb_branch_sync`) |
| 95 | Estimate rows dropped by conditions outside the range (meta_query two_filters) |
| 96 | Reduce per-row cost (top of the profile measured on GCE) |
| 97 | Do not fetch rows that `SQL_CALC_FOUND_ROWS` only counts |
| 98 | Use the index for equality on values just below one that covers over 90% of rows |
| 99 | Do not open a row reader for each lookup in a lookup JOIN |
| 100 | Rewrite the benchmark statements to the form WordPress actually sends, and compare with MySQL again on GCE |
| 101 | Match MySQL's JOIN order for two meta conditions plus a sort on another key |
| 102 | Reduce the cost of each lookup in a lookup JOIN |
| 103 | Reduce allocations in index scans |
| 104 | Reduce the cost of sample-based estimates |
| 105 | Fuzzing SQL and the protocol |
| 106 | Readiness right after a restart (statistics build status in `heimdb_status` and metrics) |
| 107 | Rerun the plugin checks |
| 108 | Long-running load test |
| 109 | Find remaining engine memory use with tests and metrics |
| 110 | Survey and triage upstream fixes (no engine changes) |
| 111 | Upstream fixes, round 1 (robustness, security, data) |
| 112 | Upstream fixes, round 2 (subqueries and join plans) |
| 113 | Upstream fixes, round 3 (small fixes that do not change plan shapes) |
| 114 | Upstream fixes, round 4 (functions and type values) |
| 115 | Upstream fixes, round 5 (plans and SQL forms), and differences carried over |
| 116 | Carried-over value differences and older differences (date/time, numbers, ENUM, SQL forms) |
| 117 | Index entries for DECIMAL, FLOAT, DOUBLE and ENUM columns |
| 118 | Value differences plugins hit (FROM_UNIXTIME, FLOAT digits, TEXT warnings, zero dates) |
| 119 | Remaining paths that turn TEXT column values into numbers (warning 1292, VARIANCE and STDDEV values, 1366 from POW) |
| 120 | Data format version 8 (FLOAT(M,D) and DOUBLE(M,D), index entries for SET and BIT columns) |
| 121 | Rebuild old indexes in the background after startup |
| 122 | Do not check types per row in WHERE comparisons of full reads (slowdown measured in stage 121) |
| 123 | Remaining page slowdowns, and a breakdown of the fixed per-statement cost |
| 124 | Cost of the result-sending path (per-statement goroutine and watching for dropped connections) |
| 125 | Per-row cost of full-read statements (reusing decoded rows, and comparing strings with identical bytes) |
| 126 | Measuring behavior under load (concurrent connections, fixed rate, mixed writes, beyond the limit, long duration) |
| 127 | Scan released artifacts for vulnerabilities in CI on a schedule (toolchain version, published image, advisories for the 2 forked modules) |
| 128 | Throughput not scaling with 2 connections (wake a sleeping Go thread when a statement starts, and keep a network poller) |
| 129 | Preparing for publication (removing internal names, English README, contribution and vulnerability reporting channels) |
| 130 | Measuring the effect of a backup on running queries (reads wait too when the backup copies files; recorded) |
| 131 | Reads no longer wait for a backup's write lock, and `-backup-dir` can be inside `-data` (for Docker, a backup on the same volume hard-links the files) |

In stages 2 and 3, without indexes, every lookup is a full scan; this is acceptable at test data sizes. Character-set failures in the wpdb group get lower priority; only the dropped-connection problem is fixed, by returning an error for unsupported character sets.

## WordPress-specific problems

This database sets itself apart by solving the following problems that generic branching cannot. They are handled in earnest in stages 6 and 7, but earlier design must not get in their way.

**URL overrides.** WordPress keeps the site URL in `siteurl` and `home` in `wp_options`, and staging has to run on a different domain. A mechanism lets each branch override these two values, so that a branch runs on a different URL the moment it is created. How to handle absolute URLs in post content is undecided.

**ID conflicts.** If staging and production each assign the same IDs, they cannot be merged. The database controls AUTO\_INCREMENT, so it hands out a separate ID range to each branch and prevents conflicts structurally. The scheme chosen is fixed-width blocks (stage 6): 64-bit columns in branch ID n are numbered from (n - 1) × 10^10, and main (n = 1) keeps its current numbers. For 32-bit columns, main stays below 10^9, and branches get a range of width 10^7 starting at 10^9 + ((n - 2) mod 100) × 10^7 (stage 61). About 900,000 branches fit within the integers JavaScript can handle safely (about 9 × 10^15).

**Merges that follow the meaning of each table.** What you want to bring from staging to production is settings and static pages; comments and orders that came into production in the meantime must not be lost. The merge policy is as follows (decided in stage 7; the table prefix is `wp_` by default).

| Target | Policy |
| --- | --- |
| `wp_options` | Three-way, matched by `option_name`. Not applied: `siteurl`, `home`, `blog_public`, `cron`, `rewrite_rules` (regenerated after the merge), DB versions such as `db_version`, transients (`_transient_*`, `_site_transient_*`), locks (`*_lock`, `*.lock`), `recently_activated`, `fresh_site`, `auto_core_update_notified`, `https_*` |
| `wp_posts` | Three-way per column. `comment_count` is not compared and is recounted after the merge. Auto-drafts (`auto-draft`) and revisions are not applied. A post whose author is not in the parent is a conflict |
| `wp_postmeta` | Three-way per column. `_edit_lock`, `_edit_last`, and meta of posts that are not applied, are not applied |
| `wp_term_taxonomy` | Three-way per column. `count` is not compared and is recounted after the merge |
| `wp_terms`, `wp_termmeta`, `wp_term_relationships`, `wp_links`, plugin tables | Three-way per column |
| `wp_comments`, `wp_commentmeta`, `wp_users`, `wp_usermeta` | Changes on the branch side are not applied (production's changes are always kept) |

Reported as warnings: both sides separately added the same meta key to the same post; comments remain in production on a post deleted in staging.

Absolute URLs in post content are not handled. After a merge, run `wp search-replace '<staging URL>' '<production URL>' --skip-columns=guid` on production (the block editor embeds absolute URLs in content, so this is needed every time).

**Search is left to something outside the database.** HeimDB does not support full-text search (`MATCH ... AGAINST`) (stage 10). WordPress core search uses `LIKE '%word%'`, so it works on HeimDB as is (a full scan). If you need relevance ranking or Japanese word segmentation, use an external search service such as Elasticsearch / OpenSearch (ElasticPress) or Algolia (WP Search with Algolia). The search index lives outside the database and does not follow branches or time travel, so the following go into the operating procedure (built in when productized).

- These plugins send changes to the index from WordPress hooks (such as `save_post`), but HeimDB's merge (`heimdb_branch_merge`) and restore (`heimdb_restore`) rewrite the database directly, so the hooks do not run. Rebuild the index after a merge or restore (`wp elasticpress sync`, `wp algolia reindex` and so on).
- Give staging a separate index from production (with the same index, staging edits mix into production search results). Switch the index name prefix in `wp-config.php` (the same way the URL is switched with `WP_HOME`).
- Even though `AS OF` can read a past state, the search index holds only the current state.

**Files are outside the database.** Plugin and theme code and uploaded media are out of scope for this database. The database is responsible for content and settings; code is handled with Git and media separately as mostly append-only. How to combine them is designed when productized.

## Validation method

The yardstick is the wordpress-develop PHPUnit test suite, always compared against a baseline taken on real MySQL 8.0. Only "tests that pass on the real one and fail on the fake" are investigated, not absolute counts.

### Environment

- **Fake MySQL**: listens on `0.0.0.0:3306` on the host. With logrus at Debug level, each received query is logged as a `Starting query` line with the connection ID.
- **Real MySQL**: `docker run -d --name realmysql -e MYSQL_ROOT_PASSWORD=root -e MYSQL_DATABASE=wordpress -p 3307:3306 mysql:8.0`
- **WordPress**: `wordpress:php8.3-apache` and `wordpress:cli` with Docker Compose. Set `WORDPRESS_DB_HOST=host.docker.internal:3306` and `host.docker.internal:host-gateway` in `extra_hosts`.
- **PHPUnit**: an image based on `php:8.3-cli` with mysqli, zip, gd (with freetype, jpeg, webp) and Composer. Without GD the tests do not start.
- **wp-tests-config.php**: switch the target with environment variables. Use a TCP host name, not `localhost`.

```php
define( 'DB_NAME', 'wordpress' );
define( 'DB_USER', 'root' );
define( 'DB_PASSWORD', getenv( 'WP_TESTS_DB_PASSWORD' ) ?: '' );
define( 'DB_HOST', getenv( 'WP_TESTS_DB_HOST' ) ?: 'host.docker.internal:3306' );
```

### Running and comparing

```bash
# Baseline (real MySQL)
wpphp env WP_TESTS_DB_HOST=host.docker.internal:3307 WP_TESTS_DB_PASSWORD=root \
  vendor/bin/phpunit --group option --log-junit real-option.xml
# Fake MySQL
wpphp vendor/bin/phpunit --group option --log-junit fake-option.xml
# Check the count (do not use --list-tests; it ignores --group)
grep -c '<testcase ' fake-option.xml
```

- Groups in order: option, meta, query, wpdb. The full suite is about 29,370 tests.
- The test suite drops every table in the database at startup. Do not point it at a database with important data.
- For a test that passes alone and fails in its group, suspect transactions or state carried over between tests.

### Validation tools

- `scripts/wpphp`: runs a command in wordpress-develop using the PHPUnit container (the `wpphp` above).
- `go run ./cmd/junitdiff real-option.xml heimdb-option.xml`: lists only the tests that pass on the real one and fail on HeimDB.
- `scripts/wordpress/docker-compose.yml`: WordPress and wp-cli connected to HeimDB (port 3306 on the host).
- `scripts/wp-login-check.sh`: checks that you can log in to the admin screen.
- `go run ./cmd/replay -log heimdb.log -conn 1,2,3 -mysql ... -heimdb ...`: takes a connection's queries in order from HeimDB's query log (`-debug`), runs them on real MySQL and on HeimDB, and reports queries whose results differ. It drops the tables in both databases, so point it at dedicated databases.
- `internal/backend/dml_test.go`: runs SQL scripts on HeimDB and compares with a golden file made on real MySQL 8.0 (`internal/testdata/dml_golden.txt`). Pass the MySQL target with `-update -mysql` to regenerate it.
- `scripts/compat/run.sh <port> <out.json>` and `go run ./cmd/compatdiff compat/baseline/mysql-8.0.json <out.json>`: the MySQL compatibility test suite (`compat/README.md`; stage 91). It runs statements through WordPress's connection (wpdb), records the returned values, and compares them with the record from real MySQL 8.0. It finishes in a few seconds, so run it before the full PHPUnit suite whenever the engine changes. Known differences are in `compat/known-differences.txt`.

## Working rules for Claude Code

The working rules for development with Claude Code are in [`CLAUDE.md`](../CLAUDE.md) at the top of the repository.
