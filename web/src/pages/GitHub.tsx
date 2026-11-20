import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { api, GitHubStatus, Repo } from "../api";
import { Empty, ErrorNote, Icon, Loading, Switch, useAsync, useAction } from "../components/ui";
import { ago } from "../util";

export default function GitHub() {
  const run = useAction();
  const [params] = useSearchParams();
  const st = useAsync(() => api.get<GitHubStatus>("/api/github"), []);
  const [busy, setBusy] = useState(false);
  const [filter, setFilter] = useState("");

  const install = async () => {
    const r = await run(() => api.get<{ url: string }>("/api/github/install"));
    if (r) location.href = r.url;
  };
  const sync = async () => {
    setBusy(true);
    const d = await run(() => api.post<GitHubStatus>("/api/github/sync"), "Synced repositories from GitHub");
    if (d) st.setData(d);
    setBusy(false);
  };
  const allow = async (r: Repo, allowed: boolean) => {
    const [owner, name] = r.full_name.split("/");
    const ok = await run(
      () => api.put(`/api/github/repos/${owner}/${name}`, { allowed }).then(() => true),
      allowed ? `Sentinel may now create issues in ${r.full_name}` : `Revoked ${r.full_name}`,
    );
    if (ok) st.setData((d) => d && { ...d, repos: d.repos.map((x) => (x.full_name === r.full_name ? { ...x, allowed } : x)) });
  };
  const setDefault = async (repo: string) => {
    const ok = await run(() => api.put("/api/github/default", { repo }).then(() => true), repo ? `Default repository: ${repo}` : "Default repository cleared");
    if (ok) st.setData((d) => d && { ...d, default_repo: repo });
  };

  if (st.error) return <ErrorNote error={st.error} />;
  if (!st.data) return <Loading />;
  const d = st.data;
  const allowed = d.repos.filter((r) => r.allowed);
  const repos = d.repos.filter((r) => r.full_name.toLowerCase().includes(filter.toLowerCase()));

  return (
    <>
      <div className="page-head">
        <span className="sub">Connect the Sentinel GitHub App and choose which repositories it may file issues in.</span>
        {d.mode === "app" && (
          <div className="actions">
            <button onClick={sync} disabled={busy}>
              {busy ? "Syncing…" : "Sync from GitHub"}
            </button>
            <button className="primary" onClick={install}>
              <Icon.github /> {d.installations.length ? "Add / change repositories" : "Install on GitHub"}
            </button>
          </div>
        )}
      </div>
      {params.get("installed") && <div className="notice ok" style={{ marginBottom: 16 }}>Installation connected. Now allow the repositories Sentinel may create issues in.</div>}
      {params.get("error") && <div className="notice bad" style={{ marginBottom: 16 }}>{params.get("error")}</div>}
      {d.dry_run && <div className="notice warn" style={{ marginBottom: 16 }}>DRY_RUN is on: issue operations are logged instead of sent to GitHub.</div>}

      {d.mode !== "app" ? (
        <div className="card">
          <h2>{d.mode === "token" ? "Connected with a personal access token" : "GitHub is not connected"}</h2>
          {d.mode === "token" && (
            <p className="muted">
              Issues go to each service's repository, the default below, or <code>{d.env_default_repo || "GITHUB_REPO"}</code>. Switch to a GitHub App to restrict repositories here.
            </p>
          )}
          <AppSetup d={d} />
        </div>
      ) : (
        <div className="grid two" style={{ marginBottom: 16 }}>
          <div className="card">
            <h2>Installations</h2>
            {d.installations.length === 0 ? (
              <Empty title="Not installed yet">Install the app on your account or organization and pick repositories.</Empty>
            ) : (
              <table>
                <tbody>
                  {d.installations.map((i) => (
                    <tr key={i.id}>
                      <td>
                        <strong>{i.account}</strong> <span className="badge">{i.account_type}</span>
                      </td>
                      <td className="muted small">{i.repo_selection === "all" ? "all repositories" : "selected repositories"}</td>
                      <td className="muted small nowrap">synced {ago(i.updated_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
          <div className="card">
            <h2>Default repository</h2>
            <p className="small muted" style={{ marginTop: 0 }}>
              Used for services without their own repository.
            </p>
            <select value={d.default_repo} onChange={(e) => setDefault(e.target.value)} style={{ width: "100%" }} aria-label="Default repository">
              <option value="">None — services without a repo stay pending</option>
              {allowed.map((r) => (
                <option key={r.full_name}>{r.full_name}</option>
              ))}
            </select>
          </div>
        </div>
      )}

      {d.mode === "app" && (
        <div className="card flush">
          <div className="card-head" style={{ padding: "14px 16px 0" }}>
            <h2>Repositories</h2>
            <span className="small muted">
              {allowed.length} of {d.repos.length} authorized
            </span>
            <div className="right">
              <input value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="Filter" aria-label="Filter repositories" />
            </div>
          </div>
          {d.repos.length === 0 ? (
            <Empty title="No repositories visible to the app" />
          ) : (
            <table style={{ marginTop: 10 }}>
              <thead>
                <tr>
                  <th>Repository</th>
                  <th>Visibility</th>
                  <th style={{ width: 200 }}>Sentinel may create issues</th>
                </tr>
              </thead>
              <tbody>
                {repos.map((r) => (
                  <tr key={r.full_name}>
                    <td className="mono">{r.full_name}</td>
                    <td>{r.private ? <span className="badge">private</span> : <span className="badge accent">public</span>}</td>
                    <td>
                      <Switch checked={r.allowed} onChange={(v) => allow(r, v)} label={`Allow ${r.full_name}`} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
      {d.mode === "app" && (
        <details className="card">
          <summary style={{ cursor: "pointer", fontWeight: 600 }}>GitHub App settings reference</summary>
          <AppSetup d={d} />
        </details>
      )}
      <p className="small muted" style={{ marginTop: 16 }}>
        Automated fixes (Sentinel's fixer or GitHub Actions) are set up under <Link to="/settings/fixer">Settings → Fixer</Link>.
      </p>
    </>
  );
}

function AppSetup({ d }: { d: GitHubStatus }) {
  return (
    <div style={{ marginTop: 12 }}>
      <ol className="steps">
        <li>
          Create a GitHub App (Settings → Developer settings → GitHub Apps → New) on your account or organization.
        </li>
        <li>
          Callback URL: <code>{d.callback_url}</code> (used for “Sign in with GitHub”).
        </li>
        <li>
          Setup URL: <code>{d.setup_url}</code> and tick “Redirect on update”.
        </li>
        <li>
          Repository permissions: <strong>Issues: Read &amp; write</strong>, <strong>Metadata: Read-only</strong>, and for Sentinel's fixer also{" "}
          <strong>Contents</strong> and <strong>Pull requests: Read &amp; write</strong>. Organization permissions: <strong>Members: Read-only</strong> (only for{" "}
          <code>ALLOWED_ORG</code>).
        </li>
        <li>Webhook: not needed (uncheck “Active”).</li>
        <li>
          Generate a private key and client secret, then start Sentinel with <code>GITHUB_APP_ID</code>, <code>GITHUB_APP_PRIVATE_KEY_FILE</code>, <code>GITHUB_APP_CLIENT_ID</code>,{" "}
          <code>GITHUB_APP_CLIENT_SECRET</code>.
        </li>
      </ol>
    </div>
  );
}
