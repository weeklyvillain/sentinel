import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { api, MetricSeriesInfo } from "../api";
import { Empty, ErrorNote, Loading, Modal, RangePicker, useAsync, useToast } from "../components/ui";
import { PanelEditor } from "../dashboards/editor";
import { DashSpec, newId, Panel, TEMPLATES, usesServiceVar } from "../dashboards/model";
import { PanelBody, ROW_PX, usePanelData } from "../dashboards/panels";
import { ago } from "../util";

interface Dashboard {
  id: number;
  name: string;
  description: string;
  spec: DashSpec;
  updated_at: string;
}

function emptySpec(): DashSpec {
  return { panels: [], range: "6h", refresh: 60 };
}

export default function Dashboards() {
  const toast = useToast();
  const nav = useNavigate();
  const list = useAsync(() => api.get<Dashboard[]>("/api/dashboards"), []);
  const catalog = useAsync(() => api.get<MetricSeriesInfo[]>("/api/metrics"), []);
  const [creating, setCreating] = useState(false);

  const create = async (name: string, description: string, spec: DashSpec) => {
    try {
      const d = await api.post<Dashboard>("/api/dashboards", { name, description, spec });
      nav(`/dashboards/${d.id}`);
    } catch (e) {
      toast((e as Error).message, true);
    }
  };
  const fromTemplate = (t: (typeof TEMPLATES)[number]) => {
    const spec = t.spec();
    if (spec.service_var !== undefined) {
      const withMetric = (catalog.data ?? []).filter((m) => !t.needs || m.name === t.needs).map((m) => m.service);
      spec.service_var = withMetric[0] ?? "";
    }
    create(t.name, t.description, spec);
  };

  return (
    <>
      <div className="page-head">
        <h1>Dashboards</h1>
        <span className="sub">Your own views of metrics and logs: graphs, gauges, stats and bar lists.</span>
        <div className="actions">
          <button className="primary" onClick={() => setCreating(true)}>
            + New dashboard
          </button>
        </div>
      </div>
      <ErrorNote error={list.error} />
      {!list.data ? (
        <Loading />
      ) : (
        <div className="grid dash-cards">
          {list.data.map((d) => (
            <Link key={d.id} to={`/dashboards/${d.id}`} className="card dash-card">
              <div className="dash-card-name">{d.name}</div>
              <div className="small muted">{d.description || `${d.spec.panels?.length ?? 0} panels`}</div>
              <div className="small faint" style={{ marginTop: "auto" }}>
                {d.spec.panels?.length ?? 0} panels · updated {ago(d.updated_at)}
              </div>
            </Link>
          ))}
          {list.data.length === 0 && (
            <div className="card" style={{ gridColumn: "1 / -1" }}>
              <Empty title="No dashboards yet">Start from a template below, or create an empty one and add panels.</Empty>
            </div>
          )}
        </div>
      )}

      <div className="page-head" style={{ marginTop: 28 }}>
        <h2 style={{ margin: 0 }}>Templates</h2>
        <span className="sub">Ready-made dashboards for the metrics you already send. Everything in them can be edited.</span>
      </div>
      <div className="grid dash-cards">
        {TEMPLATES.map((t) => {
          const available = !t.needs || (catalog.data ?? []).some((m) => m.name === t.needs);
          return (
            <div key={t.name} className="card dash-card">
              <div className="dash-card-name">{t.name}</div>
              <div className="small muted">{t.description}</div>
              <div style={{ marginTop: "auto" }}>
                <button className="small" onClick={() => fromTemplate(t)} disabled={!available} title={available ? "" : `No service sends ${t.needs} yet`}>
                  Use template
                </button>
                {!available && <span className="small faint"> needs {t.needs}</span>}
              </div>
            </div>
          );
        })}
      </div>
      {creating && <NewDashboard onClose={() => setCreating(false)} onCreate={(n, d) => create(n, d, emptySpec())} />}
    </>
  );
}

function NewDashboard({ onClose, onCreate }: { onClose: () => void; onCreate: (name: string, description: string) => void }) {
  const [name, setName] = useState("");
  const [desc, setDesc] = useState("");
  return (
    <Modal
      title="New dashboard"
      onClose={onClose}
      footer={
        <>
          <button onClick={onClose}>Cancel</button>
          <button className="primary" disabled={!name.trim()} onClick={() => onCreate(name.trim(), desc.trim())}>
            Create
          </button>
        </>
      }
    >
      <label className="field">
        Name
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="Payments" autoFocus />
      </label>
      <label className="field">
        Description
        <input value={desc} onChange={(e) => setDesc(e.target.value)} placeholder="optional" />
      </label>
    </Modal>
  );
}

const REFRESH = [
  { v: 0, l: "Off" },
  { v: 30, l: "30s" },
  { v: 60, l: "1m" },
  { v: 300, l: "5m" },
];

export function DashboardView() {
  const { id } = useParams();
  const toast = useToast();
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const d = useAsync(() => api.get<Dashboard>(`/api/dashboards/${id}`), [id]);
  const catalog = useAsync(() => api.get<MetricSeriesInfo[]>("/api/metrics"), []);
  const [draft, setDraft] = useState<Dashboard | null>(null);
  const [editing, setEditing] = useState<Panel | null>(null);
  const [dragging, setDragging] = useState<string | null>(null);
  const [tick, setTick] = useState(0);

  const dash = draft ?? d.data;
  const spec = dash?.spec;
  const range = params.get("range") ?? spec?.range ?? "6h";
  const service = params.get("service") ?? spec?.service_var ?? "";
  const refresh = Number(params.get("refresh") ?? spec?.refresh ?? 0);
  const setParam = (k: string, v: string) => {
    const p = new URLSearchParams(params);
    p.set(k, v);
    setParams(p, { replace: true });
  };
  const services = useMemo(() => [...new Set((catalog.data ?? []).map((m) => m.service))].sort(), [catalog.data]);

  useEffect(() => {
    if (!refresh || draft) return;
    const t = setInterval(() => setTick((x) => x + 1), refresh * 1000);
    return () => clearInterval(t);
  }, [refresh, draft]);

  if (d.error) return <ErrorNote error={d.error} />;
  if (!dash || !spec) return <Loading />;
  const panels = spec.panels ?? [];
  const edit = !!draft;

  const setPanels = (ps: Panel[]) => setDraft((x) => (x ? { ...x, spec: { ...x.spec, panels: ps } } : x));
  const savePanel = (p: Panel) => {
    setPanels(panels.some((x) => x.id === p.id) ? panels.map((x) => (x.id === p.id ? p : x)) : [...panels, p]);
    setEditing(null);
  };
  const save = async () => {
    if (!draft) return;
    try {
      const spec = { ...draft.spec, range, refresh, service_var: usesServiceVar(draft.spec) ? service : undefined };
      await api.put(`/api/dashboards/${draft.id}`, { name: draft.name, description: draft.description, spec });
      toast("Dashboard saved");
      setDraft(null);
      d.reload();
    } catch (e) {
      toast((e as Error).message, true);
    }
  };
  const remove = async () => {
    if (!confirm(`Delete dashboard "${dash.name}"?`)) return;
    await api.del(`/api/dashboards/${dash.id}`);
    nav("/dashboards");
  };
  const move = (from: string, to: string) => {
    const a = panels.findIndex((p) => p.id === from);
    const b = panels.findIndex((p) => p.id === to);
    if (a < 0 || b < 0 || a === b) return;
    const next = [...panels];
    const [x] = next.splice(a, 1);
    next.splice(b, 0, x);
    setPanels(next);
  };
  const blank = (): Panel => ({
    id: newId(),
    title: "",
    type: "timeseries",
    w: 6,
    h: 3,
    queries: [{ id: "A", source: "metrics", service: usesServiceVar(spec) ? "$service" : "", agg: "avg" }],
    options: {},
  });

  return (
    <>
      <div className="page-head">
        <div style={{ minWidth: 0 }}>
          <div className="small muted">
            <Link to="/dashboards">Dashboards</Link>
          </div>
          {edit ? (
            <input className="dash-title-input" value={draft!.name} onChange={(e) => setDraft({ ...draft!, name: e.target.value })} aria-label="Dashboard name" />
          ) : (
            <h1 style={{ marginTop: 4 }}>{dash.name}</h1>
          )}
        </div>
        <div className="actions">
          {usesServiceVar(spec) && (
            <select value={service} onChange={(e) => setParam("service", e.target.value)} aria-label="Service">
              {[service, ...services.filter((s) => s !== service)].map((s) => (
                <option key={s} value={s}>
                  {s || "choose a service"}
                </option>
              ))}
            </select>
          )}
          <RangePicker value={range} onChange={(v) => setParam("range", v)} />
          <select value={refresh} onChange={(e) => setParam("refresh", e.target.value)} aria-label="Auto refresh" title="Auto refresh">
            {REFRESH.map((r) => (
              <option key={r.v} value={r.v}>
                ⟳ {r.l}
              </option>
            ))}
          </select>
          {edit ? (
            <>
              <button onClick={() => setEditing(blank())}>+ Add panel</button>
              <button onClick={() => setDraft(null)}>Cancel</button>
              <button className="primary" onClick={save}>
                Save
              </button>
            </>
          ) : (
            <>
              <button onClick={() => setTick((t) => t + 1)} title="Refresh now">
                ⟳
              </button>
              <button onClick={() => setDraft(structuredClone(dash))}>Edit</button>
              <button className="ghost danger" onClick={remove} title="Delete dashboard">
                Delete
              </button>
            </>
          )}
        </div>
      </div>
      {edit && <div className="notice" style={{ marginBottom: 12 }}>Editing: drag panels to reorder, use ✎ to change one. Save when done.</div>}
      {panels.length === 0 ? (
        <div className="card">
          <Empty title="This dashboard is empty">{edit ? "Add a panel to start." : "Press Edit, then Add panel."}</Empty>
        </div>
      ) : (
        <div className="dash-grid">
          {panels.map((p) => (
            <div
              key={p.id}
              className={"dash-panel card" + (dragging === p.id ? " dragging" : "")}
              style={{ gridColumn: `span ${p.w}`, height: p.h * ROW_PX }}
              draggable={edit}
              onDragStart={() => setDragging(p.id)}
              onDragEnd={() => setDragging(null)}
              onDragOver={(e) => edit && e.preventDefault()}
              onDrop={() => dragging && move(dragging, p.id)}
            >
              <PanelCard
                panel={p}
                range={range}
                service={service}
                tick={tick}
                edit={edit}
                onEdit={() => setEditing(p)}
                onDuplicate={() => setPanels([...panels, { ...structuredClone(p), id: newId(), title: p.title + " (copy)" }])}
                onRemove={() => setPanels(panels.filter((x) => x.id !== p.id))}
              />
            </div>
          ))}
        </div>
      )}
      {editing && <PanelEditor initial={editing} range={range} service={service} onClose={() => setEditing(null)} onSave={savePanel} />}
    </>
  );
}

function PanelCard(props: { panel: Panel; range: string; service: string; tick: number; edit: boolean; onEdit: () => void; onDuplicate: () => void; onRemove: () => void }) {
  const { panel: p } = props;
  const { data, error, loading } = usePanelData(p, props.range, props.service, props.tick);
  const bodyH = p.h * ROW_PX - (p.title || props.edit ? 46 : 24);
  return (
    <>
      {(p.title || props.edit) && (
        <div className="panel-head">
          <span className="panel-title" title={p.title}>
            {p.title || <span className="faint">untitled</span>}
          </span>
          {loading && <span className="dot warn" title="Loading" />}
          {props.edit && (
            <span className="panel-tools">
              <button className="small ghost" onClick={props.onEdit} title="Edit panel" aria-label="Edit panel">
                ✎
              </button>
              <button className="small ghost" onClick={props.onDuplicate} title="Duplicate panel" aria-label="Duplicate panel">
                ⧉
              </button>
              <button className="small ghost" onClick={props.onRemove} title="Remove panel" aria-label="Remove panel">
                ✕
              </button>
            </span>
          )}
        </div>
      )}
      {error ? <div className="notice bad small">{error}</div> : <PanelBody panel={p} data={data} height={bodyH} />}
    </>
  );
}
