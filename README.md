# s2-sync

File sync client for [S2](https://scopeds.dev) storage.

- **`s2`** — CLI (one-shot and watch-mode bidirectional sync)
- **`s2sync`** — desktop GUI with system tray and autostart (macOS / Windows / Linux)

> ⚠️ Alpha. Binaries are not yet code-signed. There is no auto-update — to upgrade, download a new release.

## Install

Grab the [latest release](https://github.com/selfbase-dev/s2-sync/releases/latest).

### CLI

| Platform | Asset |
|---|---|
| macOS (Apple Silicon) | `s2_darwin_arm64.tar.gz` |
| macOS (Intel) | `s2_darwin_amd64.tar.gz` |
| Linux x86_64 | `s2_linux_amd64.tar.gz` |
| Linux arm64 | `s2_linux_arm64.tar.gz` |
| Windows x86_64 | `s2_windows_amd64.zip` |
| Windows arm64 | `s2_windows_arm64.zip` |

Extract the archive and put `s2` on your `PATH`.

### GUI

| Platform | Asset | First launch |
|---|---|---|
| macOS (Apple Silicon) | `s2sync_darwin_arm64.zip` | Unzip, move `s2sync.app` to `/Applications`, **right-click → Open** once (unsigned) |
| Windows x86_64 | `s2sync_windows_amd64.zip` | Unzip and run `s2sync.exe`. On SmartScreen: **More info → Run anyway** |
| Linux x86_64 | `s2sync_linux_amd64.tar.gz` | Requires `libwebkit2gtk-4.1-0`. `tar -xzf` and run `./s2sync` |

### Verify downloads

Each release includes `checksums.txt`:

```sh
shasum -a 256 -c checksums.txt --ignore-missing
```

On Windows: `Get-FileHash .\s2sync_windows_amd64.zip -Algorithm SHA256` and compare against the matching line in `checksums.txt`.

## CLI usage

```sh
s2 login                # opens your browser for OAuth sign-in
s2 sync  ./local-dir    # one-shot bidirectional sync
s2 watch ./local-dir    # continuous
```

`s2 login` runs the OAuth 2.1 + PKCE + loopback flow: a one-shot HTTP listener binds to `127.0.0.1:<random-port>`, your browser opens to consent at the S2 endpoint, and the issued tokens are stored in your OS keychain. Set `S2_TOKEN=s2_xxx` (or `--token`) to bypass OAuth for CI / scripts — that path uses a fixed token and does not auto-refresh.

The sync root is the grant's `base_path` — root or scoped grants both work. To sync a different scope, re-consent with different paths in the dashboard. Add `.s2ignore` to exclude patterns.

## Develop

### CLI

```sh
go test ./...

# E2E (needs a running S2 server; token must have can_delegate=true
# and full read/write access)
S2_ENDPOINT=http://localhost:8888 S2_TOKEN=s2_xxx \
  go test -tags e2e ./internal/sync/
```

### GUI

See [gui/README.md](gui/README.md).

## Release

```sh
git tag vX.Y.Z
git push origin vX.Y.Z
```

GitHub Actions runs GoReleaser + a Wails build matrix on tag push and publishes all CLI and GUI artifacts (with aggregated `checksums.txt`) to GitHub Releases.
