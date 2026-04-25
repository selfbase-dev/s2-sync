import { useEffect, useMemo, useRef, useState } from "react";
import { OpenFolder } from "../wailsjs/go/main/App";
import {
  EVENT_GROUPS,
  EventGroup,
  LogLevel,
  LogRecord,
  STATUS_LABEL,
  StateInfo,
  Status,
  eventGroup,
} from "./types";

interface Props {
  endpoint: string;
  folder: string;
  state: StateInfo;
  logs: LogRecord[];
  logFile: string;
  autostart: boolean;
  onStart: () => void;
  onStop: () => void;
  onPickFolder: () => void;
  onAutostartChange: (next: boolean) => void;
  onDisconnect: () => void;
  onClearLogs: () => void;
  onOpenLogFile: () => void;
}

const LEVEL_ORDER: Record<LogLevel, number> = {
  DEBUG: 0,
  INFO: 1,
  WARN: 2,
  ERROR: 3,
};

export function Dashboard({
  endpoint,
  folder,
  state,
  logs,
  logFile,
  autostart,
  onStart,
  onStop,
  onPickFolder,
  onAutostartChange,
  onDisconnect,
  onClearLogs,
  onOpenLogFile,
}: Props) {
  const logsRef = useRef<HTMLDivElement>(null);
  const [paused, setPaused] = useState(false);
  const [minLevel, setMinLevel] = useState<LogLevel>("INFO");
  const [activeGroups, setActiveGroups] = useState<Set<EventGroup | "other">>(
    new Set([...EVENT_GROUPS, "other"]),
  );

  const filtered = useMemo(() => {
    const minRank = LEVEL_ORDER[minLevel];
    return logs.filter((r) => {
      if (LEVEL_ORDER[r.level] < minRank) return false;
      const g = eventGroup(r.event);
      return activeGroups.has(g);
    });
  }, [logs, minLevel, activeGroups]);

  useEffect(() => {
    if (paused || !logsRef.current) return;
    logsRef.current.scrollTop = logsRef.current.scrollHeight;
  }, [filtered, paused]);

  const status: Status = state.status;
  const running = status === "running";
  const stopping = status === "stopping";
  const lastSync = state.lastSync
    ? new Date(state.lastSync).toLocaleTimeString()
    : null;

  const toggleGroup = (g: EventGroup | "other") => {
    const next = new Set(activeGroups);
    if (next.has(g)) next.delete(g);
    else next.add(g);
    setActiveGroups(next);
  };

  return (
    <div className="app">
      <header className="app-header">
        <div className="brand">
          <div className="brand-mark">S2</div>
          <div>
            <h1>S2 Sync</h1>
            <div className="brand-sub">{endpoint}</div>
          </div>
        </div>
        <button className="icon-btn" onClick={onDisconnect} title="Disconnect" aria-label="Disconnect">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
            <polyline points="16 17 21 12 16 7" />
            <line x1="21" y1="12" x2="9" y2="12" />
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
              <span>Start S2 Sync at login</span>
            </label>
          </div>
        </div>

        <div className="card">
          <div className="card-header">
            <span>Logs</span>
            <div className="logs-actions">
              <select
                className="logs-level"
                value={minLevel}
                onChange={(e) => setMinLevel(e.target.value as LogLevel)}
                title="Minimum level"
              >
                <option value="DEBUG">debug+</option>
                <option value="INFO">info+</option>
                <option value="WARN">warn+</option>
                <option value="ERROR">error</option>
              </select>
              <button
                className="btn small"
                onClick={() => setPaused((p) => !p)}
                title={paused ? "Resume auto-scroll" : "Pause auto-scroll"}
              >
                {paused ? "Resume" : "Pause"}
              </button>
              <button className="btn small" onClick={onClearLogs} title="Clear visible logs">
                Clear
              </button>
              <button
                className="btn small"
                onClick={onOpenLogFile}
                disabled={!logFile}
                title={logFile || "Log file unavailable"}
              >
                Open file
              </button>
            </div>
          </div>
          <div className="logs-filters">
            {[...EVENT_GROUPS, "other"].map((g) => (
              <button
                key={g}
                className={`chip ${activeGroups.has(g as EventGroup | "other") ? "on" : ""}`}
                onClick={() => toggleGroup(g as EventGroup | "other")}
              >
                {g}
              </button>
            ))}
          </div>
          <div className="card-body">
            <div className={`logs ${filtered.length === 0 ? "empty" : ""}`} ref={logsRef}>
              {filtered.length === 0 ? (
                <div className="logs-empty">(no events match filter)</div>
              ) : (
                filtered.map((r, i) => <LogRow key={i} record={r} />)
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

function LogRow({ record }: { record: LogRecord }) {
  const time = new Date(record.time).toLocaleTimeString();
  const group = eventGroup(record.event);
  return (
    <div className={`log-row level-${record.level.toLowerCase()} group-${group}`}>
      <span className="log-time">{time}</span>
      <span className={`log-level lvl-${record.level.toLowerCase()}`}>{record.level}</span>
      <span className="log-event">{record.event}</span>
      <span className="log-attrs">{formatAttrs(record.attrs)}</span>
    </div>
  );
}

function formatAttrs(attrs: Record<string, unknown> | undefined): string {
  if (!attrs) return "";
  return Object.entries(attrs)
    .map(([k, v]) => `${k}=${formatValue(v)}`)
    .join(" ");
}

function formatValue(v: unknown): string {
  if (v === null || v === undefined) return "";
  if (typeof v === "string") return v.includes(" ") ? `"${v}"` : v;
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}
