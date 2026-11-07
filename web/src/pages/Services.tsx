import { useState } from "react";
import { Link } from "react-router-dom";
import { api, GitHubStatus, Service } from "../api";
import { useMe } from "../App";
import { Empty, ErrorNote, Loading, Modal, Switch, useAsync, useToast } from "../components/ui";
import { ago, fmtFull, serviceUp } from "../util";

export default function Services() {
  const me = useMe();
  const toast = useToast();
  const svcs = useAsync(() => api.get<Service[]>("/api/services"), [], 15000);
  const gh = useAsync(() => api.get<GitHubStatus>("/api/github"), []);
  const [editing, setEditing] = useState<Partial<Service> | null>(null);

  const remove = async (s: Service) => {
    if (!confirm(`Remove ${s.name}? Its logs and issues are kept; if it sends telemetry again it reappears.`)) return;
    try {
      await api.del(`/api/services/${encodeURIComponent(s.name)}`);
      toast(`Removed ${s.name}`);
      svcs.reload();
    } catch (e) {
      toast((e as Error).message, true);
    }
  };
  const setIssues = async (s: Service, issues: boolean) => {
    try {
      await api.patch(`/api/services/${encodeURIComponent(s.name)}`, { issues });
      toast(issues ? `Issues on for ${s.name}` : `Issues off for ${s.name}: errors are still grouped, nothing is filed`);
      svcs.reload();
    } catch (e) {
      toast((e as Error).message, true);
    }
  };
  const defaultRepo = gh.data?.default_repo || gh.data?.env_default_repo;

  return (
    <>
      <div className="page-head">
        <h1>Services</h1>
        <span className="sub">Services appear automatically when they send telemetry; add one to route its issues ahead of time.</span>
        <div className="actions">
          <button className="primary" onClick={() => setEditing({})}>
            + Add service
          </button>
        </div>
      </div>
      <ErrorNote error={svcs.error} />
      <div className="card flush">
        {!svcs.data ? (
          <Loading />
        ) : svcs.data.length === 0 ? (
          <Empty title="No services yet">
            Point your OpenTelemetry exporter at <code>http://&lt;sentinel-host&gt;{me.info.otlp_addr}</code> with <code>service.name</code> set.
          </Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th style={{ width: 60 }} title="File GitHub issues for this service">Issues</th>
                <th>Service</th>
                <th>Status</th>
                <th>Last seen</th>
                <th>Issues go to</th>
                <th>Labels</th>
                <th className="num">Active groups</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {svcs.data.map((s) => {
                const st = serviceUp(s);
                return (
                  <tr key={s.name} style={s.issues ? undefined : { opacity: 0.75 }}>
                    <td>
                      <Switch checked={s.issues} onChange={(v) => setIssues(s, v)} label={`Issues for ${s.name}`} />
                    </td>
                    <td>
                      <div style={{ fontWeight: 600 }}>{s.name}</div>
                      <div className="small faint">
                        {s.env || "—"}
                        {s.description ? ` · ${s.description}` : ""}
                      </div>
                    </td>
                    <td>
                      {st === "up" && !s.watch && (
                        <span className="badge" title="Not watched for silence. Add it under Uptime → Watched services to be alerted when it stops sending.">
                          sending
                        </span>
                      )}
                      {st === "up" && s.watch && <span className="badge ok"><span className="dot ok" /> up</span>}
                      {st === "down" && <span className="badge bad" title={`since ${fmtFull(s.down_since)}`}><span className="dot bad" /> down</span>}
                      {st === "never" && <span className="badge">waiting for data</span>}
                    </td>
                    <td className="muted nowrap" title={fmtFull(s.last_seen)}>
                      {ago(s.last_seen)}
                    </td>
                    <td className="nowrap">
                      {s.repo ? <span className="mono">{s.repo}</span> : defaultRepo ? <span className="muted">default ({defaultRepo})</span> : <span style={{ color: "var(--warn)" }}>not set</span>}
                    </td>
                    <td>
                      {(s.labels ?? []).map((l) => (
                        <span key={l} className="badge" style={{ marginRight: 4 }}>
                          {l}
                        </span>
                      ))}
                    </td>
                    <td className="num">
                      {s.active_groups ? <Link to={`/issues?service=${encodeURIComponent(s.name)}`}>{s.active_groups}</Link> : <span className="faint">0</span>}
                    </td>
                    <td className="nowrap" style={{ textAlign: "right" }}>
                      <Link className="btn small" to={`/logs?service=${encodeURIComponent(s.name)}`}>
                        Logs
                      </Link>{" "}
                      <button className="small" onClick={() => setEditing(s)}>
                        Edit
                      </button>{" "}
                      <button className="small danger" onClick={() => remove(s)}>
                        Remove
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
      <p className="small faint">
        A service is flagged down after {me.info.down_after} without logs, traces or metrics. Repositories must be authorized on the <Link to="/github">GitHub</Link> page.
      </p>
      {editing && (
        <ServiceForm
          initial={editing}
          gh={gh.data}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            svcs.reload();
          }}
        />
      )}
    </>
  );
}

function ServiceForm(props: { initial: Partial<Service>; gh?: GitHubStatus; onClose: () => void; onSaved: () => void }) {
  const toast = useToast();
  const isNew = !props.initial.name;
  const [name, setName] = useState(props.initial.name ?? "");
  const [repo, setRepo] = useState(props.initial.repo ?? "");
  const [labels, setLabels] = useState((props.initial.labels ?? []).join(", "));
  const [description, setDescription] = useState(props.initial.description ?? "");
  const [err, setErr] = useState<string>();
  const [saving, setSaving] = useState(false);
  const allowed = (props.gh?.repos ?? []).filter((r) => r.allowed);
  const appMode = props.gh?.mode === "app";

  const save = async () => {
    setSaving(true);
    setErr(undefined);
    try {
      const body = { name, repo, labels: labels.split(",").map((l) => l.trim()).filter(Boolean), description };
      if (isNew) await api.post("/api/services", body);
      else await api.put(`/api/services/${encodeURIComponent(name)}`, body);
      toast(`Saved ${name}`);
      props.onSaved();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      title={isNew ? "Add service" : `Edit ${props.initial.name}`}
      onClose={props.onClose}
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" onClick={save} disabled={saving || !name.trim()}>
            {saving ? "Saving…" : "Save"}
          </button>
        </>
      }
    >
      <ErrorNote error={err} />
      <label className="field">
        Service name
        <input value={name} onChange={(e) => setName(e.target.value)} disabled={!isNew} placeholder="checkout" autoFocus={isNew} />
        <span className="help">Must equal the service.name resource attribute your app sends.</span>
      </label>
      <label className="field">
        GitHub repository for issues
        {appMode ? (
          <select value={repo} onChange={(e) => setRepo(e.target.value)}>
            <option value="">Use default repository{props.gh?.default_repo ? ` (${props.gh.default_repo})` : ""}</option>
            {allowed.map((r) => (
              <option key={r.full_name} value={r.full_name}>
                {r.full_name}
              </option>
            ))}
          </select>
        ) : (
          <input value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="owner/name (empty = default)" />
        )}
        {appMode && allowed.length === 0 && (
          <span className="help">
            No repositories authorized yet — allow some on the <Link to="/github">GitHub</Link> page.
          </span>
        )}
      </label>
      <label className="field">
        Extra labels
        <input value={labels} onChange={(e) => setLabels(e.target.value)} placeholder="team-payments, p2" />
        <span className="help">Added to issues for this service (comma separated).</span>
      </label>
      <label className="field">
        Description
        <input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Optional" />
      </label>
    </Modal>
  );
}
