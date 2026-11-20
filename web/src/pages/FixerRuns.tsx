import { useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { api, qs } from "../api";
import { Empty, ErrorNote, Loading, Seg, Stat, useAsync } from "../components/ui";
import { ago, fmtFull, fmtNum } from "../util";

// Issues → Fixer: every job Sentinel's fixer ran (fixes and investigations),
// what Claude Code did and what it used.

interface FixerRun {
  id: number;
  kind: "fix" | "analyze";
  group_id: number;
  analysis_id?: number;
  repo: string;
  issue_number?: number;
  attempt: number;
  status: string; // running | pr | no_fix | failed | dry_run | done | lost
  started_at: string;
  finished_at?: string;
  auth?: "subscription" | "api_key";
  model?: string;
  turns: number;
  duration_ms: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cost_usd: number;
  pr_url?: string;
  summary?: string;
  error?: string;
  log_lines: number;
  service?: string;
  group_title?: string;
  issue_url?: string;
}

interface FixerStats {
  runs: number;
  by_status: Record<string, number>;
  fixes: number;
  investigations: number;
  turns: number;
  duration_ms: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cost_usd: number;
}

interface LogLine {
  seq: number;
  at: string;
  kind: "step" | "error" | "text" | "tool" | "tool_result" | "tool_error";
  text: string;
}

type Period = "24h" | "7d" | "30d";

const STATUS: Record<string, { label: string; cls: string }> = {
  running: { label: "running", cls: "accent" },
  pr: { label: "PR opened", cls: "ok" },
  done: { label: "investigated", cls: "ok" },
  no_fix: { label: "no fix", cls: "warn" },
  dry_run: { label: "dry run", cls: "" },
  failed: { label: "failed", cls: "bad" },
  lost: { label: "lost", cls: "bad" },
};

function RunStatus({ s }: { s: string }) {
  const st = STATUS[s] ?? { label: s, cls: "" };
  return <span className={"badge " + st.cls}>{st.label}</span>;
}

const tokens = (r: { input_tokens: number; output_tokens: number; cache_read_tokens: number; cache_write_tokens: number }) =>
  r.input_tokens + r.output_tokens + r.cache_read_tokens + r.cache_write_tokens;

function dur(ms: number) {
  if (!ms) return "—";
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
}

const usd = (n: number) => (n ? `$${n < 1 ? n.toFixed(3) : n.toFixed(2)}` : "—");

export function FixerRuns() {
  const [params, setParams] = useSearchParams();
  const runID = Number(params.get("run") ?? 0);
  const open = (id?: number) => {
    const p = new URLSearchParams(params);
    if (id) p.set("run", String(id));
    else p.delete("run");
    setParams(p);
  };
  return runID ? <RunView id={runID} onBack={() => open()} /> : <RunList onOpen={open} />;
}

function RunList({ onOpen }: { onOpen: (id: number) => void }) {
  const [period, setPeriod] = useState<Period>("7d");
  const [kind, setKind] = useState("");
  const list = useAsync(() => api.get<{ runs: FixerRun[]; stats: Record<Period, FixerStats> }>("/api/fixer/runs" + qs({ kind, limit: 200 })), [kind], 5000);
  const st = list.data?.stats[period];
  const prs = st?.by_status["pr"] ?? 0;
  const failed = (st?.by_status["failed"] ?? 0) + (st?.by_status["lost"] ?? 0);
  const subscription = (list.data?.runs ?? []).some((r) => r.auth === "subscription");
  return (
    <>
      <div className="row" style={{ margin: "0 0 12px" }}>
        <span className="small muted">
          Every job Sentinel's fixer ran with Claude Code: fixes and investigations, what it did and what it used. Set it up under{" "}
          <Link to="/settings/github">Settings → GitHub</Link>.
        </span>
        <div className="row" style={{ marginLeft: "auto" }}>
          <select value={kind} onChange={(e) => setKind(e.target.value)} aria-label="Kind">
            <option value="">Fixes and investigations</option>
            <option value="fix">Fixes</option>
            <option value="analyze">Investigations</option>
          </select>
          <Seg<Period>
            value={period}
            onChange={setPeriod}
            options={[
              { value: "24h", label: "24 hours" },
              { value: "7d", label: "7 days" },
              { value: "30d", label: "30 days" },
            ]}
          />
        </div>
      </div>
      <ErrorNote error={list.error} />
      {st && (
        <div className="grid stats" style={{ marginBottom: 16 }}>
          <Stat label="Runs" value={fmtNum(st.runs)} hint={`${st.fixes} fixes · ${st.investigations} investigations`} />
          <Stat label="Pull requests opened" value={fmtNum(prs)} hint={st.fixes ? `${Math.round((prs / st.fixes) * 100)}% of fixes` : "no fixes yet"} tone={prs ? "ok" : undefined} />
          <Stat label="Failed" value={fmtNum(failed)} hint={`${st.by_status["no_fix"] ?? 0} without a fix`} tone={failed ? "bad" : undefined} />
          <Stat
            label="Tokens"
            value={fmtNum(tokens(st))}
            hint={`${fmtNum(st.output_tokens)} out · ${fmtNum(st.cache_read_tokens)} cache read · ${fmtNum(st.input_tokens + st.cache_write_tokens)} in`}
          />
          <Stat
            label={subscription ? "Cost (API-equivalent)" : "Cost"}
            value={usd(st.cost_usd)}
            hint={subscription ? "as reported by Claude Code; a subscription is not billed per token" : "as reported by Claude Code"}
          />
          <Stat label="Claude Code time" value={dur(st.duration_ms)} hint={`${fmtNum(st.turns)} turns`} />
        </div>
      )}
      <div className="card flush">
        {!list.data ? (
          <Loading />
        ) : list.data.runs.length === 0 ? (
          <Empty title="No fixer runs yet">When the fixer picks up a fix or an investigation, it shows up here with its log.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Started</th>
                <th>Job</th>
                <th>Issue</th>
                <th>Result</th>
                <th>Model</th>
                <th style={{ textAlign: "right" }}>Turns</th>
                <th style={{ textAlign: "right" }}>Tokens</th>
                <th style={{ textAlign: "right" }}>Cost</th>
                <th style={{ textAlign: "right" }}>Time</th>
              </tr>
            </thead>
            <tbody>
              {list.data.runs.map((r) => (
                <tr key={r.id} className="clickable" onClick={() => onOpen(r.id)}>
                  <td className="small muted nowrap" title={fmtFull(r.started_at)}>
                    {ago(r.started_at)}
                  </td>
                  <td className="nowrap">{r.kind === "analyze" ? "Investigation" : `Fix, attempt ${r.attempt}`}</td>
                  <td>
                    <div style={{ fontWeight: 600 }}>
                      {r.service || r.repo || "—"}
                      {r.issue_number ? <span className="muted"> #{r.issue_number}</span> : null}
                    </div>
                    <div className="small faint" style={{ maxWidth: 380, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }} title={r.group_title}>
                      {r.group_title}
                    </div>
                  </td>
                  <td className="nowrap">
                    <RunStatus s={r.status} />
                  </td>
                  <td className="mono small nowrap">{r.model || "—"}</td>
                  <td className="small" style={{ textAlign: "right" }}>
                    {r.turns || "—"}
                  </td>
                  <td className="small nowrap" style={{ textAlign: "right" }} title={`${fmtNum(r.output_tokens)} out, ${fmtNum(r.cache_read_tokens)} cache read`}>
                    {tokens(r) ? fmtNum(tokens(r)) : "—"}
                  </td>
                  <td className="small" style={{ textAlign: "right" }}>
                    {usd(r.cost_usd)}
                  </td>
                  <td className="small muted nowrap" style={{ textAlign: "right" }}>
                    {r.status === "running" ? <Elapsed since={r.started_at} /> : dur(r.duration_ms)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}

function Elapsed({ since }: { since: string }) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  return <>{dur(now - new Date(since).getTime())}</>;
}

function RunView({ id, onBack }: { id: number; onBack: () => void }) {
  const [run, setRun] = useState<FixerRun>();
  const [lines, setLines] = useState<LogLine[]>([]);
  const [err, setErr] = useState<string>();
  const [follow, setFollow] = useState(true);
  const last = useRef(0);
  const end = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let stop = false;
    let timer: ReturnType<typeof setTimeout>;
    last.current = 0;
    setLines([]);
    const load = async () => {
      try {
        const d = await api.get<{ run: FixerRun; log: LogLine[] }>(`/api/fixer/runs/${id}` + qs({ after: last.current }));
        if (stop) return;
        setRun(d.run);
        setErr(undefined);
        if (d.log.length) {
          last.current = d.log[d.log.length - 1].seq;
          setLines((l) => [...l, ...d.log]);
        }
        if (d.run.status === "running") timer = setTimeout(load, 2000);
      } catch (e) {
        if (stop) return;
        setErr((e as Error).message);
        timer = setTimeout(load, 5000);
      }
    };
    load();
    return () => {
      stop = true;
      clearTimeout(timer);
    };
  }, [id]);

  useEffect(() => {
    if (follow && run?.status === "running") end.current?.scrollIntoView({ block: "nearest" });
  }, [lines, follow, run?.status]);

  if (!run) return err ? <ErrorNote error={err} /> : <Loading />;
  const start = new Date(run.started_at).getTime();
  return (
    <>
      <div className="row" style={{ marginBottom: 12 }}>
        <button className="small" onClick={onBack}>
          ← All runs
        </button>
        <h2 style={{ margin: 0 }}>
          {run.kind === "analyze" ? "Investigation" : `Fix, attempt ${run.attempt}`}: {run.service || run.repo}
          {run.issue_number ? ` #${run.issue_number}` : ""}
        </h2>
        <RunStatus s={run.status} />
      </div>
      <ErrorNote error={err} />
      <div className="card">
        <div className="kv" style={{ gridTemplateColumns: "150px 1fr" }}>
          {run.group_title && (
            <>
              <span className="k">Error</span>
              <span className="v">
                <Link to={`/issues/${run.group_id}`}>{run.group_title}</Link>
                {run.issue_url && (
                  <>
                    {" · "}
                    <a href={run.issue_url} target="_blank" rel="noreferrer">
                      GitHub issue ↗
                    </a>
                  </>
                )}
              </span>
            </>
          )}
          <span className="k">Repository</span>
          <span className="v">{run.repo || "none (from the report alone)"}</span>
          <span className="k">Started</span>
          <span className="v">
            {fmtFull(run.started_at)}
            {run.finished_at ? `, took ${dur(new Date(run.finished_at).getTime() - start)}` : ""}
          </span>
          <span className="k">Claude Code</span>
          <span className="v">
            {run.model || "default model"}
            {run.auth ? ` · ${run.auth === "api_key" ? "API key" : "subscription"}` : ""}
            {run.turns ? ` · ${run.turns} turns · ${dur(run.duration_ms)}` : ""}
          </span>
          {tokens(run) > 0 && (
            <>
              <span className="k">Tokens</span>
              <span className="v">
                {fmtNum(tokens(run))} total: {fmtNum(run.output_tokens)} out, {fmtNum(run.input_tokens)} in, {fmtNum(run.cache_read_tokens)} cache read,{" "}
                {fmtNum(run.cache_write_tokens)} cache write
              </span>
              <span className="k">Cost</span>
              <span className="v">
                {usd(run.cost_usd)}
                {run.auth === "subscription" ? " API-equivalent (not billed per token on a subscription)" : ""}
              </span>
            </>
          )}
          {run.pr_url && (
            <>
              <span className="k">Pull request</span>
              <span className="v">
                <a href={run.pr_url} target="_blank" rel="noreferrer">
                  {run.pr_url} ↗
                </a>
              </span>
            </>
          )}
          {run.error && (
            <>
              <span className="k">Error</span>
              <span className="v" style={{ color: "var(--bad)" }}>
                {run.error}
              </span>
            </>
          )}
        </div>
        {run.summary && <pre className="code" style={{ marginTop: 12 }}>{run.summary}</pre>}
      </div>
      <div className="card">
        <div className="card-head">
          <h2>Log</h2>
          <div className="right">
            {run.status === "running" && (
              <label className="small">
                <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> Follow
              </label>
            )}
            <span className="small faint">{lines.length} lines</span>
          </div>
        </div>
        {lines.length === 0 ? (
          <p className="small muted">
            {run.log_lines > 0
              ? "This run's log is no longer kept (logs are kept for 30 days)."
              : run.status === "running"
                ? "Waiting for the fixer…"
                : "No log for this run."}
          </p>
        ) : (
          <div className="run-log">
            {lines.map((l) => (
              <LogRow key={l.seq} l={l} start={start} />
            ))}
            <div ref={end} />
          </div>
        )}
      </div>
    </>
  );
}

const KIND_LABEL: Record<LogLine["kind"], string> = {
  step: "fixer",
  error: "error",
  text: "claude",
  tool: "tool",
  tool_result: "result",
  tool_error: "tool error",
};

function LogRow({ l, start }: { l: LogLine; start: number }) {
  const s = Math.max(0, Math.round((new Date(l.at).getTime() - start) / 1000));
  const t = `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
  const long = (l.kind === "tool_result" || l.kind === "tool_error") && (l.text.length > 240 || l.text.split("\n").length > 4);
  return (
    <div className={"log-row " + l.kind}>
      <span className="t" title={fmtFull(l.at)}>
        {t}
      </span>
      <span className="k">{KIND_LABEL[l.kind] ?? l.kind}</span>
      {long ? (
        <details>
          <summary>{l.text.split("\n")[0].slice(0, 160)}…</summary>
          <pre>{l.text}</pre>
        </details>
      ) : (
        <pre>{l.text}</pre>
      )}
    </div>
  );
}
