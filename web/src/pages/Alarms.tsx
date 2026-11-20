import { useState } from "react";
import { Link } from "react-router-dom";
import { Alarm, AlarmEvent, AlarmKind, api, MetricSeriesInfo, Service, Webhook } from "../api";
import { Empty, ErrorNote, Loading, Modal, Switch, useAsync, useToast } from "../components/ui";
import { ago, fmtDuration, fmtFull, fmtNum } from "../util";

const KIND_LABEL: Record<AlarmKind, string> = {
  log_count: "Log count",
  metric: "Metric threshold",
  service_down: "Service down",
};

function describe(a: Alarm): string {
  const scope = a.service || "any service";
  if (a.kind === "service_down") return `When ${scope} stops sending telemetry`;
  if (a.kind === "metric") return `${a.agg}(${a.metric}) ${a.op} ${a.threshold} over ${fmtDuration(a.window_s)} · ${scope}`;
  const what = a.query ? `logs matching "${a.query}"` : "log lines";
  const sev = a.min_severity >= 17 ? " (error+)" : a.min_severity >= 13 ? " (warn+)" : "";
  return `Count of ${what}${sev} ${a.op} ${a.threshold} in ${fmtDuration(a.window_s)} · ${scope}`;
}

export default function Alarms() {
  const toast = useToast();
  const alarms = useAsync(() => api.get<Alarm[]>("/api/alarms"), [], 15000);
  const events = useAsync(() => api.get<AlarmEvent[]>("/api/alarms/events"), [], 15000);
  const hooks = useAsync(() => api.get<{ webhooks: Webhook[] }>("/api/webhooks"), []);
  const [editing, setEditing] = useState<Partial<Alarm> | null>(null);
  const names = new Map((alarms.data ?? []).map((a) => [a.id, a.name]));

  const toggle = async (a: Alarm, enabled: boolean) => {
    try {
      await api.put(`/api/alarms/${a.id}`, { ...a, enabled });
      alarms.reload();
    } catch (e) {
      toast((e as Error).message, true);
    }
  };
  const remove = async (a: Alarm) => {
    if (!confirm(`Delete alarm "${a.name}"?`)) return;
    await api.del(`/api/alarms/${a.id}`);
    alarms.reload();
  };

  return (
    <>
      <div className="page-head">
        <span className="sub">Evaluated every 15s; webhooks are notified when an alarm starts firing and when it resolves.</span>
        <div className="actions">
          <button className="primary" onClick={() => setEditing({})}>
            + New alarm
          </button>
        </div>
      </div>
      <ErrorNote error={alarms.error} />
      <div className="card flush">
        {!alarms.data ? (
          <Loading />
        ) : alarms.data.length === 0 ? (
          <Empty title="No alarms">Create one for error spikes, slow endpoints or services going quiet.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th style={{ width: 50 }}>On</th>
                <th>Alarm</th>
                <th>State</th>
                <th className="num">Current</th>
                <th>Notifies</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {alarms.data.map((a) => (
                <tr key={a.id}>
                  <td>
                    <Switch checked={a.enabled} onChange={(v) => toggle(a, v)} label="Enabled" />
                  </td>
                  <td>
                    <div style={{ fontWeight: 600 }}>{a.name}</div>
                    <div className="small muted">{describe(a)}</div>
                  </td>
                  <td className="nowrap">
                    {!a.enabled ? (
                      <span className="badge">disabled</span>
                    ) : a.state === "firing" ? (
                      <span className="badge bad">● firing</span>
                    ) : (
                      <span className="badge ok">ok</span>
                    )}
                    <div className="small faint" title={fmtFull(a.last_changed)}>
                      {a.last_changed ? `since ${ago(a.last_changed)}` : ""}
                    </div>
                  </td>
                  <td className="num">{a.last_eval ? fmtNum(a.last_value) : "—"}</td>
                  <td className="small">
                    {a.webhook_ids.length === 0 ? (
                      <span className="faint">nobody</span>
                    ) : (
                      a.webhook_ids.map((id) => (hooks.data?.webhooks.find((h) => h.id === id)?.name ?? `#${id}`)).join(", ")
                    )}
                  </td>
                  <td className="nowrap" style={{ textAlign: "right" }}>
                    <button className="small" onClick={() => setEditing(a)}>
                      Edit
                    </button>{" "}
                    <button className="small danger" onClick={() => remove(a)}>
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <div className="card flush">
        <div className="card-head" style={{ padding: "14px 16px 0" }}>
          <h2>History</h2>
        </div>
        {(events.data ?? []).length === 0 ? (
          <Empty title="No state changes yet" />
        ) : (
          <table>
            <tbody>
              {events.data!.map((e) => (
                <tr key={e.id}>
                  <td className="nowrap muted small" title={fmtFull(e.time)}>
                    {ago(e.time)}
                  </td>
                  <td className="nowrap">{e.state === "firing" ? <span className="badge bad">firing</span> : <span className="badge ok">resolved</span>}</td>
                  <td className="nowrap">{names.get(e.alarm_id) ?? `#${e.alarm_id}`}</td>
                  <td className="small muted">{e.message}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {editing && (
        <AlarmForm
          initial={editing}
          webhooks={hooks.data?.webhooks ?? []}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            alarms.reload();
            toast("Alarm saved");
          }}
        />
      )}
    </>
  );
}

const WINDOWS = [60, 300, 600, 900, 1800, 3600, 21600, 86400];

function AlarmForm(props: { initial: Partial<Alarm>; webhooks: Webhook[]; onClose: () => void; onSaved: () => void }) {
  const [a, setA] = useState<Partial<Alarm>>({
    name: "",
    enabled: true,
    kind: "log_count",
    service: "",
    query: "",
    min_severity: 17,
    metric: "",
    agg: "avg",
    op: ">",
    threshold: 10,
    window_s: 300,
    webhook_ids: [],
    ...props.initial,
  });
  const [err, setErr] = useState<string>();
  const [preview, setPreview] = useState<{ value: number; firing: boolean; no_data: boolean; message: string }>();
  const svcs = useAsync(() => api.get<Service[]>("/api/services"), []);
  const metrics = useAsync(() => api.get<MetricSeriesInfo[]>("/api/metrics" + (a.service ? `?service=${encodeURIComponent(a.service)}` : "")), [a.service]);
  const set = (patch: Partial<Alarm>) => {
    setA((x) => ({ ...x, ...patch }));
    setPreview(undefined);
  };
  const metricNames = [...new Set((metrics.data ?? []).map((m) => m.name))];

  const save = async () => {
    setErr(undefined);
    try {
      if (a.id) await api.put(`/api/alarms/${a.id}`, a);
      else await api.post("/api/alarms", a);
      props.onSaved();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  const runPreview = async () => {
    setErr(undefined);
    try {
      setPreview(await api.post("/api/alarms/preview", a));
    } catch (e) {
      setErr((e as Error).message);
    }
  };

  return (
    <Modal
      title={a.id ? `Edit ${props.initial.name}` : "New alarm"}
      onClose={props.onClose}
      wide
      footer={
        <>
          {preview && (
            <span className={"small " + (preview.firing ? "" : "muted")} style={{ marginRight: "auto", color: preview.firing ? "var(--bad)" : undefined }}>
              {preview.no_data ? "No data in window" : `Now: ${fmtNum(preview.value)} → ${preview.firing ? "would fire" : "ok"}`}
            </span>
          )}
          <button onClick={runPreview}>Preview</button>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" onClick={save} disabled={!a.name}>
            Save
          </button>
        </>
      }
    >
      <ErrorNote error={err} />
      <div className="form-grid">
        <label className="field full">
          Name
          <input value={a.name} onChange={(e) => set({ name: e.target.value })} placeholder="Checkout error spike" autoFocus />
        </label>
        <label className="field">
          Type
          <select value={a.kind} onChange={(e) => set({ kind: e.target.value as AlarmKind })}>
            {Object.entries(KIND_LABEL).map(([k, l]) => (
              <option key={k} value={k}>
                {l}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          Service
          <select value={a.service} onChange={(e) => set({ service: e.target.value })}>
            <option value="">Any service</option>
            {(svcs.data ?? []).map((s) => (
              <option key={s.name}>{s.name}</option>
            ))}
          </select>
        </label>

        {a.kind === "log_count" && (
          <>
            <label className="field">
              Matching text
              <input value={a.query} onChange={(e) => set({ query: e.target.value })} placeholder='optional, e.g. "payment failed"' className="mono" />
              <span className="help">Same syntax as the Logs search.</span>
            </label>
            <label className="field">
              Minimum level
              <select value={a.min_severity} onChange={(e) => set({ min_severity: +e.target.value })}>
                <option value={0}>Any</option>
                <option value={13}>Warn+</option>
                <option value={17}>Error+</option>
                <option value={21}>Fatal</option>
              </select>
            </label>
          </>
        )}
        {a.kind === "metric" && (
          <>
            <label className="field">
              Metric
              <input list="metric-names" value={a.metric} onChange={(e) => set({ metric: e.target.value })} className="mono" placeholder="http.server.request.duration" />
              <datalist id="metric-names">
                {metricNames.map((m) => (
                  <option key={m} value={m} />
                ))}
              </datalist>
            </label>
            <label className="field">
              Aggregation
              <select value={a.agg} onChange={(e) => set({ agg: e.target.value })}>
                {["avg", "sum", "max", "min", "count", "last"].map((x) => (
                  <option key={x}>{x}</option>
                ))}
              </select>
            </label>
          </>
        )}
        {a.kind !== "service_down" && (
          <>
            <label className="field">
              Condition
              <div className="row">
                <select value={a.op} onChange={(e) => set({ op: e.target.value })} style={{ width: 70 }}>
                  {[">", ">=", "<", "<=", "=="].map((o) => (
                    <option key={o}>{o}</option>
                  ))}
                </select>
                <input type="number" value={a.threshold} onChange={(e) => set({ threshold: +e.target.value })} style={{ flex: 1 }} />
              </div>
            </label>
            <label className="field">
              Window
              <select value={a.window_s} onChange={(e) => set({ window_s: +e.target.value })}>
                {WINDOWS.map((w) => (
                  <option key={w} value={w}>
                    last {fmtDuration(w)}
                  </option>
                ))}
              </select>
            </label>
          </>
        )}
        <div className="field full">
          Notify webhooks
          {props.webhooks.length === 0 ? (
            <span className="help">
              No webhooks yet — <Link to="/settings/webhooks">create one</Link>.
            </span>
          ) : (
            <div className="row">
              {props.webhooks.map((h) => (
                <label key={h.id} className="check">
                  <input
                    type="checkbox"
                    checked={a.webhook_ids!.includes(h.id)}
                    onChange={(e) => set({ webhook_ids: e.target.checked ? [...a.webhook_ids!, h.id] : a.webhook_ids!.filter((x) => x !== h.id) })}
                  />
                  {h.name}
                </label>
              ))}
            </div>
          )}
        </div>
      </div>
    </Modal>
  );
}
