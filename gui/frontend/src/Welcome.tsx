import { useEffect, useState } from "react";
import {
  EnsureFolder,
  PickFolder,
  SaveToken,
  SetSavedFolder,
  StartSync,
} from "../wailsjs/go/main/App";
import { BrowserOpenURL } from "../wailsjs/runtime/runtime";

interface Props {
  endpoint: string;
  defaultFolder: string;
  initialFolder: string;
  onConnected: (folder: string) => void;
}

export function Welcome({ endpoint, defaultFolder, initialFolder, onConnected }: Props) {
  const [token, setToken] = useState("");
  const [folder, setFolder] = useState(initialFolder);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    setFolder(initialFolder);
  }, [initialFolder]);

  const pickFolder = async () => {
    const f = await PickFolder(folder);
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
          <p className="form-hint">
            Don't have one?{" "}
            <a
              href="#"
              onClick={(e) => {
                e.preventDefault();
                BrowserOpenURL(endpoint);
              }}
            >
              Open S2 dashboard
            </a>
            {" "}and create a token.
          </p>
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
