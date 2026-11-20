import { useMemo, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { api, LogRecord, OpStats, qs, Series, Span, TraceSummary } from "../api";
import { SeriesChart } from "../components/charts";
import { CopyLink, Empty, ErrorNote, Loading, RangePicker, Seg, useAsync } from "../components/ui";
import { useServices } from "../shared";
import { colorFor, fmtFull, fmtNum, fmtTime, LEVEL_COLORS, levelOf } from "../util";

/** Formats microseconds. */
export function fmtUS(us: number): string {
  if (us < 1000) return `${Math.round(us)} µs`;
  if (us < 1_000_000) return `${+(us / 1000).toPrecision(3)} ms`;
  return `${+(us / 1_000_000).toPrecision(3)} s`;
}

const fmtMS = (ms: number) => fmtUS(ms * 1000);

type Tab = "traces" | "performance";

export default function Traces() {
  const [params, setParams] = useSearchParams();
  const tab = (params.get("tab") as Tab) ?? "traces";
  const range = params.get("range") ?? "1h";
  const service = params.get("service") ?? "";
  const set = (k: string, v: string) => {
    const p = new URLSearchParams(params);
    if (v) p.set(k, v);
    else p.delete(k);
    setParams(p, { replace: true });
  };
  const svcs = useServices();
  return (
    <>
      <div className="page-head">
        <h1>Traces</h1>
        <span className="sub">Requests across your services, and how fast and reliable each endpoint is</span>
        <div className="actions">
          <CopyLink />
        </div>
      </div>
      <div className="card">
        <div className="row">
          <Seg<Tab>
            value={tab}
            onChange={(v) => set("tab", v)}
            options={[
              { value: "traces", label: "Traces" },
              { value: "performance", label: "Performance" },
            ]}
          />
          <select value={service} onChange={(e) => set("service", e.target.value)} aria-label="Service">
            <option value="">All services</option>
            {(svcs.data ?? []).map((s) => (
              <option key={s.name}>{s.name}</option>
            ))}
          </select>
          <RangePicker value={range} onChange={(v) => set("range", v)} />
        </div>
      </div>
      {tab === "performance" ? (
        <Performance range={range} service={service} onOpen={(o) => setParams(new URLSearchParams({ tab: "traces", range, service: o.service, operation: o.name }))} />
      ) : (
        <TraceSearch range={range} service={service} params={params} set={set} />
      )}
    </>
  );
}

function TraceSearch({ range, service, params, set }: { range: string; service: string; params: URLSearchParams; set: (k: string, v: string) => void }) {
  const nav = useNavigate();
  const operation = params.get("operation") ?? "";
  const minMS = params.get("min_ms") ?? "";
  const errors = params.get("errors") === "true";
  const [op, setOp] = useState(operation);
  const [min, setMin] = useState(minMS);
  const ops = useAsync(() => api.get<{ operations: OpStats[] }>(`/api/operations${qs({ range, service })}`), [range, service]);
  const list = useAsync(
    () => api.get<{ traces: TraceSummary[] }>(`/api/traces${qs({ range, service, operation, min_ms: minMS, errors, limit: 100 })}`),
    [range, service, operation, minMS, errors],
    30000,
  );
  const max = Math.max(1, ...(list.data?.traces ?? []).map((t) => t.duration_us));
  return (
    <>
      <div className="card">
        <form
          className="row"
          onSubmit={(e) => {
            e.preventDefault();
            set("operation", op);
            set("min_ms", min);
          }}
        >
          <input list="ops" value={op} onChange={(e) => setOp(e.target.value)} placeholder="Operation, e.g. GET /api/cart" style={{ minWidth: 260 }} aria-label="Operation" />
          <datalist id="ops">
            {[...new Set((ops.data?.operations ?? []).map((o) => o.name))].map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
          <input value={min} onChange={(e) => setMin(e.target.value)} placeholder="min ms" style={{ width: 90 }} aria-label="Minimum duration in ms" inputMode="decimal" />
          <label className="small">
            <input type="checkbox" checked={errors} onChange={(e) => set("errors", e.target.checked ? "true" : "")} /> with errors
          </label>
          <button type="submit">Search</button>
        </form>
      </div>
      <ErrorNote error={list.error} />
      <div className="card flush">
        {!list.data ? (
          <Loading />
        ) : list.data.traces.length === 0 ? (
          <Empty title="No traces in this range">
            Send traces to <code>/v1/traces</code>. Every span is stored (sampled with <code>TRACE_SAMPLE_RATE</code>; spans with errors are always kept).
          </Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Trace</th>
                <th>Services</th>
                <th>Started</th>
                <th style={{ width: "28%" }}>Duration</th>
                <th className="num">Spans</th>
                <th className="num">Errors</th>
              </tr>
            </thead>
            <tbody>
              {list.data.traces.map((t) => (
                <tr key={t.trace_id} className="clickable" onClick={() => nav(`/traces/${t.trace_id}`)}>
                  <td>
                    <div className="truncate" style={{ maxWidth: 360 }} title={t.root_name}>
                      <strong>{t.root_service}</strong> {t.root_name}
                    </div>
                    <div className="small faint mono">{t.trace_id.slice(0, 16)}…</div>
                  </td>
                  <td className="small">
                    {t.services.map((s) => (
                      <span key={s} className="svc-dot" style={{ background: colorFor(s, t.services) }} title={s} />
                    ))}{" "}
                    {t.services.length}
                  </td>
                  <td className="nowrap muted" title={fmtFull(t.start)}>
                    {fmtTime(t.start)}
                  </td>
                  <td>
                    <div className="dur">
                      <span className="dur-bar" style={{ width: `${Math.max(1, (100 * t.duration_us) / max)}%`, background: t.errors ? "var(--bad)" : "var(--accent)" }} />
                      <span className="dur-v">{fmtUS(t.duration_us)}</span>
                    </div>
                  </td>
                  <td className="num">{t.spans}</td>
                  <td className="num">{t.errors ? <span className="badge bad">{t.errors}</span> : "0"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}

function usePanel(range: string, query: Record<string, unknown>) {
  const body = JSON.stringify({ queries: [{ id: "A", source: "spans", ...query }] });
  return useAsync(() => api.post<{ series: Series[]; bucket_ms: number }>(`/api/panel/query${qs({ range })}`, JSON.parse(body)), [range, body], 60000);
}

function Performance({ range, service, onOpen }: { range: string; service: string; onOpen: (o: OpStats) => void }) {
  const ops = useAsync(() => api.get<{ operations: OpStats[] }>(`/api/operations${qs({ range, service })}`), [range, service], 60000);
  const by = service ? ["operation"] : ["service"];
  const rate = usePanel(range, { service, stat: "rate", group_by: by, limit: 8 });
  const p95 = usePanel(range, { service, stat: "p95", group_by: by, limit: 8 });
  const errs = usePanel(range, { service, stat: "error_rate", group_by: by, limit: 8 });
  const [sort, setSort] = useState<keyof OpStats>("count");
  const rows = useMemo(() => [...(ops.data?.operations ?? [])].sort((a, b) => (b[sort] as number) - (a[sort] as number)), [ops.data, sort]);
  const th = (k: keyof OpStats, label: string) => (
    <th className="num clickable" onClick={() => setSort(k)} title="Sort">
      {label}
      {sort === k ? " ▾" : ""}
    </th>
  );
  const chart = (title: string, d: typeof rate, unit: string) => (
    <div className="card" style={{ margin: 0 }}>
      <div className="card-head">
        <h2>{title}</h2>
      </div>
      {d.data ? <SeriesChart series={d.data.series} bucketMs={d.data.bucket_ms} unit={unit} height={170} /> : <Loading />}
    </div>
  );
  return (
    <>
      <div className="grid" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(300px, 1fr))", gap: 16, marginBottom: 16 }}>
        {chart("Requests per second", rate, "/s")}
        {chart("p95 latency (ms)", p95, "ms")}
        {chart("Error rate (%)", errs, "%")}
      </div>
      <ErrorNote error={ops.error} />
      <div className="card flush">
        {!ops.data ? (
          <Loading />
        ) : rows.length === 0 ? (
          <Empty title="No requests in this range">Request statistics come from server and consumer spans (and root spans) of your traces.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Operation</th>
                <th>Service</th>
                {th("count", "Requests")}
                {th("rate_per_s", "Req/s")}
                {th("error_rate", "Errors")}
                {th("p50_ms", "p50")}
                {th("p95_ms", "p95")}
                {th("p99_ms", "p99")}
                {th("avg_ms", "Avg")}
              </tr>
            </thead>
            <tbody>
              {rows.map((o) => (
                <tr key={o.service + o.name} className="clickable" onClick={() => onOpen(o)} title="Show traces of this operation">
                  <td className="truncate" style={{ maxWidth: 360 }}>
                    {o.name}
                  </td>
                  <td>{o.service}</td>
                  <td className="num">{fmtNum(o.count)}</td>
                  <td className="num">{fmtNum(o.rate_per_s)}</td>
                  <td className="num">
                    {o.errors ? <span className={"badge " + (o.error_rate > 0.05 ? "bad" : "warn")}>{(100 * o.error_rate).toFixed(o.error_rate < 0.01 ? 2 : 1)}%</span> : "0%"}
                  </td>
                  <td className="num">{fmtMS(o.p50_ms)}</td>
                  <td className="num">{fmtMS(o.p95_ms)}</td>
                  <td className="num">{fmtMS(o.p99_ms)}</td>
                  <td className="num">{fmtMS(o.avg_ms)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}

// --- one trace -------------------------------------------------------------

interface TraceResp {
  trace_id: string;
  spans: Span[];
  summary?: TraceSummary;
  logs?: LogRecord[];
}

interface Row {
  span: Span;
  depth: number;
  start: number; // µs since trace start
}

/** Orders spans depth-first under their parents (orphans at the top level). */
function layout(spans: Span[]): { rows: Row[]; t0: number; total: number } {
  const ids = new Set(spans.map((s) => s.span_id));
  const kids = new Map<string, Span[]>();
  const roots: Span[] = [];
  for (const s of spans) {
    if (s.parent_id && ids.has(s.parent_id)) {
      const l = kids.get(s.parent_id) ?? [];
      l.push(s);
      kids.set(s.parent_id, l);
    } else roots.push(s);
  }
  const us = (s: Span) => new Date(s.start).getTime() * 1000;
  const t0 = Math.min(...spans.map(us));
  const end = Math.max(...spans.map((s) => us(s) + s.duration_us));
  const rows: Row[] = [];
  const seen = new Set<string>();
  const walk = (s: Span, depth: number) => {
    if (seen.has(s.span_id)) return;
    seen.add(s.span_id);
    rows.push({ span: s, depth, start: us(s) - t0 });
    for (const k of (kids.get(s.span_id) ?? []).sort((a, b) => us(a) - us(b))) walk(k, depth + 1);
  };
  roots.sort((a, b) => us(a) - us(b)).forEach((r) => walk(r, 0));
  return { rows, t0, total: Math.max(1, end - t0) };
}

export function TraceView() {
  const { id } = useParams();
  const d = useAsync(() => api.get<TraceResp>(`/api/traces/${id}`), [id]);
  const [sel, setSel] = useState<string | null>(null);
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const lay = useMemo(() => (d.data?.spans.length ? layout(d.data.spans) : null), [d.data]);
  if (d.error) return <ErrorNote error={d.error} />;
  if (!d.data) return <Loading />;
  const { spans, summary, logs = [] } = d.data;
  const services = [...new Set(spans.map((s) => s.service))];
  const selected = spans.find((s) => s.span_id === sel);
  const spanLogs = selected ? logs.filter((l) => l.span_id === selected.span_id) : [];

  // Hide descendants of collapsed spans.
  const visible: Row[] = [];
  if (lay) {
    let hideBelow = -1;
    for (const r of lay.rows) {
      if (hideBelow >= 0 && r.depth > hideBelow) continue;
      hideBelow = collapsed.has(r.span.span_id) ? r.depth : -1;
      visible.push(r);
    }
  }
  const hasKids = (sid: string) => spans.some((s) => s.parent_id === sid);
  const ticks = [0, 0.25, 0.5, 0.75, 1];

  return (
    <>
      <div className="page-head">
        <div style={{ minWidth: 0 }}>
          <div className="small muted">
            <Link to="/traces">Traces</Link> / <span className="mono">{id}</span>
          </div>
          <h1 style={{ marginTop: 4 }}>{summary ? `${summary.root_service}: ${summary.root_name}` : "Trace"}</h1>
        </div>
        <div className="actions">
          <Link className="btn" to={`/logs?trace_id=${id}&range=7d`}>
            Logs of this trace
          </Link>
          <CopyLink />
        </div>
      </div>
      {!lay ? (
        <div className="card">
          <Empty title="No spans stored for this trace">
            It may be older than <code>TRACE_RETENTION</code>, not sampled (<code>TRACE_SAMPLE_RATE</code>), or only its error spans were sent. Its{" "}
            <Link to={`/logs?trace_id=${id}&range=7d`}>log lines</Link> may still be there.
          </Empty>
        </div>
      ) : (
        <>
          <div className="grid stats" style={{ marginBottom: 16 }}>
            <div className="card stat">
              <div className="label">Duration</div>
              <div className="value">{fmtUS(lay.total)}</div>
              <div className="hint">{summary && fmtFull(summary.start)}</div>
            </div>
            <div className="card stat">
              <div className="label">Spans</div>
              <div className="value">{spans.length}</div>
              <div className="hint">{summary?.errors ? `${summary.errors} with errors` : "no errors"}</div>
            </div>
            <div className="card stat">
              <div className="label">Services</div>
              <div className="value">{services.length}</div>
              <div className="hint">
                {services.map((s) => (
                  <span key={s} style={{ marginRight: 8 }}>
                    <span className="svc-dot" style={{ background: colorFor(s, services) }} /> {s}
                  </span>
                ))}
              </div>
            </div>
          </div>
          <div className="trace-layout">
            <div className="card flush waterfall">
              <div className="wf-head">
                <div className="wf-name">Span</div>
                <div className="wf-time">
                  {ticks.map((t) => (
                    <span key={t} style={{ left: `${t * 100}%` }}>
                      {fmtUS(t * lay.total)}
                    </span>
                  ))}
                </div>
              </div>
              {visible.map((r) => {
                const s = r.span;
                const left = (100 * r.start) / lay.total;
                const width = Math.max(0.3, (100 * s.duration_us) / lay.total);
                const err = s.status === 2;
                return (
                  <div key={s.span_id} className={"wf-row" + (sel === s.span_id ? " on" : "")} onClick={() => setSel(s.span_id === sel ? null : s.span_id)}>
                    <div className="wf-name" style={{ paddingLeft: 8 + r.depth * 14 }} title={s.name}>
                      {hasKids(s.span_id) ? (
                        <button
                          className="wf-toggle"
                          onClick={(e) => {
                            e.stopPropagation();
                            const n = new Set(collapsed);
                            if (n.has(s.span_id)) n.delete(s.span_id);
                            else n.add(s.span_id);
                            setCollapsed(n);
                          }}
                          aria-label={collapsed.has(s.span_id) ? "Expand" : "Collapse"}
                        >
                          {collapsed.has(s.span_id) ? "▸" : "▾"}
                        </button>
                      ) : (
                        <span className="wf-toggle" />
                      )}
                      <span className="svc-dot" style={{ background: colorFor(s.service, services) }} />
                      <span className="faint small">{s.service}</span> {s.name}
                      {err && <span className="badge bad" style={{ marginLeft: 6 }}>error</span>}
                    </div>
                    <div className="wf-track">
                      <span
                        className="wf-bar"
                        style={{ left: `${left}%`, width: `${width}%`, background: err ? "var(--bad)" : colorFor(s.service, services) }}
                        title={`${fmtUS(s.duration_us)} at +${fmtUS(r.start)}`}
                      />
                      <span className="wf-dur" style={{ left: `min(calc(${left + width}% + 4px), calc(100% - 60px))` }}>
                        {fmtUS(s.duration_us)}
                      </span>
                    </div>
                  </div>
                );
              })}
            </div>
            {selected && (
              <div className="card span-detail">
                <div className="card-head">
                  <h2 className="truncate" title={selected.name}>
                    {selected.name}
                  </h2>
                  <button className="small ghost" onClick={() => setSel(null)} aria-label="Close">
                    ✕
                  </button>
                </div>
                <div className="kv" style={{ gridTemplateColumns: "120px 1fr" }}>
                  <span className="k">service</span>
                  <span className="v">
                    {selected.service}
                    {selected.version ? ` ${selected.version}` : ""}
                  </span>
                  <span className="k">kind</span>
                  <span className="v">{selected.kind}</span>
                  <span className="k">duration</span>
                  <span className="v">{fmtUS(selected.duration_us)}</span>
                  <span className="k">start</span>
                  <span className="v">{fmtFull(selected.start)}</span>
                  <span className="k">status</span>
                  <span className="v">{["unset", "ok", "error"][selected.status] ?? selected.status}{selected.status_message ? `: ${selected.status_message}` : ""}</span>
                  <span className="k">span id</span>
                  <span className="v">{selected.span_id}</span>
                </div>
                {selected.attrs && Object.keys(selected.attrs).length > 0 && (
                  <>
                    <h3 className="small muted" style={{ margin: "14px 0 6px" }}>
                      Attributes
                    </h3>
                    <div className="kv" style={{ gridTemplateColumns: "minmax(110px, 180px) 1fr" }}>
                      {Object.entries(selected.attrs)
                        .sort(([a], [b]) => a.localeCompare(b))
                        .map(([k, v]) => (
                          <span key={k} style={{ display: "contents" }}>
                            <span className="k">{k}</span>
                            <span className="v">{v}</span>
                          </span>
                        ))}
                    </div>
                  </>
                )}
                {(selected.events ?? []).length > 0 && (
                  <>
                    <h3 className="small muted" style={{ margin: "14px 0 6px" }}>
                      Events
                    </h3>
                    {selected.events!.map((e, i) => (
                      <div key={i} className="small" style={{ marginBottom: 8 }}>
                        <strong>{e.name}</strong> <span className="faint">{fmtTime(e.time)}</span>
                        {e.attrs?.["exception.message"] && <div>{e.attrs["exception.message"]}</div>}
                        {e.attrs?.["exception.stacktrace"] && <pre className="code" style={{ maxHeight: 200 }}>{e.attrs["exception.stacktrace"]}</pre>}
                      </div>
                    ))}
                  </>
                )}
                {spanLogs.length > 0 && (
                  <>
                    <h3 className="small muted" style={{ margin: "14px 0 6px" }}>
                      Logs
                    </h3>
                    <div className="crumbs">
                      {spanLogs.map((l) => (
                        <div key={l.id} className="crumb">
                          <span className="crumb-l" style={{ color: LEVEL_COLORS[levelOf(l.severity)] }}>
                            {levelOf(l.severity)}
                          </span>
                          <span className="crumb-b">{l.body}</span>
                        </div>
                      ))}
                    </div>
                  </>
                )}
              </div>
            )}
          </div>
        </>
      )}
    </>
  );
}
