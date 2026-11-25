<!--
The description of tsukinowa/heimdb on Docker Hub. Paste it into the Docker Hub screen (Repository, General).
- Short description (up to 100 characters): the one line below
- Repository overview: everything below "---"
It is written so that Docker Hub users can use the image from this description alone, without going to GitHub. The source is https://github.com/tsukinowasha/heimdb . When compose.yaml changes, update this too.
The Docker Hub description is limited to 25,000 bytes. The tags section describes only the latest version in detail; for older versions, keep only what to watch for when upgrading.
-->

Short description:

```
A branchable, time-travelling MySQL-compatible database for WordPress. Development preview.
```

---

# HeimDB

A database for WordPress with branches and time travel. It speaks the MySQL protocol, and WordPress, wp-cli and mysqldump see it as MySQL 8.0, so you only change the database host.

- **Time travel**: read the data as of any past time with `AS OF`, and restore a site to that time (a restore can be undone too).
- **Branches**: create a staging copy of production instantly without copying data, review the diff, and merge it back. Comments and users that arrived on production in the meantime are kept, and the site URL is not overwritten.
- **Drop-in for MySQL**: move a site in with mysqldump and the `mysql` command, and back out the same way.
- **One site per process**: a container serves one WordPress database (`-db`) and its branches (`wordpress@staging`).
- **Memory limit**: by default 80% of the container's limit. A statement that would pass it stops with error 1041 and is rolled back; the process keeps running.
- **Managed with SQL**: `CALL heimdb_*` procedures from any MySQL client.
- **Prometheus metrics** at `/metrics`, online and [age](https://age-encryption.org/)-encrypted backups, synchronous read-only replicas, TLS, a slow query log, and a change watch that logs takeover-like changes (a new administrator, `siteurl`, activated plugins).

> **Development preview.** The data format is version 8 since 0.2.41. A newer version converts older data when it starts; an older version refuses newer data. When a version changes the format so that older versions cannot open the data, its minor version goes up (0.3.x to 0.4.0). Pin a version and keep backups.

HeimDB is built only for WordPress. Views, triggers, stored procedures, full-text search and character sets other than utf8mb4 are not supported and return explicit errors. So is SQL that reads or writes files on the server (`LOAD_FILE()`, `SELECT ... INTO OUTFILE`, `LOAD DATA`).

## Tags

- `0.3.2`, `latest`: linux/amd64, linux/arm64. `heimdb_branch_sync` no longer copies comments and users, and `heimdb_merge_include` is refused (51011) while there are branches. If a pattern was removed on 0.3.1 or older after a merge or a sync of a branch that still exists, recreate that branch before merging it. Same data format (version 8) and replication records as 0.3.1. From 0.3.0, the minor version goes up when the data format changes so that older versions cannot open upgraded data (0.3.x to 0.4.0), and other releases raise the patch version.

Older tags can still be pulled. Versions that need care when upgrading (see Usage and Configuration for features):

- `0.2.41`: **Data format version 8**: older versions cannot open the data after the upgrade. 0.2.41 and newer primaries refuse 0.2.40 and older replicas (they read FLOAT(M,D) as FLOAT and do not maintain the new index entries after a promotion). Upgrade the replica first. Then run `CALL heimdb_rebuild_indexes()` on the primary to give older SET and BIT indexes their entries. Columns created before keep their plain `float` and `double` types; Wordfence and MainWP Child issue one ALTER to their declared types after the upgrade, and after that ALTER `heimdb_branch_sync` to branches made before it is refused because the table definitions differ.
- `0.2.39`: **Data format version 7**: older versions cannot open the data after the upgrade. 0.2.39 and newer primaries refuse 0.2.38 and older replicas (they do not maintain the new index entries after a promotion). Upgrade the replica first. Then run `CALL heimdb_rebuild_indexes()` on the primary to give older DECIMAL, FLOAT, DOUBLE and ENUM indexes their entries.
- `0.2.33`: **Data format version 6**: older versions cannot open the data after the upgrade. 0.2.33 and newer primaries refuse 0.2.32 and older replicas (they do not know sync records and would read merge bases wrongly after a promotion). Upgrade the replica first.
- `0.2.20`: **Data format version 5**: older versions cannot open the data after the upgrade. 0.2.20 and newer primaries refuse 0.2.19 and older replicas.
- `0.2.19`: **Data format version 4**: older versions cannot open the data after the upgrade. 0.2.19 and newer primaries refuse 0.2.18 and older replicas.
- `0.2.18`: plugins activated on 0.2.17 or older may be missing indexes (`SHOW INDEX ... WHERE` returned wrong answers). Check with `SHOW INDEX FROM table`.
- `0.2.16`: For a post added on both sides with the same content, the row diff's `parent_change` is now `insert`. `heimdb_branch_diff(name, '')` is now an error; call `heimdb_branch_diff(name)` for the summary.
- `0.2.15`: The row diff adds three columns at the end, `resolved_by`, `resolved_at` and `conflict_values`; code that reads columns by position must read them.
- `0.2.12`: Refused merges return error numbers 51001 to 51009.
- `0.2.11`: **Data format version 3**: older versions cannot open the data after the upgrade. 0.2.11 and newer primaries refuse 0.2.10 and older replicas.
- `0.2.8`: Do not connect a 0.2.7 or older replica to a 0.2.8 or newer primary.
- `0.2.5`: On 0.2.4 and older, `ALTER USER` without `IDENTIFIED` cleared the password: reset the password of any account changed that way.
- `0.1.6`: **Data format version 2**: 0.1.5 and older cannot open the data after the upgrade. Upgrade the primary first, then rebuild the replica from a new backup.
- `0.1.0`: do not use. SQL could read and write files on the server. Fixed in 0.1.1.

Pin a version (`0.3.2`), not `latest`. Before upgrading, take a backup (`CALL heimdb_backup(...)` or mysqldump). With a replica, upgrade the replica first, then the primary (also from 0.1.6 or newer to 0.2.11, 0.2.19, 0.2.20, 0.2.33, 0.2.39 and 0.2.41: their data formats are versions 3, 4, 5, 6, 7 and 8, but the records sent to replicas are unchanged). Only when the record format changes (0.1.5 or older to 0.1.6 or newer), upgrade the primary first, then the replica, or the upgraded replica cannot follow the old primary or go back. A primary and a replica check that they use the same record format version, and the replica stops instead of applying anything if they do not.

## Quick start

```sh
docker run -d --name heimdb -p 3306:3306 -v heimdb-data:/data \
  -e HEIMDB_ROOT_PASSWORD='secret' -e HEIMDB_SITE_PASSWORD='site-secret' tsukinowa/heimdb:0.3.2

mysql -h 127.0.0.1 -P 3306 -u root -p wordpress
```

HeimDB runs as a non-root user (UID 65532) and does not change ownership at start, unlike the official MySQL image. Named volumes work as is; for a bind-mounted host directory, run `chown -R 65532:65532 <dir>` first, or HeimDB stops with `open store: open /data/LOCK: permission denied`.

On first start, `HEIMDB_ROOT_PASSWORD` creates `root@'%'` with all privileges, for the administrator. `HEIMDB_SITE_PASSWORD` creates `wordpress@'%'` with privileges on the production database `wordpress` only, if it does not exist yet. Connect WordPress as `wordpress`, not as root: a hole in a plugin would otherwise reach HeimDB's procedures (restore, branch deletion), user management and the other branches.

## WordPress with a staging site (Docker Compose)

Save this as `compose.yaml`:

```yaml
name: heimdb

x-db-host: &db-host
  WORDPRESS_DB_HOST: db:3306
  WORDPRESS_DB_NAME: wordpress

services:
  db:
    image: tsukinowa/heimdb:${HEIMDB_VERSION:-0.3.2}
    environment:
      HEIMDB_ROOT_PASSWORD: ${HEIMDB_ROOT_PASSWORD:?set HEIMDB_ROOT_PASSWORD}  # creates root@'%' on first start
      HEIMDB_SITE_PASSWORD: ${HEIMDB_SITE_PASSWORD:?set HEIMDB_SITE_PASSWORD}  # creates wordpress@'%' if missing
    volumes:
      - heimdb-data:/data
    stop_grace_period: 1m   # longer than HeimDB's -shutdown-timeout (30s)

  wordpress:
    image: wordpress:php8.3-apache
    depends_on: [db]
    ports:
      - "${WP_PORT:-8080}:80"
    environment:
      <<: *db-host
      WORDPRESS_DB_USER: wordpress
      WORDPRESS_DB_PASSWORD: ${HEIMDB_SITE_PASSWORD:?set HEIMDB_SITE_PASSWORD}
    volumes:
      - wordpress:/var/www/html

  # On the first start of staging, copy production's files (languages, plugins, themes, uploads).
  # Like the database branch, staging's files start as a copy. Later starts keep staging's files.
  # STAGING_DB_PASSWORD is checked here because ${...:?} applies even when the profile is not used.
  staging-files:
    image: wordpress:php8.3-apache
    profiles: ["staging"]
    environment:
      STAGING_DB_PASSWORD: ${STAGING_DB_PASSWORD:-}
    entrypoint:
      - sh
      - -c
      - |
        [ -n "$$STAGING_DB_PASSWORD" ] || { echo "set STAGING_DB_PASSWORD" >&2; exit 1; }
        [ -e /to/wp-includes/version.php ] || cp -a /from/. /to/
    volumes:
      - wordpress:/from:ro
      - wordpress-staging:/to
    restart: "no"

  # A second WordPress on the branch "staging" (database wordpress@staging), connecting as a user
  # limited to that branch
  staging:
    image: wordpress:php8.3-apache
    profiles: ["staging"]
    depends_on:
      db:
        condition: service_started
      staging-files:
        condition: service_completed_successfully
    ports:
      - "${STAGING_PORT:-8081}:80"
    environment:
      <<: *db-host
      WORDPRESS_DB_NAME: wordpress@${STAGING_BRANCH:-staging}
      WORDPRESS_DB_USER: staging
      WORDPRESS_DB_PASSWORD: ${STAGING_DB_PASSWORD:-}
      WORDPRESS_CONFIG_EXTRA: |
        define('WP_HOME', 'http://localhost:${STAGING_PORT:-8081}');
        define('WP_SITEURL', 'http://localhost:${STAGING_PORT:-8081}');
    # Files live outside the database, so staging has its own volume (files are not merged)
    volumes:
      - wordpress-staging:/var/www/html

  # wp-cli is the administrator's tool, so it connects as root (CALL heimdb_*, creating users)
  cli:
    image: wordpress:cli
    profiles: ["cli"]
    depends_on: [db]
    user: "33:33"
    environment:
      <<: *db-host
      WORDPRESS_DB_USER: root
      WORDPRESS_DB_PASSWORD: ${HEIMDB_ROOT_PASSWORD:?set HEIMDB_ROOT_PASSWORD}
    volumes:
      - wordpress:/var/www/html

volumes:
  heimdb-data:
  wordpress:
  wordpress-staging:
```

Start production and install WordPress (http://localhost:8080):

```sh
export HEIMDB_ROOT_PASSWORD='secret' HEIMDB_SITE_PASSWORD='site-secret'
docker compose up -d
docker compose run --rm cli wp core install --url=http://localhost:8080 --title=Site \
  --admin_user=admin --admin_password='change-me' --admin_email=you@example.com
```

WordPress connects as `wordpress`. Only wp-cli (`cli`) uses root.

Staging (http://localhost:8081) connects as a user `staging` limited to its branch, so a compromised staging site cannot reach production. Create the branch and the user, then start staging. Staging does not start without `STAGING_DB_PASSWORD`.

```sh
export STAGING_DB_PASSWORD='staging-secret'
docker compose run --rm cli wp db query "CALL heimdb_branch_create('staging')"
docker compose run --rm cli wp db query "CREATE USER 'staging'@'%' IDENTIFIED BY '$STAGING_DB_PASSWORD'; GRANT ALL ON \`wordpress@staging\`.* TO 'staging'@'%'"
docker compose --profile staging up -d staging
```

After editing on staging, review the diff and merge it into production:

```sh
docker compose run --rm cli wp db query "CALL heimdb_branch_diff('staging')"
docker compose run --rm cli wp db query "CALL heimdb_branch_merge('staging')"
```

Files (languages, plugins, themes, uploads) are not merged. Staging starts with a copy of production's files. If you install or update a plugin, theme or language on staging, install the same on production before merging. Without the copy, staging would show English instead of your language, and opening its admin would deactivate missing plugins and switch the theme, which a merge would carry to production.

To start staging over from production's current database and files (the `staging` user stays):

```sh
docker compose --profile staging rm -sf staging staging-files
docker volume rm heimdb_wordpress-staging
docker compose run --rm cli wp db query "CALL heimdb_branch_delete('staging')"
docker compose run --rm cli wp db query "CALL heimdb_branch_create('staging')"
docker compose --profile staging up -d staging
```


## Usage

HeimDB is operated with SQL `CALL` statements, run by an administrator (a user with all privileges).

### Branches

```sql
CALL heimdb_branch_create('staging');                               -- from production now
CALL heimdb_branch_create('test', 'main', '2026-10-08 09:00:00');   -- from a past time
CALL heimdb_branch_list();
CALL heimdb_branch_delete('test');
```

A branch appears as a database named `wordpress@<branch>`. Grant access to one branch only with a normal `GRANT`:

```sql
CREATE USER 'staging'@'%' IDENTIFIED BY 'pw' WITH MAX_USER_CONNECTIONS 20;   -- concurrent connections
GRANT ALL ON `wordpress@staging`.* TO 'staging'@'%';
CALL heimdb_branch_limit('staging', '2G', '30s');   -- branch size and per-statement time limits
```

### Diff and merge

```sql
CALL heimdb_branch_diff('staging');               -- summary per table
CALL heimdb_branch_diff('staging', 'wp_posts');   -- per row ('*' for all tables)
CALL heimdb_branch_diff('staging', 'wp_posts', 0, 100);   -- per row, 100 rows at a time
CALL heimdb_branch_merge('staging', 'dry_run');   -- check the merge and counts without writing
CALL heimdb_branch_merge('staging');              -- one commit into the parent
```

Rows are matched by primary key (`option_name` for `wp_options`) and compared column by column against the fork point. Columns changed only on the branch are merged, and columns changed only on production are kept. Environment-specific options (`siteurl`, `home`, transients, cron, ...), auto-drafts, revisions, comments and users are not merged. Table and index changes made by plugins (dbDelta) are merged. A merge that writes anything ends its result with a `heimdb_restore` call that undoes it.

If both sides changed the same column, the row is a conflict and the merge is refused until you choose a side:

```sql
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=42', 'branch');  -- take staging's value
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=42', 'parent');  -- keep production's value
CALL heimdb_branch_resolve('staging', 'wp_posts', '*', 'parent');      -- all conflicts in the table
```

The row is written as it appears in the `id` column of the diff (`'ID=42'`).

Tables that a plugin rebuilds after a merge (such as Yoast SEO's indexes) can be left out of merges; production keeps them as they are:

```sql
CALL heimdb_merge_exclude('wp_yoast_%');
```

A pattern can be removed (`heimdb_merge_include`) only when there are no branches.

### Pull production's changes into a branch

When other branches were merged while `staging` was still under review, pull production's latest into `staging` and review again:

```sql
CALL heimdb_branch_sync('staging', 'dry_run');  -- check counts and conflicts without writing
CALL heimdb_branch_sync('staging');             -- refused (51004) if both sides changed the same column
CALL heimdb_branch_sync('staging', 'branch');   -- on conflicts, keep staging's value
CALL heimdb_branch_sync('staging', 'parent');   -- on conflicts, take production's value
```

Columns changed only on production are written to the branch in one commit; staging's own changes are kept. Comments, users, environment-specific options and excluded tables are not copied (to get production's comments and users, recreate the branch). Production is not locked. Tables whose definitions differ between the two sides are refused (51010). The result ends with a `heimdb_restore` call that undoes the sync.

Block themes create a navigation post (`wp_navigation`) the first time the front page is shown. If both production and staging show it for the first time after the branch is created, the two navigation posts conflict. Keep production's with `'parent'`.

### Time travel and restore

```sql
SET time_zone = '+09:00';   -- times are read in the session time zone

-- read the past
SELECT ID, post_title FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;
-- bring back one deleted post
INSERT INTO wp_posts SELECT * FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;

-- list the rows changed since a time, in the order they changed
CALL heimdb_changes('main', '2026-10-08 09:00:00');
CALL heimdb_changes('main', '2026-10-08 09:00:00', 'wp_users,wp_usermeta');  -- only these tables

-- restore the whole site (comments and users are kept as they are now)
CALL heimdb_restore_preview('main', '2026-10-08 09:00:00');
CALL heimdb_restore('main', '2026-10-08 09:00:00');
CALL heimdb_restore('main', '2026-10-08 09:00:00', 'wp_options,wp_posts');  -- only these tables
```

A restore writes the old versions as a new commit, so history is not lost. Its last result row shows the `CALL` that undoes it. Old versions are kept for 30 days by default (`-retention`).

`heimdb_changes` shows each changed row with its id, the change (`insert`, `update`, `delete`), the changed columns, when it last changed, and the change watch description if any. Rows that change routinely (transients, cron, edit locks) are counted in the last row but not shown.

Without a table list, a restore keeps users. If users were added, had their password or email changed, or were given the administrator role after the restore time, the result (and the preview) has `not restored` rows saying so, and the log has a warning: an administrator added by an attacker would otherwise stay. Restore with `'*'` to bring users back too.

### Status, backup and GC

```sql
CALL heimdb_status();                  -- disk usage, GC, connections, open transactions, replication, change watch
CALL heimdb_backup('before-upgrade');  -- consistent online backup into -backup-dir
CALL heimdb_gc();                      -- remove versions older than the retention now
```

### Change watch

HeimDB logs a warning when a commit changes what usually changes when a WordPress site is taken over. It does not block anything.

```
level=warning msg="HeimDB: watch: wordpress@172.18.0.4 on wordpress: option default_role: \"subscriber\" -> \"administrator\""
level=warning msg="HeimDB: watch: wordpress@172.18.0.4 on wordpress: administrator role given: user ID 7 (wp_capabilities)"
```

- Watched: the options `siteurl`, `home`, `admin_email`, `users_can_register`, `default_role`, `template`, `stylesheet`, `active_plugins` and the role definitions; users added or deleted, password and email changes (password hashes are never logged); the administrator role given to a user; on multisite, `site_admins` and network-activated plugins.
- Each line shows the user and the address of the connection, and the database (`wordpress@staging` for a branch). Changes merged from a branch end with `(merge of branch staging)`. Only committed changes are logged. Restores and replicas log nothing.
- `CALL heimdb_status()` shows the count since start (`watch_changes`) and the last one (`watch_last_change`, `watch_last_change_at`).
- Normal operation logs too: the WordPress install, user registration, the first login of each user after upgrading to WordPress 6.8 (password rehash), `wp search-replace` and `wp db import`.
- If a line is unexpected: list what changed since shortly before it with `heimdb_changes`, check `heimdb_restore_preview`, then `heimdb_restore` (see Time travel). Files outside the database (plugins, uploads) are not restored.

## Configuration

| | |
| --- | --- |
| Port | 3306 |
| Data | `/data` (use a volume) |
| User | nonroot (UID 65532) |
| `HEIMDB_ROOT_PASSWORD` | Password of `root@'%'`, created on first start only (or set again with `-reset-passwords` / `-reset-users`) |
| `HEIMDB_SITE_PASSWORD` | Password of the WordPress user (`-site-user`, default `wordpress`), created with privileges on the `-db` database only if it does not exist yet |
| `HEIMDB_RESET_PASSWORDS`, `HEIMDB_RESET_USERS` | Set to `1` for `-reset-passwords` or `-reset-users` (below) |

Arguments after the image name are passed to HeimDB:

```sh
docker run -d -p 3306:3306 -v heimdb-data:/data \
  -e HEIMDB_ROOT_PASSWORD='secret' tsukinowa/heimdb:0.3.2 \
  -backup-dir /data/backups -slow-query 1s
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-db` | `wordpress` | Database name of production (the `main` branch) |
| `-site-user` | `wordpress` | Name of the WordPress user created with `HEIMDB_SITE_PASSWORD` |
| `-retention` | `720h` | How long old versions are kept for `AS OF` and restore (0 keeps all) |
| `-gc-interval` | `1h` | How often old versions are removed |
| `-backup-dir` | | Where `CALL heimdb_backup()` writes (disabled if empty). Use a directory inside `/data`, such as `/data/backups` (since 0.3.1): a backup then hard-links the data files and takes milliseconds, while on another volume it copies every file and all writes wait meanwhile |
| `-backup-key` | | `name=path`: `CALL heimdb_backup('<backup>', '<name>')` writes one file `<backup>.tar.gz.age` encrypted to the [age](https://age-encryption.org/) recipients (`age1...`, one per line) in the file at `path`, read at every backup; the recipients are logged, with a warning when the file changed. Repeat for more keys. Since 0.2.28 |
| `-reset-passwords` | `false` | At startup, set the passwords of `root@'%'` and of an existing `-site-user` to `HEIMDB_ROOT_PASSWORD` and `HEIMDB_SITE_PASSWORD`, keeping the other users (for a store restored from a backup). Since 0.2.28 |
| `-reset-users` | `false` | At startup, drop every user but `root@'%'` (and the `-site-user`, given `HEIMDB_SITE_PASSWORD`, which keeps its privileges), and set their passwords; missing ones are created. Since 0.2.28 |
| `-slow-query` | | Log statements slower than this (the startup value; an administrator can change it with `SET GLOBAL long_query_time` and `slow_query_log`) |
| `-slow-query-redact` | `false` | Replace the values in logged slow statements with `?` |
| `-idle-lock-timeout` | `30s` | Close a connection idle in a transaction that has held a branch's write lock this long (0 disables) |
| `-shutdown-timeout` | `30s` | How long to wait for running statements on stop |
| `-branch-procedures` | `false` | Lets a user with only a branch's database call diff, resolve, changes and restore on the branch it is connected to, given `GRANT EXECUTE` (pass `''` as the branch; also `HEIMDB_BRANCH_PROCEDURES=1`) |
| `-memory-limit` | 80% of the container limit | Memory limit (like `4G`; or set `HEIMDB_MEMORY_LIMIT`; `0` disables). Near it, a statement fails with error 1041 and its transaction is rolled back, instead of the process running out of memory |
| `-rebuild-indexes` | `true` | After start, rebuild in the background main's indexes made without entries by 0.2.40 or earlier (or set `HEIMDB_REBUILD_INDEXES=false`). Since 0.2.42 |
| `-tls-cert`, `-tls-key` | | Enable TLS with these files (mount them readable by UID 65532) |
| `-metrics-addr` | | Serve Prometheus metrics at `/metrics` on this address (like `0.0.0.0:9104`; no authentication, so do not publish the port; read it from the same Docker network). Since 0.2.21 |

Read-only replicas and the remaining flags are documented in [`docs/operations.md`](https://github.com/tsukinowasha/heimdb/blob/main/docs/operations.md).

- Stop with `docker stop` (SIGTERM). Give it more time than `-shutdown-timeout` (`docker stop -t 60`, or `stop_grace_period` in Compose).
- To restore from a backup, start HeimDB with the backup directory as its data directory.
- To restore an encrypted backup, unpack it into an empty data volume first, then start as usual (add `-reset-passwords` or `-reset-users` where the production passwords are not known). It fails, leaving nothing, on a wrong key or a damaged file:

```sh
docker run --rm -v verify-data:/data -v /path/to/backups:/backups:ro -v /path/to/verify.key:/run/secrets/verify.key:ro \
  tsukinowa/heimdb:<version> restore-backup -in /backups/<backup>.tar.gz.age -identity /run/secrets/verify.key
```

## License

Source code: [github.com/tsukinowasha/heimdb](https://github.com/tsukinowasha/heimdb). Report bugs and differences from MySQL there; report vulnerabilities privately (see SECURITY.md in the repository).

Apache License 2.0. Copyright 2026 Tsukinowa Inc. The license notices of the dependencies are in `/licenses` in the image.
