# s2sync GUI

Wails (Go + React-TS) desktop frontend for s2-sync. Sits in the system tray, manages autostart, and drives the same sync engine as the `s2` CLI.

## Prerequisites

- Go (version from `../go.mod`)
- Node.js 20+
- [Wails CLI](https://wails.io/docs/gettingstarted/installation) v2.12.0
  ```sh
  go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0
  ```
- **Linux only**: `libgtk-3-dev`, `libwebkit2gtk-4.1-dev`, `pkg-config`

## Dev

```sh
wails dev
```

Vite dev server with hot reload for the frontend. Go methods are callable from the browser devtools at http://localhost:34115.

## Build

```sh
wails build                          # macOS / Windows
wails build -tags webkit2_41         # Linux (WebKitGTK 4.1)
```

Output lands in `build/bin/`:
- macOS: `s2sync.app`
- Windows: `s2sync.exe`
- Linux: `s2sync` binary

## Release

CI (`.github/workflows/release.yml`) runs the matrix build on tag push (`v*`) and uploads archives to GitHub Releases alongside the CLI. See the top-level [README](../README.md#release).
