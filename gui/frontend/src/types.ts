export type Status = "idle" | "running" | "stopping" | "error";

export interface StateInfo {
  status: Status;
  // True while a sync run is in flight. Orthogonal to status: a Running
  // service is "up to date" when syncing is false and "syncing now"
  // when it is true.
  syncing?: boolean;
  mount?: { path: string };
  error?: string;
  lastSync?: string;
}

export type LogLevel = "DEBUG" | "INFO" | "WARN" | "ERROR";

// LogRecord matches the JSON the Wails callback sink emits and the
// JSON Lines written to ~/Library/Application Support/s2sync/sync.log.
// Stable contract — do not rename fields without updating both sides.
export interface LogRecord {
  time: string;
  level: LogLevel;
  event: string;
  attrs?: Record<string, unknown>;
}

export const MAX_LOG_LINES = 200;

export const STATUS_LABEL: Record<Status, string> = {
  idle: "Idle",
  running: "Syncing",
  stopping: "Stopping…",
  error: "Error",
};
