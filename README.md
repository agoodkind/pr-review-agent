# pr-review-agent

A Go executable for end-to-end pull request reviews.

The service posts one check and one complete review for each pull request head. It silently resolves fixed findings on later heads.

## Install

Install the latest release through the canonical go-makefile installer:

```bash
curl -fsSL https://raw.githubusercontent.com/agoodkind/go-makefile/main/install.sh | \
    bash -s -- --repo agoodkind/pr-review-agent --binary pr-review-agent
```

## Develop

Run the shared build, lint, and test pipeline:

```bash
make check
```

List every available target:

```bash
make help
```

## Operate

Use the [operations guide](docs/operations.md) for runtime configuration, image verification, health checks, and recovery.

Use the [verification ledger](docs/verification.md) to record release, deployment, GitHub App, live review, and retirement evidence.
