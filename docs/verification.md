# Verification ledger

This ledger separates local source evidence from release, deployment, and live production evidence.

| Area | Status | Evidence |
| --- | --- | --- |
| Merged Go source | Verified | `origin/main` was `fe255ee13ccf6b3bc1db711d1ef72c4b633d61eb` before this change. |
| Baseline tests | Verified | `go test ./... -count=1` passed on the merged source. |
| Baseline race tests | Verified | `go test -race ./... -count=1` passed on the merged source. |
| Baseline quality gates | Verified | `make check` passed on the merged source. |
| Confirmed defects | Verified | Plaintext Clyde transport and cross file hunk identity each failed through a public boundary before repair. |
| Focused repairs | Verified | Both regression tests pass after repair. |
| Local image | Verified | Canonical release builds produced a static Linux AMD64 binary. Its pinned nonroot image ran the stamped version. |
| Final branch gates | Verified | `go test ./... -count=1`, `go test -race ./... -count=1`, and `make check` passed after the branch changes. |
| Hostile input and lifecycle repetitions | Verified | Signature, malformed body, body limit, duplicate delivery, keyed concurrency, and shutdown tests passed 20 race enabled runs. |
| Continuous integration | Pending | Record the branch run and every required check. |
| Immutable release | Pending | Record the release tag, source commit, immutability setting, checksums, and attestations. |
| Canonical install | Pending | Record installer output, executable version, and source commit. |
| Published image | Pending | Record the registry digest, Linux architectures, provenance, and attestation. |
| Rollback point | Pending | Record the current Worker version and image digest before deployment. |
| Cloudflare deployment | Pending authorization | Record the deployed Worker version, image digest, readiness, routing, startup, and secret free logs. |
| GitHub App | Pending authorization | Record App ID `4571682`, installation scope, webhook URL, bot identity, permissions, and subscribed events. |
| Live review lifecycle | Pending authorization | Record one check and review per head, review body, inline findings, decisions, duplicate suppression, new head review, silent resolution, and absence of comments or replies. |
| Python fork cleanup | Pending authorization | Record disabled workflows and archived repository state without deleting history or artifacts. |
