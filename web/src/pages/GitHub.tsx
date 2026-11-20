import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api, GitHubStatus, Repo } from "../api";
import { Empty, ErrorNote, Icon, Loading, Switch, useAsync, useAction, useToast } from "../components/ui";
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
      <AutofixSetup />
    </>
  );
}

interface FixerStatus {
  connected: boolean;
  last_seen?: string;
  version?: string;
  auth?: "subscription" | "api_key";
  working?: { id: number; kind?: string; repo: string; issue: number; since: string };
  last_error?: string;
}

interface AutofixInfo {
  mode: "actions" | "fixer";
  path?: string;
  workflow?: string;
  label?: string;
  note?: string;
  fixer?: FixerStatus;
}

function FixerCard({ f }: { f: FixerStatus }) {
  return (
    <div className="card">
      <div className="card-head">
        <h2>Automated fixes: Sentinel's fixer</h2>
        <div className="right">
          {f.connected ? <span className="badge ok">● connected</span> : <span className="badge bad">not connected</span>}
        </div>
      </div>
      <p className="small muted">
        When <strong>Auto-fix</strong> is on for a service (Settings → Services), or someone presses <strong>Fix it</strong> on an issue, the issue is queued
        for the fixer container. It runs Claude Code on the repository and opens a pull request that says <code>Fixes #N</code>; Sentinel links it on the issue.
      </p>
      <div className="kv" style={{ gridTemplateColumns: "160px 1fr" }}>
        <span className="k">Last asked for work</span>
        <span className="v">{f.last_seen ? ago(f.last_seen) : "never"}</span>
        <span className="k">Claude sign-in</span>
        <span className="v">
          {f.auth === "subscription" ? "your Claude subscription (CLAUDE_CODE_OAUTH_TOKEN)" : f.auth === "api_key" ? "API key (billed per token)" : "—"}
        </span>
        <span className="k">Working on</span>
        <span className="v">
          {f.working
            ? f.working.kind === "analyze"
              ? `investigating an error${f.working.repo ? ` in ${f.working.repo}` : ""}, since ${ago(f.working.since)}`
              : `fixing ${f.working.repo}#${f.working.issue}, since ${ago(f.working.since)}`
            : "nothing"}
        </span>
        {f.last_error && (
          <>
            <span className="k">Last error</span>
            <span className="v" style={{ color: "var(--bad)" }}>
              {f.last_error}
            </span>
          </>
        )}
      </div>
      {!f.connected && (
        <ol className="steps small" style={{ marginTop: 12 }}>
          <li>
            On your own machine run <code>claude setup-token</code> and put the token in <code>deploy/.env</code> as <code>CLAUDE_CODE_OAUTH_TOKEN</code>.
          </li>
          <li>
            Set the same <code>FIXER_TOKEN</code> for Sentinel and the fixer, then <code>docker compose --profile fixer up -d --build</code>.
          </li>
          <li>
            Give the GitHub App <strong>Contents</strong> and <strong>Pull requests</strong> (read and write) and accept the new permissions on each installation.
          </li>
        </ol>
      )}
    </div>
  );
}

function AutofixSetup() {
  const toast = useToast();
  const info = useAsync(() => api.get<AutofixInfo>("/api/autofix"), [], 15000);
  if (!info.data) return null;
  if (info.data.mode === "fixer") return <FixerCard f={info.data.fixer!} />;
  const fixerStatus = info.data.fixer;
  const wf = { data: info.data as Required<Pick<AutofixInfo, "path" | "workflow" | "label" | "note">> };
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(wf.data!.workflow);
      toast("Workflow copied");
    } catch {
      toast("Could not copy; select the text instead", true);
    }
  };
  return (
    <div className="card">
      <div className="card-head">
        <h2>Automated fixes</h2>
        <div className="right">
          <button className="small" onClick={copy}>
            Copy workflow
          </button>
        </div>
      </div>
      <p className="small muted">
        When <strong>Auto-fix</strong> is on for a service (Settings → Services), or someone presses <strong>Fix it</strong> on an issue, Sentinel adds the{" "}
        <code>{wf.data.label}</code> label to the GitHub issue. In each repository that should get fixes:
      </p>
      <ol className="steps small">
        <li>
          Save this workflow as <code>{wf.data.path}</code>. It runs Claude Code in GitHub Actions, which fixes the error and opens a pull request with{" "}
          <code>Fixes #N</code>.
        </li>
        <li>
          Add the repository secret <code>ANTHROPIC_API_KEY</code> (or install the Claude GitHub App with <code>claude /install-github-app</code> and use{" "}
          <code>claude_code_oauth_token</code>).
        </li>
        <li>Add the setup your tests need (Go, Node, …) before the Claude step.</li>
      </ol>
      {wf.data.note && <div className="notice small" style={{ marginBottom: 8 }}>{wf.data.note}</div>}
      <p className="small muted">
        Sentinel follows the pull request on the issue's timeline and shows it on the issue; merging it closes the issue. No extra GitHub App permissions are
        needed.
      </p>
      <details>
        <summary className="small muted">Show the workflow</summary>
        <pre className="code" style={{ maxHeight: 420 }}>{wf.data.workflow}</pre>
      </details>
      {fixerStatus && (
        <p className="small muted" style={{ marginTop: 10 }}>
          Sentinel's fixer container (for investigations):{" "}
          {fixerStatus.connected ? <span className="badge ok">connected</span> : <span className="badge bad">not connected</span>}
        </p>
      )}
    </div>
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
