# s2

CLI for [S2](https://scopeds.dev) — bidirectional file sync with S2 remote storage.

## Install

Download a binary from [GitHub Releases](https://github.com/selfbase-dev/s2-sync/releases) and place it in your `PATH`.

## Usage

```sh
# Authenticate
s2 login

# One-shot bidirectional sync
s2 sync ./local-dir

# Watch mode (continuous sync)
s2 watch ./local-dir
```

The sync root is determined by the token's `base_path` — root or scoped, any token works the same way. To sync a different scope, issue a new token.

Token can also be set via `S2_TOKEN` env var.

Add a `.s2ignore` file to exclude patterns from sync.

## Test

```sh
# Unit tests
go test ./...

# E2E tests (requires running S2 server)
S2_ENDPOINT=http://localhost:8888 S2_TOKEN=s2_xxx go test -tags e2e ./internal/sync/
```

**E2E requirements:**
- `S2_TOKEN`: token with `can_delegate=true` and full read/write access (root or scoped — any scope works)

## Release

```sh
git tag v0.2.0
git push origin v0.2.0
```

GitHub Actions runs GoReleaser on tag push and publishes binaries to GitHub Releases.
