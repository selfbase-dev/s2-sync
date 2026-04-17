export type Status = "idle" | "running" | "stopping" | "error";

export interface StateInfo {
  status: Status;
  mount?: { path: string };
  error?: string;
  lastSync?: string;
}

export type EventType = "started" | "stopped" | "synced" | "error" | "log";

export interface Event {
  type: EventType;
  message?: string;
  time: string;
}

export const MAX_LOG_LINES = 200;

export const STATUS_LABEL: Record<Status, string> = {
  idle: "Idle",
  running: "Syncing",
  stopping: "Stopping…",
  error: "Error",
};
