import { useState } from "react";
import { api } from "../api";
import { ErrorNote, Loading, Switch, useAction, useAsync, useToast } from "../components/ui";
import { ago } from "../util";

// Settings → Access: who may sign in, the token collectors send, dry run,
// and Sentinel's GitHub App.

interface AccessView {
  users: string[];
  org: string;
  env_users?: string[];
  env_org?: string;
  me: string;
  ingest_token: string;
  ingest_env: boolean;
  ingest_endpoint: string;
  dry_run: boolean;
  dry_run_env: boolean;
  app?: { slug: string; name: string; html_url: string; owner: string; owner_type: string; created_at: string; in_use: boolean };
}

export default function Access() {
  const a = useAsync(() => api.get<AccessView>("/api/access"), []);
  if (!a.data) return a.error ? <ErrorNote error={a.error} /> : <Loading />;
  return (
    <>
      <People v={a.data} onSaved={a.reload} />
      <Ingest v={a.data} onSaved={a.reload} />
      <DryRun v={a.data} onSaved={a.reload} />
      {a.data.app && <App v={a.data} />}
    </>
  );
}

function People({ v, onSaved }: { v: AccessView; onSaved: () => void }) {
  const run = useAction();
  const [users, setUsers] = useState(v.users.join("\n"));
  const [org, setOrg] = useState(v.org);
  const list = users
    .split(/[\s,]+/)
    .map((u) => u.trim())
    .filter(Boolean);
  const dirty = list.join("\n") !== v.users.join("\n") || org.trim() !== v.org;
  const save = async () => {
    if (await run(() => api.put("/api/access", { users: list, org: org.trim() }), "Access saved")) onSaved();
  };
  return (
    <div className="card">
      <div className="card-head">
        <h2>Who can sign in</h2>
      </div>
      <p className="small muted">Everyone signs in with GitHub. Add people by their GitHub username, or let a whole organization in.</p>
      <div className="form-grid">
        <label className="field">
          GitHub users
          <textarea className="mono" rows={5} value={users} onChange={(e) => setUsers(e.target.value)} placeholder={"octocat\nmonalisa"} />
          <span className="help">One per line. You are {v.me || "signed in"}.</span>
        </label>
        <label className="field">
          GitHub organization
          <input value={org} onChange={(e) => setOrg(e.target.value)} placeholder="your-org" />
          <span className="help">Every member of this organization may sign in. Sentinel's GitHub App needs Members read access there.</span>
        </label>
      </div>
      {((v.env_users && v.env_users.length > 0) || v.env_org) && (
        <p className="small muted">
          Also allowed by <code>deploy/.env</code>: {[...(v.env_users ?? []), ...(v.env_org ? [`members of ${v.env_org}`] : [])].join(", ")}.
        </p>
      )}
      <button className="primary" onClick={save} disabled={!dirty}>
        Save
      </button>
    </div>
  );
}

function Ingest({ v, onSaved }: { v: AccessView; onSaved: () => void }) {
  const run = useAction();
  const toast = useToast();
  const [shown, setShown] = useState(false);
  const copy = async (s: string, what: string) => {
    try {
      await navigator.clipboard.writeText(s);
      toast(`${what} copied`);
    } catch {
      toast("Could not copy; select the text instead", true);
    }
  };
  const rotate = async () => {
    if (!confirm("Replace the ingest token? Collectors with the old one are refused until you update them.")) return;
    if (await run(() => api.post("/api/access/ingest-token"), "New ingest token")) {
      setShown(true);
      onSaved();
    }
  };
  const env = `SENTINEL_ENDPOINT=${v.ingest_endpoint}\nSENTINEL_INGEST_TOKEN=${v.ingest_token}`;
  return (
    <div className="card">
      <div className="card-head">
        <h2>Ingest token</h2>
        <div className="right">
          {!v.ingest_env && (
            <button className="small" onClick={rotate}>
              Replace
            </button>
          )}
        </div>
      </div>
      <p className="small muted">
        Collectors send it as <code>Authorization: Bearer …</code> with every request to <code>{v.ingest_endpoint}/v1/…</code>.
        {v.ingest_env && " It is set with INGEST_TOKEN in deploy/.env."}
      </p>
      <div className="row">
        <input className="mono" readOnly value={shown ? v.ingest_token : "•".repeat(24)} style={{ flex: 1, minWidth: 220 }} />
        <button className="small" onClick={() => setShown((s) => !s)}>
          {shown ? "Hide" : "Show"}
        </button>
        <button className="small" onClick={() => copy(v.ingest_token, "Token")}>
          Copy
        </button>
      </div>
      <p className="small muted" style={{ marginTop: 10 }}>
        For <code>deploy/otel-collector-app-server.yaml</code> on each app server:{" "}
        <button className="small" onClick={() => copy(env, "Collector settings")}>
          Copy collector settings
        </button>
      </p>
    </div>
  );
}

function DryRun({ v, onSaved }: { v: AccessView; onSaved: () => void }) {
  const run = useAction();
  const [restarting, setRestarting] = useState(false);
  const toggle = async (on: boolean) => {
    if (await run(() => api.put("/api/access/dry-run", { on }))) {
      setRestarting(true);
      setTimeout(() => {
        setRestarting(false);
        onSaved();
      }, 3000);
    }
  };
  return (
    <div className="card">
      <div className="card-head">
        <h2>Dry run</h2>
        <div className="right">
          <Switch checked={v.dry_run} onChange={toggle} disabled={v.dry_run_env || restarting} label="Dry run" />
        </div>
      </div>
      <p className="small muted">
        In dry run Sentinel logs what it would do on GitHub (issues, comments, labels) instead of doing it. Switching restarts Sentinel, which takes a few
        seconds.
        {v.dry_run_env && " It is set with DRY_RUN in deploy/.env; remove it there to switch it here."}
        {restarting && " Restarting…"}
      </p>
    </div>
  );
}

function App({ v }: { v: AccessView }) {
  const a = v.app!;
  return (
    <div className="card">
      <div className="card-head">
        <h2>GitHub App</h2>
        <div className="right">{a.in_use ? <span className="badge ok">in use</span> : <span className="badge warn">GITHUB_APP_* in deploy/.env is used instead</span>}</div>
      </div>
      <p className="small muted">
        Sentinel created <strong>{a.name || a.slug}</strong> on {a.owner_type === "Organization" ? "the organization" : "the account"} <strong>{a.owner}</strong>{" "}
        {ago(a.created_at)}. Its key is kept in Sentinel's database.{" "}
        <a href={a.html_url} target="_blank" rel="noreferrer">
          Open on GitHub ↗
        </a>
      </p>
    </div>
  );
}
