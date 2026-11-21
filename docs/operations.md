# Operations

Details on the flags, Docker, replicas, TLS and metrics (Prometheus). For the basics, see the [README](../README.md); for the `heimdb_*` procedures, see [`procedures.md`](procedures.md).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `0.0.0.0:3306` | Address to listen on |
| `-data` | `data` | Data directory |
| `-db` | `wordpress` | Database name of production (the main branch) |
| `-root-password` | | Password of `root@'%'`, created on first start |
| `-site-user` | `wordpress` | Name of the user WordPress connects as |
| `-site-password` | | Password of the user WordPress connects as (or the environment variable `HEIMDB_SITE_PASSWORD`). If set, HeimDB creates that user with privileges on the `-db` database only (if it does not exist yet; an existing user is not changed) |
| `-retention` | `720h` (30 days) | How long old versions are kept for `AS OF` and restore (0 keeps them forever) |
| `-gc-interval` | `1h` | Interval for deleting old versions |
| `-backup-dir` | | Where `CALL heimdb_backup()` writes (empty disables it). Put it on the same filesystem and mount point as `-data`, or inside `-data` (for example `-data /data -backup-dir /data/backups`; the name of the directory right under `-data` must not contain `.` or be a name the store uses, such as `LOCK`). All writes wait while a backup is taken, and on another filesystem or mount point every file is copied (see "Operating notes" below) |
| `-backup-key` | | `name=path`. `CALL heimdb_backup('<backup name>', '<name>')` writes an encrypted backup to the age recipients (`age1...`, one per line) in the file at that path. The file is read on every backup (repeat the flag for several keys; requires `-backup-dir`; see "Encrypted backups" below) |
| `-reset-passwords` | `false` | On start, set the password of `root@'%'` to `-root-password` and, if the `-site-user` exists, its password to `-site-password` (other users are kept; or the environment variable `HEIMDB_RESET_PASSWORDS=1`). For opening a store restored from a backup; see "Encrypted backups" below |
| `-reset-users` | `false` | On start, delete every user other than `root@'%'` and, if `-site-password` is set, the site user (its privileges are kept and its password overwritten; it is created if missing), and overwrite the password of `root@'%'` (or the environment variable `HEIMDB_RESET_USERS=1`) |
| `-shutdown-timeout` | `30s` | On SIGTERM, how long to wait for running statements (after that, HeimDB exits without closing the data) |
| `-slow-query` | `0` | Log statements that take longer than this (0 disables it). This is the value at start; it can be changed without a restart with `SET GLOBAL long_query_time` and `slow_query_log` (see "Slow query log" below) |
| `-slow-query-redact` | `false` | Fold the values in statements in the slow query log into `?` |
| `-cache-size` | 1/8 of the limit (16 to 128 MiB) | Size of the block cache (like `innodb_buffer_pool_size`; bytes). If not set, 1/8 of the memory limit, kept between 16 MiB and 128 MiB (128 MiB if there is no limit) |
| `-memory-limit` | 80% of the container's limit | Memory limit of the process (`4G`, `512M` and so on; or the environment variable `HEIMDB_MEMORY_LIMIT`). Near it, a statement stops with error 1041 and its transaction is rolled back (the process does not crash). If not set, 80% of the container's memory limit (cgroup). `0` means no limit (see "Memory limit" below) |
| `-lock-wait-timeout` | `50s` | How long to wait for the write lock (like `innodb_lock_wait_timeout`) |
| `-idle-lock-timeout` | `30s` | Close a connection that holds the write lock longer than this while idle in a transaction (0 never closes it; see "When writes are stuck" below) |
| `-tls-cert`, `-tls-key` | | TLS certificate and key (PEM). HeimDB accepts TLS on MySQL connections and sends to replicas over HTTPS |
| `-require-tls` | `false` | Reject MySQL connections that do not use TLS (requires `-tls-cert`) |
| `-replication-addr` | | HTTP (HTTPS with `-tls-cert`) address for sending the log to replicas (empty sends nothing) |
| `-replication-token` | | Shared secret between production and replicas (or the environment variable `HEIMDB_REPLICATION_TOKEN`) |
| `-replication-log-retention` | `24h` | How long the commit log is kept for replicas (0 keeps nothing) |
| `-replication-sync-timeout` | `10s` | Before returning a commit, how long to wait for a replica to report that it applied it (after that, HeimDB switches to asynchronous; 0 does not wait) |
| `-replicate-from` | | Run as a read-only replica of this production (`host:port`, `https://host:port`) |
| `-replication-ca` | | CA (PEM) for checking production's certificate (with it, `-replicate-from` uses https; without it, the CAs the OS trusts) |
| `-replica-name` | Host name | Name of the replica shown in production's `heimdb_status` |
| `-metrics-addr` | | Serve metrics in the Prometheus format at `http://<address>/metrics` (empty does not open it. There is no authentication, so do not expose it; see "Metrics (Prometheus)" below) |
| `-debug` | `false` | Log every statement |
| `-no-auth` | `false` | Anyone can connect without authentication (for development) |
| `-rebuild-indexes` | `true` | After start, rebuild in the background, one at a time, the indexes on production (main) that have no entries because they were made by an old HeimDB (0.2.38 or earlier; 0.2.40 or earlier for `SET` and `BIT`) (or the environment variable `HEIMDB_REBUILD_INDEXES`; if the flag is given, the flag wins). Turn it off with `-rebuild-indexes=false`. Does not run on replicas. See "Rebuilding old indexes" below |
| `-branch-procedures` | `false` | Users with privileges on a branch only can call the diff, resolve, change list and restore of their own branch (or the environment variable `HEIMDB_BRANCH_PROCEDURES=1`; if the flag is given, the flag wins). The diff also returns values changed on the parent, so do not set this if you use branches to separate tenants ("Procedures branch users can call" in [`procedures.md`](procedures.md)) |

- On first start, HeimDB creates `root@'%'` (all privileges) with the password from `-root-password` (or the environment variable `HEIMDB_ROOT_PASSWORD`). If there are no users and neither is given, HeimDB does not start.
- HeimDB runs in UTC (the same as the default of MySQL on RDS and similar services).
- To stop it, send SIGTERM. HeimDB waits up to `-shutdown-timeout` for running statements to finish, then closes the data.

## Docker

The image is [`tsukinowa/heimdb`](https://hub.docker.com/r/tsukinowa/heimdb) on Docker Hub (linux/amd64, linux/arm64). Data is in `/data`, the port is 3306, and flags added after `docker run` are passed to HeimDB as they are. The license notices of the dependencies are in `/licenses` in the image.

```sh
docker run -d --name heimdb -p 3306:3306 -v heimdb-data:/data \
  -e HEIMDB_ROOT_PASSWORD='secret' -e HEIMDB_SITE_PASSWORD='site-secret' tsukinowa/heimdb:0.2.14 -slow-query 1s
```

HeimDB runs as nonroot (UID 65532) and does not change ownership on start (unlike the official MySQL image). Named volumes work as they are, but when you mount a host directory, run `chown -R 65532:65532 <directory>` first (otherwise HeimDB exits right away with `open store: open /data/LOCK: permission denied`). The same applies to `-backup-dir`.

### Running with WordPress

[`compose.yaml`](../compose.yaml) at the top of the repository starts HeimDB and WordPress together. HeimDB's port is not opened to the host (only the WordPress and wp-cli containers use it).

```sh
export HEIMDB_ROOT_PASSWORD='secret' HEIMDB_SITE_PASSWORD='site-secret'  # or write them in .env; it does not start without them
docker compose up -d                       # WordPress at http://localhost:8080 (change with WP_PORT)
docker compose run --rm cli wp core install --url=http://localhost:8080 --title=Site \
  --admin_user=admin --admin_password='...' --admin_email=you@example.com
```

WordPress connects as the user `wordpress`, which has privileges on the production database only (HeimDB creates it from `HEIMDB_SITE_PASSWORD`). Only wp-cli (`cli`) uses root (for `CALL heimdb_*` and for creating users).

Staging (a second WordPress connected to the branch `wordpress@staging`; http://localhost:8081, change with `STAGING_PORT`) connects as the user `staging`, which has privileges on that branch only (if staging is taken over, the attacker cannot reach production). Create the branch and the user before starting it. Without `STAGING_DB_PASSWORD`, staging does not start.

```sh
export STAGING_DB_PASSWORD='staging-secret'
docker compose run --rm cli wp db query "CALL heimdb_branch_create('staging')"
docker compose run --rm cli wp db query "CREATE USER 'staging'@'%' IDENTIFIED BY '$STAGING_DB_PASSWORD'; GRANT ALL ON \`wordpress@staging\`.* TO 'staging'@'%'"
docker compose --profile staging up -d staging
# After editing on staging, review the diff and merge into production
docker compose run --rm cli wp db query "CALL heimdb_branch_diff('staging')"
docker compose run --rm cli wp db query "CALL heimdb_branch_merge('staging')"
```

- The staging URL is switched with `WP_HOME` and `WP_SITEURL`.
- Files (languages, plugins, themes, uploads) live outside the DB, so staging uses a separate volume from production (updating a plugin on staging does not change production's files). The first time staging starts, `staging-files` copies production's files into that volume. Like the DB branch, the files start from a copy taken at the time of branching (without the copy, staging would be in English, and opening the admin screen would deactivate the plugins whose files are missing and switch the theme to the default one, and that would be merged into production).
- **Files are not merged**. Before merging, install on production the same plugins, themes and languages you installed or updated on staging (`wp plugin install` and so on). Anything installed on production after staging was created does not appear on staging (recreate staging).

To recreate staging (start again from production's current DB and files; the user `staging` keeps working):

```sh
docker compose --profile staging rm -sf staging staging-files
docker volume rm heimdb_wordpress-staging
docker compose run --rm cli wp db query "CALL heimdb_branch_delete('staging')"
docker compose run --rm cli wp db query "CALL heimdb_branch_create('staging')"
docker compose --profile staging up -d staging
```

- A block theme creates the navigation (a `wp_navigation` post) the first time the front page is shown. If the front page is shown for the first time on both production and staging after branching, both sides get a navigation with the same name, and the merge stops on a conflict. Look at the conflicting row with `heimdb_branch_diff('staging', 'wp_posts')`, resolve it with `CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=…', 'parent')` (keeps production's), then merge (for how to specify the resolution, see "heimdb_branch_resolve" in [`procedures.md`](procedures.md)).

### Operating notes

- **Pin a version**. Specify a version such as `0.2.14` rather than `latest`, and follow "Upgrading HeimDB" below when you upgrade.
- Stop with `docker stop` (SIGTERM). HeimDB waits for running statements up to `-shutdown-timeout` (30 seconds by default), so make `docker stop -t` or `stop_grace_period` longer than that (if shorter, the process is killed).
- To use `CALL heimdb_backup()`, pass a directory inside the data volume with `-backup-dir` (for example `-backup-dir /data/backups`). A separate volume (such as `-v heimdb-backups:/backups` and `-backup-dir /backups`, the example up to 0.3.0) is another mount point, so every backup copies all files (next item). To move backups off the volume, copy them from there afterwards (`docker cp`, or a container that mounts the same volume). The backups share files with the data, so the volume frees that space only when old backups are deleted. `restore-backup` needs an empty data directory, so to restore into the same volume, move `/data/backups` out first.
- **Backups are fast only on the same filesystem as `-data`**. There a backup hard-links the data files and takes milliseconds (13 to 19 ms for 16 MB and 2 GB of data under load; "During a backup" in [`bench.md`](bench.md)). Otherwise it copies every file while all writes wait (11.7 seconds for 2 GB on a GCE pd-balanced disk; up to 0.3.0, reads waited too once an `INSERT` was waiting). Hard links also fail across mount points of the same disk, so **with `/data` and `/backups` as two Docker volumes every backup is a full copy**; use `/data/backups` (previous item). The log says `heimdb_backup took ... and paused all writes meanwhile` when a backup took over a second. Until this is improved, keep the data small enough for the copy time to be acceptable, or take backups when writes are few. An encrypted backup (`-backup-key`) holds the writes only while it hard-links or copies the files, then spends seconds writing the encrypted file (5.4 seconds for 2 GB), which slows the pages running at the same time (read p99 from about 90 ms to 370 ms in that measurement).
- A `heimdb_backup` backup copies the whole database, so it contains the old versions that GC had not deleted when it was taken (about 30 days with the default `-retention`), the branches, and the users and privileges. If you start HeimDB on that directory, `AS OF`, `heimdb_restore` and creating a branch from a past time work over the same range as when the backup was taken (a mysqldump backup has only the current rows and no history). However, GC counts `-retention` from the current time, so if you start a backup older than `-retention`, the first GC (`-gc-interval`, 1 hour by default) deletes the versions before the last state (the last state is kept). To look at the history of an old backup, start it with `-retention 0` (deletes nothing).
- When you copy a backup to another place (another project, a test environment), encrypt it with a key (see "Encrypted backups" below). In a plaintext backup, both the table rows and the recent writes in the WAL (log) can be read as they are.
- **Tables need no maintenance** (accepted since 0.2.22). `OPTIMIZE TABLE`, `CHECK TABLE`, `REPAIR TABLE` and `ANALYZE TABLE` do nothing and return `OK` (`REPAIR` returns the same "not supported" `note` as InnoDB). GC deletes old versions, Pebble compacts the disk by itself, and Pebble checks for corruption with checksums when it reads. The optimization of WP-Optimize and Advanced Database Cleaner, `wp-admin/maint/repair.php` and `wp db optimize` do not fail, but change nothing. wp-cli's `wp db check` / `optimize` / `repair` use MariaDB's `mariadb-check`, which requires TLS by default, so if HeimDB runs without `-tls-cert`, add `--skip-ssl` (`wp db check --skip-ssl`).
- If the log shows `HeimDB: recovered from a panic in a statement` (with a stack trace), it is a bug in HeimDB. The statement fails with error 1105 (`HeimDB: internal error: ...`), its writes are rolled back, and the server keeps running. The connection also stays open, but if HeimDB had already started sending result rows (in the middle of a `SELECT` that returns many rows), the connection is closed, as with other errors. What cannot be rolled back: operations of a multi-operation `ALTER TABLE` that had already finished, and a panic after an autocommit statement finished its commit. Report it with the stack trace and the statements from around that time (from the slow query log and so on).
- **Memory guidance** (GCE n2-standard-4, 3.1 million rows shaped like `wp_postmeta`, 0.2.11; "Memory" in [`bench.md`](bench.md)). Resident memory (RSS after opening and reading every row) is about 500 MiB, of which the live heap is about 200 MiB (including the 128 MiB block cache and about 40 MiB for things such as the collation tables). Resident memory hardly depends on the number of rows in the tables. What grows with the number of rows is the following (peak RSS). With a limit, these stop with error 1041 before going over it.

  | Statement | 3.1 million rows, no limit | Limit 3,276 MiB |
  |---|---|---|
  | `ALTER TABLE ... ADD COLUMN` (independent of row count since 0.2.8) | 621 MiB | Succeeds |
  | `ORDER BY ... LIMIT` that cannot use an index | 976 MiB | Succeeds |
  | `GROUP BY` on a column whose values are all different | 1,755 MiB | Succeeds |
  | `UPDATE` of the whole table | 2,625 MiB | Succeeds |
  | `DELETE` of the whole table | 4,640 MiB | 1041 |
  | Loading 3.1 million rows in one transaction (the `mariadb-dump` default) | 5,643 MiB | 1041 (at `COMMIT`) |
  | Merging a branch that changed every row | 4,439 MiB (55 seconds) | 1041 |
  | Merging a branch that added a column | 5,477 MiB (81 seconds) | 1041 |
  | Restoring after every row was changed | 10,540 MiB (154 seconds) | 1041 |

  Up to 0.2.10, `UPDATE` used 4,042 MiB, `DELETE` 9,344 MiB, loading 11,192 MiB, merges 7,871 to 11,869 MiB (881 seconds for the one that added a column), and restore more than 12 GiB. Up to 0.2.7, `ALTER TABLE` and `CREATE INDEX` were also proportional to the number of rows, and `ADD COLUMN` on 3.11 million rows used 7.8 GiB. Up to 0.2.8, reads (a `SELECT` that cannot use an index, `COUNT(*)`, range searches) were also proportional.

  Write-heavy statements grow because of uncommitted writes and the copies made during commit. Uncommitted writes take about 0.5 KB per row in the shape of `wp_postmeta` (the row and its index entries; live heap). During commit, on top of that, HeimDB temporarily uses the commit batch (twice its contents, including the log sent to replicas) and the work for Pebble's write-ahead log (WAL), about 4 times the uncommitted writes in total. With a limit of 3,276 MiB, one transaction can hold up to about 1.5 million rows in the shape of `wp_postmeta` (on GCE, 1.5 million rows went in, and 2 million rows hit 1041 at `COMMIT`). Merges and restores write into their own transaction, so they grow the same way: within a limit L, the amount you can change at once is about a third of L in write size (with a limit of 3,276 MiB, a restore that reverts all 3.1 million rows hits 1041; restore tables or time ranges separately). If it stops, nothing is changed. When a restore that names tables reverts only one table of a pair, it also reads the primary keys of the changed rows in the table that is not reverted (`heimdb_restore` in `docs/procedures.md`).
- **Run with a limit**. Without a limit (`-memory-limit 0`), Go's GC lets memory grow to twice the live heap before collecting, so after a large statement RSS grows to nearly twice the live heap (a restore of 3.1 million rows, after a live heap of 6.7 GiB, went over the cgroup's 12 GiB and crashed). With a limit, GC runs before memory nears the limit, and statements stop with 1041.
- **Memory limit** (since 0.2.9). So that the remaining cases above do not use up memory and crash the process, near `-memory-limit` HeimDB stops the statements that are reading or writing rows at that moment with error 1041 (`HeimDB: out of memory: ...`) and rolls back their transactions (for an explicit transaction, the whole transaction is rolled back, not only the statement). It stops them when the live heap goes over 75% of the limit. Memory is counted for the whole process, so the statement that stops is not necessarily the one using the most memory (another statement running at the same time can stop. If an idle transaction holds a large change, other statements keep stopping until it ends). The memory the process uses (RSS) can exceed the limit by 10 to 20% because of memory outside the heap and batches that grow all at once. This is why the default is 80% of the container's limit. For the container's limit, HeimDB reads only its own cgroup's `memory.max` (with nested cgroups where only a parent has a limit, there is no limit, so set `-memory-limit`). When running outside a container, set `-memory-limit` (if you put several sites on one VM, make the total fit in the VM's memory).
- **Resident memory counted against the limit** (since 0.2.11). The live heap includes the memory used right after start (about 40 MiB, mostly the collation tables) and the block cache (the official image is built without cgo, so Pebble allocates the cache on the Go heap). The cache fills as data is read, and GC does not shrink it. Up to 0.2.10, if 75% of the limit was smaller than these two, every statement hit 1041 once the cache filled, and restarting did not help (with the default 128 MiB cache, limits of 256 MiB or less). Since 0.2.11, if `-cache-size` is not set, the cache is 1/8 of the limit, and with settings where the memory right after start + the cache + headroom for statements (32 MiB) exceeds 75% of the limit, HeimDB reports the limit it needs in an error and does not start. Resident memory hardly depends on the number of rows in the tables. To monitor, compare `memory_live_bytes` and `memory_abort_bytes` in `CALL heimdb_status()` (`heimdb_status()` reads no rows, so it works even while statements are being stopped).
- **Loading dumps**. HeimDB holds uncommitted writes in memory (about 0.5 KB per row in the shape of `wp_postmeta`, about 4 times that during commit), so loading a large table in one transaction uses memory in proportion to the number of rows. `mariadb-dump` by default wraps each table in `SET autocommit=0` and `COMMIT` (`--no-autocommit` is on by default), so take the dump with `--skip-no-autocommit` (3.11 million rows of `wp_postmeta` crashed at 7.7 GiB with the 0.2.9 defaults, and went in in 193 seconds with the option. With 0.2.11, the defaults use 5.6 GiB, and with a limit of 3,276 MiB it hits 1041 at `COMMIT`). MySQL's `mysqldump` does not wrap by default (it does not add `--no-autocommit`). With a limit, HeimDB stops with error 1041 instead of crashing.
- **Size limit of one transaction** (since 0.2.11). When uncommitted writes exceed 1,536 MiB, the statement is rolled back with error 1197 (`HeimDB: the transaction writes more than 1536 MiB ...`) (the transaction's earlier writes remain, and you can choose `COMMIT` or `ROLLBACK`). This is because Pebble cannot write a batch over 4 GiB, and the commit batch is twice its contents. For merges and restores the limit is 1,900 MiB; over it, they stop with 1197 without changing anything (restore tables separately). If the memory limit is small, 1041 comes before this size.
- Pass the TLS certificate and key by mounting the files and using `-tls-cert` and `-tls-key`. The process runs as nonroot (UID 65532), so give that user read permission.
- To rebuild the image, run `docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=0.2.14 -t tsukinowa/heimdb:0.2.14 --push .` at the top of the repository.

## Upgrading HeimDB

Versioning: from 0.3.0, the release that published the source (the code is the same as 0.2.45), the minor version goes up (0.3.x to 0.4.0) when the data format changes so that older versions can no longer open it (when backward compatibility is dropped); other releases raise the patch version. Even a version that changes the data format converts old data on start.

The data format is version 8 from 0.2.41 (for FLOAT(M,D), DOUBLE(M,D) and UNSIGNED types, and index entries for SET and BIT columns), version 7 in 0.2.39 and 0.2.40 (for index entries for DECIMAL, FLOAT, DOUBLE and ENUM columns), version 6 from 0.2.33 to 0.2.38 (for the record of syncing the parent's changes into a branch), version 5 from 0.2.20 to 0.2.32 (for tables without a primary key), version 4 in 0.2.19 (for primary keys containing strings, byte strings, dates and ENUM), version 3 from 0.2.11 to 0.2.18 (for writing the log of a large commit in 1 MiB pieces), version 2 from 0.1.6 to 0.2.10 (for single-column string primary keys), and version 1 from 0.1.0 to 0.1.5. A newer version reads data of an older version as it is, and on start only rewrites the version number (there is no conversion, so it finishes immediately). An older HeimDB refuses to start on data of a newer version (so it does not misread it). When the format changes, the new HeimDB converts data of the older version on start, and the release notes say so.

Without a replica:

1. Take `CALL heimdb_backup('before-upgrade')` (under `-backup-dir`).
2. Stop HeimDB (SIGTERM; `docker stop`) and start the new version with the same `-data`.
3. To go back, start the old version with the backup directory from step 1 as `-data` (for a version that changes the data format, the old version cannot open the upgraded data).

With a synchronous replica:

- Between versions with the same data format (a normal update): stop the replica, start it with the new version, wait for it to catch up (`replication_behind` is 0s and production's `replication_sync` is `sync`), then upgrade production the same way. While production is stopped, WordPress cannot write (to switch to the replica instead of upgrading production, follow the promotion steps in "Replicas"). Do not connect a replica of 0.2.7 or earlier to production of 0.2.8 or later (since 0.2.8, `ALTER TABLE` and `CREATE INDEX` on a large table are committed in several parts, with a mark that protects the data in progress from GC. An old replica does not know the mark, and if its own GC deletes the partly written data, it diverges from production).
- 0.2.11 uses data format version 3, but the format of the log sent to replicas (the version production and replica check with each other) stays at version 2. It connects with 0.2.10 and earlier, so, as with a normal update, upgrade the replica first. The upgraded replica's data is version 3, so to go back to 0.2.10 or earlier, rebuild it from a production backup.
- 0.2.19 uses data format version 4, but, as with 0.2.11, the format of the log sent to replicas stays at version 2. Upgrade the replica first. The upgraded replica's data is version 4, so to go back to 0.2.18 or earlier, rebuild it from a production backup. Production on 0.2.19 rejects replicas of 0.2.18 or earlier by the replication log level (next items) (replicas of 0.2.18 or earlier cannot open tables with the primary keys that can be created since 0.2.19, such as WooCommerce's `wc_product_attributes_lookup` and Wordfence's `wfls_role_counts`).
- 0.2.20 uses data format version 5, but, as with 0.2.19, the format of the log sent to replicas stays at version 2. Upgrade the replica first. The upgraded replica's data is version 5, so to go back to 0.2.19 or earlier, rebuild it from a production backup. Production on 0.2.20 rejects replicas of 0.2.19 or earlier by the replication log level (next items) (replicas of 0.2.19 or earlier cannot open tables without a primary key, which can be created since 0.2.20, such as the log tables of WP Cerber and Loginizer).
- 0.2.33 uses data format version 6, but, as with 0.2.20, the format of the log sent to replicas stays at version 2. Upgrade the replica first. The upgraded replica's data is version 6, so to go back to 0.2.32 or earlier, rebuild it from a production backup. Production rejects replicas of 0.2.32 or earlier by the replication log level (replicas of 0.2.32 or earlier do not know the record of syncing the parent's changes into a branch (`heimdb_branch_sync`), and would misread the merge base after being promoted to production).
- 0.2.39 uses data format version 7, but, as with 0.2.33, the format of the log sent to replicas stays at version 2. Upgrade the replica first. The upgraded replica's data is version 7, so to go back to 0.2.38 or earlier, rebuild it from a production backup. Production rejects replicas of 0.2.38 or earlier by the replication log level (replicas of 0.2.38 or earlier do not know the key level of indexes containing DECIMAL, FLOAT, DOUBLE and ENUM columns, and would not maintain those entries on writes after being promoted to production). After the upgrade, existing indexes on DECIMAL and similar columns still read every row, as before. Since 0.2.42, production rebuilds the ones on main in the background after start (see "Rebuilding old indexes" below). To create the entries while staying on 0.2.41 or earlier, call `CALL heimdb_rebuild_indexes()` on production (docs/procedures.md).
- 0.2.41 uses data format version 8, but, as with 0.2.39, the format of the log sent to replicas stays at version 2. Upgrade the replica first. The upgraded replica's data is version 8, so to go back to 0.2.40 or earlier, rebuild it from a production backup. Production rejects replicas of 0.2.40 or earlier by the replication log level (replicas of 0.2.39 and 0.2.40 read the FLOAT(M,D) and DOUBLE(M,D) types as types without digits, would not round on writes after being promoted to production, and would not maintain index entries containing SET and BIT columns). After the upgrade, FLOAT and DOUBLE columns of existing tables stay as types without digits (a column created earlier as `FLOAT(10,7)` is still `float`), and existing indexes on SET and BIT columns still read every row, as before. Since 0.2.42, production rebuilds the ones on main in the background after start (see "Rebuilding old indexes" below). To create the entries while staying on 0.2.41, call `CALL heimdb_rebuild_indexes()` on production (docs/procedures.md).
- Since 0.2.11, production rejects replicas that do not report a replication log level (0.2.10 or earlier, which cannot be told apart from 0.2.7 or earlier) and replicas whose level is too low (`HeimDB: the replica is too old for this primary` appears in production's `replica <name>` row, the replica's log and `replication_last_error`, and the replica stops without applying the log). Following the "replica first" order above avoids this. If you upgraded production first, upgrade the replica too and it catches up from where it left off.
- Versions that change the format of the log's contents (the format version of the log sent to replicas): upgrade **production first** (if you upgrade the replica first, the replica of the new version converts its own data to the new format, cannot connect to the old production, and cannot go back to the old version either). Production and replica check the format version with each other, and if it differs, the replica stops without applying a single log entry (`HeimDB: data format version differs (primary N, replica M)` appears in the log and in `replication_last_error` of `heimdb_status`, and also in production's `replica <name>` row), so between upgrading production and upgrading the replica, the replica does not catch up. HeimDB 0.1.5 and earlier does not report a version and is treated as version 1.
  - From 0.1.5 or earlier to 0.1.6 or later (version 1 to 2): there is no conversion of the contents, so after upgrading production, stop the replica and start it with the new version, and it catches up from where it left off (no need to rebuild it).
  - Versions that convert the contents (in the future): after upgrading production, rebuild the replica from a new production backup (the release notes will say so).

## Checking for vulnerabilities

`.github/workflows/vuln.yml` (stages 49 and 127) checks for known vulnerabilities on every push to main, on PRs and every Monday. It can also be run by hand (`gh workflow run vuln`).

- `govulncheck`: with the latest stable Go, checks for vulnerabilities in the functions HeimDB's code actually calls.
- `toolchain`: checks with the `toolchain` version in go.mod (the same Go that builds the image). Vulnerabilities in the standard library depend on the Go version used, so this fails when a Go fix release comes out. Raise the `toolchain` version in go.mod and the `golang:` version in `Dockerfile`, and release (Dependabot does not raise the `toolchain` in go.mod).
- `image` (weekly and by hand only): extracts the amd64 binary from the published `tsukinowa/heimdb:latest` and checks it with `govulncheck -mode binary`. It fails if the published image was built with an old Go or old dependencies. Fix it by releasing a new version. `Dockerfile` builds with the symbol table kept (with `-s`, govulncheck also reports vulnerabilities in functions that are never called).
- `advisories`: the go-mysql-server and vitess forked into `internal/sqlengine` are invisible to govulncheck and Dependabot (their module path is HeimDB's). This job looks up GitHub's Go advisories for the two modules and fails if there is one not in `.github/known-advisories.txt`. When it fails, read the PR that fixed the advisory and check whether it applies to the same path in the fork. If it does, fix it as a stage that brings in the upstream fix (recorded in the list of changes in `docs/engine.md`) and release. If it does not (a path not in v0.20.0, a path WordPress does not use and HeimDB rejects, and so on), add it to `known-advisories.txt` with the reason.

Locally, `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` runs the same check (with your local Go version).

## Change watch

When rows that change in a WordPress takeover change, HeimDB logs a warning (it does not block the change).

```
level=warning msg="HeimDB: watch: wordpress@172.18.0.4 on wordpress: option default_role: \"subscriber\" -> \"administrator\""
level=warning msg="HeimDB: watch: wordpress@172.18.0.4 on wordpress: administrator role given: user ID 7 (wp_capabilities)"
```

| What is watched | What is logged |
| --- | --- |
| The options `siteurl`, `home`, `admin_email`, `users_can_register`, `default_role`, `template`, `stylesheet` | The values before and after (truncated if long) |
| The option `active_plugins` | Plugins activated and plugins deactivated |
| The option `<prefix>user_roles` (role definitions) | Only that it changed |
| Users added and deleted | Login name and ID |
| Password changes | Login name and ID (the hash is not logged) |
| Email address changes | The addresses before and after |
| A user given the administrator role (`<prefix>capabilities`) | User ID |
| Multisite super admins (`site_admins`) | The lists before and after |
| Network-wide plugin activation on multisite (`active_sitewide_plugins`) | Plugins activated and plugins deactivated |

- `wordpress@172.18.0.4` at the start is the user who made the change; after `on` is the database (`wordpress@staging` for a branch). Changes that came in through a merge get `(merge of branch staging)`.
- Tables are recognized by the end of their name and their columns, so it works even if the prefix is not `wp_`, and also for multisite's `wp_2_options`.
- Only committed changes are logged (changes from rollbacks and failed statements are not). WordPress installation, settings changes in the admin screen and changes made with wp-cli are logged too.
- They can also be seen in `watch_changes` (count since start), `watch_last_change` and `watch_last_change_at` of `CALL heimdb_status()` (they go back to 0 on restart).
- Rows reverted by a restore (`heimdb_restore`) are not logged, and nothing is logged on replicas. `siteurl`, `home` and users are not brought into production by the merge rules, so merges do not log them either. Table-level operations (`TRUNCATE`, `DROP TABLE`, `RENAME TABLE`) are not watched.
- Things that also appear in normal operation: WordPress installation, customer sign-ups on sites with registration open and on online shops (`user added`), each user's first login after upgrading to WordPress 6.8 or later (the password hash is rehashed, so `password changed`), and `wp search-replace` or `wp db import` (logged in bulk).

### If you see a line you don't recognize

1. Look at the rows changed since a little before that time: `CALL heimdb_changes('main', '2026-10-12 03:00:00')`. Check the rows whose `detail` has a watch description, and the rows changed around the same time (posts, plugin tables).
2. Look at what would be reverted: `CALL heimdb_restore_preview('main', '2026-10-12 03:00:00')`. By default users are not reverted, so administrators added later and similar appear in the `not restored` rows.
3. Revert: `CALL heimdb_restore('main', '2026-10-12 03:00:00')` (`'*'` to revert users too; users and comments registered after that time are also removed). The call in the last row of the result undoes the restore. When you restore by naming tables, name the row tables and their meta tables together, such as posts with `wp_postmeta` and `wp_term_relationships` (if you name only one, the result's `detail` shows a call that reverts both; `heimdb_restore` in `docs/procedures.md`).
4. On the WordPress side too, change the administrators' passwords, delete the files of unknown plugins and so on (files outside the database are not reverted).

## Encrypted backups

A way to open a backup in an environment with different privileges from production (a test environment and so on) (since 0.2.28). A backup contains all branches and history, and also the DB users and their password hashes (`mysql_native_password`, which is weak against brute force).

**Keys**: create an [age](https://age-encryption.org/) key pair on the side that opens the backup (`age-keygen -o verify.key`). Only the opening side has the private key file; give production only the public key (one line, `age1...`). Production's HeimDB holds only the public key, so even if the production machine is taken over, past backups cannot be decrypted. If you write several public keys on separate lines in one file, the backup can be decrypted with any of the private keys.

```sh
./heimdb -data ./data -backup-dir ./backups -backup-key verify=/run/secrets/verify-recipients
```

```sql
CALL heimdb_backup('', 'verify');   -- the name is the time (20261030T082028.000000Z.tar.gz.age)
```

- The backup is a single file `<name>.tar.gz.age` (0600): the whole store (including the WAL) as a tar.gz, encrypted. While writing, it goes to `<name>.tar.gz.age.partial`, and is renamed when done. The copy before encryption (`.<name>.ckpt`) is removed even on failure or `KILL QUERY`. On start, HeimDB removes partly written files left by the previous run.
- The public key file is read on every backup, so replacing it needs no restart. On every backup, the key name and recipients are logged (`HeimDB: heimdb_backup <name>: encrypted with key verify to age1...`). If the contents changed since the last read, a warning shows the before and after side by side (`HeimDB: recipients of backup key verify changed since ...`). If this appears when no replacement was planned, it is a sign the file was rewritten.
- Only the key name appears in SQL statements. The only recipients accepted are age X25519 public keys (passphrases and SSH keys are not accepted, so that no secret is kept on the server).
- `heimdb_backup` without a key creates a directory that can be opened as it is, as before.

**Opening**: without starting the server, extract into an empty data directory, then start.

```sh
heimdb -data ./restored restore-backup -in 20261030T082028.000000Z.tar.gz.age -identity verify.key
# Docker: the ENTRYPOINT passes -data /data, so continue after it
docker run --rm -v verify-data:/data -v /path/to/backups:/backups:ro -v /path/to/verify.key:/run/secrets/verify.key:ro \
  tsukinowa/heimdb:<version> restore-backup -in /backups/20261030T082028.000000Z.tar.gz.age -identity /run/secrets/verify.key
```

- It first extracts into a temporary directory inside the data directory, and moves the files only after checking that decryption completed to the end and that the store files are there. If the key is wrong or missing, the file is corrupt or cut off, or the data directory is not empty, it leaves nothing behind and exits with a non-zero code.
- The format is standard age, so `age -d -i verify.key <file> | tar -xz -C <directory>` also opens it (on failure, the partly extracted files remain, so `restore-backup` is safer).
- The extracted store has production's users, so start it with `-reset-passwords` (overwrites the passwords of `root@'%'` and `-site-user` with `-root-password` and `-site-password`; other users and privileges are kept) or `-reset-users` (deletes all users and roles other than `root@'%'` and, if `-site-password` is set, the site user; overwrites the passwords of `root@'%'` and the site user, keeps the site user's privileges, and creates them if missing). `-reset-passwords` leaves root and site users on hosts other than `'%'` (such as `root@'localhost'`) with production's passwords, so if there are any, a warning is logged (depending on where a client connects from, it may match those users; delete them or use `-reset-users`). If no site password is given, nothing is done to the site user. In a store without users, both only create users, and if the values are already the same they write nothing (leaving the flag on does not rewrite them on every start). One line is logged on every start. They cannot be used on replicas or with `-no-auth`. Used on production, they are copied to replicas, like `ALTER USER`. They do not touch WordPress users (`wp_users`).
- The running store itself (the data directory) is not encrypted. Protect it with encryption of the disk it is on.

## Replicas

You can run read-only replicas that receive production's commits in order and apply them (branches, `AS OF` history, and users and privileges are the same too). If a machine fails, promote a replica and continue. Failover is manual.

```sh
# Production: send the log to replicas (HTTP; to encrypt it, see "TLS" below)
export HEIMDB_REPLICATION_TOKEN='long-random-secret'
./heimdb -data ./data -backup-dir ./backups -replication-addr 10.0.0.1:3310
# Take a backup on production (CALL heimdb_backup('replica')) and copy that directory to the replica machine
# (for an encrypted backup, extract it with restore-backup on the replica machine; see "Encrypted backups" above)
./heimdb -data ./backups/replica -replicate-from 10.0.0.1:3310    # replica
```

- The replica catches up from the time of the backup using production's log. If stopped and restarted, it continues from where it left off. Writes return an error.
- Production keeps the log for `-replication-log-retention` (24 hours by default). If the time between the backup and starting the replica, or the time the replica was stopped, is longer than that, the replica cannot catch up, so rebuild it from a backup (the replica reports the error in its log and `heimdb_status` and stops).
- GC, which deletes old versions, is not replicated. A replica runs GC with its own `-retention` and its own clock (so that production's GC does not delete versions a replica is in the middle of reading). The replica's `AS OF` and the range you can restore after promotion depend on the replica's `-retention`, so give the replica the same value as production (or longer). `-replication-log-retention` is how long the log for a replica to catch up is kept, and has nothing to do with this range.
- To see how far a replica has caught up, look at `replication_position` and `replication_behind` (the gap from production's last commit) in the replica's `CALL heimdb_status()`, and the `replica <name>` row on production.
- Synchronous (like MySQL's semi-synchronous replication): after writing a commit to disk, production returns OK only after it receives a report that a replica applied it and made it durable on disk (until then, other connections cannot see it either). If no report comes within `-replication-sync-timeout` (10 seconds by default), production returns OK and switches to asynchronous, and switches back to synchronous when the replica catches up (both are logged). On start it is asynchronous, and becomes synchronous when the replica catches up. The current state is `replication_sync` (`sync`, `async`, `off`) in production's `heimdb_status`. Every write waits for a round trip to the replica and for the disk to be durable, so commits get slower (about 5 ms each with a replica on the same machine; adding a comment takes about 1.7 times as long). If you do not need synchronous replication, set `-replication-sync-timeout 0`. If the replica always applies slower than the timeout, each time it catches up and switches back to synchronous, the next write stalls for the timeout and switches to asynchronous (same as MySQL). In that case, review the replica's machine or stop using synchronous replication.

Promotion (when switching production, or when production fails):

1. Stop production (if it failed and stopped, leave it).
2. Check that every commit that was returned OK is on the replica.
   - When production could be stopped: if the last `replication:` line in production's log is `last commit ... was acknowledged by replica <name>`, that replica has everything up to the last commit. When stopping, production keeps waiting past the timeout while the replica's reports are advancing. If it is `was NOT acknowledged`, restart production, let the replica catch up (`replication_behind` is 0s), and stop it again. If the log has no `last commit` line (it exited with `statements still running` because running statements did not finish), decide as in the next case, "when the production machine fails".
   - When the production machine fails: if `replication_primary_sync` in the replica's `heimdb_status` (production's sync state at the last response received) is `sync`, and production stopped well before the time in `replication_last_contact` plus `-replication-sync-timeout` (allow a few seconds of margin for clock skew between machines), every commit that was returned OK is on the replica (for production to switch to asynchronous, a period with no reports must last as long as the timeout. `replication_last_contact` is a time before which, it can be said, production was waiting for reports only for commits it started waiting on after that time; it is a little earlier than the time of receipt). If it is `async`, commits made while asynchronous may not have arrived.
   - With several replicas, promote the replica whose `replication_position` is furthest ahead, and make the decision above on that replica (production returns OK on a report from any one replica, so a lagging replica may also see production as `sync`).
3. Stop the replica, remove `-replicate-from`, and restart it on the same data. It becomes the new production (it keeps its own log too, so it can serve as the source for the next replica as it is). The history (the range of `AS OF` and restore) is kept as it is. If indexes without entries made by an old HeimDB remain, they are rebuilt in the background after it starts as production (see "Rebuilding old indexes" below). Do not remove `-retention` (if you forget it, it becomes the default 30 days). Commit times after promotion are always later than the last commit before it (they do not go backwards even if the new machine's clock is behind).
4. Point WordPress's `DB_HOST` at the new production. Do not reconnect the old production as a replica; rebuild it from a backup of the new production (it may have commits that only it has, which were waiting for reports. If connected, it stops because its position is not in the new production's log).

You may also restart a failed production as production, without switching. Commits that were waiting for reports (OK had not been returned) become visible after the restart and are also sent to the replica (same as MySQL).

## TLS

With `-tls-cert` and `-tls-key`, HeimDB accepts TLS on MySQL connections (when the client asks for it) and sends to replicas (`-replication-addr`) over HTTPS. Adding `-require-tls` rejects connections without TLS before checking the password (error 3159). TLS 1.2 or later. To replace the certificate, restart.

When running without `-tls-cert`, do not make clients require TLS. Clients that require TLS (`ssl` in Node.js mysql2, `--ssl-mode=REQUIRED` in the mysql client and so on) cannot connect (mysql2 reports `Server does not support secure connection`, and the mysql client `SSL connection error: SSL is required but the server doesn't support it`).

An example of making a certificate with your own CA (ECDSA P-256; the handshake is faster than with RSA). Put in `subjectAltName` every name and IP that clients and replicas use to connect.

```sh
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 \
  -subj "/CN=HeimDB CA" -keyout ca-key.pem -out ca.pem
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=heimdb" \
  -keyout server-key.pem -out server.csr
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -days 825 -out server.pem \
  -extfile <(printf "subjectAltName=DNS:db.example.internal,IP:10.0.0.1\nextendedKeyUsage=serverAuth")

./heimdb -data ./data -tls-cert server.pem -tls-key server-key.pem -require-tls
```

- WordPress: add `define( 'MYSQL_CLIENT_FLAGS', MYSQLI_CLIENT_SSL );` to `wp-config.php` (with `-require-tls` and without this, WordPress shows "Error establishing a database connection"). WordPress has no way to pass a CA, so **the connection is encrypted but HeimDB's certificate is not verified** (PHP's mysqlnd does not verify without a CA). This prevents eavesdropping but not a man-in-the-middle impersonating the server, so keep WordPress and HeimDB inside a private network or a VPN.
- The mysql command and mysqldump (MySQL 8): encrypt by default (`--ssl-mode=PREFERRED`). To verify the certificate, use `--ssl-mode=VERIFY_IDENTITY --ssl-ca=ca.pem`.
- Replicas: `-replicate-from https://10.0.0.1:3310 -replication-ca ca.pem` (with `-replication-ca`, `https://` can be omitted). If the certificate cannot be verified, the replica reports the error in its log and in `replication_last_error` of `heimdb_status`, and keeps retrying. The shared secret is encrypted too.
- Status: `tls` (`off`, `on`, `required`), `tls_cert_not_after` (the certificate's expiry) and `connection_tls` (whether this connection uses TLS) in `heimdb_status`. Before the expiry, make a new certificate and restart.
- One connection takes about 2.5 ms instead of about 1 ms, but page load times do not change beyond the noise.

## When writes are stuck

Each branch has one writing transaction at a time, and it holds the write lock until `COMMIT` or `ROLLBACK`. Other writes wait for `-lock-wait-timeout` (50 seconds by default) and then fail with error 1205. If a connection stops while holding the lock, writes stay stuck (a connection that went idle inside a transaction, or a statement inside a transaction that does not finish).

- **HeimDB releases it by itself**: a connection that holds the lock longer than `-idle-lock-timeout` (30 seconds by default) while idle in a transaction is closed. The closed connection's transaction is rolled back, the lock is released, and the waiting writes continue. The log shows `HeimDB: closing a connection idle in a transaction with the write lock ...` with the connection ID, user, how long it held the lock, and the last statement. Administrator connections are closed too (do not leave an interactive `BEGIN` open). Connections running a statement are not closed (stop those with the execution time limit of `main`; `heimdb_branch_limit`).
- **Find the holder**: in the metrics (which need no DB privileges), `heimdb_branch_write_lock_holder{branch}` (the holder's connection ID; 0 for internal operations such as merge and restore), `heimdb_branch_write_lock_held_seconds` (how long it has been held), `heimdb_branch_write_lock_idle_seconds` (how long the holder has been idle; 0 while running), and `heimdb_branch_write_lock_waiters` (the number of waiting statements). When a write fails with 1205, the log shows `HeimDB: lock wait timeout; the write lock of branch main is held by connection 8613 (wordpress) for 5m3s, running a statement, last statement: ...` (at most once every 10 seconds). Administrators can also see it in the `write_lock <branch>` row of `CALL heimdb_status()`.
- **Close it yourself**: as the site user, `SHOW PROCESSLIST` (or `SELECT * FROM information_schema.processlist`) shows the connections of your own user (no PROCESS privilege needed; same as MySQL). Closing the holder's ID with `KILL <ID>` rolls back that connection's transaction and releases the lock. To stop only the statement, use `KILL QUERY <ID>` (for a statement inside an explicit transaction, the lock stays). You cannot close other users' connections (1095). Statements waiting for the lock can also be stopped with `KILL QUERY`.
- **Restart only as a last resort**: a restart loses the clues about the holder (the connection, the last statement).

## Slow query log

With `-slow-query 1s` and the like, statements that took at least that long are logged as warnings.

```
level=warning msg="slow query: SELECT ..." connectionID=12 database=wordpress duration=1.234s indexes=wp_postmeta.meta_key,wp_posts.PRIMARY lock_wait=0s rows_examined=20412 user=wordpress
```

- `duration` is from receiving the statement until the result has been written. Times are in steps of 1 µs, shown as `312µs`, `1.234ms`, `1.5s` and so on (`µ` is U+00B5; up to 0.2.26 the step was 1 ms). `rows_examined` is the number of table rows and index entries the statement read (looking up a table row from an index counts both). `indexes` is the indexes that were read (`table.index`; the primary key is `PRIMARY`). Wait times appear only when there was a wait: `lock_wait` (the write lock), `replica_wait` (the report from a synchronous replica), `stats_wait` (waiting for table statistics to be built; the first statement on each table after a restart).
- Administrators can change the threshold without a restart: `SET GLOBAL long_query_time = 0.5` (seconds, decimals allowed), `SET GLOBAL slow_query_log = OFF` (`ON` to turn it back on). `-slow-query` is the value at start, and a restart goes back to it. Per-connection `SET SESSION long_query_time` is ignored (so that the site's PHP cannot flood the log from its own connection). The current value is `slow_query` in `CALL heimdb_status()`.
- The log can contain text entered by visitors (search terms, form values). With `-slow-query-redact`, values in statements are folded into `?` (the statement is rewritten in lowercase; for a statement that cannot be parsed, only the first two words). User management statements (`CREATE USER` and so on) have their text hidden even without the flag.
- Ignored variables: `wait_timeout`, `interactive_timeout` (idle connections are not closed; see `-idle-lock-timeout` above), `innodb_lock_wait_timeout`, `lock_wait_timeout` (`-lock-wait-timeout`), `net_read_timeout`, `net_write_timeout` and per-connection `long_query_time` accept `SET` but are not read. A per-connection `SET` logs `HeimDB: SET <variable> is accepted but has no effect` once per variable.
- Execution time limit for statements: per-connection `SET SESSION max_execution_time = 5000` (milliseconds), or the administrator's `SET GLOBAL max_execution_time` (the default for connections made after it), stops `SELECT` statements (including `WITH ... SELECT` and a `UNION` that starts with a parenthesis) as MySQL does, and returns error 3024. It does not apply to a `SELECT` inside `INSERT ... SELECT`, `UPDATE`, `SET` or `DO`. If there is a branch limit (`heimdb_branch_limit`), the shorter one applies, and it cannot be removed with `SET`. A statement whose client disconnected stops within a few seconds on Linux once the TCP connection is closed (if PHP keeps the connection open while waiting, it does not stop).

## Right after a restart

HeimDB keeps the statistics used for query plans (the number of rows in each table and the distribution of values in each index) only in memory. On start, it builds the statistics of every table on main in the background while accepting connections. A statement that uses a table before its statistics are built waits for them (the time to read the whole table. With 200,000 posts and 3.2 million rows of `wp_postmeta`, building takes about 1 second from start, and statements are delayed by at most about 0.4 seconds). The time waited appears in `heimdb_stats_wait_seconds_total` and in `stats_wait` in the slow query log.

To wait before sending outside traffic, watch one of these:
- Until the metric `heimdb_stats_ready` becomes 1 (needs no DB privileges).
- Until `stats` in `CALL heimdb_status()` becomes `ready`. `stats_ready_at` (when building finished), `stats_build_duration` (how long it took) and `stats_tables` (the numbers of tables built and failed, like `18 built, 0 failed`) are shown too. It becomes `ready` even if some tables failed (for those tables, the first statement that uses them builds the statistics). If the list of tables could not be read, it becomes `failed` and `stats_error` shows the reason (in that case, the first statement on each table builds the statistics).

`ready` means that the one run at start has finished. Rebuilding statistics afterwards because of writes or `ALTER TABLE` does not bring it back to `building`. Branch tables are not warmed up (for each branch, the first statement that uses a table builds its statistics). Replicas build them too, but this is separate from whether the replica has caught up, so on a replica, watch it together with `replication_behind`.

## Rebuilding old indexes

Indexes containing `DECIMAL`, `FLOAT`, `DOUBLE` or `ENUM` columns made by HeimDB 0.2.38 or earlier, and indexes containing `SET` or `BIT` columns made by 0.2.40 or earlier, have no entries, and every lookup reads all rows of the table (such as `min_max_price` and `stock_quantity` of WooCommerce's `wc_product_meta_lookup`). Since 0.2.42, production rebuilds such indexes on main in the background after it starts and begins accepting connections (the same writes as `CALL heimdb_rebuild_indexes()`; turn it off with `-rebuild-indexes=false`).

- Indexes are rebuilt one at a time. During each one, writes to main wait (reads are not blocked), and the lock is released between indexes. A table of 100,000 rows takes 0.1 to 0.4 seconds; 1 million rows take about 1 to 2 seconds for a non-unique index and 4 to 5 seconds for a unique index (measured on macOS; stage 121). Writes during that time wait until it finishes (at most 3 seconds during a unique index on 1 million rows). Waiting writes wait up to `-lock-wait-timeout` (50 seconds by default).
- It waits only 5 seconds for the write lock. If a long transaction holds the lock and it cannot be taken, that index is skipped and retried on the next start.
- Indexes whose estimated row count exceeds 5 million rows for a non-unique index or 1 million rows for a unique index are skipped, because they would make writes wait too long. Rebuild them with `CALL heimdb_rebuild_indexes()` at a quiet time.
- If rows have duplicate values for a unique index (`CREATE UNIQUE INDEX` in 0.2.38 or earlier did not check for duplicates), that index is skipped and the rest continue. Fix the duplicates, then call `CALL heimdb_rebuild_indexes()`.
- Skipped indexes are logged as warnings with the table name, index name and reason. At the beginning, the number of indexes to rebuild is logged; for each index, its name, estimated rows and time taken; and at the end, a summary.
- The holder of the write lock is shown as `rebuild indexes` (`write_lock main` in `heimdb_status`; the metric `heimdb_branch_write_lock_holder` is 0).
- Watch progress with `index_rebuild` (`off`, `running`, `done`), `index_rebuild_done_at`, `index_rebuild_duration`, `index_rebuild_indexes` (the numbers rebuilt and skipped in the last run, like `6 rebuilt, 1 skipped`) and `index_rebuild_error` (the reason for the last skip) in `CALL heimdb_status()`, and the metrics `heimdb_index_rebuild_ready`, `heimdb_index_rebuild_seconds` and `heimdb_index_rebuild_skipped`.
- When stopped (SIGTERM), the index in progress is abandoned and its partial entries are removed (any left over are removed on the next start). The remaining indexes are rebuilt on the next start.
- It runs again after main is reverted with `CALL heimdb_restore()` (if the reverted table definitions have old indexes, they are rebuilt).
- If you restore a backup taken in the middle of a rebuild, the indexes up to that point come back old and are rebuilt on the next start.
- Branches are not rebuilt (branches are usually short-lived). Connect to the branch's database and call `CALL heimdb_rebuild_indexes()`. The warning at start also shows branch names.
- It does not run on replicas. The entries production creates reach replicas through the log. When a replica is promoted, it rebuilds them when it starts as production.

## Metrics (Prometheus)

With `-metrics-addr 127.0.0.1:9104` and the like, `GET /metrics` returns metrics in the Prometheus text format. Anything that reads the Prometheus format can collect them: Prometheus, Grafana Cloud (Alloy), Google Cloud Managed Service for Prometheus (Ops Agent), New Relic (nri-prometheus of the infrastructure agent, or the OpenTelemetry Collector), Datadog and so on. It can be opened on both production and replicas.

- **There is no authentication.** The collector reads from the same host or from inside the same network. Open it on `127.0.0.1` or an internal address, and do not expose it through the firewall. In Docker, use `-metrics-addr 0.0.0.0:9104`, do not publish the port to the host (do not write it in `ports`), and read it at `db:9104` from a collector on the same network.
- Only counts and sizes are exposed; statement contents, user names, database names and table names are not. Branch names (`branch`) and replica names (`replica`, `-replica-name`) are.
- Current values are collected on every read (nothing heavy). Reading every 30 seconds or every minute is enough.

| Metric | Type | Meaning |
|---|---|---|
| `heimdb_build_info{version}` | gauge | HeimDB version (value is 1) |
| `heimdb_start_time_seconds` | gauge | Start time (Unix time) |
| `heimdb_replica` | gauge | 1 on a replica, 0 on production |
| `heimdb_connections`, `heimdb_connections_total` | gauge, counter | Open connections; connections since start |
| `heimdb_running_statements` | gauge | Running statements |
| `heimdb_statements_total{type}` | counter | Finished statements. `type` is the first word of the statement: `select`, `insert`, `update`, `delete`, `replace`, `call`, `ddl`, `transaction`, `set`, `show`, `other` (statements with syntax errors are `other` too) |
| `heimdb_statement_errors_total{code}` | counter | Statements that ended in an error. `code` is the MySQL error number returned to the client (1205 lock wait, 1041 memory, 1146 table does not exist, 1213 deadlock, 3024 execution time limit; syntax errors are 1105). 1041, 1205, 1213 and 3024 are exposed as 0 before they first occur. Only executions are counted, not prepares (`COM_STMT_PREPARE`), so errors at the prepare stage are not counted either (WordPress does not use server-side prepares) |
| `heimdb_statement_duration_seconds` | histogram | Time to execute the statement and send its result |
| `heimdb_slow_statements_total` | counter | Statements written to the slow query log (see "Slow query log" above; 0 if it is off) |
| `heimdb_branch_write_lock_holder{branch}`, `heimdb_branch_write_lock_held_seconds{branch}`, `heimdb_branch_write_lock_idle_seconds{branch}` | gauge | Connection ID of the write lock holder (0 for internal operations), how long it has been held, how long the holder has been idle (0 while running). Only for branches with a holder (see "When writes are stuck" above) |
| `heimdb_connection_write_lock_held_seconds{branch}` | gauge | How long a connection has held the branch's write lock (internal operations such as merge and restore are not included). Always exposed for every branch, 0 if no connection holds it (temporary table locks are not exposed, since only that connection waits for them). Can be used for a "a connection holds it too long" alert in monitoring that cannot join two metrics (Cloud Monitoring and so on). If the same process also has branches for test environments, `{branch="main"}` shows production only (in 0.2.25 there was no branch label, and it was the longest time across all branches) |
| `heimdb_branch_write_lock_waiters{branch}` | gauge | Statements waiting for the write lock (only for branches with waiters) |
| `heimdb_longest_statement_seconds` | gauge | Duration of the longest running statement |
| `heimdb_idle_lock_disconnects_total` | counter | Connections closed by `-idle-lock-timeout` |
| `heimdb_lock_wait_seconds_total`, `heimdb_replica_wait_seconds_total`, `heimdb_stats_wait_seconds_total` | counter | Total time statements waited: for the write lock, for reports from a synchronous replica, and for table statistics to be built (the first statement on each table after a restart) |
| `heimdb_stats_ready`, `heimdb_stats_build_seconds` | gauge | 1 once the statistics of main's tables have been built after start (0 until then; 1 even if some tables failed); seconds it took (not exposed until finished). See "Right after a restart" above |
| `heimdb_index_rebuild_ready`, `heimdb_index_rebuild_seconds`, `heimdb_index_rebuild_skipped` | gauge | 1 once the old indexes on main have been rebuilt after start (0 while rebuilding, with `-rebuild-indexes=false`, and on replicas); seconds it took (not exposed until finished); number skipped. See "Rebuilding old indexes" above |
| `heimdb_memory_limit_bytes`, `heimdb_memory_abort_bytes`, `heimdb_memory_live_bytes` | gauge | Memory limit, the threshold at which statements are stopped, live heap (see the memory limit in "Operating notes" above; 0 if there is no limit) |
| `heimdb_memory_aborted_statements_total` | counter | Statements stopped by the memory limit |
| `heimdb_engine_caches` | gauge | Number of caches of intermediate statement results (GROUP BY, DISTINCT, sorting, subqueries) held by the SQL engine. 0 when no statement is running. If it does not go back to 0 when there are no statements and keeps growing, cleanup is leaking (stages 108 and 109) |
| `heimdb_cache_size_bytes`, `heimdb_cache_used_bytes` | gauge | Size of the block cache and the part in use |
| `go_goroutines` | gauge | Number of goroutines |
| `heimdb_disk_bytes` | gauge | Disk usage of the data |
| `heimdb_branches`, `heimdb_branch_size_bytes{branch}` | gauge | Number of branches (including main); size of each branch (estimate; `size_bytes` of `heimdb_branch_list`) |
| `heimdb_last_commit_timestamp_seconds` | gauge | Time of the last commit |
| `heimdb_open_transactions`, `heimdb_oldest_snapshot_age_seconds` | gauge | Open transactions; age of the oldest snapshot (it holds back the GC boundary; 0 if none). Read-only transactions are included (a logical dump with `mysqldump --single-transaction` keeps it growing for the whole dump). To see whether writes are stuck, use the write lock metrics |
| `heimdb_pinned_times` | gauge | Number of times in use (`AS OF`, restore, creating a branch from a past time) |
| `heimdb_gc_boundary_age_seconds` | gauge | Age of the GC boundary (not exposed if nothing has been deleted yet) |
| `heimdb_gc_last_run_timestamp_seconds`, `heimdb_gc_last_duration_seconds`, `heimdb_gc_last_failed` | gauge | Time of the last GC, how long it took, 1 if it failed (not exposed if GC has not run since start) |
| `heimdb_replication_sync`, `heimdb_replication_async_switches_total` | gauge, counter | Production: 1 if synchronous; number of switches to asynchronous |
| `heimdb_replica_behind_seconds{replica}`, `heimdb_replica_last_seen_seconds{replica}` | gauge | Production: each replica's lag and time since its last contact |
| `heimdb_replication_behind_seconds`, `heimdb_replication_last_contact_seconds`, `heimdb_replication_failing` | gauge | Replica: lag behind production, time since the last contact, 1 if the last request failed or stalled |
| `heimdb_tls_cert_expiry_timestamp_seconds` | gauge | Expiry of the TLS certificate (with `-tls-cert`) |
| `heimdb_watch_changes_total` | counter | Changes found by the change watch |

Example configurations are in [`scripts/metrics`](../scripts/metrics).

- **Prometheus:** [`prometheus.yml`](../scripts/metrics/prometheus.yml) (reads `db:9104`) and example alerting rules in [`rules.yml`](../scripts/metrics/rules.yml). The rules cover: metrics cannot be read, memory near the threshold or statements being stopped, lock wait failures, old snapshots, GC failures, replica lag / no contact / cannot receive, certificate expiry, and risky changes. Production remembers the replicas that connected to it until it is restarted, so if you remove a replica, `HeimDBReplicaGone` for that name keeps firing until production is restarted (silence it in Alertmanager). Send the alerts from Alertmanager or Grafana. Put the monitoring on a different VM from HeimDB (on the same VM, if the whole VM stops, the alerts stop too).
- **Viewing locally (Prometheus and Grafana):** [`compose.yml`](../scripts/metrics/compose.yml). It reads a local HeimDB run with `-metrics-addr 127.0.0.1:9104` (through Docker Desktop's `host.docker.internal`), and loads the rules in `rules.yml` and an example dashboard, [`grafana-dashboard.json`](../scripts/metrics/grafana-dashboard.json) (query counts and times, errors, connections, memory, disk, snapshots, alerts). With `docker compose -f scripts/metrics/compose.yml up -d`, Grafana is at http://127.0.0.1:3000 (no login) and Prometheus at http://127.0.0.1:9090 . The ports are opened on 127.0.0.1 only. It is for trying things out, and Grafana makes you an administrator without login.
- **New Relic (OpenTelemetry Collector):** [`otel-collector.yml`](../scripts/metrics/otel-collector.yml). The Collector (`otel/opentelemetry-collector-contrib`) reads HeimDB and sends to New Relic over OTLP. Pass the license key in the environment variable `NEW_RELIC_LICENSE_KEY`.
- **New Relic (infrastructure agent):** if the agent is installed on the VM, add `http://127.0.0.1:9104/metrics` to the targets of the Prometheus integration (nri-prometheus) (see New Relic's documentation for the configuration format).
