import { Fragment, useState } from "react";
import { api, Delivery, Webhook } from "../api";
import { Empty, ErrorNote, Loading, Modal, Switch, useAsync, useToast } from "../components/ui";
import { ago, fmtFull } from "../util";

const EVENT_HELP: Record<string, string> = {
  "issue.created": "A new GitHub issue was filed",
  "issue.regression": "A closed issue was reopened because the error came back",
  "issue.updated": "An update comment was posted (at most once per update interval)",
  "service.down": "A service stopped sending telemetry",
  "service.recovered": "A down service is sending again",
};

interface HooksResp {
  webhooks: Webhook[];
  event_types: string[];
}

export default function Webhooks() {
  const toast = useToast();
  const hooks = useAsync(() => api.get<HooksResp>("/api/webhooks"), []);
  const [editing, setEditing] = useState<Partial<Webhook> | null>(null);
  const [log, setLog] = useState<Webhook | null>(null);

  const test = async (h: Webhook) => {
    try {
      const d = await api.post<Delivery>(`/api/webhooks/${h.id}/test`);
      toast(d.error ? `Test failed: ${d.error}` : `Delivered (HTTP ${d.status}, ${d.duration_ms}ms)`, !!d.error);
    } catch (e) {
      toast((e as Error).message, true);
    }
  };
  const toggle = async (h: Webhook, enabled: boolean) => {
    await api.put(`/api/webhooks/${h.id}`, { ...h, enabled });
    hooks.reload();
  };
  const remove = async (h: Webhook) => {
    if (!confirm(`Delete webhook "${h.name}"?`)) return;
    await api.del(`/api/webhooks/${h.id}`);
    hooks.reload();
  };

  return (
    <>
      <div className="page-head">
        <h1>Webhooks</h1>
        <span className="sub">Push issue and outage events to Slack, Discord or any HTTP endpoint. Alarms pick their webhooks on the Alarms page.</span>
        <div className="actions">
          <button className="primary" onClick={() => setEditing({})}>
            + New webhook
          </button>
        </div>
      </div>
      <ErrorNote error={hooks.error} />
      <div className="card flush">
        {!hooks.data ? (
          <Loading />
        ) : hooks.data.webhooks.length === 0 ? (
          <Empty title="No webhooks" />
        ) : (
          <table>
            <thead>
              <tr>
                <th style={{ width: 50 }}>On</th>
                <th>Name</th>
                <th>Format</th>
                <th>Events</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {hooks.data.webhooks.map((h) => (
                <tr key={h.id}>
                  <td>
                    <Switch checked={h.enabled} onChange={(v) => toggle(h, v)} label="Enabled" />
                  </td>
                  <td>
                    <div style={{ fontWeight: 600 }}>
                      {h.name} {h.has_secret && <span className="badge" title="Requests are signed with X-Sentinel-Signature">signed</span>}
                    </div>
                    <div className="small faint mono truncate" style={{ maxWidth: 360 }}>
                      {safeHost(h.url)}
                    </div>
                  </td>
                  <td>
                    <span className="badge">{h.format}</span>
                  </td>
                  <td className="small">{h.events.length ? h.events.join(", ") : <span className="faint">alarms only</span>}</td>
                  <td className="nowrap" style={{ textAlign: "right" }}>
                    <button className="small" onClick={() => test(h)}>
                      Send test
                    </button>{" "}
                    <button className="small" onClick={() => setLog(h)}>
                      Deliveries
                    </button>{" "}
                    <button className="small" onClick={() => setEditing(h)}>
                      Edit
                    </button>{" "}
                    <button className="small danger" onClick={() => remove(h)}>
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      <div className="card">
        <h2>Payload</h2>
        <p className="small muted" style={{ marginTop: 0 }}>
          <code>json</code> webhooks receive a POST with headers <code>X-Sentinel-Event</code>, <code>X-Sentinel-Delivery</code> and, with a secret,{" "}
          <code>X-Sentinel-Signature: sha256=HMAC_SHA256(secret, body)</code>. Failed deliveries (network errors, 429, 5xx) are retried twice.
        </p>
        <pre className="code">{`{
  "event": "issue.created",
  "timestamp": "2026-11-07T10:01:05Z",
  "service": "checkout",
  "message": "New issue for checkout: Payment provider timeouts (34 occurrences)",
  "group": { "id": 1, "fingerprint": "ca3f…", "kind": "log", "title": "…", "count": 34, "env": "prod" },
  "issue": { "repo": "acme/checkout", "number": 42, "url": "https://github.com/acme/checkout/issues/42" },
  "alarm": null
}`}</pre>
      </div>
      {editing && (
        <WebhookForm
          initial={editing}
          eventTypes={hooks.data?.event_types ?? []}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            hooks.reload();
            toast("Webhook saved");
          }}
        />
      )}
      {log && <DeliveryLog hook={log} onClose={() => setLog(null)} />}
    </>
  );
}

function safeHost(u: string) {
  try {
    const x = new URL(u);
    return x.host + (x.pathname.length > 1 ? x.pathname.slice(0, 12) + "…" : "");
  } catch {
    return u;
  }
}

function WebhookForm(props: { initial: Partial<Webhook>; eventTypes: string[]; onClose: () => void; onSaved: () => void }) {
  const [h, setH] = useState<Partial<Webhook>>({ name: "", url: "", format: "json", events: ["issue.created", "service.down", "service.recovered"], enabled: true, ...props.initial });
  const [secret, setSecret] = useState("");
  const [clearSecret, setClearSecret] = useState(false);
  const [err, setErr] = useState<string>();
  const set = (p: Partial<Webhook>) => setH((x) => ({ ...x, ...p }));
  const save = async () => {
    setErr(undefined);
    const body = { name: h.name, url: h.url, format: h.format, events: h.events, enabled: h.enabled, secret: secret || undefined, clear_secret: clearSecret };
    try {
      if (h.id) await api.put(`/api/webhooks/${h.id}`, body);
      else await api.post("/api/webhooks", body);
      props.onSaved();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <Modal
      title={h.id ? `Edit ${props.initial.name}` : "New webhook"}
      onClose={props.onClose}
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" onClick={save} disabled={!h.name || !h.url}>
            Save
          </button>
        </>
      }
    >
      <ErrorNote error={err} />
      <label className="field">
        Name
        <input value={h.name} onChange={(e) => set({ name: e.target.value })} placeholder="#ops-alerts" autoFocus />
      </label>
      <label className="field">
        URL
        <input value={h.url} onChange={(e) => set({ url: e.target.value })} placeholder="https://hooks.slack.com/services/…" className="mono" />
      </label>
      <label className="field">
        Format
        <select value={h.format} onChange={(e) => set({ format: e.target.value as Webhook["format"] })}>
          <option value="json">JSON (generic)</option>
          <option value="slack">Slack incoming webhook</option>
          <option value="discord">Discord webhook</option>
        </select>
      </label>
      <label className="field">
        Signing secret
        <input type="password" value={secret} onChange={(e) => setSecret(e.target.value)} placeholder={h.has_secret ? "•••••• (unchanged)" : "optional"} autoComplete="new-password" />
        {h.has_secret && (
          <label className="check small">
            <input type="checkbox" checked={clearSecret} onChange={(e) => setClearSecret(e.target.checked)} /> remove the existing secret
          </label>
        )}
      </label>
      <div className="field">
        Events
        {props.eventTypes.map((ev) => (
          <label key={ev} className="check" style={{ fontWeight: 400 }}>
            <input
              type="checkbox"
              checked={h.events!.includes(ev)}
              onChange={(e) => set({ events: e.target.checked ? [...h.events!, ev] : h.events!.filter((x) => x !== ev) })}
            />
            <code>{ev}</code> <span className="faint small">{EVENT_HELP[ev]}</span>
          </label>
        ))}
      </div>
      <label className="check">
        <input type="checkbox" checked={h.enabled} onChange={(e) => set({ enabled: e.target.checked })} /> Enabled
      </label>
    </Modal>
  );
}

function DeliveryLog({ hook, onClose }: { hook: Webhook; onClose: () => void }) {
  const d = useAsync(() => api.get<Delivery[]>(`/api/webhooks/${hook.id}/deliveries`), [hook.id], 10000);
  const [open, setOpen] = useState<number | null>(null);
  return (
    <Modal title={`Deliveries — ${hook.name}`} onClose={onClose} wide>
      {!d.data ? (
        <Loading />
      ) : d.data.length === 0 ? (
        <Empty title="Nothing delivered yet" />
      ) : (
        <table>
          <thead>
            <tr>
              <th>When</th>
              <th>Event</th>
              <th>Result</th>
              <th className="num">Time</th>
              <th className="num">Attempts</th>
            </tr>
          </thead>
          <tbody>
            {d.data.map((x) => (
              <Fragment key={x.id}>
                <tr className="clickable" onClick={() => setOpen(open === x.id ? null : x.id)}>
                  <td className="nowrap" title={fmtFull(x.time)}>
                    {ago(x.time)}
                  </td>
                  <td className="mono small">{x.event}</td>
                  <td>{x.error ? <span className="badge bad">{x.error}</span> : <span className="badge ok">{x.status}</span>}</td>
                  <td className="num">{x.duration_ms}ms</td>
                  <td className="num">{x.attempt}</td>
                </tr>
                {open === x.id && (
                  <tr>
                    <td colSpan={5}>
                      <pre className="code">{pretty(x.payload)}</pre>
                    </td>
                  </tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
    </Modal>
  );
}

function pretty(s?: string) {
  try {
    return JSON.stringify(JSON.parse(s ?? ""), null, 2);
  } catch {
    return s;
  }
}
