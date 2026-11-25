# HeimDB procedures

You operate HeimDB entirely with SQL `CALL heimdb_*(...)` statements. You can call them from anything that connects to MySQL: the mysql client, wp-cli's `wp db query`, phpMyAdmin and so on.

- Only administrators (SUPER privilege: `root` or a user with `GRANT ALL ON *.*`) can call them. The staging site's user cannot. There are two exceptions.
  - The read-only [`heimdb_merge_excludes()`](#heimdb_merge_excludes) can also be called by the site's users and by users with branch-only privileges (EXECUTE privilege on that database, which `GRANT ALL ON <database>.*` includes).
  - With the startup flag `-branch-procedures`, a user with branch-only privileges can call diff, resolve, the change list and restore on their own branch ([Procedures branch users can call](#procedures-branch-users-can-call)).
- `heimdb_branch_diff`, `heimdb_branch_resolve`, `heimdb_changes`, `heimdb_restore_preview` and `heimdb_restore` act on the branch of the connected database when you pass `''` as the branch (`staging` when connected to `wordpress@staging`, `main` when connected to `wordpress`).
- Times are `'YYYY-MM-DD HH:MM:SS'` (fractional seconds allowed), read as a time in the session's `time_zone`. With an offset, as in `'2026-10-10 12:31:10.396594+00:00'`, the time is taken as is (HeimDB returns times in this form).
- Branch names use lowercase letters, digits, `_` and `-`. `main` is production.
- On a replica, procedures that change data (creating, deleting, resolving, merging, restoring) fail with `HeimDB: this server is a read-only replica`.
- A wrong number of arguments returns an error (the connection is not closed).

## Overview

| Procedure | What it does | Changes data | Branch users |
| --- | --- | --- | --- |
| [`heimdb_branch_create(name[, parent[, time]])`](#heimdb_branch_create) | Create a branch | ○ | |
| [`heimdb_branch_list()`](#heimdb_branch_list) | List branches | | |
| [`heimdb_branch_delete(name)`](#heimdb_branch_delete) | Delete a branch | ○ | |
| [`heimdb_branch_limit(name, size, time)`](#heimdb_branch_limit) | Limit a branch's size and statement execution time | ○ | |
| [`heimdb_branch_diff(name[, table[, offset, count]])`](#heimdb_branch_diff) | Show the diff against the parent | | With the flag |
| [`heimdb_branch_resolve(name, table, row, side)`](#heimdb_branch_resolve) | Record which side resolves a conflict | ○ | With the flag (named rows only) |
| [`heimdb_branch_merge(name[, 'dry_run'])`](#heimdb_branch_merge) | Merge into the parent (`'dry_run'` only checks) | ○ | |
| [`heimdb_branch_sync(name[, side][, 'dry_run'])`](#heimdb_branch_sync) | Bring the parent's changes into the branch | ○ | |
| [`heimdb_merge_exclude(pattern)`](#heimdb_merge_exclude) | Add tables to exclude from merges | ○ | |
| [`heimdb_merge_include(pattern)`](#heimdb_merge_include) | Stop excluding tables from merges (only when there are no branches) | ○ | |
| [`heimdb_merge_excludes()`](#heimdb_merge_excludes) | List the tables excluded from merges (the site's users can call it too) | | Yes |
| [`heimdb_changes(branch, time[, tables])`](#heimdb_changes) | Show the rows changed since a time | | With the flag |
| [`heimdb_restore_preview(branch, time[, tables])`](#heimdb_restore_preview) | Show what a restore would change | | With the flag |
| [`heimdb_restore(branch, time[, tables])`](#heimdb_restore) | Restore to a past time | ○ | With the flag |
| [`heimdb_status()`](#heimdb_status) | Status for operations | | |
| [`heimdb_backup([name[, key]])`](#heimdb_backup) | Take a backup while running (encrypted if a key is given) | | |
| [`heimdb_gc()`](#heimdb_gc) | Delete old versions now | | |
| [`heimdb_rebuild_indexes()`](#heimdb_rebuild_indexes) | Create entries for existing indexes on DECIMAL, SET, BIT and similar columns | ○ | |

"Branch users" says whether a user with [branch-only privileges](#branch-only-privileges) can call it. "With the flag" means they can call it only when the startup flag `-branch-procedures` is set, and only on the branch they are connected to.

It is not a procedure, but you can read data as of a past time with `AS OF` ([time travel](#as-of)).

## Branches

A branch is a writable copy of the DB that splits off from the parent's state at some point. Creating one is instant and copies no data (it holds only the rows you change). A branch appears as a database named `wordpress@<branch>`. Set WordPress's `DB_NAME` to that name and WordPress runs on the branch.

### heimdb_branch_create

```sql
CALL heimdb_branch_create('staging');                                -- from the current main
CALL heimdb_branch_create('child', 'staging');                       -- with a given parent
CALL heimdb_branch_create('test', 'main', '2026-10-08 09:00:00');    -- from a past time
```

Returns the created branch as one row (same columns as [`heimdb_branch_list`](#heimdb_branch_list)). You can create a branch from a past time only within the period old versions are kept (`-retention`, default 30 days).

Since 0.2.42, creating a branch takes the parent's write lock (stage 121). If a transaction is writing to the parent, or an index is being built (`CREATE INDEX`, an `ALTER TABLE` that rewrites the table, `heimdb_rebuild_indexes`, the rebuild after startup), it waits for that to finish, up to `-lock-wait-timeout` (then `1205`). It does not wait if the calling connection's transaction holds the parent's lock. Before that, the fork point could fall in the middle of an index build that commits in several parts, and if the index was then rebuilt on that branch, rows deleted on the branch could still be found through the index.

AUTO_INCREMENT values for rows added on a branch come from a range that does not overlap production. Merged rows enter production with those numbers, and production keeps numbering within its own range afterwards.

| Column type | Production (main) | Branch ID n (2 or more) |
|---|---|---|
| `bigint` (signed or unsigned) | 1 up to below 10^10 | a range of width 10^10 from (n - 1) × 10^10 (the first branch starts at `10000000000`) |
| `int` (signed or unsigned) | 1 up to below 10^9 | a range of width 10^7 from 10^9 + ((n - 2) mod 100) × 10^7 (the first branch starts at `1000000000`) |
| `mediumint` and smaller, temporary tables | not split | not split |

The `int` ranges reuse 100 slots. Numbering starts from the largest visible value in the slot + 1 (including merged rows of earlier branches that used the same slot), so numbers normally do not overlap. They overlap when two branches whose IDs differ by 100 add rows to the same table at the same time, when a branch is created from a time before the merge of the previous branch in the same slot, and for numbers of rows deleted on production after a merge (the branch 100 later may number them again). Overlapping rows, and rows added on both sides in types whose ranges are not split, become merge conflicts ("Tables that need care when merging branches" in `docs/plugins.md`).

`ALTER TABLE ... AUTO_INCREMENT = v` works within the range. A v below the range's lower bound is raised to the lower bound, and a v above the range (for example after loading a dump that contains another branch's numbers) is ignored (MySQL numbers from that value). When the range is used up, INSERT fails with `HeimDB: AUTO_INCREMENT range is exhausted: table <table>, values below <limit> on this branch` (for an `int` table on production, when the values below 10^9 are used up).

### heimdb_branch_list

```sql
CALL heimdb_branch_list();
```

```
+---------+--------+----------------------------+-------------------+------------+----------------+------------------+
| branch  | parent | forked_at                  | database          | size_bytes | max_size_bytes | max_execution_ms |
+---------+--------+----------------------------+-------------------+------------+----------------+------------------+
| main    | NULL   | NULL                       | wordpress         |          0 |           NULL |             NULL |
| staging | main   | 2026-10-10 21:30:46.566968 | wordpress@staging |          0 |     2147483648 |            30000 |
+---------+--------+----------------------------+-------------------+------------+----------------+------------------+
```

- `forked_at`: when the branch forked (in the session's `time_zone`).
- `size_bytes`: an estimate of the size of the data the branch holds. Data not yet flushed to disk is not counted, so it can be 0 right after creation.
- `max_size_bytes`, `max_execution_ms`: limits set with [`heimdb_branch_limit`](#heimdb_branch_limit) (`NULL` if none).

### heimdb_branch_delete

```sql
CALL heimdb_branch_delete('test');
```

Refused if the branch has child branches (`HeimDB: branch has child branches`). `main` cannot be deleted. Recorded resolutions are deleted with the branch.

### heimdb_branch_limit

```sql
CALL heimdb_branch_limit('staging', '2G', '30s');   -- size limit, and execution time limit per statement
CALL heimdb_branch_limit('staging', '0', '0');      -- remove (0 means no limit)
```

Keeps a test branch from using up the disk and time of the HeimDB it shares with production. Returns the configured branch as one row (same columns as [`heimdb_branch_list`](#heimdb_branch_list)). The size is a number with `K`, `M`, `G` or `T` (times 1024), or a number of bytes. The time takes `ms`, `s` or `m`. On `main` you can set only the execution time limit (a size limit is not allowed, so production is not stopped by mistake: `CALL heimdb_branch_limit('main', '0', '30s')`). The execution time limit on `main` applies only to `SELECT` and write statements (`INSERT`, `UPDATE`, `DELETE`, `REPLACE`) on non-administrator connections, not to DDL or `CALL` (so it does not stop a large `ALTER TABLE` in a plugin update). If a heavy WordPress cron write (such as deleting expired transients) exceeds the limit, it is stopped and retried every time, so make the limit well above what page requests need. Child branches do not inherit limits. Limits reach replicas too.

- **Size limit**: at the start of a write (before a statement writes its first row, and before DDL), if the same estimate as `size_bytes` (cached for about 1 second) is over the limit, the write is refused with error 1114 (`The table 'wp_posts' is full`, the same as MySQL when the disk or tablespace is full), and a warning is logged (`HeimDB: branch staging is over its size limit`, at most once a minute per branch). Reads, and `DROP TABLE` and `TRUNCATE` (which remove tables), go through. A statement that has started is not stopped, so the branch can go over by the data not yet flushed to disk (tens of MB) plus what one statement writes (an `ALTER` that adds a column to a large table rewrites the whole table). Use it together with the execution time limit. When a branch hits the limit, raise the limit or recreate the branch (deleting rows does not reduce the size: the deletion marks are writes too, and old versions stay until GC). Merges (from a child into this branch) and restores are not stopped.
- **Transaction size**: uncommitted writes are held in memory, so keep a transaction that writes to a branch with a size limit within that limit (at most 256MB). A statement that goes over is rolled back with error 1197 (the same as MySQL when `max_binlog_cache_size` is exceeded). Earlier writes in the transaction remain.
- **Execution time limit**: applies to every statement (reads, writes, DDL) of a user with privileges on this branch ([branch-only privileges](#branch-only-privileges)). It does not depend on the connected database (it applies when connecting without a database, and after `USE` of another database). If the user has privileges on several branches, the shorter limit applies. It does not apply to administrators (SUPER privilege: `root` or a user with `GRANT ALL ON *.*`); global privileges without SUPER, such as `GRANT SELECT ON *.*`, do not lift it. Without authentication (`-no-auth`), it applies to every statement except the `heimdb_*` procedures when the connected database is this branch. When the limit passes, the statement is stopped and error 3024 is returned (`Query execution was interrupted, maximum statement execution time exceeded`, the same as MySQL's `max_execution_time`). The stopped statement's writes do not remain (inside an explicit transaction, only that statement is rolled back). However, an autocommit statement stopped right after it finished committing may return error 3024 with its writes kept (MySQL does the same). The site's PHP cannot lift it with `SET` (the per-connection `max_execution_time` applies only to `SELECT` statements and cannot be longer than this limit; "Slow query log" in [`operations.md`](operations.md)).
- **Temporary tables**: temporary tables are stored outside the limited space, so a user with privileges on a branch with limits (without authentication, a connection to that branch) cannot create them (error 1235). WordPress does not use them.

Limit the number of connections with `MAX_USER_CONNECTIONS` on the user ([branch-only privileges](#branch-only-privileges)).

### Branch-only privileges

For a staging site, use a user with privileges on that branch only (if the site is compromised, the attacker cannot reach production).

```sql
CREATE USER 'staging'@'%' IDENTIFIED BY 'pw' WITH MAX_USER_CONNECTIONS 20;
GRANT ALL ON `wordpress@staging`.* TO 'staging'@'%';
```

`MAX_USER_CONNECTIONS` is the limit on the account's concurrent connections (0 means no limit). Connections over it are refused with error 1203, as in MySQL. You can change it later with `ALTER USER 'staging'@'%' WITH MAX_USER_CONNECTIONS 10` (without `IDENTIFIED`, the password does not change). The global `@@max_user_connections` has no effect (it would also apply to production's users).

### Procedures branch users can call

With the startup flag `-branch-procedures` (or the environment variable `HEIMDB_BRANCH_PROCEDURES=1`; if the flag is given, including `-branch-procedures=false`, the flag wins), users with branch-only privileges can call the following 5. This lets the WordPress admin screen (a plugin) show the diff and record conflict resolutions through `$wpdb` as it is. It is off by default; when off, only administrators can call them, as before.

| Procedure | What a branch user can do |
| --- | --- |
| [`heimdb_branch_diff`](#heimdb_branch_diff) | Show the diff of their own branch |
| [`heimdb_branch_resolve`](#heimdb_branch_resolve) | Record conflict resolutions on their own branch for named rows (`'*'` is for administrators only) |
| [`heimdb_changes`](#heimdb_changes), [`heimdb_restore_preview`](#heimdb_restore_preview) | Show their own branch's changes and what a restore would do |
| [`heimdb_restore`](#heimdb_restore) | Restore their own branch to a past time |

Merging, creating and deleting branches, changing limits, and changing the tables excluded from merges stay administrator-only (a limit is pointless if the branch user can remove it).

- **The target comes from the connection.** Only the branch of the connected database can be the target. Passing `''` as the branch is the recommended form (`CALL heimdb_branch_diff('')`). You may write the name, but if it differs from the connected branch, it is an error (`HeimDB: a branch user can call heimdb_branch_diff only on the branch it is connected to (connected to "wordpress@staging")`). Qualifying `CALL` with a database name cannot point to another branch.
- **main cannot be the target.** The flag does not let production's site user (privileges on `wordpress`) call them (`HeimDB: only an administrator can call heimdb_restore on the main branch`).
- **Times before the branch was created cannot be used.** Passing `heimdb_changes`, `heimdb_restore_preview` or `heimdb_restore` a time before the branch was created is an error (`HeimDB: a branch user cannot use a time before the branch was created (...)`). At such a time the branch is the parent's state, so it would show the parent's history. `SELECT ... AS OF` can still read times before the branch was created, as before (the parent's state before the branch was created; values changed on the parent after creation cannot be read).
- **The required privilege is EXECUTE on the connected database.** `GRANT ALL ON <database>.*` includes it. If you grant privileges one by one, add it.

  ```sql
  GRANT EXECUTE ON `wordpress@staging`.* TO 'staging'@'%';
  ```

- **Branch limits apply.** The execution time limit of [`heimdb_branch_limit`](#heimdb_branch_limit) also applies to procedures called by branch users (diff, resolve, the change list and restore check the limit as they read, and stop with error 3024 when it passes). Limit concurrent connections with `MAX_USER_CONNECTIONS`. Limits still do not apply to administrators (`root` and so on).
- **The diff reveals the parent's changes.** The diff summary and the per-row diff return the rows changed on the parent (production) after the fork, and the parent's values of conflicting columns (`conflict_values`). Branch users have no privileges on the parent, so in terms of privileges, information flows from the parent. This is not a problem for a site's production and its own test environment, but if you use branches to separate tenants, do not set the flag.

To split up conflict resolution, the branch user (the admin screen) records resolutions row by row, and an administrator checks `resolved_by` and `resolution` in the diff before merging ([heimdb_branch_resolve](#heimdb_branch_resolve)).

## Diff and merge

The usual flow is: look at the diff, record resolutions if there are conflicts, then merge. You can keep working on the same branch and merge as many times as you like (from the second time, the diff is against the previous merge).

```sql
CALL heimdb_branch_diff('staging');                                 -- summary
CALL heimdb_branch_diff('staging', 'wp_posts');                     -- per row
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=1', 'branch');
CALL heimdb_branch_merge('staging');
```

### Merge rules

- Rows are matched by primary key (`option_name` for `wp_options` only).
- For each column, three states are compared: the fork point (from the second time, the previous merge), the current branch, and the current parent. A column changed only on the branch is applied, and a column changed only on the parent is kept. If both sides changed the same column to different values, it is a conflict. Modification times such as `post_modified` are not conflicts; the newer one wins.
- If both sides separately added a post with the same natural key (`post_type` and `post_name`) or a term (`slug`), it is a conflict. But for posts with the same content, it is not a conflict: the parent's row is kept and the branch's row is skipped (`skip`, `same as the parent's ID=4`). Not compared: the primary key, and the posts' `guid`, `post_date`, `post_date_gmt`, `post_modified` and `post_modified_gmt` (URLs and creation times, which differ between the sides even with the same content). This is because on a block theme site, just opening pages on both sides after creating a branch makes WordPress create a default navigation (`wp_navigation`) on each side. The branch's meta and other rows attached to the skipped row are skipped too, as "rows that belong to a post that is not merged". If you edit that row on the branch after the merge, it goes back to a duplicate conflict. Terms stay conflicts even with the same content (skipping a block theme's `wp_theme` term would also drop the link between templates edited on the branch and the theme, and the templates would not work on production). Duplicated post meta is only a warning. (Since 0.2.16. Before that, posts with the same content were conflicts too.)
- Post meta, term relationships, term_taxonomy and term meta added on the branch are skipped if they point to a post or term that is not on the parent and is not created by this merge either (`belongs to a post that is not on the parent`). For example, when meta is added later to a post skipped by an earlier merge (or resolved with `'parent'`). This keeps orphan rows out of the parent (since 0.2.16).
- The following are not merged (`skip`).
  - Environment-specific options: `siteurl`, `home`, `blog_public`, `cron`, `rewrite_rules`, `db_version` and so on, `_transient_*`, `*_lock`, `https_*`
  - Auto-drafts, revisions, and post edit locks (`_edit_lock`, `_edit_last`)
  - Comments and users (`wp_comments`, `wp_commentmeta`, `wp_users`, `wp_usermeta`). Production's side is the correct one
- Added tables and changes to columns and indexes (a plugin's dbDelta) are merged too. Columns are compared one by one, and columns changed only on the branch are applied. Columns changed only on the parent (production) stay as they are, and in rows coming from the branch, columns that exist only on the parent get their default values. Only a table whose primary key columns differ between the sides becomes a whole-table conflict (`schema differs`). Dropped tables are not merged.
- A merge is one commit. If it fails, nothing is written. After a merge, the posts' comment counts and the terms' counts are recounted on production, and `rewrite_rules` is emptied (WordPress rebuilds it on the next request).
- If `db_version` differs between the sides, the merge is refused (the diff reports it in the first row of the summary). Run the WordPress database upgrade on both sides before merging.
- [Tables excluded from merges](#tables-excluded-from-merges) are left alone (rows, column definitions and indexes are not compared or written).
- The WordPress table prefix (`$table_prefix` in `wp-config.php`) is found from the parent's tables ([How core tables are identified](#how-core-tables-are-identified)). If two or more prefixes are found, diff and merge are refused (`ambiguous_prefix`).

### heimdb_branch_diff

Only reads; writes nothing. Without a table, it returns a summary per table.

```sql
CALL heimdb_branch_diff('staging');
```

```
+------------+---------+---------+---------+---------+---------+-----------+----------+----------+--------+
| table_name | status  | inserts | updates | deletes | skipped | conflicts | resolved | warnings | detail |
+------------+---------+---------+---------+---------+---------+-----------+----------+----------+--------+
| wp_options | changed |       0 |       1 |       0 |       1 |         0 |        0 |        0 | NULL   |
| wp_posts   | changed |       1 |       1 |       0 |       0 |         1 |        0 |        0 | NULL   |
+------------+---------+---------+---------+---------+---------+-----------+----------+----------+--------+
```

- `status`: `changed`, `new table` (created on the branch), `dropped on branch`, `dropped on parent`, `schema differs` (the primary key columns differ between the sides), `unchanged` (the merge changes nothing, but all rows are compared; see `detail` below. It is a notice about cost, so leave it out when checking whether there is anything to merge), `excluded` ([excluded from merges](#tables-excluded-from-merges); the merge does nothing), `not excluded` (a pattern matched, but it is a core table and is not excluded; that table's summary is in another row).
- `inserts`, `updates`, `deletes`: the number of operations the merge performs on the parent. In `excluded` rows, the number of rows added, changed and deleted on the branch side (not applied; adding a column changes the shape of every row, so all rows count as changed). `skipped` is rows not merged, `conflicts` is unresolved conflicts, and `resolved` is conflicts with a recorded resolution.
- `detail`: column and index changes (`columns: +w`), and the cost of the merge.
  - `rewrite N rows`: the merge rewrites the parent's table (columns were changed on the branch). N is the number of rows on the parent.
  - `full compare N rows`: one side rewrote the table (a column change or `TRUNCATE`), so all rows are compared, not only the changed ones. N is the number of rows compared. On the parent side, posts, terms and post meta (which are checked from the fork point for overlap with rows added on the parent) end up like this, and it costs that much every time until the branch is recreated.
  - Both affect how long writes to the parent wait during the merge. The diff does not take the parent's lock, so you can find out beforehand whether a merge will be heavy.

If `db_version` differs between the sides, the following row comes first, followed by the per-table summary (the merge is refused; a per-row diff for a given table is an error).

```
| * | db_version differs | 0 | 0 | 0 | 0 | 0 | 0 | 0 | branch 60717, parent 60421; run the WordPress database upgrade on both sides before merging |
```

With a table (`'*'` for all tables), it returns the decision for each row.

```sql
CALL heimdb_branch_diff('staging', '*');
```

```
+------------+----------------------+---------------+---------------+----------+------------+--------------+---------------------------------------------+
| table_name | id                   | branch_change | parent_change | action   | resolution | columns      | detail                                      |
+------------+----------------------+---------------+---------------+----------+------------+--------------+---------------------------------------------+
| wp_options | option_name=blogname | update        | none          | update   | NULL       | option_value | NULL                                        |
| wp_options | option_name=siteurl  | update        | none          | skip     | NULL       | NULL         | environment-specific option                 |
| wp_posts   | ID=1                 | update        | update        | conflict | NULL       | post_title   | the same columns were changed on both sides |
| wp_posts   | ID=2                 | update        | none          | update   | NULL       | post_content | NULL                                        |
| wp_posts   | ID=10000000000       | insert        | none          | insert   | NULL       | NULL         | NULL                                        |
+------------+----------------------+---------------+---------------+----------+------------+--------------+---------------------------------------------+
```

- `id`: the row's name. Pass it to [`heimdb_branch_resolve`](#heimdb_branch_resolve) in this form as is. Column and index changes are `COLUMN name` and `INDEX name`, and a mismatch in the table definition is `*`.
- `branch_change`, `parent_change`: the change on each side (`insert`, `update`, `delete`, `none`). These are changes to the row with the same primary key, but for duplicate posts or terms with the same natural key, `parent_change` is `insert`, pointing to the counterpart row added on the parent with a different primary key (since 0.2.16; before that, `none`).
- `action`: what the merge does: `insert`, `update`, `delete`, `skip`, `conflict`, `warning`. An excluded table is one row with `id` `*` and `excluded` (or `not excluded`) instead of per-row decisions.
- `resolution`: the recorded resolution (`branch`, `parent`). If the row changed after it was recorded, it is `stale`, and you decide again.
- `columns`: the columns to apply (`update`) or the columns that disagree (`conflict`).
- `detail`: the reason.
- `resolved_by`, `resolved_at`: the user who recorded the resolution (`resolution`), in the form `'staging'@'%'`, and the time (UTC). For rows matched by a wildcard resolution, the user and time that recorded the wildcard. NULL for resolutions recorded before 0.2.15 and for rows of tables whose primary key columns differ (`id` is `*`).
- `conflict_values`: for conflicting rows (`action` is `conflict`), the current values of both sides for the conflicting columns (JSON). NULL for other rows. Branch users cannot read values changed on the parent after the branch was created, so this lets the screen for choosing a resolution show both sides' values. Values are strings written the same way as in SELECT results (`DATE` is `2026-03-04`, `DECIMAL(19,4)` is `12.5000`, a zero datetime is `0000-00-00 00:00:00`), and `NULL` and the side with no row (the side that deleted it) are `null`. Keys are the column names in lowercase (`ID` is `id`). In a delete/update conflict, the columns changed by the updating side are shown. For duplicate posts or terms with the same natural key, all columns (including the primary key) that differ from the parent's counterpart row (the row with the primary key in `detail`) are shown.

  ```
  {"post_title":{"branch":"Hello from staging","parent":"Hello (edited on main)"}}
  ```

The 3 columns `resolved_by`, `resolved_at` and `conflict_values` were added at the end in 0.2.15. Code that reads columns by name (`$wpdb->get_results` and so on) needs no change, but code that reads them by count and order (Go's `rows.Scan` and so on) must read the 3 added columns.

For the per-row diff, passing an offset and a count returns only that range (because `CALL` cannot be wrapped in `LIMIT`).

```sql
CALL heimdb_branch_diff('', 'wp_postmeta', 0, 500);     -- first 500 rows
CALL heimdb_branch_diff('', 'wp_postmeta', 500, 500);   -- the rest
```

If there are more rows, one row with `table_name` and `id` `*` and `action` `more` is added at the end. Its `detail` holds the number of remaining rows and the next call (`3120 more rows; next: CALL heimdb_branch_diff('staging', 'wp_postmeta', 500, 500)`). The order is the same as without an offset (by table name, then by primary key within a table). The diff is computed again on every call, so only the receiving side (PHP) gets lighter; HeimDB's computation and memory do not change (the cost depends on the number of changed rows, not the table size). If the branch or the parent changes in between, page boundaries shift. The table cannot be left out (`'*'` can be used).

### heimdb_branch_resolve

```sql
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=1', 'branch');   -- take the branch side for this row
CALL heimdb_branch_resolve('staging', 'wp_posts', '*', 'parent');      -- all conflicts in this table go to the parent side
CALL heimdb_branch_resolve('staging', '*', '*', 'parent');             -- all conflicts go to the parent side
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=1', 'none');     -- cancel the record
```

The arguments are the branch, the table (`'*'` for all), the row (in the diff's `id` form; `'*'` for all conflicts in that table), and the side.

The last argument says which side's values to take for the conflicting row. In the diff example above, production changed the `post_title` of post ID=1 to `Hello (edited on main)`, and staging changed it to `Hello from staging`.

| Side | Meaning | Title after merging in this example |
| --- | --- | --- |
| `'branch'` | Take the branch's (staging's) value | `Hello from staging` |
| `'parent'` | Keep the parent's (production's) value | `Hello (edited on main)` |
| `'none'` | Cancel the recorded resolution and make it unresolved again | (cannot merge) |

Only the conflicting columns are decided. Columns changed only on staging (for example `post_content`) get staging's value even with `'parent'`.

- `branch`: conflicting columns also take the branch's value. In a delete/update conflict, the branch side wins (deleted, or kept). For duplicate posts or terms with the same natural key, the branch row's contents are copied onto production's existing row. It is refused for a different row with the same primary key, a post without an author, and a table whose primary key columns differ (`schema differs`). A wildcard `'branch'` does not match these rows, and they stay conflicts. Then the diff's `detail` and the merge error show `only 'parent' can resolve this: <reason>`.
- `parent`: conflicting columns keep the parent's value (columns that do not conflict are applied). For a table whose primary key columns differ (`schema differs`), the merge proceeds without that table. Column changes are merged column by column, so a table whose columns only changed in a plugin update is not left out.
- `none`: cancels the record.

A resolution for a named row takes priority over a wildcard. When naming a row, that row must be a conflict in the current diff (a bare number such as `'42'` is not found; use `'ID=42'`). Records are deleted when a merge succeeds and when the branch is deleted.

A record keeps the user who recorded it and the time, shown in the diff's `resolved_by` and `resolved_at`. Each time a resolution is recorded, one line is also logged (`HeimDB: 'staging'@'%' resolved wp_posts ID=1 on branch staging: branch`).

[Branch users](#procedures-branch-users-can-call) can record only named rows. A call with `'*'` for the table or the row (including deleting with `'none'`) fails with `HeimDB: a branch user must name a row; only an administrator can resolve with '*'`. A record for a named row remembers both sides' values of the conflicting columns at the time it was recorded, and is not used if either side's values change afterwards (`resolution` becomes `stale` and the merge is refused). An administrator's merge never goes through with values different from what the branch user saw.

### heimdb_branch_merge

```sql
CALL heimdb_branch_merge('staging');
```

```
+------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
| table_name | status | inserts | updates | deletes | skipped | conflicts | resolved | warnings | detail |
+------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
| wp_options | merged |       0 |       1 |       0 |       1 |         0 |        0 |        0 | NULL   |
| wp_posts   | merged |       1 |       2 |       0 |       0 |         0 |        1 |        0 | NULL   |
+------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
```

- `status`: `merged`; things not applied, such as `dropped on branch (not merged)`; `excluded` (an excluded table; the counts are the branch-side changes that were not applied); `not excluded` (a pattern matched, but it is a core table and was not excluded; `detail` shows the matching pattern). Tables with nothing to change are not shown (`excluded` and `not excluded` rows are shown even without changes).
- How to read the result: [Reading merge results from a plugin](#reading-merge-results-from-a-plugin).

If unresolved conflicts remain, it is refused without writing anything ([Reasons a merge is refused](#reasons-a-merge-is-refused)).

```
ERROR 51004 (HY000): HeimDB: merge refused (unresolved_conflicts): unresolved conflicts: wp_posts ID=1; resolve them with heimdb_branch_resolve
```

It cannot be called in the middle of a transaction (`COMMIT` or `ROLLBACK` first). Writes to the parent wait during the merge.

If the merge wrote anything, the last row holds a statement that undoes the merge.

```
| * | undo | 0 | 0 | 0 | 0 | 0 | 0 | 0 | CALL heimdb_restore('main', '2026-10-14 01:23:45.678901+00:00', 'wp_options,wp_posts') |
```

- The time is exactly the parent's state just before the merge (the merge takes the parent's lock before reading the diff, so the parent did not change after that point). It is in a form read as the same time in any `time_zone`.
- The tables are only those the merge wrote (rows, columns, indexes, created tables, recounted counts, the emptied `rewrite_rules`).
- It is [`heimdb_restore`](#heimdb_restore), so undoing also removes what production wrote to those tables after the merge. It cannot be undone after old versions are deleted ([`heimdb_gc`](#heimdb_gc)).
- Posts are restored but comments are not, so the posts' comment counts are recounted from the current comments (comments added on production after the merge remain).
- The branch's merge base also goes back to before the merge, so you can fix the branch and merge again. Recorded resolutions stay deleted by the merge, so resolve the conflicts again.

The result shows only the tables the merge changed (the `unchanged` rows of the diff summary are not shown).

Dry run: passing `'dry_run'` as the second argument goes through the same path as a real merge to the end (unresolved conflicts and a `db_version` difference give the same errors), then discards it without committing. Recorded resolutions and the merge base remain. The result is the same table rows as a real merge, plus a final `* | dry run` row. It holds the parent's lock as long as a real merge, so production's writes wait during that time.

```sql
CALL heimdb_branch_merge('staging', 'dry_run');
```

### heimdb_branch_sync

Brings the parent's (production's) changes into a branch. For example, after branches A and B have been merged into production, you bring production's latest into branch C, whose review takes longer, and then check it. The branch's changes under review (not yet merged) are kept.

```sql
CALL heimdb_branch_sync('c', 'dry_run');   -- check the counts and conflicts without writing
CALL heimdb_branch_sync('c');              -- refused without writing anything if there are conflicts
CALL heimdb_branch_sync('c', 'branch');    -- conflicting columns keep the branch's value
CALL heimdb_branch_sync('c', 'parent');    -- conflicting columns take the parent's value
CALL heimdb_branch_sync('c', 'parent', 'dry_run');
```

```
+-------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
| table_name  | status | inserts | updates | deletes | skipped | conflicts | resolved | warnings | detail |
+-------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
| wp_options  | synced |       0 |       1 |       0 |       1 |         0 |        0 |        0 | NULL   |
| wp_posts    | synced |       1 |       1 |       0 |       0 |         0 |        0 |        0 | NULL   |
| *           | undo   |       0 |       0 |       0 |       0 |         0 |        0 |        0 | CALL heimdb_restore('c', '2026-11-01 08:00:00.000000+00:00', 'wp_options,wp_posts') |
+-------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
```

- It compares the same three states as a merge, in the opposite direction. Columns changed only on the parent are written to the branch, and columns changed only on the branch are kept. The base is the time of the previous sync (if none, the previous merge; if neither, the fork).
- What is synced: all rows (including posts, meta, terms, auto-drafts and revisions) except the ones below. Tables created on the parent are created on the branch and their rows copied. The comment counts of the copied posts (from the branch's own comments) and the terms' counts are recounted on the branch side, and `rewrite_rules` is emptied.
- Comments and users (`wp_comments`, `wp_commentmeta`, `wp_users`, `wp_usermeta`) are not synced (up to 0.3.1 they were). The branch's rows stay as they are, and if the parent's rows changed since the previous sync (or the fork), the result has a `not synced` row (with counts of 0; an `ALTER TABLE` that rewrote the parent's table counts as a change). Multisite subsite tables (`wp_2_comments` and so on) are ordinary tables, so they are synced and merged. Merges do not apply these rows either. Differing definitions are refused (51010) as for other tables. To get them, recreate the branch (merge or give up its changes first). Until then, a copied post may have an author who is not on the branch, and comments of a post that the sync deleted stay on the branch. With WooCommerce, customers, reviews and order notes are in these tables, so they are not synced, while orders are. Only when the branch does not have the table at all (it was created before WordPress was installed) is the table copied.
- What is also not synced: environment-specific options (`siteurl`, `home`, transients, `cron` and so on, as in a merge); [tables excluded from merges](#tables-excluded-from-merges); new parent rows with the same natural key (posts: type and slug; terms: slug) as a post or term the branch added (the branch's row is kept and it is shown in `warnings`; meta and other rows hanging off that row are not copied either); rows (meta, term relationships) hanging off parent posts or terms that were not brought in because a conflict took the branch side; and tables dropped on the parent. Tables excluded from merges are shown in `excluded` rows (with counts of 0).
- If there are conflicts (rows where both sides changed the same column to different values, rows deleted on one side and changed on the other, rows added on both sides with the same ID but different contents), it is refused with 51004 without writing anything, and the conflicting rows are shown. The second argument, `'branch'` or `'parent'`, decides all conflicts at once for one side. Branch values kept with `'branch'` go into production at the next merge. A conflict where both sides added different rows with the same ID can only be decided with `'parent'` (`'branch'` also refuses with 51004, because keeping the branch's row would overwrite production's row at the next merge). To decide row by row, change that row on the branch to the parent's values, then sync. Records from [`heimdb_branch_resolve`](#heimdb_branch_resolve) are not used (after a sync, records made before it are no longer used).
- If there are tables whose definitions (columns, indexes, primary key) differ between the sides, it is refused with 51010 without writing anything. Run the same plugin update (dbDelta) or `ALTER TABLE` on the branch too, then sync.
- If `db_version` differs, it is refused (51005).
- Writes to the branch wait during the sync, but the parent (production) is not locked (it is read from a snapshot). Merges, syncs and restores of the same branch do not run at the same time. It cannot be called in the middle of a transaction.
- The last row is a statement that undoes the sync. The time is the branch's state just before the sync. Undoing also deletes the sync's record and moves the base back to before the sync (a merge after undoing behaves as if the sync never happened).
- Files (plugins, themes, uploads) are not synced.
- A table deleted on the branch before the previous merge is recreated by the sync if it exists on the parent (because the comparison uses the branch at the previous merge as the base). When removing a plugin, remove it on production too before syncing.
- The refusal message is `HeimDB: sync refused (<word>): <description>`, and the words and numbers are the same as in [Reasons a merge is refused](#reasons-a-merge-is-refused).

After a sync, [`heimdb_branch_diff`](#heimdb_branch_diff) and [`heimdb_branch_merge`](#heimdb_branch_merge) compare against the parent state the sync read, so copied rows are not counted as the branch's changes. If production is restored to before the sync after the sync, the sync's record remains (restoring production is treated as a change on production, and the copied rows are not put into production again).

### Tables excluded from merges

Tables you do not want in merges (indexes that can be rebuilt and so on) are excluded by a table name pattern. The merge does nothing to excluded tables. Production's (the parent's) definition and data are used as they are; the table is neither emptied nor replaced. Rows, column definitions and indexes are not compared, so the merge does not stop even if the branch went through a plugin update and the table definition changed on only one side. Tables created on the branch are not created on the parent, and tables dropped on the branch are not dropped. They are not in the merge's `undo` table list either (undoing does not touch them).

```sql
CALL heimdb_merge_exclude('wp_yoast_%');   -- add
CALL heimdb_merge_include('wp_yoast_%');   -- remove (only when there are no branches)
CALL heimdb_merge_excludes();              -- list
```

- A pattern is a table name where, as in `LIKE`, `%` matches zero or more characters and `_` matches any one character (there is no escape: `wp_yoast_%` matches `wp_yoast_indexable`, and `wp_posts` also matches `wpxposts`). It is case-insensitive.
- Tables that match a pattern but never existed on the branch side (tables created only on production) get no row in the result.
- There is one setting per server (one site), and it applies to merges of all branches. It is an unversioned setting written through commits, so it follows replicas and backups. It is empty on a newly created site.
- Patterns can be added at any time, but can be removed only when there are no branches other than `main` (see [`heimdb_merge_include`](#heimdb_merge_include)). Changes on a branch that were not merged before its table was excluded are no longer merged. When tables refer to each other, exclude them together. Renaming a table (`RENAME TABLE`) so that it no longer matches a pattern puts it back into merges in the same way as removing the pattern (old rows can come back to production): do not do it while branches exist.
- The steps for excluding Yoast SEO: "Tables to exclude from merges" in [`plugins.md`](plugins.md).

#### heimdb_merge_exclude

Adds a pattern and returns the list after adding. If the same pattern (including case differences) already exists, it does nothing (safe to run any number of times). Only administrators can call it.

If production's tables determine the prefix, a pattern that matches a [core table](#how-core-tables-are-identified) is an error. Before WordPress is installed (the prefix cannot be determined), it is accepted, and the merge does not exclude core tables (it shows `not excluded` rows in the result).

```
ERROR 51009 (HY000): HeimDB: merge refused (core_table): the pattern "wp_%" matches the WordPress core table wp_posts, which cannot be excluded from merges
```

#### heimdb_merge_include

Removes a pattern and returns the list after removing. If it does not exist, it does nothing. Only administrators can call it. It removes the matching pattern case-insensitively.

It is refused with 51011 (`branches_exist`) while there is any branch other than `main`, and nothing is changed (removing a pattern that does not exist still does nothing). A merge or a sync moves the branch's base forward without touching excluded tables, so if such a table were compared again, the merge could write old values over production's, or bring back rows deleted on production. Merge or give up the branches, delete them, remove the pattern, then create them again.

```
ERROR 51011 (HY000): HeimDB: merge refused (branches_exist): a pattern can be removed only when there are no branches; delete them first: staging
```

Up to 0.3.1, patterns could be removed at any time. If a pattern was removed after a merge or a sync of a branch that still exists, recreate that branch before merging it.

#### heimdb_merge_excludes

Returns the list (a `pattern` column, in the order added). Besides administrators, the site's users and users with branch-only privileges can call it (so plugins can show the status; they cannot change it).

### How core tables are identified

WordPress core tables cannot be excluded from merges (the merge rules, such as the relation between posts and meta and recounting counts, would break). Core tables are tables whose names match, case-insensitively and exactly, the prefix below followed by `posts`, `postmeta`, `options`, `comments`, `commentmeta`, `terms`, `term_taxonomy`, `term_relationships`, `termmeta`, `links`, `users` or `usermeta`. Only the end of the name is not enough, so `wp_foo_posts` is not a core table (it can be excluded).

The prefix is found from the parent's (production's) tables: a prefix for which both `<prefix>options` with an `option_name` column and `<prefix>posts` exist. If there is one such pair, that is the prefix; if none, `wp_`. Multisite subsite pairs (another pair's prefix + digits + `_`, such as `wp_2_options` and `wp_2_posts` for the prefix `wp_` that has the network table `<prefix>blogs`) are not counted. Subsite tables are merged as ordinary tables without the rules for posts and meta and so on, and are not treated as core tables either, so they can be excluded if a pattern matches. Without `wp_blogs`, `wp_2_` is counted as a second pair too (the case of an old installation's tables left on a site with a digits-only prefix). With two or more pairs (`wp_` and `wp_old_` with an old installation's tables left behind, leftovers of a plugin that makes a copy in the same database, such as WP Staging's `wpstg0_`, and so on), the prefix cannot be determined, so diff, merge and recording resolutions are refused with `ambiguous_prefix` (delete or rename the unused tables).

The following statement lists the pairs (it works on both HeimDB and MySQL). Multisite subsite pairs (a row's prefix + digits + `_`, where that row has `<prefix>blogs`) are listed too, so exclude those; if two or more remain, it is refused.

```sql
SELECT LEFT(c.table_name, CHAR_LENGTH(c.table_name) - 7) AS prefix
FROM information_schema.columns c
JOIN information_schema.tables t
  ON t.table_schema = c.table_schema
 AND t.table_name = CONCAT(LEFT(c.table_name, CHAR_LENGTH(c.table_name) - 7), 'posts')
WHERE c.table_schema = DATABASE() AND c.column_name = 'option_name' AND c.table_name LIKE '%options';
```

### Reasons a merge is refused

When a merge (including `dry_run`), a sync, a diff, recording a resolution, or adding or removing an exclusion is refused, a reason word and an error number for each word are given. The form is `HeimDB: merge refused (<word>): <description>` (the same form for diff and recording resolutions; for sync it is `HeimDB: sync refused (<word>): <description>`). The SQLSTATE is `HY000`. The numbers come from the range MySQL leaves free for third parties (50000 to 51999). With PHP's mysqli, you can branch on `errno` (`mysqli_errno()`, `mysqli_sql_exception::getCode()`).

**Words and numbers are not translated; they are only added to, never changed or removed** (plugins branch on them). The description text may change.

| Number | Word | Meaning |
| --- | --- | --- |
| 51001 | `branch_not_found` | The branch does not exist |
| 51002 | `no_parent` | Tried to merge a branch with no parent (`main`) |
| 51003 | `in_transaction` | This connection's transaction is writing to the parent (`COMMIT` or `ROLLBACK` first) |
| 51004 | `unresolved_conflicts` | There are unresolved conflicts (look with [`heimdb_branch_diff`](#heimdb_branch_diff) and resolve with [`heimdb_branch_resolve`](#heimdb_branch_resolve)) |
| 51005 | `db_version_differs` | `db_version` differs between the sides (run the WordPress database upgrade on both sides) |
| 51006 | `ambiguous_prefix` | There are two or more WordPress table prefixes ([How core tables are identified](#how-core-tables-are-identified)) |
| 51007 | `parent_changed` | A parent row changed during the merge (normally does not happen; merge again) |
| 51008 | `read_only_replica` | Tried to merge on a replica |
| 51009 | `core_table` | A [`heimdb_merge_exclude`](#heimdb_merge_exclude) pattern matches a core table |
| 51010 | `schema_differs` | There are tables whose definitions differ between the sides, so the sync does not proceed ([`heimdb_branch_sync`](#heimdb_branch_sync)) |
| 51011 | `branches_exist` | Tried to remove a pattern with [`heimdb_merge_include`](#heimdb_merge_include) while there are branches |

Besides these, 1041 is returned if a merge stops at the memory limit, and 1197 if a merge's writes are too large (`docs/operations.md`). Neither changes anything. Other errors (wrong arguments and so on) are 1105 and have no word.

### Reading merge results from a plugin

A merge is an operation inside the DB server, so HeimDB cannot call PHP hooks (something like `heimdb_after_merge`). A plugin calls the merge, reads the result and fires its hooks.

- Success: the result rows of [`heimdb_branch_merge`](#heimdb_branch_merge). Rows whose `table_name` is `*` are not tables (`undo`, `dry run`).
  - Changed tables: rows whose `status` is `merged` (the scope for cache purging).
  - Excluded tables: rows whose `status` is `excluded`. If the counts are not 0, the table changed on the branch (use this to decide whether a rebuild is needed).
  - Patterns that could not exclude: rows whose `status` is `not excluded`.
  - Number of resolved conflicts: the `resolved` column.
  - Statement to undo: the `detail` of the row whose `status` is `undo` (a call to [`heimdb_restore`](#heimdb_restore)).
- Failure: an error. Numbers and words: [Reasons a merge is refused](#reasons-a-merge-is-refused). Nothing was written.
- When undone (`undo`): in the result of [`heimdb_restore`](#heimdb_restore), the rows whose `action` is `restore rows` are the restored tables (columns `table_name`, `action`, `inserts`, `updates`, `deletes`, `detail`). Tables excluded from merges are not restored, so rebuild indexes that were rebuilt after the merge to match the restored contents. A `heimdb_restore` without a table list (or with `'*'`) restores excluded tables too, so use the table list of the `undo` statement as it is.
- Do not wait for slow work (rebuilding indexes and so on) inside the hook; hand it to a WordPress scheduled event or a job.

## Time travel and restore

HeimDB does not overwrite rows; it adds a version on every change. Old versions are kept for `-retention` (default 30 days), and any time within that period can be read.

### AS OF

```sql
SET time_zone = '+09:00';
-- read data as of a past time
SELECT ID, post_title FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;
-- bring back just one deleted post
INSERT INTO wp_posts SELECT * FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;
```

Tables with `AS OF` are read-only (writing is an error).

### heimdb_changes

Returns the rows changed from a time until now, in the order of when each row last changed. Writes nothing. Use it to find out what changed when the watch log ("Change watch" in [`operations.md`](operations.md)) alerts you, or when you suspect the site was compromised.

```sql
CALL heimdb_changes('main', '2026-10-12 03:00:00');                          -- all tables
CALL heimdb_changes('main', '2026-10-12 03:00:00', 'wp_users,wp_usermeta');   -- the given tables
```

```
+-------------+--------------------------+--------+-------------------------+----------------------------------+------------------------------------------------------+
| table_name  | id                       | change | columns                 | changed_at                       | detail                                               |
+-------------+--------------------------+--------+-------------------------+----------------------------------+------------------------------------------------------+
| wp_options  | option_name=default_role | update | option_value            | 2026-10-12 03:10:05.123456+00:00 | option default_role: "subscriber" -> "administrator" |
| wp_users    | ID=7                     | insert | NULL                    | 2026-10-12 03:10:09.551200+00:00 | user added: "evil" (ID 7)                            |
| wp_usermeta | umeta_id=88              | insert | NULL                    | 2026-10-12 03:10:09.551200+00:00 | administrator role given: user ID 7 (wp_capabilities) |
| wp_posts    | ID=12                    | update | post_content,post_title | 2026-10-12 03:11:40.000100+00:00 | NULL                                                 |
| *           | *                        | not shown | NULL                 | NULL                             | 41 rows that change routinely (transients, cron, edit locks) |
+-------------+--------------------------+--------+-------------------------+----------------------------------+------------------------------------------------------+
```

- `id`: the row's name (written the same way as in [`heimdb_branch_diff`](#heimdb_branch_diff); `option_name` for `wp_options`).
- `change`: the result of comparing the row at the given time with the current row (`insert`, `update`, `delete`). Rows changed and then changed back are not shown. `columns` is the columns changed in an `update`.
- `changed_at`: when the row last changed (UTC).
- `detail`: for a change the watch covers, the same description as in the watch log.
- Tables created or dropped after the time, and tables rewritten by a column change or `TRUNCATE`, are one row per table instead of per row (`table created`, `table dropped`, `table changed`).
- Rows that change about every minute (`_transient_*`, `_site_transient_*`, `cron`, posts' `_edit_lock`) are not shown; their number is in the last row.
- Values before and after are not shown. Read them with `AS OF` (`SELECT option_value FROM wp_options AS OF '2026-10-12 03:00:00' WHERE option_name = 'default_role'`).
- The time must be within the period old versions are kept (`-retention`). It works on any branch (pass the branch name).

### heimdb_restore_preview

Takes the same arguments as [`heimdb_restore`](#heimdb_restore) and returns what would be restored. Writes nothing.

```sql
CALL heimdb_restore_preview('main', '2026-10-10 21:31:09');
```

```
+------------+--------------+---------+---------+---------+--------+
| table_name | action       | inserts | updates | deletes | detail |
+------------+--------------+---------+---------+---------+--------+
| wp_options | restore rows |       0 |       1 |       0 | NULL   |
| wp_posts   | restore rows |       1 |       0 |       0 | NULL   |
+------------+--------------+---------+---------+---------+--------+
```

- `action`: `restore rows` (restore rows), `restore definition` (restore the table including its definition), `drop` (drop a table that did not exist at that time).
- `inserts`, `updates`, `deletes`: the number of operations performed to restore.
- `detail`: warnings (for example, when comments are not restored, comments remain on posts that the restore removes).

### heimdb_restore

```sql
CALL heimdb_restore('main', '2026-10-08 09:00:00');                         -- everything except comments and users
CALL heimdb_restore('main', '2026-10-08 09:00:00', 'wp_options,wp_posts');  -- the given tables (comma-separated)
CALL heimdb_restore('main', '2026-10-08 09:00:00', '*');                    -- everything
```

Restores a branch to its state at a past time. Without tables, comments and users (`wp_comments`, `wp_commentmeta`, `wp_users`, `wp_usermeta`) are left as they are now (so comments added after the time and users created after it are not lost). When only one of posts and comments is restored, the posts' comment counts are recounted.

Without tables, if the user tables that are not restored have, after the time, added users, changed passwords or email addresses, or an administrator role granted, the result has rows whose `action` is `not restored` (the same for `heimdb_restore_preview`; a restore also logs them as warnings). This is because administrators added later would remain even after restoring from a compromise. The counts are the number of those changed rows (not all rows changed in that table). To restore users too, restore with `'*'`, or delete those users in WordPress.

```
| wp_users    | not restored | 1 | 0 | 0 | user added: "evil" (ID 7); to restore users too: CALL heimdb_restore('main', '2026-10-12 03:00:00.000000+00:00', '*') |
```

When you give tables, give each of the following groups together. Restoring only one side makes rows and meta (taxonomy) disagree. A deleted post comes back without its featured image, custom fields and categories, and post meta created later remains. For posts that keep their content in meta (Elementor, ACF and so on), only the title and status come back, and the content stays as it is now.

| Row table | Restore together with |
|---|---|
| `wp_posts` | `wp_postmeta`, `wp_term_relationships` |
| `wp_comments` | `wp_commentmeta` |
| `wp_users` | `wp_usermeta` |

If you give only one side of a group, and both the row and the meta with the same ID changed after the time, the `detail` of the restored table's row lists those IDs (the first 5 and the number of the rest) for each group, and at the end gives one call that adds all the tables of the matching groups (the same for `heimdb_restore_preview`; a restore also logs them as warnings). The restore is not stopped. IDs where only one side changed (only the post title was fixed, and so on) are not shown, because restoring them causes no mismatch. Meta that WordPress adds and removes on every post edit or save (`_edit_lock`, `_edit_last`, and `_encloseme`, `_pingme` and `_trackbackme`, which are added when a published post is saved and removed by the cron ping) is not counted. If either table of a group was rewritten (`ALTER TABLE`, `TRUNCATE`), created or dropped after the time, rows are not compared and only that fact is shown.

```
| wp_posts | restore rows | 1 | 0 | 1 | posts 7, 8 also changed in wp_postmeta after the restore time, which is not restored; posts 7, 8 also changed in wp_term_relationships after the restore time, which is not restored; to restore them together: CALL heimdb_restore('main', '2026-10-18 06:33:19.158171+00:00', 'wp_posts,wp_postmeta,wp_term_relationships') |
```

Taxonomy counts (`count` in `wp_term_taxonomy`) are not recounted. Groups of plugin tables (WooCommerce orders and order meta and so on) and multisite subsite tables (`wp_2_posts` and so on) are not checked. The default restore without tables does not split groups. The check holds in memory the primary keys of changed rows in the tables not restored, so with a small memory limit (`-memory-limit`) and after most of such a table has changed, even a light restore of only posts may stop with error 1041 (nothing is changed; restore the group's tables together, or raise the limit).

A restore is one commit that writes past versions as new versions; history is not deleted. The last row of the result (`undo`) gives a call that cancels the restore (goes back to just before the restore).

```
+------------+--------------+---------+---------+---------+-----------------------------------------------------------------------------+
| table_name | action       | inserts | updates | deletes | detail                                                                      |
+------------+--------------+---------+---------+---------+-----------------------------------------------------------------------------+
| wp_posts   | restore rows |       1 |       0 |       0 | NULL                                                                        |
| *          | undo         |       0 |       0 |       0 | CALL heimdb_restore('main', '2026-10-10 12:31:10.396594+00:00', 'wp_posts') |
+------------+--------------+---------+---------+---------+-----------------------------------------------------------------------------+
```

A time in the future is an error. A time after the last commit does nothing. It cannot be called in the middle of a transaction.

For branches whose parent is the restored branch, the merge base goes back to one before the restored time (if none, the fork point). After undoing a merge, you can merge the same branch again (changes already in place do nothing, because both sides have the same values).

## Operations

### heimdb_status

```sql
CALL heimdb_status();
```

Returns rows of items (`name`) and values (`value`). Items with no value are `NULL`. Times are in UTC with an offset.

| Item | Contents |
| --- | --- |
| `disk_bytes` | Disk usage of the data |
| `branches` | Number of branches |
| `connections`, `running_statements` | Number of connections, and number of running statements (including this call) |
| `open_transactions`, `oldest_snapshot_age_seconds` | Number of open transactions, and the age in seconds of the oldest one. A transaction left open for a long time stops GC. Read-only transactions (logical dumps and so on) are included |
| `pinned_times`, `oldest_pinned_time_age_seconds` | Number of times protected from GC for restores and so on, and the oldest one |
| `gc_retention`, `gc_boundary`, `gc_boundary_age_seconds` | The period old versions are kept, and the GC boundary (times before it cannot be read) |
| `gc_last_run_at`, `gc_last_duration`, `gc_last_removed`, `gc_last_error` | The last GC |
| `memory_limit_bytes`, `memory_abort_bytes` | The memory limit (`-memory-limit`), and the point at which statements stop with error 1041 (75% of the limit). Both 0 without a limit |
| `memory_live_bytes` | The heap live at the last GC. When it exceeds `memory_abort_bytes`, statements start to be stopped. Monitor by comparing these two ("Memory limit" under "Operating notes" in [`operations.md`](operations.md)) |
| `memory_aborted_statements` | Number of statements stopped by the memory limit since startup |
| `cache_size_bytes`, `cache_used_bytes` | The block cache size and how much is in use now. In the official image (no cgo), it is included in `memory_live_bytes` |
| `idle_lock_timeout`, `idle_lock_disconnects` | How long a connection can hold the write lock while doing nothing before it is closed (`-idle-lock-timeout`), and how many were closed since startup |
| `write_lock <branch>`, `write_lock_waiters <branch>` | The write lock's holder (connection ID and user, or the name of an internal operation), how long it has been held, its state (running, or how long idle), and its last statement; the number of waiting statements ("When writes are stuck" in [`operations.md`](operations.md)) |
| `stats`, `stats_ready_at`, `stats_build_duration`, `stats_tables`, `stats_error` | Statistics of main's tables, built in the background after startup. `stats` is `building` (being built), `ready` (the one run at startup finished; it is `ready` even if some tables failed, and `stats_tables` shows the counts, such as `18 built, 0 failed`), or `failed` (the table list could not be read; the reason is in `stats_error`). The finish time and the duration are shown only when `ready` ("Right after a restart" in [`operations.md`](operations.md)) |
| `slow_query`, `slow_query_redact` | The slow query log threshold (`off`, or a duration such as `1s`; changed by `SET GLOBAL long_query_time` and `slow_query_log`), and whether values are redacted (`-slow-query-redact`) |
| `tls`, `tls_cert_not_after`, `connection_tls` | Connection encryption (`off`, `on`, `required`), the certificate's expiry, and whether this connection uses TLS |
| `watch_changes`, `watch_last_change`, `watch_last_change_at` | The number of changes found by the change watch since startup, and the last one ("Change watch" in [`operations.md`](operations.md)) |
| `role`, `last_commit` | `primary` or `replica`, and the time of the last commit |
| `pending_commit` | A commit waiting for a synchronous replica's acknowledgement (not yet visible) (production only; shown only while waiting) |
| `replication_*`, `replica <name>` | Replication status ("Replicas" in [`operations.md`](operations.md)) |

### heimdb_backup

```sql
CALL heimdb_backup('before-upgrade');   -- with a name
CALL heimdb_backup();                   -- the name is the time (20261010T123114.438826Z and so on)
CALL heimdb_backup('', 'verify');       -- one file encrypted to the recipient of -backup-key verify=...
```

```
+---------------------------------+----------------------------------+-------+
| path                            | backed_up_at                     | bytes |
+---------------------------------+----------------------------------+-------+
| /backups/before-upgrade         | 2026-10-10 12:31:10.414812+00:00 | 13156 |
+---------------------------------+----------------------------------+-------+
```

Creates a consistent backup while running, under the `-backup-dir` given at startup (without it, `HeimDB: backups are disabled`). It includes all branches, the history within the retention period, and users and privileges. Names use only letters, digits, `.`, `_` and `-` (it cannot write to an arbitrary path). If the same name exists, it is refused.

Start HeimDB with that directory as `-data` and it comes back to the state at `backed_up_at`. It contains password hashes, so it is readable only by the user that runs HeimDB.

If you give the name of a `-backup-key` key as the second argument, the whole store (including the WAL) becomes one file `<name>.tar.gz.age`, encrypted to that key's age recipient (`path` is that file and `bytes` its size). An unknown key name is an error. To open it, extract it with `heimdb restore-backup` ("Encrypted backups" in [`operations.md`](operations.md)).

### heimdb_gc

```sql
CALL heimdb_gc();
```

```
+----------------------------------+----------+--------+---------+---------------+
| boundary                         | versions | tables | indexes | merge_records |
+----------------------------------+----------+--------+---------+---------------+
| 2026-09-10 12:31:14.400206+00:00 |        0 |      0 |       0 |             0 |
+----------------------------------+----------+--------+---------+---------------+
```

Deletes versions older than the retention period (`-retention`) now, and returns the boundary and the numbers deleted. Normally the server runs it every `-gc-interval` (default 1 hour), so you do not need to call it. Times before the boundary can no longer be used for `AS OF`, restores, or creating branches. If there is an open transaction or a time in the middle of a restore, the boundary stops there. With `-retention 0`, old versions are not deleted, so it is an error.

### heimdb_rebuild_indexes

```sql
CALL heimdb_rebuild_indexes();
```

```
+---------------------------+----------------+
| table                     | index          |
+---------------------------+----------------+
| wp_wc_product_meta_lookup | stock_quantity |
| wp_wc_product_meta_lookup | min_max_price  |
+---------------------------+----------------+
```

Among the tables of the connected database (main for `wordpress`, that branch for `wordpress@staging`), rebuilds indexes that contain `DECIMAL`, `FLOAT`, `DOUBLE` or `ENUM` columns and were created by a HeimDB before stage 117 (0.2.38 or earlier), and indexes that contain `SET` or `BIT` columns and were created by a HeimDB before stage 120 (0.2.40 or earlier), and returns what it rebuilt. Such indexes have no entries, and every lookup reads the whole table (WooCommerce's `min_max_price` and `stock_quantity` in `wc_product_meta_lookup`, and so on). Indexes on `SET` columns are used for range lookups only with `=` and `IN`, as in MySQL (ordering comparisons such as `<` and `>` read the whole table). After the rebuild, they are used for range lookups like newly created indexes. Newly created tables and indexes, and indexes after an `ALTER TABLE` that rewrites the table (adding a column, changing a type and so on), have entries from the start (`ALTER TABLE ... FORCE` does nothing in HeimDB, so it does not rebuild).

- As with `CREATE INDEX`, writes to that branch wait during the rebuild (reads are not stopped). Large tables are committed in several parts, and if it stops halfway, the partly built index is removed at the next startup.
- For a unique index, if rows have duplicate values, it stops with error `1062` (indexes already rebuilt in earlier tables remain). Fix the duplicates and call it again to rebuild the rest. If there is nothing to rebuild, it does nothing (safe to call any number of times).
- Times before the rebuild (`AS OF`, restores) and branches created before the rebuild still read the whole table, as before. For a branch, connect to the branch's database and call it to rebuild on that branch.
- It cannot be called on a replica (call it on production and the created entries reach replicas too).
- Since 0.2.42, production rebuilds main's indexes one by one in the background after startup (`-rebuild-indexes`, on by default; "Rebuilding old indexes" in [`operations.md`](operations.md)). You call the procedure for branches, for indexes the background work skipped (duplicates in a unique index, indexes with many rows, indexes whose lock could not be taken), and when `-rebuild-indexes=false`. If the procedure and the background work run at the same time, whichever starts later skips what is already rebuilt. The procedure still stops at `1062`, as before (the background work skips and continues).
- At startup, if any branch has indexes that have not been rebuilt, a warning listing their names per database is logged (`HeimDB: N indexes on DECIMAL, FLOAT, DOUBLE, ENUM, SET or BIT columns were made by an older HeimDB (0.2.38 or earlier, or 0.2.40 or earlier for SET and BIT) and have no entries, ...`). The end of the message says that main's indexes are being rebuilt in the background when the background work runs (`main's are rebuilt in the background now`), that you should call the procedure, as before, when `-rebuild-indexes=false`, and on a replica that production rebuilds them and the entries arrive through replication.
