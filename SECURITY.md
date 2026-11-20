# Security policy

## Reporting a vulnerability

Please do not report security vulnerabilities in public issues, pull requests or discussions.

Report them privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability** (GitHub's private vulnerability reporting). Include the HeimDB version, how to reproduce the problem, and its impact as you understand it.

We aim to acknowledge a report within 5 business days, and to agree with you on a fix and a disclosure date. Fixes are released as a new version on Docker Hub ([`tsukinowa/heimdb`](https://hub.docker.com/r/tsukinowa/heimdb)) and GitHub, with a GitHub security advisory.

## Supported versions

HeimDB is a development preview. Only the latest release receives security fixes; upgrade to it (see "HeimDB のバージョンを上げる" in [`docs/operations.md`](docs/operations.md)).

## Scope

In scope: the HeimDB server (`cmd/heimdb`), its SQL engine fork (`internal/sqlengine`), and the Docker image. Examples: authentication or privilege bypass (including access across branches), SQL that reads or writes files on the server, crashes or memory exhaustion triggered by a client, and data corruption.

Out of scope: the metrics endpoint when exposed beyond localhost (it has no authentication by design; see `-metrics-addr` in [`docs/operations.md`](docs/operations.md)), weak passwords chosen by the operator, and the scripts under `scripts/` and the compatibility suite under `compat/`, which are development tools.

Vulnerabilities in dependencies are tracked by CI (`govulncheck` and the advisories of the forked modules; see "脆弱性の確かめ方" in [`docs/operations.md`](docs/operations.md)). Report them to us only if HeimDB is actually affected.

---

脆弱性は、公開の Issue や PR ではなく、GitHub の **Security** タブの **Report a vulnerability**（非公開の脆弱性報告）で知らせてください。HeimDB のバージョン、再現の手順、影響を書いてください。5 営業日のうちに返事をし、直しと公開の日を相談します。直しは最新のバージョンにだけ入ります。
