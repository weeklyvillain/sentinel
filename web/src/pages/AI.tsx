import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { AIModel, AIState, AITestResult, AISettings, Analysis, api } from "../api";
import { Empty, ErrorNote, Loading, useAsync, useToast } from "../components/ui";
import { ago, fmtFull } from "../util";

// Small models that run on a CPU-only server. Sizes are the download.
const SUGGESTED = [
  { name: "qwen2.5:1.5b", note: "≈1 GB, ~1.5 GB RAM" },
  { name: "qwen2.5:3b", note: "≈1.9 GB, ~2.5 GB RAM" },
  { name: "llama3.2:3b", note: "≈2 GB, ~2.6 GB RAM" },
];

function secs(ms: number) {
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 90) return `${s.toFixed(1)} s`;
  return `${Math.floor(s / 60)} min ${Math.round(s % 60)} s`;
}

function gb(bytes?: number) {
  return bytes ? `${(bytes / 1e9).toFixed(1)} GB` : "";
}

export default function AI() {
  const state = useAsync(() => api.get<AIState>("/api/ai"), []);
  return (
    <>
      <div className="page-head">
        <h1>AI analysis</h1>
        <span className="sub">
          Before an error group becomes a GitHub issue, a model writes its title and analysis. Jobs run one at a time in the background; an
          issue is filed when its analysis is done. If it fails, the issue uses the built-in template.
        </span>
      </div>
      <ErrorNote error={state.error} />
      {!state.data ? <Loading /> : <SettingsCard state={state.data} onSaved={state.reload} />}
      <Queue />
    </>
  );
}

function SettingsCard({ state, onSaved }: { state: AIState; onSaved: () => void }) {
  const toast = useToast();
  const [f, setF] = useState<AISettings>(state.settings);
  const [apiKey, setApiKey] = useState("");
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [test, setTest] = useState<AITestResult | null>(null);
  const [err, setErr] = useState<string>();
  const set = (p: Partial<AISettings>) => setF((x) => ({ ...x, ...p }));
  const dirty = JSON.stringify(f) !== JSON.stringify(state.settings) || apiKey !== "";

  const payload = () => ({
    provider: f.provider,
    model: f.model,
    base_url: f.base_url,
    timeout_s: Number(f.timeout_s),
    ...(apiKey ? { api_key: apiKey } : {}),
  });

  const save = async () => {
    setSaving(true);
    setErr(undefined);
    try {
      await api.put("/api/ai", payload());
      setApiKey("");
      toast("AI settings saved");
      onSaved();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setSaving(false);
    }
  };
  const tryIt = async () => {
    setTesting(true);
    setTest(null);
    setErr(undefined);
    try {
      setTest(await api.post<AITestResult>("/api/ai/test", payload()));
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setTesting(false);
    }
  };

  return (
    <div className="card" style={{ marginBottom: 16 }}>
      <div className="row" style={{ marginBottom: 12 }}>
        <h3 style={{ margin: 0 }}>Provider</h3>
        <span className="small muted" style={{ marginLeft: "auto" }}>
          In use: <span className="mono">{state.provider_name}</span>
        </span>
      </div>
      <ErrorNote error={err} />
      <div className="form-grid">
        <label className="field">
          Provider
          <select value={f.provider} onChange={(e) => set({ provider: e.target.value as AISettings["provider"] })}>
            <option value="openai">Local / OpenAI-compatible (Ollama, llama.cpp, vLLM…)</option>
            <option value="anthropic">Anthropic (Claude)</option>
            <option value="none">None (template only)</option>
          </select>
        </label>
        <label className="field">
          Timeout per analysis (seconds)
          <input type="number" min={5} max={3600} value={f.timeout_s} onChange={(e) => set({ timeout_s: Number(e.target.value) })} />
          <span className="help">A small model on a CPU needs minutes, not seconds.</span>
        </label>
        {f.provider === "openai" && (
          <label className="field">
            Base URL
            <input className="mono" value={f.base_url} onChange={(e) => set({ base_url: e.target.value })} placeholder="http://ollama:11434/v1" />
          </label>
        )}
        {f.provider !== "none" && (
          <label className="field">
            Model
            <input
              className="mono"
              list="ai-models"
              value={f.model}
              onChange={(e) => set({ model: e.target.value })}
              placeholder={f.provider === "anthropic" ? "claude-haiku-5-5 (default)" : "qwen2.5:1.5b"}
            />
          </label>
        )}
        {f.provider !== "none" && (
          <label className="field">
            API key
            <input
              type="password"
              autoComplete="new-password"
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
              placeholder={
                state.settings.has_api_key
                  ? "•••••• (stored; type to replace)"
                  : state.settings.env_api_key
                    ? "from the environment"
                    : f.provider === "openai"
                      ? "not needed for Ollama"
                      : "required"
              }
            />
          </label>
        )}
      </div>

      {f.provider === "openai" && <Models baseURL={f.base_url} current={f.model} onPick={(m) => set({ model: m })} pull={state.pull} />}

      <div className="row" style={{ marginTop: 14, gap: 8 }}>
        <button onClick={tryIt} disabled={testing || f.provider === "none"}>
          {testing ? "Analyzing a sample error…" : "Try it on a sample error"}
        </button>
        <button className="primary" onClick={save} disabled={saving || !dirty}>
          Save
        </button>
        {testing && <span className="small muted">This waits for the model; on a CPU that can take a few minutes.</span>}
      </div>
      {test && <TestResult r={test} />}
    </div>
  );
}

function TestResult({ r }: { r: AITestResult }) {
  if (r.error)
    return (
      <div className="notice bad" style={{ marginTop: 12 }}>
        <b>Failed after {secs(r.took_ms)}</b> ({r.provider}): {r.error}
      </div>
    );
  const d = r.draft!;
  return (
    <div className="notice ok" style={{ marginTop: 12, color: "inherit" }}>
      <div className="small muted" style={{ marginBottom: 6 }}>
        {r.provider} answered in <b>{secs(r.took_ms)}</b>
      </div>
      <div style={{ fontWeight: 600, marginBottom: 6 }}>{d.title}</div>
      <div style={{ marginBottom: 6 }}>{d.summary}</div>
      <div className="small" style={{ marginBottom: 6 }}>
        <b>Likely cause:</b> {d.likely_cause}
      </div>
      <ul className="small" style={{ margin: 0, paddingLeft: 18 }}>
        {(d.next_steps ?? []).map((s, i) => (
          <li key={i}>{s}</li>
        ))}
      </ul>
    </div>
  );
}

function Models({ baseURL, current, onPick, pull: initialPull }: { baseURL: string; current: string; onPick: (m: string) => void; pull?: AIState["pull"] }) {
  const toast = useToast();
  const models = useAsync(() => api.get<AIModel[]>(`/api/ai/models?base_url=${encodeURIComponent(baseURL)}`), [baseURL]);
  const [pull, setPull] = useState(initialPull);
  const [name, setName] = useState("qwen2.5:1.5b");
  const pulling = !!pull && !pull.done;

  useEffect(() => {
    if (!pulling) return;
    const t = setInterval(async () => {
      const s = await api.get<AIState>("/api/ai");
      setPull(s.pull);
      if (s.pull?.done) {
        models.reload();
        if (s.pull.error) toast(`Download failed: ${s.pull.error}`, true);
        else toast(`Downloaded ${s.pull.model}`);
      }
    }, 1500);
    return () => clearInterval(t);
  }, [pulling]); // eslint-disable-line react-hooks/exhaustive-deps

  const start = async (m: string) => {
    try {
      setPull(await api.post("/api/ai/pull", { model: m, base_url: baseURL }));
    } catch (e) {
      toast((e as Error).message, true);
    }
  };
  const pct = pull && pull.total ? Math.round((pull.completed / pull.total) * 100) : 0;

  return (
    <div className="card" style={{ marginTop: 14, background: "var(--panel-2)" }}>
      <datalist id="ai-models">
        {(models.data ?? []).map((m) => (
          <option key={m.name} value={m.name} />
        ))}
      </datalist>
      <div className="row" style={{ marginBottom: 8 }}>
        <h3 style={{ margin: 0 }}>Models on the server</h3>
        <button className="small ghost" style={{ marginLeft: "auto" }} onClick={models.reload}>
          Refresh
        </button>
      </div>
      {models.error ? (
        <div className="notice warn">Can't reach {baseURL}: {models.error}</div>
      ) : !models.data ? (
        <Loading />
      ) : models.data.length === 0 ? (
        <div className="small muted">No models downloaded yet.</div>
      ) : (
        <div className="row" style={{ flexWrap: "wrap", gap: 6 }}>
          {models.data.map((m) => (
            <button key={m.name} className={"small" + (m.name === current ? " primary" : "")} onClick={() => onPick(m.name)}>
              <span className="mono">{m.name}</span> {m.size ? <span className="faint">{gb(m.size)}</span> : null}
            </button>
          ))}
        </div>
      )}
      <div className="row" style={{ marginTop: 12, gap: 8, flexWrap: "wrap" }}>
        <input className="mono" style={{ maxWidth: 220 }} value={name} onChange={(e) => setName(e.target.value)} list="ai-suggested" aria-label="Model to download" />
        <datalist id="ai-suggested">
          {SUGGESTED.map((s) => (
            <option key={s.name} value={s.name}>
              {s.note}
            </option>
          ))}
        </datalist>
        <button onClick={() => start(name)} disabled={pulling || !name}>
          Download model
        </button>
        <span className="small faint">Ollama only. Suggested for a 2-CPU, 4 GB server: {SUGGESTED.map((s) => `${s.name} (${s.note})`).join(", ")}.</span>
      </div>
      {pull && (
        <div style={{ marginTop: 10 }}>
          <div className="small muted">
            {pull.model}: {pull.error ? <span style={{ color: "var(--bad)" }}>{pull.error}</span> : pull.done ? "downloaded" : `${pull.status}${pct ? ` · ${pct}%` : ""}`}
          </div>
          {!pull.done && (
            <div style={{ height: 6, background: "var(--border)", borderRadius: 3, marginTop: 6, overflow: "hidden" }}>
              <div style={{ width: `${pct}%`, height: "100%", background: "var(--accent)", transition: "width .5s" }} />
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function Timing({ a, now }: { a: Analysis; now: number }) {
  const t = (s?: string) => (s ? new Date(s).getTime() : 0);
  if (a.status === "running") return <span>running {secs(now - t(a.started_at))}</span>;
  if (a.status === "queued") return <span title={fmtFull(a.queued_at)}>queued {ago(a.queued_at)}</span>;
  return <span title={fmtFull(a.finished_at)}>took {secs(t(a.finished_at) - t(a.started_at))}</span>;
}

function StatusBadge({ s }: { s: Analysis["status"] }) {
  const cls = { running: "warn", queued: "", done: "ok", failed: "bad" }[s];
  return (
    <span className={`badge ${cls}`}>
      {s === "running" && <span className="dot warn" />} {s}
    </span>
  );
}

function Queue() {
  const toast = useToast();
  const list = useAsync(() => api.get<Analysis[]>("/api/analyses"), [], 3000);
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  const retry = async (a: Analysis) => {
    try {
      await api.post(`/api/analyses/${a.id}/retry`);
      toast("Queued again");
      list.reload();
    } catch (e) {
      toast((e as Error).message, true);
    }
  };
  const waiting = (list.data ?? []).filter((a) => a.status === "queued" || a.status === "running").length;
  let pos = 0;

  return (
    <>
      <div className="page-head" style={{ marginTop: 24 }}>
        <h2 style={{ margin: 0 }}>Analysis queue</h2>
        <span className="sub">{waiting ? `${waiting} waiting` : "Nothing waiting."} Outages skip the queue and are filed immediately.</span>
      </div>
      <ErrorNote error={list.error} />
      <div className="card flush">
        {!list.data ? (
          <Loading />
        ) : list.data.length === 0 ? (
          <Empty title="No analyses yet">When an error group is ready to become an issue, its analysis shows up here.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>State</th>
                <th>Group</th>
                <th>Result</th>
                <th>Model</th>
                <th>Time</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.data.map((a) => {
                if (a.status === "queued") pos++;
                let title = "";
                if (a.draft) {
                  try {
                    title = JSON.parse(a.draft).title;
                  } catch {
                    /* ignore */
                  }
                }
                return (
                  <tr key={a.id}>
                    <td className="nowrap">
                      <StatusBadge s={a.status} /> {a.status === "queued" && <span className="faint small">#{pos}</span>}
                    </td>
                    <td>
                      <Link to={`/issues/${a.group_id}`} style={{ fontWeight: 600 }}>
                        {a.service}
                      </Link>
                      <div className="small faint" style={{ maxWidth: 360, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }} title={a.group_title}>
                        {a.group_title}
                      </div>
                    </td>
                    <td style={{ maxWidth: 380 }}>
                      {a.error ? (
                        <span className="small" style={{ color: "var(--bad)" }}>
                          {a.error}
                        </span>
                      ) : title ? (
                        <span className="small">{title}</span>
                      ) : (
                        <span className="faint small">—</span>
                      )}
                      {a.issue_url && (
                        <div className="small">
                          <a href={a.issue_url} target="_blank" rel="noreferrer">
                            issue ↗
                          </a>
                        </div>
                      )}
                    </td>
                    <td className="mono small">{a.provider || "—"}</td>
                    <td className="small muted nowrap">
                      <Timing a={a} now={now} />
                    </td>
                    <td style={{ textAlign: "right" }}>
                      {a.status === "failed" && a.group_status === "pending" && (
                        <button className="small" onClick={() => retry(a)}>
                          Retry
                        </button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}
