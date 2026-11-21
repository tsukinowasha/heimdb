# HeimDB

English | [日本語](README.ja.md)

A database for WordPress with branches and time travel. It speaks the MySQL protocol, and WordPress, wp-cli and mysqldump see it as MySQL 8.0, so you only change the database host.

- **Time travel**: read the data as of any past time with `AS OF`, and restore a site to that time (a restore can be undone too).
- **Branches**: create a staging copy of production instantly without copying data, review the diff, and merge it back. The merge knows WordPress's tables: comments and users that arrived on production in the meantime are kept, and the site URL is not overwritten.
- **Drop-in for MySQL**: WordPress, wp-cli and mysqldump connect as to MySQL 8.0. Move a site in with mysqldump and the `mysql` command, and back out to MySQL the same way.
- **One site per process**: a process serves one WordPress database (`-db`) and its branches (`wordpress@staging`), with its own data directory. Run one process per site.
- **Memory limit**: `-memory-limit` (by default 80% of the container's limit) caps the process. A statement that would pass it stops with error 1041 and its transaction is rolled back; the process keeps running.
- **Managed with SQL**: branches, merges, restores, backups and status are `CALL heimdb_*` procedures, usable from any MySQL client.
- **Prometheus metrics**: statement counts and times, connections, memory, disk, GC and replication at `/metrics`.
- **Online and encrypted backups**: a consistent backup of all branches and the history while running (writes wait while it is taken), optionally encrypted with [age](https://age-encryption.org/).
- **Replicas**: read-only replicas that apply production's commits, synchronous by default (like MySQL's semi-synchronous replication), with manual promotion.
- **Change watch**: logs a warning when rows that change in a site takeover change (a new administrator, `siteurl`, `default_role`, activated plugins, ...). Find the time and restore to just before it.
- **TLS and slow query log**: TLS for client connections and replication, and a slow query log that can be switched on while running.
- **Single binary**: a static Go binary with no cgo, and a Docker image for linux/amd64 and linux/arm64.

> **Development preview.** The data format is version 8 since 0.2.41. Data written by older versions (formats 1 to 7) is converted when HeimDB starts; if a future version changes the format, that version converts the data when it starts. Older versions cannot open upgraded data. When a release changes the format that way, its minor version goes up (0.3.x to 0.4.0); other releases raise the patch version. Take backups and pin a version (see "Upgrading HeimDB" in [`docs/operations.md`](docs/operations.md)). Differences from MySQL are in [`docs/differences.md`](docs/differences.md), the plan and history in [`docs/plan.md`](docs/plan.md).

HeimDB is built only for WordPress. Views, triggers, stored procedures, full-text search and character sets other than utf8mb4 are not supported and return explicit errors.

## Performance

On the same WordPress and data (5,002 posts, 200 pages, 20,000 comments, no object cache) on a GCE n2-standard-4, compared with MySQL 8.0 with Docker's default settings:

- A page's DB time is 1.5 to 2.1 times MySQL's, and the whole page 1.1 to 1.3 times (search is faster than MySQL). Adding a comment and updating an option are faster than MySQL.
- Under load, the maximum rate is 88% of MySQL's, with no errors or disconnects past the maximum or with 500 connections.
- Resident memory is about 500 MiB, almost independent of table size.

Speed is not a goal: public pages are expected to be served from a page cache. Setup, all values and how to measure: [`docs/bench.md`](docs/bench.md).

## Build and run

Requires Go 1.27 or newer.

```sh
go build -o heimdb ./cmd/heimdb
./heimdb -addr 0.0.0.0:3306 -data ./data -root-password 'secret' -site-password 'site-secret'
```

On first start, `-root-password` creates `root@'%'` (the administrator). With `-site-password`, HeimDB also creates `wordpress@'%'`, the user WordPress connects as, with privileges on the production database only (if it does not exist yet). The production data appears as the database named by `-db` (default `wordpress`).

A Docker image is also available ([`tsukinowa/heimdb`](https://hub.docker.com/r/tsukinowa/heimdb)). To run HeimDB together with WordPress, use [`compose.yaml`](compose.yaml) at the top of the repository:

```sh
export HEIMDB_ROOT_PASSWORD='secret' HEIMDB_SITE_PASSWORD='site-secret'
docker compose up -d                                   # WordPress at http://localhost:8080
docker compose run --rm cli wp core install --url=http://localhost:8080 --title=Site \
  --admin_user=admin --admin_password='...' --admin_email=you@example.com
```

Inside a container, the memory limit (`-memory-limit`) defaults to 80% of the container's limit; set it when running outside a container. Near the limit, a statement stops with error 1041 and its transaction is rolled back; the process does not crash.

Flags, Docker (starting a staging site and merging it), memory, replicas and TLS: [`docs/operations.md`](docs/operations.md).

## Using it from WordPress

Point `wp-config.php` at HeimDB:

```php
define( 'DB_NAME', 'wordpress' );          // 'wordpress@staging' for the staging branch
define( 'DB_USER', 'wordpress' );          // not root: a user with privileges on this database only
define( 'DB_PASSWORD', 'site-secret' );
define( 'DB_HOST', 'heimdb-host:3306' );
```

Do not connect WordPress as root: a hole in a plugin would otherwise reach HeimDB's procedures (restore, branch deletion), user management and the other branches. root is for the administrator's `CALL heimdb_*` and wp-cli.

Switch the staging site's URL with `WP_HOME` and `WP_SITEURL` in that site's `wp-config.php`.

A site running on MySQL can be moved with mysqldump (and moved back to MySQL the same way):

```sh
mysqldump --single-transaction --set-gtid-purged=OFF -h mysql-host -u root -p wordpress > site.sql
mysql -h heimdb-host -u root -p wordpress < site.sql
```

HeimDB keeps uncommitted writes in memory. `mariadb-dump` wraps each table in one transaction by default, so add `--skip-no-autocommit` for large tables.

Plugins that work, and those that partly do not (WooCommerce's full-text order search does not), are listed in [`docs/plugins.md`](docs/plugins.md).

## Usage

The administrator operates HeimDB with SQL `CALL`s. The procedures, their arguments and results: [`docs/procedures.md`](docs/procedures.md).

### Time travel

```sql
-- Read the data as of a past time (interpreted in the session's time_zone)
SELECT ID, post_title FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;

-- Bring back one deleted post
INSERT INTO wp_posts SELECT * FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;
```

### Restore

```sql
-- Rows changed since that time
CALL heimdb_changes('main', '2026-10-08 09:00:00');
-- How many rows a restore would change
CALL heimdb_restore_preview('main', '2026-10-08 09:00:00');
-- Restore to that time (comments and users are kept; '*' restores everything)
CALL heimdb_restore('main', '2026-10-08 09:00:00');
CALL heimdb_restore('main', '2026-10-08 09:00:00', 'wp_options,wp_posts');  -- comma-separated tables
```

A restore writes the past versions as new versions, so no history is lost. Restoring to the "time just before the restore" shown in its result undoes it.

### Branches

```sql
CALL heimdb_branch_create('staging');                 -- from main as it is now
CALL heimdb_branch_create('test', 'main', '2026-10-08 09:00:00');  -- from a past time
CALL heimdb_branch_list();
CALL heimdb_branch_delete('test');
```

A branch appears as a database named `wordpress@staging`. Connect the staging WordPress as a user with privileges on that branch only (granted with plain `GRANT`), so a compromised staging site cannot reach production:

```sql
CREATE USER 'staging'@'%' IDENTIFIED BY 'pw';
GRANT ALL ON `wordpress@staging`.* TO 'staging'@'%';
```

### Diff and merge

```sql
CALL heimdb_branch_diff('staging');                -- summary per table
CALL heimdb_branch_diff('staging', 'wp_posts');    -- verdict per row ('*' for all tables)
-- For rows where both sides changed a column to different values (conflicts), record which side wins
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=42', 'branch');   -- the row as in the diff's id column; 'branch' | 'parent' | 'none'
CALL heimdb_branch_merge('staging', 'dry_run');    -- check whether it merges and how many rows, without writing
CALL heimdb_branch_merge('staging');               -- apply to the parent (main) in one commit
```

Rows are matched by primary key (`option_name` for `wp_options`), and each column is compared three ways. Columns changed only on the branch are applied, and columns changed only on production are kept. If unresolved conflicts remain, the merge refuses without writing anything. Index and column definition changes (a plugin's dbDelta) are merged too. When a merge writes something, its last result row shows the `heimdb_restore` statement that undoes it. Tables you do not want merged, such as rebuildable indexes, can be excluded with `CALL heimdb_merge_exclude('wp_yoast_%')` (production's table is kept). A refused merge returns a reason word and an error number (51001 and up).

After another branch has been merged into production, you can bring production's latest changes into a branch under review (`CALL heimdb_branch_sync('staging')`). Columns changed only on the parent are written to the branch, and the branch's changes are kept. Conflicts are refused, or decided all at once with `'branch'` or `'parent'`. A later merge does not count the copied rows as the branch's changes. Details: "heimdb_branch_sync" in [`docs/procedures.md`](docs/procedures.md).

### Backups and status

```sql
CALL heimdb_backup('before-upgrade');   -- a consistent backup into -backup-dir while running
CALL heimdb_status();                   -- disk usage, GC, connections, open transactions, the risky-change watch, ...
CALL heimdb_gc();                       -- run the deletion of old versions now
CALL heimdb_rebuild_indexes();          -- give entries to indexes on DECIMAL, SET, BIT and other columns made by older versions (main does this in the background after starting)
```

A backup contains all branches, the history within the retention period, and users and privileges. Starting HeimDB with the backup directory as `-data` brings everything back to that time.

With `-metrics-addr 127.0.0.1:9104`, the same values plus statement counts and times are served in Prometheus format at `/metrics` (for New Relic, Grafana and others). It has no authentication, so do not expose it. Metrics and configuration examples: "Metrics (Prometheus)" in [`docs/operations.md`](docs/operations.md).

Read-only replicas, and promoting one when production fails: [`docs/operations.md`](docs/operations.md).

## Development

```sh
go test $(go list ./... | grep -v internal/sqlengine)        # unit tests and comparisons with real MySQL results (internal/testdata)
go test -race -timeout 30m $(go list ./... | grep -v internal/sqlengine)
go test ./internal/sqlengine/...                              # the upstream tests of the SQL engine (a fork of go-mysql-server)
go test ./internal/backend -run '^$' -fuzz '^FuzzQuery$' -fuzztime 1h -parallel 4   # SQL fuzzing (raw packets: FuzzPackets in internal/backend/fuzz_test.go)
```

- [`CONTRIBUTING.md`](CONTRIBUTING.md): reporting issues and sending pull requests (DCO sign-off). Security issues: [`SECURITY.md`](SECURITY.md)
- [`docs/differences.md`](docs/differences.md): differences from MySQL (compatibility test results, what is deliberately unsupported, smaller differences recorded so far)
- [`docs/procedures.md`](docs/procedures.md): the `heimdb_*` procedures
- [`docs/operations.md`](docs/operations.md): flags, Docker, memory, upgrading, replicas, TLS
- [`docs/plugins.md`](docs/plugins.md): which plugins work with HeimDB and how that is judged (research notes in [`docs/plugins-research.md`](docs/plugins-research.md))
- [`docs/plan.md`](docs/plan.md): plan, key design, a summary of each development stage, how compatibility is verified (comparing WordPress's PHPUnit results with a MySQL baseline)
- [`docs/bench.md`](docs/bench.md): performance and memory compared with MySQL
- [`docs/engine.md`](docs/engine.md): where the SQL engine (`internal/sqlengine`, a fork of go-mysql-server v0.20.0) comes from and what was changed
- [`compat/README.md`](compat/README.md): a MySQL compatibility suite run through WordPress's connection (wpdb), compared with recorded results from real MySQL in seconds
- `Dockerfile`: the Docker image. `compose.yaml`: HeimDB and WordPress together. `cmd/heimdb`: the server. `cmd/junitdiff`: compares PHPUnit results. `cmd/compatdiff`: compares compatibility suite records. `cmd/replay`: replays queries from a log
- `scripts/`: WordPress (`scripts/wordpress`), PHPUnit (`scripts/wpphp`), benchmarks (`scripts/bench`), checking plugins' SQL (`scripts/plugin-check`), a time-travel demo (`scripts/demo-asof.sh`)

## License

Copyright 2026 Tsukinowa Inc.

[Apache License 2.0](LICENSE) ([NOTICE](NOTICE)). The SQL engine (`internal/sqlengine`) is derived from go-mysql-server v0.20.0 (DoltHub, Inc., Apache License 2.0) and modified by Tsukinowa Inc. ([`docs/engine.md`](docs/engine.md)). Pebble is BSD 3-Clause; the other dependencies are Apache 2.0, BSD or MIT, except hashicorp/golang-lru, which is Mozilla Public License 2.0 (file-level copyleft, used unmodified). When distributing binaries, include the dependencies' license notices.

The MySQL compatibility suite (`compat/`) builds on WordPress's test framework and is licensed under [GPL-2.0-or-later](compat/LICENSE). It is not part of the Go module or the Docker image.
