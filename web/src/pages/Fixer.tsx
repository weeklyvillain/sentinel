import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import { ErrorNote, useAsync, useAction, useToast } from "../components/ui";
import { ago } from "../util";

// Settings → Fixer: how automated fixes run (Sentinel's fixer container or
// a GitHub Actions workflow), the fixer's Claude sign-in, models, effort and
// what it may run.

export default function Fixer() {
  return (
    <>
      <div className="page-head">
        <span className="sub">
          Claude Code fixes errors and opens pull requests, and can investigate new errors. Runs are listed under{" "}
          <Link to="/issues?tab=fixer">Issues → Fixer</Link>.
        </span>
      </div>
      <AutofixSetup />
    </>
  );
}

interface FixerStatus {
  connected: boolean;
  last_seen?: string;
  version?: string;
  auth?: "subscription" | "api_key";
  auth_from?: "dashboard" | "fixer";
  needs_sign_in?: boolean;
  working?: { id: number; kind?: string; repo: string; issue: number; since: string; run_id?: number };
  last_error?: string;
}

interface FixerSettings {
  mode: "actions" | "fixer";
  env_mode: "actions" | "fixer";
  claude_kind?: "subscription" | "api_key";
  has_claude: boolean;
  model: string;
  analyze_model: string;
  commands: string[];
  default_commands: string[];
  effort: string;
  analyze_effort: string;
  permissions: Permissions;
}

type Permissions = "commands" | "auto" | "all";

const PERMISSIONS: { value: Permissions; label: string; help: string }[] = [
  { value: "commands", label: "Only the commands below", help: "Claude reads and edits files and runs only the commands listed below." },
  {
    value: "auto",
    label: "Auto mode",
    help: "Claude runs whatever the fix needs (installing packages, scripts, other tools). Claude Code's safety check blocks risky actions; the commands below are allowed without it.",
  },
  {
    value: "all",
    label: "Everything",
    help: "No checks at all. Claude can run any command in the fixer container, with network access.",
  },
];

interface FixerModel {
  id: string;
  name: string;
  alias?: boolean;
}

interface FixerModels {
  aliases: FixerModel[];
  models?: FixerModel[];
  error?: string;
  efforts: string[];
}

interface AutofixInfo {
  mode: "actions" | "fixer";
  settings: FixerSettings;
  fixer: FixerStatus;
  path: string;
  workflow: string;
  label: string;
  note?: string;
}

function AutofixSetup() {
  const info = useAsync(() => api.get<AutofixInfo>("/api/autofix"), [], 15000);
  const run = useAction();
  if (!info.data) return <ErrorNote error={info.error} />;
  const d = info.data;
  const setMode = async (mode: "actions" | "fixer") => {
    if (mode === d.mode) return;
    if (await run(() => api.put("/api/autofix", { mode }), mode === "fixer" ? "Fixes now run in Sentinel's fixer" : "Fixes now run in GitHub Actions"))
      info.reload();
  };
  return (
    <>
      <div className="card">
        <div className="card-head">
          <h2>Automated fixes</h2>
          <div className="right seg">
            <button className={d.mode === "fixer" ? "on" : ""} onClick={() => setMode("fixer")}>
              Sentinel's fixer
            </button>
            <button className={d.mode === "actions" ? "on" : ""} onClick={() => setMode("actions")}>
              GitHub Actions
            </button>
          </div>
        </div>
        <p className="small muted">
          When <strong>Auto-fix</strong> is on for a service (Settings → Services), or someone presses <strong>Fix it</strong> on an issue, Claude Code fixes the
          error and opens a pull request that says <code>Fixes #N</code>; Sentinel links it on the issue and merging it closes the issue.{" "}
          {d.mode === "fixer" ? (
            "Fixes run in the fixer container next to Sentinel, with the Claude sign-in below."
          ) : (
            <>
              Sentinel adds the <code>{d.label}</code> label to the issue and a workflow in the repository does the fix.
            </>
          )}
        </p>
      </div>
      <FixerCard d={d} onSaved={info.reload} />
      {d.mode === "actions" && <WorkflowCard d={d} />}
    </>
  );
}

function signInText(d: AutofixInfo) {
  const f = d.fixer;
  const what = f.auth === "api_key" ? "an Anthropic API key (billed per token)" : "your Claude subscription";
  if (f.auth_from === "dashboard") return `${what}, set here`;
  if (f.auth_from === "fixer") return `${what}, from the fixer's environment`;
  return d.settings.has_claude ? "set here" : "none yet";
}

function FixerCard({ d, onSaved }: { d: AutofixInfo; onSaved: () => void }) {
  const run = useAction();
  const f = d.fixer;
  const [kind, setKind] = useState<"subscription" | "api_key">(d.settings.claude_kind ?? "subscription");
  const [token, setToken] = useState("");
  const [model, setModel] = useState(d.settings.model);
  const [analyzeModel, setAnalyzeModel] = useState(d.settings.analyze_model);
  const [effort, setEffort] = useState(d.settings.effort);
  const [permissions, setPermissions] = useState<Permissions>(d.settings.permissions);
  const [analyzeEffort, setAnalyzeEffort] = useState(d.settings.analyze_effort);
  const [refresh, setRefresh] = useState(0);
  const models = useAsync(() => api.get<FixerModels>("/api/autofix/models" + (refresh ? "?refresh=1" : "")), [refresh, d.settings.has_claude]);
  const [commands, setCommands] = useState(d.settings.commands.join("\n"));
  const cmdList = (s: string) =>
    s
      .split("\n")
      .map((c) => c.trim().replace(/\s+/g, " "))
      .filter(Boolean);
  const dirty =
    token !== "" ||
    model !== d.settings.model ||
    analyzeModel !== d.settings.analyze_model ||
    effort !== d.settings.effort ||
    permissions !== d.settings.permissions ||
    analyzeEffort !== d.settings.analyze_effort ||
    cmdList(commands).join("\n") !== d.settings.commands.join("\n");
  const save = async () => {
    const body = {
      model,
      analyze_model: analyzeModel,
      effort,
      analyze_effort: analyzeEffort,
      permissions,
      commands: cmdList(commands),
      ...(token ? { claude_kind: kind, claude_token: token } : {}),
    };
    if (await run(() => api.put("/api/autofix", body), "Fixer settings saved")) {
      setToken("");
      onSaved();
    }
  };
  const remove = async () => {
    if (await run(() => api.put("/api/autofix", { clear_claude: true }), "Claude sign-in removed")) onSaved();
  };
  return (
    <div className="card">
      <div className="card-head">
        <h2>Sentinel's fixer</h2>
        <div className="right">
          {f.connected ? <span className="badge ok">● connected</span> : <span className="badge bad">not connected</span>}
        </div>
      </div>
      <p className="small muted">
        The fixer container runs Claude Code next to Sentinel. It does the fixes when they run in Sentinel's fixer, and the investigations when Settings → AI
        uses <strong>Claude Code in the fixer</strong>.
      </p>
      {f.needs_sign_in && !d.settings.has_claude && (
        <div className="notice small" style={{ marginBottom: 10 }}>
          The fixer is waiting for a Claude sign-in. Add one below; it picks up work on its next check, within a minute.
        </div>
      )}
      <div className="kv" style={{ gridTemplateColumns: "160px 1fr" }}>
        <span className="k">Last asked for work</span>
        <span className="v">{f.last_seen ? ago(f.last_seen) : "never"}</span>
        <span className="k">Claude sign-in</span>
        <span className="v">{signInText(d)}</span>
        <span className="k">Working on</span>
        <span className="v">
          {f.working
            ? f.working.kind === "analyze"
              ? `investigating an error${f.working.repo ? ` in ${f.working.repo}` : ""}, since ${ago(f.working.since)}`
              : `fixing ${f.working.repo}#${f.working.issue}, since ${ago(f.working.since)}`
            : "nothing"}
          {f.working?.run_id ? (
            <>
              {" · "}
              <Link to={`/issues?tab=fixer&run=${f.working.run_id}`}>watch the log</Link>
            </>
          ) : null}{" "}
          · <Link to="/issues?tab=fixer">all runs</Link>
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

      <h3 style={{ marginTop: 16 }}>Claude sign-in and models</h3>
      <div className="form-grid">
        <label className="field">
          Sign in with
          <select value={kind} onChange={(e) => setKind(e.target.value as "subscription" | "api_key")}>
            <option value="subscription">Claude subscription (token from claude setup-token)</option>
            <option value="api_key">Anthropic API key (billed per token)</option>
          </select>
        </label>
        <label className="field">
          {kind === "subscription" ? "Token" : "API key"}
          <input
            type="password"
            autoComplete="new-password"
            className="mono"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            placeholder={d.settings.has_claude ? "•••••• (stored; paste to replace)" : kind === "subscription" ? "sk-ant-oat01-…" : "sk-ant-api03-…"}
          />
          <span className="help">
            {kind === "subscription" ? (
              <>
                Run <code>claude setup-token</code> on your own machine and paste the token. Fixes count against your plan's usage limits.
              </>
            ) : (
              "Create one in the Claude Console."
            )}{" "}
            It is stored in Sentinel's database and only sent to the fixer.
          </span>
        </label>
        <label className="field">
          Model for fixes
          <ModelPicker value={model} onChange={setModel} models={models.data} placeholder="Claude Code's default" />
        </label>
        <label className="field">
          Effort for fixes
          <EffortPicker value={effort} onChange={setEffort} levels={models.data?.efforts} />
        </label>
        <label className="field">
          Model for investigations
          <ModelPicker value={analyzeModel} onChange={setAnalyzeModel} models={models.data} placeholder="the fixer's default" />
        </label>
        <label className="field">
          Effort for investigations
          <EffortPicker value={analyzeEffort} onChange={setAnalyzeEffort} levels={models.data?.efforts} />
        </label>
        <div className="field full">
          <span className="small muted">
            {models.data?.models?.length
              ? `${models.data.models.length} models available to this sign-in, plus the aliases that always pick the latest of a family.`
              : models.data?.error
                ? `Only the aliases are listed: ${models.data.error}.`
                : "Loading the models…"}{" "}
            Higher effort thinks longer and uses more tokens.{" "}
            {d.settings.has_claude && (
              <button type="button" className="small" onClick={() => setRefresh((n) => n + 1)}>
                Refresh models
              </button>
            )}
          </span>
        </div>
        <div className="field full">
          What Claude may do in fixes
          <div className="stack" style={{ gap: 6, marginTop: 4 }}>
            {PERMISSIONS.map((p) => (
              <label key={p.value} className="check" style={{ alignItems: "flex-start" }}>
                <input type="radio" name="fixer-permissions" checked={permissions === p.value} onChange={() => setPermissions(p.value)} />
                <span>
                  <strong>{p.label}</strong>
                  <span className="small muted"> · {p.help}</span>
                </span>
              </label>
            ))}
          </div>
          {permissions !== "commands" && (
            <div className={"notice small " + (permissions === "all" ? "bad" : "")} style={{ marginTop: 8 }}>
              The error report Claude works from is telemetry, which users or attackers can influence. With {permissions === "all" ? "no checks" : "auto mode"},
              a report crafted to mislead Claude could make it run commands in the fixer container, which has network access and your Claude sign-in
              in its environment. It still cannot read Sentinel's or GitHub's tokens, and the fixer never pushes changes under <code>.github/</code>.
              Investigations stay read-only either way.
            </div>
          )}
        </div>
        <label className="field full">
          {permissions === "commands" ? "Commands Claude may run in fixes" : "Commands always allowed"}
          <textarea className="mono" rows={4} value={commands} onChange={(e) => setCommands(e.target.value)} placeholder={"go build\ngo test"} />
          <span className="help">
            One command per line; it may run each with any arguments, e.g. <code>go test</code> allows <code>go test ./...</code>. No shell syntax (
            <code>;</code>, <code>&amp;&amp;</code>, <code>|</code>, <code>$</code>). Reading and editing files is always allowed; investigations only read.
            The fixer has Go and Node; other toolchains go in <code>Dockerfile.fixer</code>. These commands run your repository's code inside the fixer
            container, without access to Sentinel's or GitHub's tokens.{" "}
            {commands.trim() !== d.settings.default_commands.join("\n") && (
              <button type="button" className="small" onClick={() => setCommands(d.settings.default_commands.join("\n"))}>
                Use the defaults ({d.settings.default_commands.join(", ")})
              </button>
            )}
          </span>
        </label>
      </div>
      <div className="row" style={{ marginTop: 12, gap: 8 }}>
        <button className="primary" onClick={save} disabled={!dirty}>
          Save
        </button>
        {d.settings.has_claude && <button onClick={remove}>Remove sign-in</button>}
      </div>
      {!f.connected && (
        <p className="small muted" style={{ marginTop: 12 }}>
          The fixer starts with Sentinel (<code>docker compose up -d --build</code>) and connects by itself.
        </p>
      )}
      {d.mode === "fixer" && (
        <p className="small muted" style={{ marginTop: 8 }}>
          For fixes, give the GitHub App <strong>Contents</strong> and <strong>Pull requests</strong> (read and write) and accept the new permissions on each
          installation.
        </p>
      )}
    </div>
  );
}

function ModelPicker(p: { value: string; onChange: (v: string) => void; models?: FixerModels; placeholder: string }) {
  const listed = p.models?.models ?? [];
  const known = [...(p.models?.aliases ?? []), ...listed].some((m) => m.id === p.value);
  return (
    <select className="mono" value={p.value} onChange={(e) => p.onChange(e.target.value)}>
      <option value="">{p.placeholder}</option>
      <optgroup label="Latest of a family">
        {(p.models?.aliases ?? []).map((m) => (
          <option key={m.id} value={m.id}>
            {m.name}
          </option>
        ))}
      </optgroup>
      {listed.length > 0 && (
        <optgroup label="Models">
          {listed.map((m) => (
            <option key={m.id} value={m.id}>
              {m.name} ({m.id})
            </option>
          ))}
        </optgroup>
      )}
      {p.value && !known && <option value={p.value}>{p.value}</option>}
    </select>
  );
}

function EffortPicker(p: { value: string; onChange: (v: string) => void; levels?: string[] }) {
  return (
    <select value={p.value} onChange={(e) => p.onChange(e.target.value)}>
      <option value="">Claude Code's default</option>
      {(p.levels ?? ["low", "medium", "high", "xhigh", "max"]).map((l) => (
        <option key={l} value={l}>
          {l}
        </option>
      ))}
    </select>
  );
}

function WorkflowCard({ d }: { d: AutofixInfo }) {
  const toast = useToast();
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(d.workflow);
      toast("Workflow copied");
    } catch {
      toast("Could not copy; select the text instead", true);
    }
  };
  return (
    <div className="card">
      <div className="card-head">
        <h2>GitHub Actions workflow</h2>
        <div className="right">
          <button className="small" onClick={copy}>
            Copy workflow
          </button>
        </div>
      </div>
      <p className="small muted">In each repository that should get fixes:</p>
      <ol className="steps small">
        <li>
          Save this workflow as <code>{d.path}</code>. It runs Claude Code in GitHub Actions when Sentinel adds the <code>{d.label}</code> label.
        </li>
        <li>
          Add the repository secret <code>ANTHROPIC_API_KEY</code> (or install the Claude GitHub App with <code>claude /install-github-app</code> and use{" "}
          <code>claude_code_oauth_token</code>).
        </li>
        <li>Add the setup your tests need (Go, Node, …) before the Claude step.</li>
      </ol>
      {d.note && <div className="notice small" style={{ marginBottom: 8 }}>{d.note}</div>}
      <details>
        <summary className="small muted">Show the workflow</summary>
        <pre className="code" style={{ maxHeight: 420 }}>{d.workflow}</pre>
      </details>
    </div>
  );
}
