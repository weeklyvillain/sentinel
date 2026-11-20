import { useEffect, useState } from "react";
import { api } from "../api";
import { AuthLayout } from "../components/Showcase";
import { Icon } from "../components/ui";

// First start: enter the setup code from Sentinel's log, create the GitHub
// App in one click, install it, sign in. Shown instead of the sign-in page
// until Sentinel has its GitHub App and an admin.

export interface Bootstrap {
  setup: boolean;
  needs_app: boolean;
  needs_admin: boolean;
  claimed: boolean;
  public_url: string;
  install_url?: string;
  app_slug?: string;
}

export function Welcome({ boot, onChange }: { boot: Bootstrap; onChange: () => void }) {
  const params = new URLSearchParams(location.search);
  const created = params.get("setup") === "created";
  const [err, setErr] = useState(params.get("setup_error") ?? undefined);
  const step = !boot.claimed ? 1 : boot.needs_app ? 2 : 3;

  // After GitHub created the app Sentinel restarts; wait for it.
  useEffect(() => {
    if (!created || !boot.needs_app) return;
    const t = setInterval(onChange, 1500);
    return () => clearInterval(t);
  }, [created, boot.needs_app, onChange]);

  return (
    <AuthLayout wide>
      <h1>Set up Sentinel</h1>
      <p className="auth-sub">Three steps, all in the browser. Sentinel creates its own GitHub App; you only confirm on GitHub.</p>
        {err && <div className="notice bad">{err}</div>}
        <ol className="welcome-steps">
          <WStep n={1} title="Prove it's you" state={step > 1 ? "done" : "now"}>
            <ClaimForm
              onDone={() => {
                setErr(undefined);
                onChange();
              }}
              onError={setErr}
            />
          </WStep>
          <WStep n={2} title="Create Sentinel's GitHub App" state={step > 2 ? "done" : step === 2 ? "now" : "later"}>
            {created && boot.needs_app ? <p className="muted">GitHub created the app. Sentinel is restarting to use it…</p> : <CreateApp onError={setErr} />}
          </WStep>
          <WStep n={3} title="Install it and sign in" state={step === 3 ? "now" : "later"}>
            <p className="small muted">
              Install the app on the account or organization whose repositories should get issues (you can add more later), then sign in. You become
              Sentinel's first admin and can let others in under Settings → Access.
            </p>
            <div className="row" style={{ marginTop: 10 }}>
              {boot.install_url && (
                <a className="btn" href={boot.install_url} target="_blank" rel="noreferrer">
                  <Icon.github /> Install on GitHub
                </a>
              )}
              <a className="btn github" href="/auth/login">
                <Icon.github /> Sign in with GitHub
              </a>
            </div>
          </WStep>
        </ol>
    </AuthLayout>
  );
}

function WStep(props: { n: number; title: string; state: "done" | "now" | "later"; children: React.ReactNode }) {
  return (
    <li className={"welcome-step " + props.state}>
      <div className="welcome-head">
        <span className="setup-num">{props.state === "done" ? "✓" : props.n}</span>
        <strong>{props.title}</strong>
      </div>
      {props.state === "now" && <div className="welcome-body">{props.children}</div>}
    </li>
  );
}

function ClaimForm({ onDone, onError }: { onDone: () => void; onError: (e: string) => void }) {
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await api.post("/setup/claim", { code });
      onDone();
    } catch (x) {
      onError((x as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <form onSubmit={submit}>
      <p className="small muted">Sentinel printed a setup code in its log. On the server, run:</p>
      <pre className="code">docker compose logs sentinel | grep "setup code"</pre>
      <div className="row" style={{ marginTop: 10 }}>
        <input
          className="mono"
          value={code}
          onChange={(e) => setCode(e.target.value)}
          placeholder="ABCD-EFGH"
          autoFocus
          aria-label="Setup code"
          style={{ width: 160, textTransform: "uppercase" }}
        />
        <button className="primary" disabled={busy || code.trim().length < 8}>
          Continue
        </button>
      </div>
    </form>
  );
}

function CreateApp({ onError }: { onError: (e: string) => void }) {
  const [owner, setOwner] = useState<"user" | "org">("user");
  const [org, setOrg] = useState("");
  const [busy, setBusy] = useState(false);
  const create = async () => {
    setBusy(true);
    try {
      const f = await api.post<{ action: string; manifest: string }>("/setup/manifest", { org: owner === "org" ? org.trim() : "" });
      // GitHub's manifest flow takes a form POST.
      const form = document.createElement("form");
      form.method = "post";
      form.action = f.action;
      const input = document.createElement("input");
      input.type = "hidden";
      input.name = "manifest";
      input.value = f.manifest;
      form.appendChild(input);
      document.body.appendChild(form);
      form.submit();
    } catch (x) {
      onError((x as Error).message);
      setBusy(false);
    }
  };
  return (
    <>
      <p className="small muted">
        GitHub opens with the app filled in: issues, code and pull requests (for fixes), and organization members (for sign-in). Check it and press{" "}
        <strong>Create GitHub App</strong>; GitHub sends you back here.
      </p>
      <div className="stack" style={{ gap: 6, margin: "10px 0" }}>
        <label className="check">
          <input type="radio" checked={owner === "user"} onChange={() => setOwner("user")} /> On my GitHub account
        </label>
        <label className="check">
          <input type="radio" checked={owner === "org"} onChange={() => setOwner("org")} /> On an organization
          {owner === "org" && <input value={org} onChange={(e) => setOrg(e.target.value)} placeholder="organization" style={{ marginLeft: 8, width: 180 }} />}
        </label>
      </div>
      <button className="primary" onClick={create} disabled={busy || (owner === "org" && !org.trim())}>
        <Icon.github /> Create the GitHub App
      </button>
    </>
  );
}
