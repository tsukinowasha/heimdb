# HeimDB

A branching database built only for WordPress. The plan and background are in `docs/plan.md`.

## Working rules

- Work through the development stages one at a time. Do not start the next stage until its completion criteria are met. At the start of each stage, present a plan and get approval before implementing.
- The SQL engine is a fork of go-mysql-server v0.20.0 in `internal/sqlengine` (`docs/engine.md`). For interfaces, the code in `internal/sqlengine` and the `memory` package are the reference, not recollection or explanations on the web.
- When changing the engine, add a short row to the change list in `docs/engine.md` (what was changed where, and why), and write the details and reasons in the "Engine changes" section of that stage's record in `docs/stages/`. Add tests, and put a change marker (`// Modified by Tsukinowa Inc.: ...`) at the top of each file whose contents changed. Record it the same way when bringing in upstream fixes. The character set tables (`sql/encodings`) are read from the original module and not upgraded (collation weights affect the data format).
- Before changing the key design or encoding, update `docs/plan.md` (or `docs/design.md`) first, then implement. Keep existing data readable ("Data format version" in `docs/plan.md`). When changing it, raise the version and migrate existing data at startup.
- Write unit tests for encoding, MVCC reads and branch layering. Check ordering properties with property tests.
- Judge WordPress compatibility with the procedure in "Validation method" in `docs/plan.md`, always comparing against the baseline. Do not fix failures by guessing; reproduce them with the queries from the log first, then fix.
- Even where the spec leaves behavior undefined, match what MySQL actually does (for example, the order of ties). When matching, confirm the basis on real MySQL with `EXPLAIN` and similar.
- It is fine to be WordPress-only. SQL that WordPress does not send is unsupported, by returning an explicit error. Never drop the connection or panic.
- Within a stage, fix only regressions caused by that stage's changes and existing differences that give wrong results on the paths of WordPress core or WooCommerce. Record the others in `docs/differences.md` as candidates for later stages. A stage is 4 to 5 commits, with at most 2 reviews.
- At the end of each stage, report the results of checking the completion criteria (test counts and the difference from the baseline).
- Write the documents, the stage records and commit messages in English. Only `README.ja.md` is in Japanese; keep it in step with `README.md`.
- Write the product name as `HeimDB` (in documents, comments, logs, and the `HeimDB: ` prefix of errors). Names typed or referenced as code stay lowercase `heimdb` (the command name, module path, `heimdb_*` procedures, `HEIMDB_*` environment variables, file paths).
