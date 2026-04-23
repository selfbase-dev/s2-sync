import { useEffect, useState } from "react";
import {
  EnsureFolder,
  PickFolder,
  SaveToken,
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
  const [step, setStep] = useState<1 | 2>(1);
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

  const validateToken = async () => {
    if (!token || busy) return;
    setError("");
    setBusy(true);
    try {
      await SaveToken(token);
      setStep(2);
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  const connect = async () => {
    if (busy) return;
    setError("");
    setBusy(true);
    try {
      const folderPath = folder || defaultFolder;
      await EnsureFolder(folderPath);
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
        <h2>Welcome to S2 Sync</h2>

        <ol className="signin-steps" aria-label="Onboarding progress">
          <li className={step === 1 ? "current" : "done"}>1. Token</li>
          <li className={step === 2 ? "current" : ""}>2. Folder</li>
        </ol>

        {step === 1 ? (
          <>
            <p className="signin-help">
              Paste your S2 API token to connect. We'll confirm your scope before picking a folder.
            </p>

            <div className="form-group">
              <label className="form-label">Token</label>
              <input
                type="password"
                className="token-input"
                placeholder="s2_..."
                value={token}
                onChange={(e) => setToken(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && validateToken()}
                autoFocus
              />
            </div>

            <button
              className="btn primary connect-btn"
              onClick={validateToken}
              disabled={!token || busy}
            >
              {busy ? "Checking…" : "Next"}
            </button>

            <button
              type="button"
              className="btn link-btn"
              onClick={() => BrowserOpenURL(endpoint)}
            >
              Don't have a token? Open S2 dashboard →
            </button>

            {error && <div className="error-banner">{error}</div>}
            <p className="signin-endpoint">{endpoint}</p>
          </>
        ) : (
          <>
            <p className="signin-help">Token validated. Choose which local folder to sync.</p>

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
              disabled={busy}
            >
              {busy ? "Connecting…" : "Connect & start sync"}
            </button>

            <button
              type="button"
              className="btn link-btn"
              onClick={() => {
                setStep(1);
                setError("");
              }}
              disabled={busy}
            >
              ← Use a different token
            </button>

            {error && <div className="error-banner">{error}</div>}
            <p className="signin-endpoint">{endpoint}</p>
          </>
        )}
      </div>
    </div>
  );
}
