export type Status = "idle" | "running" | "stopping" | "error";

export interface StateInfo {
  status: Status;
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

// Event prefixes the Logs panel uses for color/group filters. Keep in
// sync with internal/log/events.go.
export const EVENT_GROUPS = ["sync", "file", "watch", "service", "oauth"] as const;
export type EventGroup = (typeof EVENT_GROUPS)[number];

export function eventGroup(event: string): EventGroup | "other" {
  const prefix = event.split(".")[0];
  return (EVENT_GROUPS as readonly string[]).includes(prefix)
    ? (prefix as EventGroup)
    : "other";
}
