import { useEffect, useState } from "react";
import {
  EnsureFolder,
  PickFolder,
  StartOAuthLogin,
  StartSync,
} from "../wailsjs/go/main/App";

interface Props {
  endpoint: string;
  defaultFolder: string;
  initialFolder: string;
  onConnected: (folder: string) => void;
}

export function Welcome({ endpoint, defaultFolder, initialFolder, onConnected }: Props) {
  const [step, setStep] = useState<1 | 2>(1);
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

  const signIn = async () => {
    if (busy) return;
    setError("");
    setBusy(true);
    try {
      await StartOAuthLogin();
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
          <li className={step === 1 ? "current" : "done"}>1. Sign in</li>
          <li className={step === 2 ? "current" : ""}>2. Folder</li>
        </ol>

        {step === 1 ? (
          <>
            <p className="signin-help">
              Sign in to your S2 account. Your browser will open to complete consent, then you'll be brought back here.
            </p>

            <button
              className="btn primary connect-btn"
              onClick={signIn}
              disabled={busy}
            >
              {busy ? "Waiting for browser…" : "Sign in with S2"}
            </button>

            {error && <div className="error-banner">{error}</div>}
            <p className="signin-endpoint">{endpoint}</p>
          </>
        ) : (
          <>
            <p className="signin-help">Signed in. Choose which local folder to sync.</p>

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

            {error && <div className="error-banner">{error}</div>}
            <p className="signin-endpoint">{endpoint}</p>
          </>
        )}
      </div>
    </div>
  );
}
