# MySQL compatibility suite

This suite runs statements over the same connection as WordPress (wpdb and mysqli) and compares the returned values with a record taken on real MySQL 8.0 (stage 91 in `docs/plan.md`). It records values exactly as the strings mysqli returns, the error message, number and SQLSTATE, `rows_affected`, `insert_id`, and `SHOW WARNINGS`. The tests do not write expected values. The same tests run on real MySQL and on the database under test, and the records are compared side by side.

The full PHPUnit comparison ("Validation method" in `docs/plan.md`, about 30 minutes) does not see differences that do not make a test fail. This suite looks at small differences from MySQL (warnings, error messages, the string form of doubles, collation) in a few seconds. It is not limited to HeimDB: you can use it to check any MySQL-compatible database for WordPress.

## Running

```sh
# HeimDB (the database name is set with -db. The tests drop and recreate WordPress tables, so use dedicated data)
heimdb -addr 127.0.0.1:3338 -data /tmp/compat-data -root-password root -db heimdb_compat &
scripts/compat/run.sh 3338 /tmp/heimdb.json
go run ./cmd/compatdiff compat/baseline/mysql-8.0.json /tmp/heimdb.json
```

- `scripts/compat/run.sh <port> <out.json>` runs `vendor/bin/phpunit -c /compat/phpunit.xml` in a PHPUnit container (the same image as `scripts/wpphp`) with wordpress-develop (`WP_DEVELOP_DIR`, default `../wordpress-develop` next to this repository) mounted at `/app` and this directory mounted at `/compat`. The database is `heimdb_compat` (`WP_TESTS_DB_NAME`), and the password is `WP_TESTS_DB_PASSWORD` (default root). On real MySQL, the database is created if it does not exist.
- `cmd/compatdiff` lists values that differ and records found on only one side, and returns exit code 1 if there are any. Intended differences, and known differences not fixed yet, go in `known-differences.txt` with a reason. When one is fixed and no longer differs, the tool says so; then delete that line.

## Connection check

First, the suite checks that the connection's character set and collation are the same as WordPress's (`utf8mb4`, `utf8mb4_unicode_520_ci`) and that `sql_mode` has none of the modes wpdb removes (strict and others) left, and stops if not (exit code 2). If the client character set is left at its default (latin1 and so on), values are stored corrupted and differences are misread. `sql_mode` itself is recorded (wpdb only removes modes from the server's `sql_mode`, so differences in the server's default show up).

## Retaking the baseline

```sh
docker run -d --name realmysql -e MYSQL_ROOT_PASSWORD=root -e MYSQL_DATABASE=wordpress -p 3307:3306 mysql:8.0
scripts/compat/run.sh 3307 /tmp/mysql-a.json
scripts/compat/run.sh 3307 /tmp/mysql-b.json
cmp /tmp/mysql-a.json /tmp/mysql-b.json && cp /tmp/mysql-a.json compat/baseline/mysql-8.0.json
```

Replace it only after confirming that two runs give the same result (so that values that change from run to run, such as the order of rows that compare equal, are not recorded). The current record is from MySQL 8.0.46.

## Adding tests

Put a class that extends `Compat_TestCase` in `tests/`. `Compat::observe_sql(label, sql)` runs a statement and records the result, error, row count and warnings, and `Compat::observe(label, value)` records any value. The key is "class::test::label".

- WordPress tests wrap each test in a transaction and roll it back at the end. `CREATE TABLE` is rewritten to a temporary table, so create tables in `set_up_before_class` and drop them in `tear_down_after_class`.
- Do not record values that change with the run order or the time (IDs, `NOW()`). Record IDs as differences, create posts with fixed dates, and set the order so that no rows compare equal.

## License

This directory is built on the WordPress test framework, so it is GPL-2.0-or-later ([LICENSE](LICENSE)). It is not part of the HeimDB Go module or the Docker image. `cmd/compatdiff` is Apache License 2.0, like HeimDB.
