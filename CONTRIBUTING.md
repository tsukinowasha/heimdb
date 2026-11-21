# Contributing to HeimDB

Thank you for your interest in HeimDB. Bug reports, reports of differences from MySQL, and pull requests are welcome.

## Scope

HeimDB is built only for WordPress (core, wp-cli, mysqldump and plugins). SQL that WordPress and its plugins do not issue is deliberately unsupported and returns an explicit error; we keep it that way rather than add general MySQL features. Where MySQL's behaviour is unspecified (for example, the order of rows that compare equal), HeimDB follows what MySQL actually does.

## Reporting issues

- **Security issues**: do not open a public issue. See [`SECURITY.md`](SECURITY.md).
- **Bugs and differences from MySQL**: open an issue with the HeimDB version (the Docker image tag, or `heimdb_build_info` in the metrics), the SQL that behaves differently (from the general log or the slow query log if possible), what MySQL 8.0 returns, and what HeimDB returns. If a plugin triggers it, name the plugin and its version.
- Known differences are listed in [`docs/differences.md`](docs/differences.md).

## Pull requests

For anything larger than a small fix, open an issue first so we can agree on the approach.

- Reproduce the problem with the query before fixing it, and add a test. Encoding, MVCC reads and branch overlays need unit tests; ordering properties need property tests.
- Changes to the SQL engine (`internal/sqlengine`, a fork of go-mysql-server v0.20.0) add a line to the change list in [`docs/engine.md`](docs/engine.md) and a `// Modified by Tsukinowa Inc.: ...` line at the top of each file whose code changes.
- Changes to the key design or encoding update [`docs/plan.md`](docs/plan.md) first, keep existing data readable, and bump the data format version with a migration at startup.
- Before pushing:

  ```sh
  test -z "$(gofmt -l cmd internal)"
  go vet $(go list ./... | grep -v internal/sqlengine)
  go test -race -timeout 30m $(go list ./... | grep -v internal/sqlengine)
  go test ./internal/sqlengine/...      # if you changed the engine
  ```

- WordPress compatibility is judged against a MySQL baseline: the compatibility suite in [`compat/`](compat/README.md) (seconds), and for larger changes WordPress's PHPUnit suite (see "Validation method" in [`docs/plan.md`](docs/plan.md)).
- Documents under `docs/` are in English. Issues and pull requests may be written in English or Japanese.

## Developer Certificate of Origin

Every commit in a pull request must be signed off under the [Developer Certificate of Origin 1.1](https://developercertificate.org/), certifying that you wrote the change or otherwise have the right to submit it under the project's license (Apache License 2.0; GPL-2.0-or-later for `compat/`). Add the line with `git commit -s`:

```
Signed-off-by: Your Name <you@example.com>
```

The name and email must match the commit's author. A CI check rejects pull requests with commits that lack it. To sign off existing commits: `git rebase --signoff origin/main` and force-push.

By contributing you agree that your contribution is licensed under the license of the files you change.

