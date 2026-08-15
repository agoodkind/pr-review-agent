# Verification ledger

This ledger separates verified source, release, deployment, and live production behavior from remaining work.

| Area | Status | Evidence |
| --- | --- | --- |
| Merged source | Verified | [PR 25](https://github.com/agoodkind/pr-review-agent/pull/25) merged as `b80e918132451c517342b753f3a8f57b9ee2d0ea`. |
| Required gates | Verified | Full tests, race tests, `make check`, and `make build` passed locally. Every job in [CI run 31869841417](https://github.com/agoodkind/pr-review-agent/actions/runs/31869841417) passed. |
| Immutable release | Verified | [Release `202608150640-12-b80e918`](https://github.com/agoodkind/pr-review-agent/releases/tag/202608150640-12-b80e918) targets the merged source. [Release run 31870002540](https://github.com/agoodkind/pr-review-agent/actions/runs/31870002540) verified checksums, provenance, release attestations, and container attestations. |
| Canonical install | Verified | The canonical installer selected `202608150640-12-b80e918` on Linux AMD64. The installed binary reported that version, source `b80e918`, and build time `2026-08-15T06:41:00Z`. |
| Published image | Verified | The attested image index is `sha256:0f396db8a7fcca6c4271bd460b9ea2c8d4ebed727ffe6d467cc08e3d46c7ce2c`. Its only runnable manifest is Linux AMD64 at `sha256:45d821e2d9c8546c5b66ccf564399de3a3c01ce0ca77c313266fc69001fba66b`. |
| Rollback point | Recorded | The predeployment Worker version is `bf204594-96ba-4a30-8899-d779f3acd10e`. Its Cloudflare image is `sha256:a35c568abc4a91b7cea03f347b18b434942e92aa355295fd7ddf8b4adffbd0b5`. |
| Cloudflare deployment | Verified | [Deployment PR 24](https://github.com/agoodkind/pr-agent-cf/pull/24) pinned the verified image. [Deployment run 31870323322](https://github.com/agoodkind/pr-agent-cf/actions/runs/31870323322) produced Worker version `a47480ce-0d17-4aea-a457-4213750b03e2`. The routed health endpoint returned `{"status":"ok"}`. |
| GitHub App identity | Verified | Public metadata confirms App ID `4571682`, name `goodkind.io PR Agent`, and bot login `goodkind-io-pr-agent[bot]`. Production used that identity to resolve its thread. |
| GitHub App cleanup | Pending | The App still has broad repository permissions and seven event subscriptions. The service needs checks write, contents read, pull requests write, and only the `pull_request` event. |
| Live approval | Verified | [Proof PR 19](https://github.com/agoodkind/pr-review-agent/pull/19) head `4b377765192d5823229b94b72e5d675f5a013070` received exactly one successful `PR-Agent Review` check and one approval for that head. |
| Live request changes | Verified | Head `acc24ff7cd9ef927d596b6b3e1c56e6c290fa6c0` received one request changes review and one importance 9 inline finding. The bot posted no reply or issue comment. |
| Editable summary | Verified | The request changes review used one visible `## Findings` body. The next approval edited that body back to a hidden marker. |
| Duplicate suppression | Verified | Closing and reopening one completed head left its check count and bot review count at one. |
| Silent thread resolution | Verified | The new production head resolved thread `PRRT_kwDOT3jWKM6ZdyZW`. GitHub records `goodkind-io-pr-agent[bot]` as the resolver, with no reply. |
| Check output | Verified | The successful check displayed `Review complete.` and linked to the reviewed repository. |
| Secret free evidence | Verified | Inspected release, deployment, health, and live review output exposed no credential values. |
| Python retirement | Pending | Keep the Python repository active until GitHub App cleanup and this ledger merge. |
