# Verification ledger

This ledger separates local source evidence from release, deployment, and live production evidence.

| Area | Status | Evidence |
| --- | --- | --- |
| Merged Go source | Verified | `origin/main` is `3cc7619e3bb9202cf8ddd5c088c03ec94ea58990`. |
| Baseline tests | Verified | `go test ./... -count=1` passed on the merged source. |
| Baseline race tests | Verified | `go test -race ./... -count=1` passed on the merged source. |
| Baseline quality gates | Verified | `make check` passed on the merged source. |
| Confirmed defects | Verified | Plaintext Clyde transport and cross file hunk identity each failed through a public boundary before repair. |
| Focused repairs | Verified | Both regression tests pass after repair. |
| Local image | Verified | Canonical release builds produced static Linux AMD64 and ARM64 binaries. Both pinned nonroot images built and ran their stamped versions. |
| Final branch gates | Verified | `go test ./... -count=1`, `go test -race ./... -count=1`, and `make check` passed after the branch changes. |
| Hostile input and lifecycle repetitions | Verified | Signature, malformed body, body limit, duplicate delivery, keyed concurrency, and shutdown tests passed 20 race enabled runs. |
| Continuous integration | Verified | The [CI run](https://github.com/agoodkind/pr-review-agent/actions/runs/31766199235) passed on `3cc7619e3bb9202cf8ddd5c088c03ec94ea58990`. |
| Immutable release | Verified | [Release `202608140314-8-3cc7619`](https://github.com/agoodkind/pr-review-agent/releases/tag/202608140314-8-3cc7619) targets the source commit. Immutable releases are enabled. Checksums and all archive attestations passed. |
| Canonical install | Verified | The `go-makefile` installer from `main` required attestation and installed `202608140314-8-3cc7619`. The executable reported source `3cc7619`. |
| Published image | Verified | The [release run](https://github.com/agoodkind/pr-review-agent/actions/runs/31766199378) published and attested `sha256:b8cf108c0dbcec251afa956c1e540aa75fcb85fa7e1861e75b0f0ee20fe119cd`. Its runnable manifests are Linux AMD64 and ARM64. |
| Rollback point | Recorded | The latest successful [production deployment](https://github.com/agoodkind/pr-agent-cf/actions/runs/31709639789) recorded Worker version `241c1c32-b505-40c3-8153-d42d3d228660` and container digest `sha256:e79dcc09e5d44cefb116fb5fb0fb96274ae9d302cf8221e1fe900b025cc5973b`. Both production health paths returned HTTP 200 before the cut. |
| Cloudflare deployment | Ready, pending authorization | [Deployment PR 13](https://github.com/agoodkind/pr-agent-cf/pull/13) is clean and pins the verified image digest. Record the deployed Worker version, image digest, readiness, routing, startup, and secret free logs after the authorized merge. |
| GitHub App | Cleanup pending authorization | Public metadata confirms App ID `4571682`, bot identity, `pull_request` and `issue_comment` subscriptions, and `issues: write`. Direct settings evidence and removal of the unused issue access remain pending. |
| Live review lifecycle | Pending authorization | Record one check and review per head, review body, inline findings, decisions, duplicate suppression, new head review, silent resolution, and absence of comments or replies. |
| Python fork cleanup | Pending authorization | Record disabled workflows and archived repository state without deleting history or artifacts. |
