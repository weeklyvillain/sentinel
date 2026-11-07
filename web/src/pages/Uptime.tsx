import { Fragment, useState } from "react";
import { api, Bucket, Check, CheckResult, CheckStats, Service } from "../api";
import { SeriesChart, StackedBars } from "../components/charts";
import { Empty, ErrorNote, Loading, Modal, RangePicker, Switch, useAsync, useToast } from "../components/ui";
import { ago, fmtDuration, fmtFull, fmtNum, serviceUp } from "../util";
import { useMe } from "../App";

function StateBadge({ c }: { c: Check }) {
  if (!c.enabled) return <span className="badge">paused</span>;
  if (c.state === "down")
    return (
      <span className="badge bad" title={`since ${fmtFull(c.down_since)}`}>
        <span className="dot bad" /> down {ago(c.down_since).replace(" ago", "")}
      </span>
    );
  if (c.state === "up")
    return (
      <span className="badge ok">
        <span className="dot ok" /> up
      </span>
    );
  return <span className="badge">waiting</span>;
}

function pct(n?: number) {
  if (n === undefined) return "—";
  return (n >= 99.995 ? "100" : n.toFixed(2)) + "%";
}

export default function Uptime() {
  const toast = useToast();
  const checks = useAsync(() => api.get<Check[]>("/api/checks"), [], 10000);
  const [editing, setEditing] = useState<Partial<Check> | null>(null);
  const [open, setOpen] = useState<number | null>(null);

  const run = async (c: Check) => {
    try {
      const r = await api.post<CheckResult>(`/api/checks/${c.id}/run`);
      toast(r.ok ? `${c.name}: HTTP ${r.status} in ${r.latency_ms}ms` : `${c.name}: ${r.error}`, !r.ok);
      checks.reload();
    } catch (e) {
      toast((e as Error).message, true);
    }
  };
  const toggle = async (c: Check, enabled: boolean) => {
    await api.put(`/api/checks/${c.id}`, { ...c, enabled });
    checks.reload();
  };
  const remove = async (c: Check) => {
    if (!confirm(`Delete uptime check "${c.name}" and its history?`)) return;
    await api.del(`/api/checks/${c.id}`);
    checks.reload();
  };
  const down = (checks.data ?? []).filter((c) => c.enabled && c.state === "down").length;

  return (
    <>
      <div className="page-head">
        <h1>Uptime</h1>
        <span className="sub">HTTP checks run from the Sentinel server, so they keep working when an app server is down.</span>
        <div className="actions">
          <button className="primary" onClick={() => setEditing({})}>
            + New check
          </button>
        </div>
      </div>
      {down > 0 && <div className="notice bad" style={{ marginBottom: 16 }}>{down} check{down > 1 ? "s are" : " is"} failing.</div>}
      <ErrorNote error={checks.error} />
      <div className="card flush">
        {!checks.data ? (
          <Loading />
        ) : checks.data.length === 0 ? (
          <Empty title="No uptime checks yet">Add the health endpoints of your apps, e.g. https://app.example.com/healthz.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th style={{ width: 50 }}>On</th>
                <th>Check</th>
                <th>State</th>
                <th className="num">Uptime 24h</th>
                <th className="num">Latency</th>
                <th>Last probe</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {checks.data.map((c) => (
                <Fragment key={c.id}>
                  <tr className="clickable" onClick={() => setOpen(open === c.id ? null : c.id)}>
                    <td onClick={(e) => e.stopPropagation()}>
                      <Switch checked={c.enabled} onChange={(v) => toggle(c, v)} label="Enabled" />
                    </td>
                    <td style={{ maxWidth: 380 }}>
                      <div style={{ fontWeight: 600 }}>
                        {c.name} {c.service && <span className="badge">{c.service}</span>}
                      </div>
                      <div className="small faint mono truncate">
                        {c.method} {c.url}
                      </div>
                    </td>
                    <td className="nowrap">
                      <StateBadge c={c} />
                    </td>
                    <td className="num">{pct(c.stats_24h?.uptime_pct)}</td>
                    <td className="num">{c.last_latency_ms ? `${c.last_latency_ms}ms` : "—"}</td>
                    <td className="small">
                      <div className="muted nowrap">{ago(c.last_checked)}</div>
                      {c.last_error && (
                        <div className="truncate" style={{ color: "var(--bad)", maxWidth: 260 }} title={c.last_error}>
                          {c.last_error}
                        </div>
                      )}
                    </td>
                    <td className="nowrap" style={{ textAlign: "right" }} onClick={(e) => e.stopPropagation()}>
                      <button className="small" onClick={() => run(c)}>
                        Run now
                      </button>{" "}
                      <button className="small" onClick={() => setEditing(c)}>
                        Edit
                      </button>{" "}
                      <button className="small danger" onClick={() => remove(c)}>
                        Delete
                      </button>
                    </td>
                  </tr>
                  {open === c.id && (
                    <tr>
                      <td colSpan={7} style={{ background: "var(--panel-2)" }}>
                        <CheckDetail c={c} />
                      </td>
                    </tr>
                  )}
                </Fragment>
              ))}
            </tbody>
          </table>
        )}
      </div>
      <p className="small faint">
        A check is marked down after its configured number of consecutive failures; that files (or reopens) a GitHub issue and sends{" "}
        <code>check.down</code> to subscribed webhooks. When it passes again the issue is closed with the outage length.
      </p>
      <WatchedServices />
      {editing && (
        <CheckForm
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            checks.reload();
            toast("Check saved");
          }}
        />
      )}
    </>
  );
}

function CheckDetail({ c }: { c: Check }) {
  const [range, setRange] = useState("24h");
  const d = useAsync(
    () => api.get<{ series: Bucket[]; bucket_ms: number; recent: CheckResult[]; stats: CheckStats }>(`/api/checks/${c.id}/results?range=${range}`),
    [c.id, range, c.last_checked],
  );
  if (!d.data) return <Loading />;
  const latency = [{ label: "latency ms", points: d.data.series.map((b) => ({ t: b.t, v: b.counts.latency_ms ?? null })) }];
  const failures = d.data.series.map((b) => ({ t: b.t, counts: { failures: b.counts.failures ?? 0 } }));
  const hasFailures = failures.some((f) => f.counts.failures > 0);
  return (
    <div className="stack" style={{ padding: "4px 0" }}>
      <div className="row">
        <span className="small muted">
          {fmtNum(d.data.stats.probes)} probes · {pct(d.data.stats.uptime_pct)} up · avg {Math.round(d.data.stats.avg_latency_ms)}ms · every{" "}
          {fmtDuration(c.interval_s)}, down after {c.failures_before_down} failure{c.failures_before_down > 1 ? "s" : ""}, expects {c.expect_status}
          {c.body_contains ? ` + "${c.body_contains}"` : ""}
        </span>
        <span style={{ marginLeft: "auto" }}>
          <RangePicker value={range} onChange={setRange} />
        </span>
      </div>
      <div className="grid two">
        <div className="card">
          <h3>Response time</h3>
          <SeriesChart series={latency} bucketMs={d.data.bucket_ms} unit="ms" area height={150} />
        </div>
        <div className="card">
          <h3>Failed probes</h3>
          {hasFailures ? (
            <StackedBars buckets={failures} keys={["failures"]} colors={{ failures: "#e05252" }} bucketMs={d.data.bucket_ms} height={150} />
          ) : (
            <Empty title="No failures in this range" />
          )}
        </div>
      </div>
      <div className="card flush">
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Result</th>
              <th className="num">Latency</th>
              <th>Error</th>
            </tr>
          </thead>
          <tbody>
            {[...d.data.recent].reverse().slice(0, 15).map((r) => (
              <tr key={r.t}>
                <td className="nowrap small" title={fmtFull(r.t)}>
                  {ago(r.t)}
                </td>
                <td>{r.ok ? <span className="badge ok">{r.status}</span> : <span className="badge bad">{r.status || "fail"}</span>}</td>
                <td className="num">{r.latency_ms}ms</td>
                <td className="small" style={{ color: "var(--bad)" }}>
                  {r.error}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

const INTERVALS = [10, 30, 60, 120, 300, 600, 1800, 3600];

function CheckForm(props: { initial: Partial<Check>; onClose: () => void; onSaved: () => void }) {
  const [c, setC] = useState<Partial<Check>>({
    name: "",
    url: "https://",
    method: "GET",
    interval_s: 60,
    timeout_s: 10,
    expect_status: "2xx",
    body_contains: "",
    service: "",
    failures_before_down: 2,
    enabled: true,
    ...props.initial,
  });
  const [err, setErr] = useState<string>();
  const svcs = useAsync(() => api.get<Service[]>("/api/services"), []);
  const set = (p: Partial<Check>) => setC((x) => ({ ...x, ...p }));
  const save = async () => {
    setErr(undefined);
    try {
      if (c.id) await api.put(`/api/checks/${c.id}`, c);
      else await api.post("/api/checks", c);
      props.onSaved();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <Modal
      title={c.id ? `Edit ${props.initial.name}` : "New uptime check"}
      onClose={props.onClose}
      wide
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" onClick={save} disabled={!c.name || !c.url}>
            Save
          </button>
        </>
      }
    >
      <ErrorNote error={err} />
      <div className="form-grid">
        <label className="field">
          Name
          <input value={c.name} onChange={(e) => set({ name: e.target.value })} placeholder="Checkout API" autoFocus />
        </label>
        <label className="field">
          Service
          <select value={c.service} onChange={(e) => set({ service: e.target.value })}>
            <option value="">— none (use the check name) —</option>
            {(svcs.data ?? []).map((s) => (
              <option key={s.name}>{s.name}</option>
            ))}
          </select>
          <span className="help">Outage issues go to this service's repository.</span>
        </label>
        <label className="field full">
          URL
          <div className="row">
            <select value={c.method} onChange={(e) => set({ method: e.target.value as Check["method"] })} style={{ width: 90 }}>
              <option>GET</option>
              <option>HEAD</option>
            </select>
            <input value={c.url} onChange={(e) => set({ url: e.target.value })} className="mono" style={{ flex: 1 }} placeholder="https://app.example.com/healthz" />
          </div>
        </label>
        <label className="field">
          Check every
          <select value={c.interval_s} onChange={(e) => set({ interval_s: +e.target.value })}>
            {INTERVALS.map((i) => (
              <option key={i} value={i}>
                {fmtDuration(i)}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          Timeout (seconds)
          <input type="number" min={1} max={60} value={c.timeout_s} onChange={(e) => set({ timeout_s: +e.target.value })} />
        </label>
        <label className="field">
          Expected status
          <input value={c.expect_status} onChange={(e) => set({ expect_status: e.target.value })} placeholder="2xx" className="mono" />
          <span className="help">e.g. 2xx, 200, or 200,204,3xx</span>
        </label>
        <label className="field">
          Body must contain
          <input value={c.body_contains} onChange={(e) => set({ body_contains: e.target.value })} placeholder="optional" className="mono" />
        </label>
        <label className="field">
          Mark down after
          <select value={c.failures_before_down} onChange={(e) => set({ failures_before_down: +e.target.value })}>
            {[1, 2, 3, 5].map((n) => (
              <option key={n} value={n}>
                {n} consecutive failure{n > 1 ? "s" : ""}
              </option>
            ))}
          </select>
        </label>
        <label className="check" style={{ alignSelf: "end" }}>
          <input type="checkbox" checked={c.enabled} onChange={(e) => set({ enabled: e.target.checked })} /> Enabled
        </label>
      </div>
    </Modal>
  );
}

// Services watched for silence: one that sends nothing for DOWN_AFTER is
// flagged down. Opt-in, because plenty of senders are quiet for minutes.
function WatchedServices() {
  const toast = useToast();
  const me = useMe();
  const svcs = useAsync(() => api.get<Service[]>("/api/services"), [], 15000);
  const [adding, setAdding] = useState("");

  const setWatch = async (name: string, watch: boolean) => {
    try {
      await api.patch(`/api/services/${encodeURIComponent(name)}`, { watch });
      toast(watch ? `Watching ${name}` : `Stopped watching ${name}`);
      setAdding("");
      svcs.reload();
    } catch (e) {
      toast((e as Error).message, true);
    }
  };
  const all = svcs.data ?? [];
  const watched = all.filter((s) => s.watch);
  const others = all.filter((s) => !s.watch);
  const after = me.info.down_after;

  return (
    <>
      <div className="page-head" style={{ marginTop: 24 }}>
        <h2 style={{ margin: 0 }}>Watched services</h2>
        <span className="sub">
          A watched service that sends no logs, traces or metrics for {after} is flagged down: Sentinel files an issue and sends{" "}
          <code>service.down</code>. Other services are never flagged for being quiet.
        </span>
        <div className="actions">
          <select value={adding} onChange={(e) => setAdding(e.target.value)} disabled={others.length === 0} aria-label="Service to watch">
            <option value="">{others.length ? "Choose a service…" : "All services are watched"}</option>
            {others.map((s) => (
              <option key={s.name} value={s.name}>
                {s.name}
              </option>
            ))}
          </select>
          <button className="primary" disabled={!adding} onClick={() => setWatch(adding, true)}>
            + Watch
          </button>
        </div>
      </div>
      <ErrorNote error={svcs.error} />
      <div className="card flush">
        {!svcs.data ? (
          <Loading />
        ) : watched.length === 0 ? (
          <Empty title="No watched services">Pick a service above to be alerted when it stops sending telemetry.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Service</th>
                <th>State</th>
                <th>Last seen</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {watched.map((s) => {
                const st = serviceUp(s);
                return (
                  <tr key={s.name}>
                    <td style={{ fontWeight: 600 }}>{s.name}</td>
                    <td>
                      {st === "up" && (
                        <span className="badge ok">
                          <span className="dot ok" /> up
                        </span>
                      )}
                      {st === "down" && (
                        <span className="badge bad" title={`since ${fmtFull(s.down_since)}`}>
                          <span className="dot bad" /> down {ago(s.down_since).replace(" ago", "")}
                        </span>
                      )}
                      {st === "never" && <span className="badge">waiting for data</span>}
                    </td>
                    <td className="muted nowrap" title={fmtFull(s.last_seen)}>
                      {ago(s.last_seen)}
                    </td>
                    <td style={{ textAlign: "right" }}>
                      <button className="small" onClick={() => setWatch(s.name, false)}>
                        Stop watching
                      </button>
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
