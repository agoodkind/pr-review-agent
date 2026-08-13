# pr-review-agent

A Go executable for end-to-end pull request reviews.

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
