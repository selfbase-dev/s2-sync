import { useEffect, useRef, useState } from "react";
import {
  HasToken,
  SaveToken,
  ClearToken,
  PickFolder,
  StartSync,
  StopSync,
  GetStatus,
  Endpoint,
  IsAutostartEnabled,
  SetAutostart,
  DefaultFolder,
  SavedFolder,
  SetSavedFolder,
  EnsureFolder,
  OpenFolder,
  ConfirmDisconnect,
} from "../wailsjs/go/main/App";
import { EventsOn } from "../wailsjs/runtime/runtime";
import "./App.css";

type Status = "idle" | "running" | "stopping" | "error";

interface StateInfo {
  status: Status;
  mount?: { path: string };
  error?: string;
  lastSync?: string;
}

interface Event {
  type: "started" | "stopped" | "synced" | "error" | "log";
  message?: string;
  time: string;
}

const MAX_LOG_LINES = 200;

const STATUS_LABEL: Record<Status, string> = {
  idle: "Idle",
  running: "Syncing",
  stopping: "Stopping…",
  error: "Error",
};

function App() {
  const [endpoint, setEndpoint] = useState("");
  const [hasToken, setHasToken] = useState<boolean | null>(null);
  const [folder, setFolder] = useState("");
  const [defaultFolder, setDefaultFolder] = useState("");
  const [state, setState] = useState<StateInfo>({ status: "idle" });
  const [logs, setLogs] = useState<Event[]>([]);
  const [autostart, setAutostartState] = useState(false);
  const activityRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    Endpoint().then(setEndpoint);
    DefaultFolder().then(setDefaultFolder);
    IsAutostartEnabled().then(setAutostartState);
    Promise.all([HasToken(), SavedFolder()]).then(([hasTok, saved]) => {
      setHasToken(hasTok);
      if (saved) setFolder(saved);
    });
    GetStatus().then((s) => {
      setState(s as StateInfo);
      if (s.mount?.path) setFolder(s.mount.path);
    });
    const off = EventsOn("sync:event", (ev: Event) => {
      setLogs((prev) => {
        const next = [...prev, ev];
        return next.length > MAX_LOG_LINES ? next.slice(-MAX_LOG_LINES) : next;
      });
      GetStatus().then((s) => setState(s as StateInfo));
    });
    return () => {
      off();
    };
  }, []);

  useEffect(() => {
    if (activityRef.current) {
      activityRef.current.scrollTop = activityRef.current.scrollHeight;
    }
  }, [logs]);

  const handlePickFolder = async () => {
    const f = await PickFolder();
    if (f) setFolder(f);
  };

  const handleStart = async () => {
    if (!folder) return;
    try {
      await EnsureFolder(folder);
      await StartSync(folder);
    } catch (e: any) {
      setState({ status: "error", error: String(e?.message ?? e) });
    }
  };

  const handleStop = async () => {
    await StopSync();
  };

  const handleAutostart = async (next: boolean) => {
    try {
      await SetAutostart(next);
      setAutostartState(next);
    } catch {
      // checkbox stays at current value
    }
  };

  const handleDisconnect = async () => {
    const ok = await ConfirmDisconnect();
    if (!ok) return;
    await ClearToken();
    setHasToken(false);
    setFolder("");
    setState({ status: "idle" });
    setLogs([]);
  };

  if (hasToken === null) {
    return <div className="app" />;
  }

  if (!hasToken) {
    return (
      <Welcome
        endpoint={endpoint}
        defaultFolder={defaultFolder}
        initialFolder={folder || defaultFolder}
        onConnected={(f) => {
          setHasToken(true);
          setFolder(f);
        }}
      />
    );
  }

  const status = state.status;
  const running = status === "running";
  const stopping = status === "stopping";
  const lastSync = state.lastSync
    ? new Date(state.lastSync).toLocaleTimeString()
    : null;

  return (
    <div className="app">
      <header className="app-header">
        <div className="brand">
          <div className="brand-mark">S2</div>
          <div>
            <h1>s2sync</h1>
            <div className="brand-sub">{endpoint}</div>
          </div>
        </div>
        <button
          className="icon-btn"
          onClick={handleDisconnect}
          title="Disconnect"
          aria-label="Disconnect"
        >
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <path
              d="M6 4l-4 4 4 4M2 8h9M11 2h2a1 1 0 011 1v10a1 1 0 01-1 1h-2"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </button>
      </header>

      <div className="app-body">
        <div className="card status-card">
          <div className={`status-dot ${status}`} />
          <div className="status-info">
            <div className="status-label">{STATUS_LABEL[status]}</div>
            <div className="status-meta">
              {lastSync ? `Last sync ${lastSync}` : "Not synced yet"}
            </div>
          </div>
          {!running && !stopping ? (
            <button
              className="btn primary"
              onClick={handleStart}
              disabled={!folder}
            >
              Start
            </button>
          ) : (
            <button
              className="btn danger"
              onClick={handleStop}
              disabled={stopping}
            >
              {stopping ? "Stopping…" : "Stop"}
            </button>
          )}
        </div>

        {state.error && status === "error" && (
          <div className="error-banner">{state.error}</div>
        )}

        <div className="card">
          <div className="card-header">Folder</div>
          <div className="card-body">
            <div className="folder-row">
              <div className={`folder-path ${folder ? "" : "empty"}`}>
                {folder || "No folder selected"}
              </div>
              <button
                className="btn"
                onClick={() => folder && OpenFolder(folder)}
                disabled={!folder}
                title="Open in Finder"
              >
                Open
              </button>
              <button
                className="btn"
                onClick={handlePickFolder}
                disabled={running || stopping}
                title="Choose a different folder"
              >
                Change
              </button>
            </div>
          </div>
        </div>

        <div className="card">
          <div className="card-header">Preferences</div>
          <div className="card-body">
            <label className="check-row">
              <input
                type="checkbox"
                checked={autostart}
                onChange={(e) => handleAutostart(e.target.checked)}
              />
              <span>Start s2sync at login</span>
            </label>
          </div>
        </div>

        <div className="card">
          <div className="card-header">Activity</div>
          <div className="card-body">
            <div
              className={`activity ${logs.length === 0 ? "empty" : ""}`}
              ref={activityRef}
            >
              {logs.length === 0
                ? "(no events yet)"
                : logs.map((e, i) => (
                    <span key={i} className="activity-line">
                      <span className="activity-time">
                        {new Date(e.time).toLocaleTimeString()}
                      </span>
                      <span className={`activity-type ${e.type}`}>{e.type}</span>{" "}
                      {e.message ?? ""}
                    </span>
                  ))}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

interface WelcomeProps {
  endpoint: string;
  defaultFolder: string;
  initialFolder: string;
  onConnected: (folder: string) => void;
}

function Welcome({ endpoint, defaultFolder, initialFolder, onConnected }: WelcomeProps) {
  const [token, setToken] = useState("");
  const [folder, setFolder] = useState(initialFolder);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    setFolder(initialFolder);
  }, [initialFolder]);

  const pickFolder = async () => {
    const f = await PickFolder();
    if (f) setFolder(f);
  };

  const connect = async () => {
    if (!token || busy) return;
    setError("");
    setBusy(true);
    try {
      const folderPath = folder || defaultFolder;
      await SaveToken(token);
      await EnsureFolder(folderPath);
      await SetSavedFolder(folderPath);
      await StartSync(folderPath);
      onConnected(folderPath);
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="signin">
      <div className="signin-card">
        <div className="brand-mark">S2</div>
        <h2>Welcome to s2sync</h2>
        <p className="signin-help">
          Sync a folder with S2. Get an API token from your S2 dashboard.
        </p>

        <div className="form-group">
          <label className="form-label">Token</label>
          <input
            type="password"
            className="token-input"
            placeholder="s2_..."
            value={token}
            onChange={(e) => setToken(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && connect()}
            autoFocus
          />
        </div>

        <div className="form-group">
          <label className="form-label">Folder</label>
          <div className="folder-row">
            <input
              type="text"
              className="token-input folder-input"
              placeholder={defaultFolder}
              value={folder}
              onChange={(e) => setFolder(e.target.value)}
            />
            <button className="btn" onClick={pickFolder} disabled={busy}>
              Choose…
            </button>
          </div>
          <p className="form-hint">
            Files in this folder will sync. Created if it doesn't exist.
          </p>
        </div>

        <button
          className="btn primary connect-btn"
          onClick={connect}
          disabled={!token || busy}
        >
          {busy ? "Connecting…" : "Connect & start sync"}
        </button>
        {error && <div className="error-banner">{error}</div>}
        <p className="signin-endpoint">{endpoint}</p>
      </div>
    </div>
  );
}

export default App;
