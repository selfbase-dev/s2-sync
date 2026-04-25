import { useEffect, useState } from "react";
import {
  ConfirmDisconnect,
  DefaultFolder,
  EnsureFolder,
  Endpoint,
  GetStatus,
  HasValidSession,
  IsAutostartEnabled,
  LogFile,
  OpenLogFile,
  PickFolder,
  RecentLogs,
  SavedFolder,
  SetAutostart,
  SignOut,
  StartSync,
  StopSync,
} from "../wailsjs/go/main/App";
import { EventsOn } from "../wailsjs/runtime/runtime";
import { Dashboard } from "./Dashboard";
import { Welcome } from "./Welcome";
import { LogRecord, MAX_LOG_LINES, StateInfo } from "./types";
import "./App.css";

function App() {
  const [endpoint, setEndpoint] = useState("");
  const [signedIn, setSignedIn] = useState<boolean | null>(null);
  const [folder, setFolder] = useState("");
  const [defaultFolder, setDefaultFolder] = useState("");
  const [state, setState] = useState<StateInfo>({ status: "idle" });
  const [logs, setLogs] = useState<LogRecord[]>([]);
  const [logFile, setLogFilePath] = useState("");
  const [autostart, setAutostartState] = useState(false);

  useEffect(() => {
    Endpoint().then(setEndpoint);
    DefaultFolder().then(setDefaultFolder);
    IsAutostartEnabled().then(setAutostartState);
    LogFile().then(setLogFilePath);
    Promise.all([HasValidSession(), SavedFolder()]).then(([ok, saved]) => {
      setSignedIn(ok);
      if (saved) setFolder(saved);
    });
    GetStatus().then((s) => {
      setState(s as StateInfo);
      if (s.mount?.path) setFolder(s.mount.path);
    });
    // Repopulate from the file so trouble-shooting context survives a
    // window reload — the file sink is the source of truth across runs.
    RecentLogs(MAX_LOG_LINES).then((lines) => {
      const parsed: LogRecord[] = [];
      for (const line of lines ?? []) {
        try {
          const r = JSON.parse(line);
          parsed.push({
            time: r.time,
            level: (r.level ?? "INFO") as LogRecord["level"],
            event: r.msg ?? "",
            attrs: stripMeta(r),
          });
        } catch {
          // ignore malformed line
        }
      }
      if (parsed.length) setLogs(parsed);
    });
    const off = EventsOn("log", (rec: LogRecord) => {
      setLogs((prev) => {
        const next = [...prev, rec];
        return next.length > MAX_LOG_LINES ? next.slice(-MAX_LOG_LINES) : next;
      });
      GetStatus().then((s) => setState(s as StateInfo));
    });
    return () => {
      off();
    };
  }, []);

  const handlePickFolder = async () => {
    const f = await PickFolder(folder);
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
    await SignOut();
    setSignedIn(false);
    setFolder("");
    setState({ status: "idle" });
    setLogs([]);
  };

  if (signedIn === null) {
    return <div className="app" />;
  }

  if (!signedIn) {
    return (
      <Welcome
        endpoint={endpoint}
        defaultFolder={defaultFolder}
        initialFolder={folder || defaultFolder}
        onConnected={(f) => {
          setSignedIn(true);
          setFolder(f);
        }}
      />
    );
  }

  return (
    <Dashboard
      endpoint={endpoint}
      folder={folder}
      state={state}
      logs={logs}
      logFile={logFile}
      autostart={autostart}
      onStart={handleStart}
      onStop={handleStop}
      onPickFolder={handlePickFolder}
      onAutostartChange={handleAutostart}
      onDisconnect={handleDisconnect}
      onClearLogs={() => setLogs([])}
      onOpenLogFile={() => OpenLogFile()}
    />
  );
}

// stripMeta returns the attribute fields of a slog JSON record, dropping
// the framework keys so the LogRecord.attrs map matches what the live
// Wails callback delivers.
function stripMeta(r: Record<string, unknown>): Record<string, unknown> | undefined {
  const out: Record<string, unknown> = {};
  for (const k of Object.keys(r)) {
    if (k === "time" || k === "level" || k === "msg") continue;
    out[k] = r[k];
  }
  return Object.keys(out).length ? out : undefined;
}

export default App;
