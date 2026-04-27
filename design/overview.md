# Sync engine design

Bidirectional sync between a local directory and an S2 remote. The CLI (`s2 sync` / `s2 watch`) and the GUI (Wails tray app) share the same core.

## Entry points

```
CLI (cmd/sync, cmd/watch)            GUI (gui/, Wails)
       │                                    │
       │                          internal/service.SyncService
       │                          (Start / Stop / Status)
       └────────────┬───────────────────────┘
                    ▼
         internal/sync.Open()
   = auth + client + state.db
                    ▼
         RunInitialSync ┐
         RunIncrementalSync ┘  ← one cycle = runner.go
         RunWatchLoop          ← runner driven by fsnotify + ticker
```

All three callers (CLI sync, CLI watch, GUI service) wire through `sync.Open()` and reduce to "spin the runner once".

## One cycle (runner.go)

```
Walk local → PollChanges(cursor) → expand dir_events → Compare (3-way)
           → safety check → Execute → Save state
```

- **Walk**: full local scan + hash + case-collision detection (`walk.go`, `case.go`)
- **PollChanges**: fetch remote changes since `cursor`. Empty cursor → bootstrap (`bootstrap.go`)
- **dir_events**: expand `mkdir` / dir delete / dir move / dir put into a per-file plan (`dir_events*.go`)
- **Compare**: 3-way merge → `[]SyncPlan{Path, Action, RevisionID, Hash}` (`compare.go`)
- **Execute**: dispatch each plan as push / pull / conflict / delete (`executor*.go`). Plans are independent — one failure doesn't block the rest.

## 3-way merge (the core)

Diffing local vs remote alone can't tell you which side changed. We keep the last-synced state (the **archive**) as the comparison anchor.

| local   | remote  | archive | action                                           |
|---------|---------|---------|--------------------------------------------------|
| changed | same    | same    | push                                             |
| same    | changed | same    | pull                                             |
| changed | changed | same    | conflict (move loser to `.sync-conflict-*`, local wins) |
| absent  | same    | same    | delete-remote                                    |
| same    | absent  | same    | delete-local                                     |

The archive is a **disposable cache**. If it's lost we fall back to an initial sync and rebuild it; user data isn't damaged.

## state.db (`.s2/state.db`)

SQLite (WAL), managed in `statedb.go`. Three tables:

| table          | role                                                                                   |
|----------------|----------------------------------------------------------------------------------------|
| `state_meta`   | cursor / endpoint / user_id / base_path / collision_keys                               |
| `files`        | the archive itself (path → local_hash, content_version, revision_id)                   |
| `pushed_seqs`  | seqs of our own pushes — filtered out of the next poll so we don't pull our own writes |

Schema mismatches or corruption are quarantined to `.corrupt.<ts>/` and a fresh empty DB is created, which naturally falls back to an initial sync. Invariant: 1 sync root ⇔ 1 token ⇔ 1 state.db.

## Why it doesn't break (key invariants)

| Scenario                                        | Mechanism                                                                                       |
|-------------------------------------------------|-------------------------------------------------------------------------------------------------|
| Concurrent push overwrites                      | `If-Match: <archive content_version>` CAS. 412/409 → conflict                                   |
| Remote changes again mid-pull                   | Fetch by pinned `revision_id` (`/api/revisions/:id`); fall back to path if unavailable          |
| Re-pulling our own push                         | Push seq recorded in `pushed_seqs`, filtered out of poll results                                |
| Local changes mid-pull                          | Local hash checked before/after the `.s2tmp` write; mismatch → abort and convert to conflict    |
| Crash mid-write                                 | `.s2tmp` + `os.Rename` for atomic file swap; state changes inside SQLite tx                     |
| Archive corruption                              | Quarantine to `.corrupt`, recreate empty DB, fall back to initial sync                          |
| Runaway deletion                                | Abort if deletes > 50% of tracked files (`--max-delete`); `--force` to override                 |
| Server-supplied path escaping the sync root     | `safeJoin` rejects writes outside the sync root (`safe_join.go`)                                |
| Large subtree (>100k) blowing up snapshot (413) | Recursive `ListDir` + S0 cursor pin + delta replay (fixpoint, capped at 20 rounds)              |
| `Foo.txt` / `foo.txt` on a case-insensitive FS  | NFC-normalize, sync the lex-first only, warn on collisions                                      |

## Watch (resident mode)

`RunWatchLoop` (`watcher.go`) keeps spinning the runner:

- fsnotify events on local files → 2s debounce → sync
- Remote poll every `--poll-interval` (default 10s) → sync if anything changed
- `syncMu` serializes runs to one at a time; ctx cancellation gives a graceful shutdown

The GUI service (`internal/service/sync_service.go`) calls the same `RunWatchLoop`. The only differences are Start/Stop control and the log sink (Wails events).

## Module layout

```
cmd/                CLI: sync / watch / login / logs / version
gui/                Wails GUI (React frontend + tray)
internal/
  service/          Start/Stop/Status wrapper for the GUI + autostart
  sync/             sync core (runner / walk / compare / executor / dir_events
                    / bootstrap / state / case / exclude / safe_join / watcher)
  client/           S2 HTTP client (changes / files / snapshot / upload)
  auth/             OAuth session (keyring-persisted)
  oauth/            OAuth flow (PKCE + loopback + RFC 7591 DCR)
  log/              slog wrapper + Wails sink
  types/            API and internal types
```

Dependency direction is one-way: `cmd` / `gui` → `service` → `sync` → `client` / `auth` → `oauth` / `types`.

## Constraints

- Chunked upload for large files (>10MB) doesn't support `If-Match` → concurrent edits are last-writer-wins
- Conflict resolution is fixed to local-wins (no 3-way content merge)
- Local change detection is full walk + hash, not inotify-diff
- Symlinks and special files are unsupported; empty local directories are not synced
- Sync scope is whatever falls under the token's `base_path`. Different scopes need a different token and a different directory.
