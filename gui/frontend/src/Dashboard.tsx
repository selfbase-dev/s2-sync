import { useEffect, useRef } from "react";
import { OpenFolder } from "../wailsjs/go/main/App";
import { Event, StateInfo, STATUS_LABEL, Status } from "./types";

interface Props {
  endpoint: string;
  folder: string;
  state: StateInfo;
  logs: Event[];
  autostart: boolean;
  onStart: () => void;
  onStop: () => void;
  onPickFolder: () => void;
  onAutostartChange: (next: boolean) => void;
  onDisconnect: () => void;
}

export function Dashboard({
  endpoint,
  folder,
  state,
  logs,
  autostart,
  onStart,
  onStop,
  onPickFolder,
  onAutostartChange,
  onDisconnect,
}: Props) {
  const activityRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (activityRef.current) {
      activityRef.current.scrollTop = activityRef.current.scrollHeight;
    }
  }, [logs]);

  const status: Status = state.status;
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
        <button className="icon-btn" onClick={onDisconnect} title="Disconnect" aria-label="Disconnect">
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
            <button className="btn primary" onClick={onStart} disabled={!folder}>
              Start
            </button>
          ) : (
            <button className="btn danger" onClick={onStop} disabled={stopping}>
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
                onClick={onPickFolder}
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
                onChange={(e) => onAutostartChange(e.target.checked)}
              />
              <span>Start s2sync at login</span>
            </label>
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
