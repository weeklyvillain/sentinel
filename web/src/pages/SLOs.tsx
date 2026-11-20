import { useState } from "react";
import { Link } from "react-router-dom";
import { api, Check, OpStats, SLO, SLOKind, SLOStatus, Webhook } from "../api";
import { Empty, ErrorNote, Loading, Modal, useAction, useAsync } from "../components/ui";
import { useServices } from "../shared";
import { ago, fmtNum } from "../util";

const KINDS: Record<SLOKind, string> = {
  availability: "Availability (requests that did not fail)",
  latency: "Latency (requests faster than a threshold)",
  uptime: "Uptime check (successful probes)",
};

function describe(o: SLO, checks: Check[]): string {
  const op = o.operation ? ` ${o.operation}` : "";
  switch (o.kind) {
    case "latency":
      return `${o.objective}% of ${o.service}${op} requests faster than ${o.threshold_ms} ms and not failed, over ${o.window_days} days`;
    case "uptime":
      return `${o.objective}% of probes of ${checks.find((c) => c.id === o.check_id)?.name ?? `check #${o.check_id}`} succeed, over ${o.window_days} days`;
  }
  return `${o.objective}% of ${o.service}${op} requests succeed, over ${o.window_days} days`;
}

const STATE: Record<SLO["state"], { cls: string; label: string; help: string }> = {
  ok: { cls: "ok", label: "ok", help: "Within budget" },
  slow_burn: { cls: "warn", label: "burning", help: "Using its error budget 6× too fast over 6h (and 30m)" },
  fast_burn: { cls: "bad", label: "burning fast", help: "Using its error budget 14.4× too fast over 1h (and 5m)" },
};

function Budget({ s }: { s: SLOStatus }) {
  const left = Math.max(-1, Math.min(1, s.budget_remaining));
  const pct = Math.round(100 * s.budget_remaining);
  const tone = left > 0.5 ? "var(--ok)" : left > 0 ? "var(--warn)" : "var(--bad)";
  return (
    <div style={{ minWidth: 140 }}>
      <div className="budget-track">
        <span className="budget-fill" style={{ width: `${Math.max(0, left) * 100}%`, background: tone }} />
      </div>
      <div className="small" style={{ color: tone }}>
        {s.no_data ? <span className="faint">no data yet</span> : pct >= 0 ? `${pct}% of budget left` : `over budget by ${-pct}%`}
      </div>
    </div>
  );
}

export default function SLOs() {
  const run = useAction();
  const slos = useAsync(() => api.get<SLOStatus[]>("/api/slos"), [], 60000);
  const hooks = useAsync(() => api.get<{ webhooks: Webhook[] }>("/api/webhooks"), []);
  const checks = useAsync(() => api.get<Check[]>("/api/checks"), []);
  const [editing, setEditing] = useState<Partial<SLO> | null>(null);
  const remove = async (o: SLO) => {
    if (!confirm(`Delete SLO "${o.name}"?`)) return;
    await run(() => api.del(`/api/slos/${o.id}`), `Deleted ${o.name}`);
    slos.reload();
  };
  return (
    <>
      <div className="page-head">
        <span className="sub">
          Objectives over your request statistics (from traces) or uptime checks. Webhooks are told when an SLO burns its error budget too fast, with
          multi-window burn rates so short blips don't page.
        </span>
        <div className="actions">
          <button className="primary" onClick={() => setEditing({})}>
            + New SLO
          </button>
        </div>
      </div>
      <ErrorNote error={slos.error} />
      <div className="card flush">
        {!slos.data ? (
          <Loading />
        ) : slos.data.length === 0 ? (
          <Empty title="No SLOs yet">
            For example: 99.9% of checkout requests succeed, or 95% of them finish within 500 ms, over 30 days.
          </Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>SLO</th>
                <th>State</th>
                <th className="num">Now</th>
                <th>Error budget</th>
                <th className="num" title="Burn rate over 1h / 6h (1× uses the budget exactly over the window)">
                  Burn 1h / 6h
                </th>
                <th />
              </tr>
            </thead>
            <tbody>
              {slos.data.map((s) => (
                <tr key={s.id}>
                  <td>
                    <div style={{ fontWeight: 600 }}>{s.name}</div>
                    <div className="small muted">{describe(s, checks.data ?? [])}</div>
                  </td>
                  <td className="nowrap">
                    <span className={"badge " + STATE[s.state].cls} title={STATE[s.state].help}>
                      {STATE[s.state].label}
                    </span>
                    <div className="small faint">{s.last_changed ? `since ${ago(s.last_changed)}` : ""}</div>
                  </td>
                  <td className="num">
                    {s.no_data ? "—" : `${s.sli.toFixed(s.sli > 99 ? 3 : 2)}%`}
                    <div className="small faint">
                      {fmtNum(s.good)} / {fmtNum(s.total)}
                    </div>
                  </td>
                  <td>
                    <Budget s={s} />
                  </td>
                  <td className="num">
                    {fmtNum(s.burn["1h"] ?? 0)}× / {fmtNum(s.burn["6h"] ?? 0)}×
                  </td>
                  <td className="nowrap" style={{ textAlign: "right" }}>
                    <button className="small" onClick={() => setEditing(s)}>
                      Edit
                    </button>{" "}
                    <button className="small danger" onClick={() => remove(s)}>
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {editing && (
        <SLOForm
          initial={editing}
          webhooks={hooks.data?.webhooks ?? []}
          checks={checks.data ?? []}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            slos.reload();
          }}
        />
      )}
    </>
  );
}

function SLOForm(props: { initial: Partial<SLO>; webhooks: Webhook[]; checks: Check[]; onClose: () => void; onSaved: () => void }) {
  const [o, setO] = useState<Partial<SLO>>({ name: "", kind: "availability", service: "", operation: "", check_id: 0, threshold_ms: 500, objective: 99.9, window_days: 30, webhook_ids: [], ...props.initial });
  const [err, setErr] = useState<string>();
  const svcs = useServices();
  const ops = useAsync(() => (o.service ? api.get<{ operations: OpStats[] }>(`/api/operations?service=${encodeURIComponent(o.service)}&range=7d`) : Promise.resolve({ operations: [] })), [o.service]);
  const set = (p: Partial<SLO>) => setO((x) => ({ ...x, ...p }));
  const save = async () => {
    setErr(undefined);
    try {
      if (o.id) await api.put(`/api/slos/${o.id}`, o);
      else await api.post("/api/slos", o);
      props.onSaved();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <Modal
      title={o.id ? `Edit ${props.initial.name}` : "New SLO"}
      onClose={props.onClose}
      wide
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" onClick={save} disabled={!o.name}>
            Save
          </button>
        </>
      }
    >
      <ErrorNote error={err} />
      <div className="form-grid">
        <label className="field full">
          Name
          <input value={o.name} onChange={(e) => set({ name: e.target.value })} placeholder="Checkout availability" autoFocus />
        </label>
        <label className="field full">
          Measures
          <select value={o.kind} onChange={(e) => set({ kind: e.target.value as SLOKind })}>
            {Object.entries(KINDS).map(([k, l]) => (
              <option key={k} value={k}>
                {l}
              </option>
            ))}
          </select>
        </label>
        {o.kind === "uptime" ? (
          <label className="field full">
            Uptime check
            <select value={o.check_id} onChange={(e) => set({ check_id: +e.target.value })}>
              <option value={0}>Choose a check</option>
              {props.checks.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
            <span className="help">
              Probe results are kept for <code>METRIC_RETENTION</code>; a longer window only counts what is kept.{" "}
              <Link to="/alerts/uptime">Manage checks</Link>
            </span>
          </label>
        ) : (
          <>
            <label className="field">
              Service
              <select value={o.service} onChange={(e) => set({ service: e.target.value, operation: "" })}>
                <option value="">Choose a service</option>
                {(svcs.data ?? []).map((s) => (
                  <option key={s.name}>{s.name}</option>
                ))}
              </select>
            </label>
            <label className="field">
              Operation
              <select value={o.operation} onChange={(e) => set({ operation: e.target.value })}>
                <option value="">All requests</option>
                {(ops.data?.operations ?? []).map((x) => (
                  <option key={x.name}>{x.name}</option>
                ))}
              </select>
            </label>
          </>
        )}
        {o.kind === "latency" && (
          <label className="field">
            Faster than (ms)
            <input type="number" value={o.threshold_ms} onChange={(e) => set({ threshold_ms: +e.target.value })} />
          </label>
        )}
        <label className="field">
          Objective (%)
          <input type="number" step="0.01" value={o.objective} onChange={(e) => set({ objective: +e.target.value })} />
          <span className="help">
            Error budget: {o.objective && o.objective < 100 ? `${+(100 - o.objective).toFixed(3)}% of events may be bad` : "—"}
          </span>
        </label>
        <label className="field">
          Window
          <select value={o.window_days} onChange={(e) => set({ window_days: +e.target.value })}>
            {[1, 7, 14, 28, 30, 90].map((d) => (
              <option key={d} value={d}>
                {d} days
              </option>
            ))}
          </select>
        </label>
        <div className="field full">
          Notify webhooks when it burns too fast
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
                    checked={o.webhook_ids!.includes(h.id)}
                    onChange={(e) => set({ webhook_ids: e.target.checked ? [...o.webhook_ids!, h.id] : o.webhook_ids!.filter((x) => x !== h.id) })}
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
