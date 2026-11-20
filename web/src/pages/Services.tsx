import { useState } from "react";
import { Link } from "react-router-dom";
import { api, GitHubStatus, Service } from "../api";
import { Empty, ErrorNote, Loading, Modal, Switch, useAction, useAsync, useToast } from "../components/ui";
import { useMe, useServices } from "../shared";
import { ago, fmtFull, serviceUp } from "../util";

export default function Services() {
  const me = useMe();
  const run = useAction();
  const svcs = useServices();
  const gh = useAsync(() => api.get<GitHubStatus>("/api/github"), []);
  const [editing, setEditing] = useState<Partial<Service> | null>(null);

  const flag = async (s: Service, patch: { issues?: boolean; watch?: boolean }, ok: string) => {
    if (await run(() => api.patch(`/api/services/${encodeURIComponent(s.name)}`, patch), ok)) svcs.reload();
  };
  const defaultRepo = gh.data?.default_repo || gh.data?.env_default_repo;

  return (
    <>
      <div className="page-head">
        <span className="sub">Services appear when they send telemetry. Add one ahead of time to route its issues.</span>
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
                <th>Service</th>
                <th>Status</th>
                <th>Issues go to</th>
                <th className="num">Active groups</th>
                <th title="File GitHub issues for this service's errors">Issues</th>
                <th title={`Flag the service as down after ${me.info.down_after} without telemetry`}>Watch</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {svcs.data.map((s) => (
                <tr key={s.name}>
                  <td>
                    <div style={{ fontWeight: 600 }}>{s.name}</div>
                    <div className="small faint">
                      {s.env || "—"}
                      {s.description ? ` · ${s.description}` : ""}
                    </div>
                  </td>
                  <td className="nowrap">
                    <ServiceState s={s} />
                  </td>
                  <td className="nowrap">
                    {s.repo ? <span className="mono">{s.repo}</span> : defaultRepo ? <span className="muted">default ({defaultRepo})</span> : <span style={{ color: "var(--warn)" }}>not set</span>}
                    {(s.labels ?? []).length > 0 && <div className="small faint">labels: {s.labels!.join(", ")}</div>}
                  </td>
                  <td className="num">
                    {s.active_groups ? <Link to={`/issues?service=${encodeURIComponent(s.name)}`}>{s.active_groups}</Link> : <span className="faint">0</span>}
                  </td>
                  <td>
                    <Switch
                      checked={s.issues}
                      label={`Issues for ${s.name}`}
                      onChange={(v) => flag(s, { issues: v }, v ? `Issues on for ${s.name}` : `Issues off for ${s.name}: errors are still grouped, nothing is filed`)}
                    />
                  </td>
                  <td>
                    <Switch
                      checked={s.watch}
                      label={`Watch ${s.name} for downtime`}
                      onChange={(v) =>
                        flag(s, { watch: v }, v ? `Watching ${s.name}: it is flagged down after ${me.info.down_after} of silence` : `Stopped watching ${s.name}`)
                      }
                    />
                  </td>
                  <td className="nowrap" style={{ textAlign: "right" }}>
                    <Link className="btn small" to={`/logs?service=${encodeURIComponent(s.name)}`}>
                      Logs
                    </Link>{" "}
                    <button className="small" onClick={() => setEditing(s)}>
                      Edit
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      <p className="small faint">
        <strong>Watch</strong>: a watched service that sends nothing for {me.info.down_after} is flagged down, gets an issue and sends <code>service.down</code>{" "}
        to webhooks. Unwatched services are never flagged for being quiet. Repositories must be authorized under{" "}
        <Link to="/settings/github">Settings → GitHub</Link>.
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

/** Up/down only means something for watched services; others just show when they were last heard from. */
function ServiceState({ s }: { s: Service }) {
  const st = serviceUp(s);
  if (st === "never") return <span className="badge">waiting for data</span>;
  if (st === "down")
    return (
      <span className="badge bad" title={`since ${fmtFull(s.down_since)}`}>
        <span className="dot bad" /> down · {ago(s.down_since)}
      </span>
    );
  return (
    <span className="row" title={fmtFull(s.last_seen)} style={{ gap: 6 }}>
      {s.watch ? <span className="dot ok" /> : null}
      <span className={s.watch ? "" : "muted"}>{s.watch ? "up" : "seen"} · {ago(s.last_seen)}</span>
    </span>
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

  const remove = async () => {
    if (!confirm(`Remove ${name}? Its logs and issues are kept; if it sends telemetry again it reappears.`)) return;
    try {
      await api.del(`/api/services/${encodeURIComponent(name)}`);
      toast(`Removed ${name}`);
      props.onSaved();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
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
          {!isNew && (
            <button className="danger" onClick={remove} style={{ marginRight: "auto" }}>
              Remove
            </button>
          )}
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
            No repositories authorized yet — allow some on the <Link to="/settings/github">GitHub</Link> settings.
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
