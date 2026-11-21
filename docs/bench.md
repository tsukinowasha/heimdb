# Performance: WordPress on HeimDB and MySQL 8.0

The latest measurements of HeimDB against MySQL 8.0 on the same WordPress and data. Each section names the HeimDB version and date it was measured with; the values have not been re-measured with later releases. The tools are in `scripts/bench/`.

- [Summary](#summary)
- [Pages, writes and concurrency](#pages-writes-and-concurrency)
- [Under load](#under-load)
- [During a backup](#during-a-backup)
- [Memory](#memory)
- [How to measure](#how-to-measure)

## Summary

- A page's DB time is 1.5 to 2.1 times MySQL's, and the whole page 1.1 to 1.3 times (search is faster than MySQL). Adding a comment and updating an option are faster than MySQL; creating and deleting a post take about the same.
- Under load, HeimDB's maximum rate is 88% of MySQL's, and its server CPU at the same rate is 1.4 times MySQL's. Past the maximum and with 500 connections there are no errors or disconnects, and a 30-minute run does not grow in time or memory.
- A backup to the same filesystem as the data takes 13 to 19 ms and does not change page times. A backup to another disk copies every file while all writes wait, and reads wait with them (11.7 seconds for 2 GB); an encrypted backup does not stop anything, but slows the pages that overlap it.
- Resident memory is about 500 MiB and barely depends on table size. Large writes, merges and restores use memory in proportion to the rows they touch, and stop with error 1041 before `-memory-limit`.

Speed is not a goal in itself: public pages are expected to be served from a page cache.

## Pages, writes and concurrency

Measured 2026-11-19 with the build released as HeimDB 0.2.42.

### Setup

- One GCE n2-standard-4 (4 vCPU, 16 GB, Tokyo, Debian 12, pd-balanced).
- Two WordPress containers (`wordpress:php8.3-apache`, WordPress 7.1.4) that differ only in the database host (`scripts/bench/docker-compose.yml`). No object cache and no page cache.
  - MySQL: Docker `mysql:8.0.46` with the default settings (`innodb_buffer_pool_size` 128 MiB), port forwarded with `-p`.
  - HeimDB: the Linux binary on the host, `-db wpbench`, other flags at their defaults (block cache 128 MiB).
- Data (`wpbench`): 5,002 posts (bodies about 1.7 KB), 200 pages, 100 categories, 200 tags, 25,006 post meta rows, 20,001 comments (60 on the newest post). It is a copy of the data generated with `scripts/bench/generate.sh` and `data.sql`, grown by earlier benchmark runs, and was loaded into both servers from the same mysqldump. Each server started with an empty data directory before the load.
- DB time and query counts come from `SAVEQUERIES` and a mu-plugin (`scripts/bench/mu-plugins/bench-db-time.php`) that puts them in response headers. `SAVEQUERIES` is on for both, which adds some PHP time.
- Pages: `scripts/bench/pages.py --newest 5003 --cat 1 --runs 30` (one request at a time, median). Concurrency: `ab -c 10 -t 30`. Writes: `scripts/bench/writes.php` 100 times inside one PHP process (`wp eval-file`), DB time per operation.
- HeimDB was measured twice, reloading the data each time, and the values are the average. MySQL was measured with every run (6 runs).

### Pages (median)

| Page | HeimDB total | MySQL total | HeimDB DB time | MySQL DB time |
|---|---|---|---|---|
| Front page | 114 ms | 97 ms | 30.1 ms | 15.3 ms |
| Single post (60 comments) | 196 ms | 175 ms | 37.4 ms | 19.1 ms |
| Category, page 2 (5,001 posts) | 130 ms | 103 ms | 45.6 ms | 21.4 ms |
| Search | 145 ms | 189 ms | 56.4 ms | 102.8 ms |
| REST API (100 posts) | 330 ms | 276 ms | 102.2 ms | 58.8 ms |
| Admin dashboard | 126 ms | 100 ms | 67.9 ms | 43.6 ms |
| Posts list (admin) | 156 ms | 131 ms | 72.7 ms | 49.9 ms |

- DB time is 1.5 to 2.1 times MySQL's; search is 0.5 times, because MySQL's search reads every post with `LIKE` more slowly.
- The whole page is 1.1 to 1.3 times MySQL's, since most of a page's time is PHP.

### Writes and concurrency

| | HeimDB | MySQL |
|---|---|---|
| Create a post (DB time) | 63.6 ms | 63.2 ms |
| Add a comment (DB time) | 3.3 ms | 6.5 ms |
| Update an option (DB time) | 1.3 ms | 3.1 ms |
| Delete a post (DB time) | 43.1 ms | 46.1 ms |
| Front page, 10 in parallel | 22.4 /s | 26.0 /s |
| Single post, 10 in parallel | 12.3 /s | 13.7 /s |

Under `ab -c 10` the pages per second are 86 to 90% of MySQL's, with no failed requests.

## Under load

Measured 2026-11-19 with HeimDB 0.2.42; the two-connection case was measured again when 0.2.45 fixed it (see [Two connections](#two-connections)).

### Setup

- Two GCE n2-standard-4 instances in the same zone (Tokyo): one for the database and one for the load generator, connected over the internal network. The load side used 4 to 14% of its CPU in every run, so it was not the bottleneck.
- MySQL: Docker `mysql:8.0.46` with the default settings (`innodb_buffer_pool_size` 128 MiB, `max_connections` 151) and `--network host`.
- HeimDB: the release build (`CGO_ENABLED=0`, `-trimpath`) on the host, `-db wpbench`, other flags at their defaults.
- Only the measured server ran. Before each server start the `wpbench` dump was loaded into an empty data directory, the OS page cache was dropped, and the server was warmed with 30 seconds of reads from 8 connections. Runs with writes started from a fresh server each time.
- Load: `cmd/loadgen`. A "page" is the fixed sequence of statements that one WordPress page sends, run in order on one connection.
  - Read pages (`scripts/bench/load/pages-read.json`): the 321 statements of the 7 pages in the table above, captured from WordPress on MySQL (all `SELECT`).
  - Write pages (`pages-write.json`): the 4 operations of `writes.php` (create a post 47 statements, add a comment 4, update an option 2, delete a post 41), captured with `SAVEQUERIES` and parameterized so they can run any number of times.
  - Without `-rate`, each connection sends the next page as soon as the previous one finishes (closed loop). With `-rate`, page k is scheduled at k/rate seconds and its time is counted from that scheduled time, so a slow server does not hide its delay by lowering the load.
- Server CPU and RSS were read from `/proc` every second. CPU is the average over the 60-second window, where 100% is one vCPU (400% is all 4). "CPU per 1,000 statements" is server CPU time divided by statements run.
- Each value is the average of 2 alternating runs of 60 seconds after a 10-second warm-up, unless noted.

### Concurrent connections (reads only, closed loop)

| Connections | MySQL pages/s | HeimDB pages/s | Ratio | MySQL CPU | HeimDB CPU | MySQL CPU per 1,000 statements | HeimDB CPU per 1,000 statements | MySQL p50 / p99 | HeimDB p50 / p99 |
|---|---|---|---|---|---|---|---|---|---|
| 1 | 23.4 | 18.1 | 0.77 | 80% | 89% | 746 ms | 1,065 ms | 44 / 105 ms | 54 / 95 ms |
| 2 | 44.2 | 28.4 | 0.64 | 164% | 136% | 810 ms | 1,041 ms | 46 / 111 ms | 67 / 146 ms |
| 4 | 59.6 | 51.1 | 0.86 | 349% | 321% | 1,278 ms | 1,369 ms | 58 / 203 ms | 80 / 130 ms |
| 8 | 65.5 | 57.5 | 0.88 | 398% | 395% | 1,325 ms | 1,498 ms | 91 / 411 ms | 125 / 300 ms |
| 16 | 64.7 | 57.0 | 0.88 | 399% | 395% | 1,346 ms | 1,512 ms | 162 / 781 ms | 230 / 713 ms |
| 32 | 63.4 | 55.5 | 0.88 | 399% | 394% | 1,378 ms | 1,554 ms | 336 / 1,555 ms | 458 / 1,531 ms |
| 64 | 62.4 | 53.9 | 0.86 | 399% | 395% | 1,388 ms | 1,603 ms | 679 / 3,125 ms | 917 / 3,070 ms |
| 128 | 59.9 | 51.8 | 0.86 | 400% | 395% | 1,453 ms | 1,704 ms | 1,371 / 6,190 ms | 1,924 / 6,247 ms |

- The maximum is 65.5 pages a second for MySQL and 57.5 for HeimDB (88%). Both use all 4 vCPUs from 8 connections on; with 128 connections MySQL falls to 92% of its maximum and HeimDB to 90%. A page is about 46 statements, so the maxima are about 3,000 and 2,600 statements a second.
- At the maximum, HeimDB's median time is longer (125 ms and 91 ms at 8 connections) and its p99 shorter (300 ms and 411 ms): MySQL's search page is slow, HeimDB's other pages are.
- RSS: MySQL 455 to 624 MB (128 MiB buffer pool plus a thread per connection), HeimDB 244 to 310 MB.
- MySQL with `innodb_buffer_pool_size=2G` gave the same rates (within 2%); `wpbench` fits in 128 MiB.

### Fixed rates (reads only, 128 connections)

Rates at 25, 50 and 75% of MySQL's maximum. Both servers kept to the schedule (p99 lateness 1.1 ms).

| Pages/s | MySQL CPU | HeimDB CPU | MySQL CPU per 1,000 statements | HeimDB CPU per 1,000 statements | Ratio | MySQL p50 / p95 / p99 | HeimDB p50 / p95 / p99 |
|---|---|---|---|---|---|---|---|
| 16.4 (25%) | 59% | 83% | 788 ms | 1,110 ms | 1.41 | 46 / 109 / 117 ms | 58 / 109 / 125 ms |
| 32.7 (50%) | 131% | 178% | 871 ms | 1,185 ms | 1.36 | 47 / 122 / 136 ms | 71 / 126 / 140 ms |
| 49.1 (75%) | 232% | 321% | 1,031 ms | 1,427 ms | 1.38 | 52 / 151 / 181 ms | 86 / 142 / 167 ms |

- At the same rate HeimDB uses 1.36 to 1.41 times MySQL's server CPU. The median is 12 to 34 ms longer; p95 and p99 are about the same.
- Median per page at 50% (MySQL and HeimDB): front page 16.1 and 33.5 ms, single post 20.4 and 42.1, category 25.7 and 57.0, search 117.6 and 70.1, REST API 52.1 and 121.0, dashboard 50.9 and 88.8, posts list 56.1 and 94.5. The same shape as the DB times in the page table.

### Mixed reads and writes (32 connections, closed loop)

| Reads:writes | MySQL pages/s | HeimDB pages/s | Ratio | MySQL statements/s | HeimDB statements/s | MySQL read p99 | HeimDB read p99 |
|---|---|---|---|---|---|---|---|
| 95:5 | 65.9 | 56.2 | 0.85 | 2,946 | 2,503 | 1,684 ms | 1,546 ms |
| 80:20 | 72.0 | 60.4 | 0.84 | 2,988 | 2,494 | 1,954 ms | 1,550 ms |

| Write at 80:20 (p50 / p99) | MySQL | HeimDB |
|---|---|---|
| Create a post | 374–409 / 576–636 ms | 599–601 / 827–865 ms |
| Add a comment | 16–17 / 57–58 ms | 49–50 / 119–129 ms |
| Update an option | 7–8 / 31–36 ms | 22 / 73–75 ms |
| Delete a post | 238–246 / 377–404 ms | 509–514 / 689–740 ms |

- No lock wait timeouts (1205), deadlocks (1213), other errors or disconnects on either server.
- HeimDB runs a branch's writes one at a time, so concurrent writes wait longer than with MySQL's row locks: at 80:20, `heimdb_lock_wait_seconds_total` grew 5.8 to 7.0 seconds over the run (MySQL's row lock waits totalled 0.6 to 0.9 seconds). Each wait is far below the 50-second timeout.
- With the CPU saturated, writes take 1.5 to 3 times MySQL's time. Run one at a time, adding a comment and updating an option are faster on HeimDB (see the writes table above), so the difference under load is CPU contention and the write lock.

### Past the limit

| Run | Scheduled pages/s | Pages/s | p50 | p99 | Errors | Max RSS | CPU per 1,000 statements |
|---|---|---|---|---|---|---|---|
| MySQL, 150% of its maximum (128 connections) | 98.2 | 54.4 | 16.7 s | 27.8 s | 0 | 588 MB | 1,620 ms |
| HeimDB, 150% of MySQL's maximum (128 connections) | 86.3 | 46.9 | 17.8 s | 27.6 s | 0 | 304 MB | 1,867 ms |
| MySQL, 500 connections (default settings) | — | 58.6 | 1.7 s | 7.3 s | 24,656 connection errors 1040 | 610 MB | 1,470 ms |
| MySQL, 500 connections (`max_connections=1000`) | — | 52.0 | 5.6 s | 23.4 s | 0 | 1,000 MB | 1,630 ms |
| HeimDB, 500 connections | — | 40.4 | 7.7 s | 25.0 s | 0 | 499 MB | 2,163 ms |

- Over the limit, both servers run about 55% of the schedule and fall further behind; the rest waits in the queue. No errors, disconnects or timeouts, and neither server crashes.
- With 500 connections, MySQL's default settings refuse everything after 151 connections with 1040 (Too many connections). HeimDB accepts all 500 with no errors, at 70% of its maximum rate (MySQL with `max_connections=1000`: 79%), because its CPU per statement grows more with many concurrent statements. Its RSS was 420 to 503 MB and returned to 308 MB after the run.

### 30 minutes at 50% (32.7 pages/s, 64 connections)

| | MySQL | HeimDB |
|---|---|---|
| Pages/s | 32.7 | 32.7 |
| p50 / p95 / p99 / max | 46.5 / 123 / 144 / 204 ms | 68.7 / 119 / 133 / 170 ms |
| CPU (30-minute average) | 131% | 175% |
| CPU per 1,000 statements | 871 ms | 1,163 ms |
| RSS | 542 MB rising to 578 MB | 260 to 265 MB |
| Errors, disconnects, late pages | 0 | 0 |

| Minute | MySQL p50 | MySQL p99 | MySQL CPU | MySQL RSS | HeimDB p50 | HeimDB p99 | HeimDB CPU | HeimDB RSS |
|---|---|---|---|---|---|---|---|---|
| 1 | 46.3 ms | 136.3 ms | 129% | 542 MB | 68.6 ms | 134.4 ms | 171% | 260 MB |
| 5 | 46.3 ms | 139.9 ms | 130% | 554 MB | 67.9 ms | 128.7 ms | 173% | 262 MB |
| 10 | 47.6 ms | 138.7 ms | 131% | 573 MB | 67.4 ms | 136.4 ms | 171% | 260 MB |
| 15 | 46.3 ms | 141.5 ms | 130% | 574 MB | 68.3 ms | 132.3 ms | 174% | 261 MB |
| 20 | 46.7 ms | 143.5 ms | 131% | 574 MB | 69.3 ms | 134.5 ms | 175% | 262 MB |
| 25 | 47.8 ms | 149.2 ms | 133% | 575 MB | 69.2 ms | 136.3 ms | 176% | 264 MB |
| 30 | 45.3 ms | 149.4 ms | 130% | 578 MB | 68.3 ms | 130.9 ms | 176% | 261 MB |

Neither server grows in time or memory over 30 minutes. HeimDB's live heap after GC stays at 82 to 94 MB (`heimdb_memory_live_bytes` 92 to 95 MB), goroutines stay at 84, and each GC pause is 0.1 to 0.2 ms.

### Two connections

0.2.42 did not scale from 1 to 2 connections (1.57 times, using only 136% CPU), because the Go runtime ran both connections' goroutines on one thread. 0.2.45 wakes an idle thread when a statement starts, no other connection is in a statement, and another connection is open. Same setup, `main` before the fix (0.2.44, "before") and 0.2.45 ("after"):

| Connections | MySQL pages/s | Before | After | MySQL CPU per 1,000 statements | Before | After |
|---|---|---|---|---|---|---|
| 1 | 22.8 | 17.4 | 17.6 | 757 ms | 1,104 ms | 1,095 ms |
| 2 | 45.0 | 27.5 | 31.5 | 783 ms | 1,045 ms | 1,220 ms |
| 4 | 59.6 | 50.6 | 50.8 | 1,284 ms | 1,372 ms | 1,376 ms |
| 8 | 64.6 | 56.7 | 57.0 | 1,343 ms | 1,516 ms | 1,511 ms |

- 2 connections now give 1.79 times 1 connection (MySQL 1.97 times), and the p99 fell from 146 ms to 104 ms. CPU per statement rises 17% at 2 connections (thread switches); 4 and 8 connections are unchanged.
- At a fixed 32.3 pages/s with 128 connections, p50 / p95 / p99 went from 70.0 / 124.5 / 139.2 ms to 64.6 / 111.3 / 124.3 ms at the same CPU.

## During a backup

Measured 2026-11-21 with HeimDB 0.3.0 (`main` at the time).

### Setup

- The same two GCE n2-standard-4 instances as [Under load](#under-load). The database instance has a second 50 GB pd-balanced disk for the backups that go to another filesystem. The data is on the boot disk (50 GB pd-balanced, ext4).
- Data: `wpbench` (16 MB on disk), and `wpbench` grown to 1.97 GB with a table that no page reads (`wp_ballast`, copies of the post bodies). Each run started HeimDB on a copy of the same data directory, then warmed it as in Under load.
- Load: `cmd/loadgen` with reads and writes 95:5 at a fixed 28 pages a second (half of HeimDB's 95:5 maximum) on 64 connections, 120 seconds after a 10-second warm-up. `CALL heimdb_backup()` ran on its own connection at 15 seconds and every 30 seconds (4 backups a run).
- Cases: no backup; to `-backup-dir` on the data's filesystem; to the second disk; encrypted with `-backup-key` to the data's filesystem.
- "During" counts the pages whose time (from the scheduled start to the end) overlapped a backup. Each value is the average of 2 runs.

### Results

| Data | Case | Backup time | Pages during a backup (reads / writes) | Read p50 / p99 / max, during | Read p50 / p99 / max, other pages | Write p99, during | Write p99, other pages |
|---|---|---|---|---|---|---|---|
| 16 MB | No backup | — | — | — | 51 / 90 / 104 ms | — | 83 ms |
| 16 MB | Same filesystem | 13 ms | 5 / 1 | 45 / 70 / 70 ms | 50 / 86 / 101 ms | 18 ms | 79 ms |
| 16 MB | Second disk | 62 ms | 11 / 0 | 65 / 85 / 85 ms | 51 / 90 / 108 ms | — | 84 ms |
| 16 MB | Encrypted | 99 ms | 16 / 1 | 72 / 111 / 111 ms | 50 / 88 / 105 ms | 79 ms | 80 ms |
| 1.97 GB | No backup | — | — | — | 49 / 86 / 116 ms | — | 84 ms |
| 1.97 GB | Same filesystem | 19 ms | 7 / 1 | 57 / 78 / 78 ms | 49 / 88 / 111 ms | 63 ms | 80 ms |
| 1.97 GB | Second disk | 11.7 s | 1,187 / 71 | 3.6 s / 12.4 s / 13.4 s | 61 ms / 5.1 s / 6.8 s | 12.9 s | 4.3 s |
| 1.97 GB | Encrypted | 5.4 s | 577 / 34 | 63 / 369 / 527 ms | 49 / 91 / 203 ms | 467 ms | 84 ms |

- No errors, disconnects or lock wait timeouts in any run.
- To the same filesystem, the backup hard-links the data files, and the pages that overlap it are no slower than the others.
- To the second disk, the backup copies every file (about 170 MB a second) while holding the write lock. For the 11.7 seconds of each backup, no page finished, reads included: an `INSERT` that waits for the lock has already taken an AUTO_INCREMENT value, and a read-only transaction that commits while a value is unsaved tries to save it and waits for the same lock. After each backup, the queued pages took about 10 seconds to catch up, so the "other pages" are slow too. `heimdb_lock_wait_seconds_total` grew by 104 and 250 seconds in the two runs, and the server logged `heimdb_backup took 11.626s and paused all writes meanwhile` (and the same for each backup).
- Encrypted, the write lock is held only for the hard links; the 5.4 seconds are spent writing the `.tar.gz.age` file (125 MB for the 1.97 GB of data, as the ballast compresses well). The pages that overlap it compete for CPU and disk. Server CPU over the run rose from 121% to 140%. The two runs differed (overlapping read p99 627 and 127 ms).
- With `wpbench`, every case finishes in under 0.1 seconds and the differences are within the run-to-run variation.

## Memory

Measured 2026-10-16 and 17 on a GCE n2-standard-4, with the builds released as HeimDB 0.2.11; these paths have not been re-measured since. The table is shaped like `wp_postmeta` (same indexes) with 3.1 million rows and `meta_value` of about 20 bytes. Peak RSS is `VmHWM`, reset with `clear_refs` just before each statement, with a fresh server per statement. "No limit" ran in a 12 GiB cgroup; "limit" ran in a 4 GiB container with `-memory-limit 3276M`.

### Resident memory

| When | RSS | Live heap | Block cache |
|---|---|---|---|
| Just after opening | 477 MiB | 200 MiB | 127 MiB |
| After reading every row (`COUNT(*)`, `LIKE`) | 503 MiB | 195 MiB | 127 MiB |

About 70 MiB of the live heap does not depend on the number of rows (36 MiB of it is the collation weight tables). On a local machine with 500,000 rows, the live heap is 42 MiB after starting and 178 MiB after reading (cache 134 MiB): resident memory barely depends on table size.

### Operations that grow with the rows they touch (peak RSS)

| Operation | No limit | Limit 3,276 MiB |
|---|---|---|
| `SELECT meta_id FROM m ORDER BY meta_value DESC LIMIT 10` | 976 MiB | 925 MiB |
| `SELECT meta_value, COUNT(*) FROM m GROUP BY meta_value ORDER BY COUNT(*) DESC LIMIT 3` | 1,755 MiB | 1,845 MiB |
| `ALTER TABLE m ADD COLUMN extra ...` | 621 MiB | 602 MiB |
| `UPDATE m SET meta_value = CONCAT(meta_value, 'x')` | 24.1 s, 2,625 MiB | 2,626 MiB |
| `DELETE FROM m` | 36.7 s, 4,640 MiB | 1041, 2,730 MiB |
| Merging a branch that changed every row | 55 s, 4,439 MiB | 1041 after 53 s, 3,333 MiB |
| Merging a branch that added a column | 81 s, 5,477 MiB | 1041 after 64 s, 3,285 MiB |
| Restore after changing every row | 154 s, 10,540 MiB (killed by the 12 GiB cgroup 20 s later) | 1041 after 73 s, 3,115 MiB |

- With a limit, statements that would pass it stop with error 1041 and their transaction is rolled back; the data is unchanged and the next statement runs normally. Without a limit, Go's GC lets the heap grow to twice the live heap before collecting, so a large restore can be killed by the container. Run HeimDB with a memory limit (it defaults to 80% of the container's limit; see [`operations.md`](operations.md)).
- During a commit, Pebble also needs memory for writing the WAL (1.2 GB for a 1-million-row restore). This cannot be changed by settings.

### Loading a dump in one transaction

`mariadb-dump`'s default output (222 `INSERT`s of 14,000 rows each, then one `COMMIT`), 3.1 million rows:

| | Result |
|---|---|
| No limit | 69 s, peak 5,643 MiB |
| Limit 3,276 MiB | 1041 at `COMMIT` after all rows were written (the commit batch needs 1,942 MiB), peak 3,064 MiB |

With the 3,276 MiB limit, 1.5 million rows commit in 31 seconds (peak 2,712 to 2,810 MiB), and 2 million rows stop at `COMMIT` with 1041. Per row, an uncommitted write holds about 0.56 KB of live heap and the commit batch needs about 0.62 KB more. mysqldump's default output runs each `INSERT` in its own autocommit transaction, so it does not hit this.

## How to measure

Pages, writes and concurrency (`scripts/bench/`):

```sh
docker compose -f scripts/bench/docker-compose.yml up -d     # WordPress for MySQL (:18181) and HeimDB (:18182)
scripts/bench/pages.py --newest 5003 --cat 1 --runs 30       # page times (median, p95, DB time, query count)
ab -c 10 -t 30 http://localhost:18182/                       # concurrency
docker compose -f scripts/bench/docker-compose.yml run --rm -T cli-heimdb wp eval-file - < scripts/bench/writes.php
```

Load (`cmd/loadgen`, `scripts/bench/load/`):

```sh
go build -o loadgen ./cmd/loadgen
./loadgen -dsn 'root:pw@tcp(10.0.0.2:3316)/wpbench' -pages scripts/bench/load/pages-read.json -conns 32 -duration 60s -warmup 10s
./loadgen -dsn ... -pages scripts/bench/load/pages-read.json,scripts/bench/load/pages-write.json -write 0.05 -conns 32
./loadgen -dsn ... -pages scripts/bench/load/pages-read.json -conns 128 -rate 33 -duration 60s   # 33 pages a second
```

Backups under load (`-side` runs a statement on its own connection during the measured window):

```sh
./loadgen -dsn ... -pages scripts/bench/load/pages-read.json,scripts/bench/load/pages-write.json -write 0.05 -conns 64 -rate 28 \
  -duration 120s -interval 1s -side 'CALL heimdb_backup()' -side-first 15s -side-every 30s
```

On GCE, `scripts/bench/load/gce.sh` creates the two instances, installs the servers and the dump, runs the measurements and fetches the results (`GCE_PROJECT` is required; `create`, `push`, `run`, `fetch`, `delete`). `agg.py` turns the results into the Under load tables, and `run.sh K` with `backup-agg.py` gives the backup table.

Measure on Linux: values measured on macOS differ.
