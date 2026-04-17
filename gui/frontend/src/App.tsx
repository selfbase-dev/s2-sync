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

function App() {
  const [endpoint, setEndpoint] = useState("");
  const [hasToken, setHasToken] = useState<boolean | null>(null);
  const [tokenInput, setTokenInput] = useState("");
  const [tokenError, setTokenError] = useState("");
  const [savingToken, setSavingToken] = useState(false);
  const [folder, setFolder] = useState("");
  const [state, setState] = useState<StateInfo>({ status: "idle" });
  const [logs, setLogs] = useState<Event[]>([]);
  const activityRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    Endpoint().then(setEndpoint);
    HasToken().then(setHasToken);
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

  const handleSaveToken = async () => {
    if (!tokenInput || savingToken) return;
    setTokenError("");
    setSavingToken(true);
    try {
      await SaveToken(tokenInput);
      setTokenInput("");
      setHasToken(true);
    } catch (e: any) {
      setTokenError(String(e?.message ?? e));
    } finally {
      setSavingToken(false);
    }
  };

  const handleClearToken = async () => {
    await ClearToken();
    setHasToken(false);
    setFolder("");
    setState({ status: "idle" });
    setLogs([]);
  };

  const handlePickFolder = async () => {
    const f = await PickFolder();
    if (f) setFolder(f);
  };

  const handleStart = async () => {
    if (!folder) return;
    try {
      await StartSync(folder);
    } catch (e: any) {
      setState({ status: "error", error: String(e?.message ?? e) });
    }
  };

  const handleStop = async () => {
    await StopSync();
  };

  // ---- Sign-in screen ----
  if (hasToken === null) {
    return <div className="app" />;
  }

  if (!hasToken) {
    return (
      <div className="signin">
        <div className="signin-card">
          <div className="brand-mark">S2</div>
          <h2>Connect to S2</h2>
          <p className="signin-help">
            Paste an API token from your S2 dashboard to start syncing.
            Tokens begin with <code>s2_</code>.
          </p>
          <input
            type="password"
            className="token-input"
            placeholder="s2_..."
            value={tokenInput}
            onChange={(e) => setTokenInput(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && handleSaveToken()}
            autoFocus
          />
          <button
            className="btn primary"
            onClick={handleSaveToken}
            disabled={!tokenInput || savingToken}
          >
            {savingToken ? "Verifying…" : "Connect"}
          </button>
          {tokenError && <div className="error-banner">{tokenError}</div>}
          <p className="signin-endpoint">{endpoint}</p>
        </div>
      </div>
    );
  }

  // ---- Main screen ----
  const status = state.status;
  const running = status === "running";
  const stopping = status === "stopping";
  const statusText: Record<Status, string> = {
    idle: "Idle",
    running: "Syncing",
    stopping: "Stopping…",
    error: "Error",
  };
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
          onClick={handleClearToken}
          title="Disconnect"
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
            <div className="status-label">{statusText[status]}</div>
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
                onClick={handlePickFolder}
                disabled={running || stopping}
              >
                Choose…
              </button>
            </div>
          </div>
        </div>

        <div className="card">
          <div className="card-header">Activity</div>
          <div className="card-body">
            <div className={`activity ${logs.length === 0 ? "empty" : ""}`} ref={activityRef}>
              {logs.length === 0
                ? "(no events yet)"
                : logs.map((e, i) => (
                    <span key={i} className="activity-line">
                      <span className="activity-time">
                        {new Date(e.time).toLocaleTimeString()}
                      </span>
                      <span className={`activity-type ${e.type}`}>
                        {e.type}
                      </span>
                      {" "}
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

export default App;
