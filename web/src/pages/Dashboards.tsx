import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Annotation, api, MetricSeriesInfo, qs } from "../api";
import { CopyLink, Empty, ErrorNote, Loading, Modal, pageRange, RangeLabel, RangePicker, rangeParams, useAsync, useAction } from "../components/ui";
import { PanelEditor } from "../dashboards/editor";
import { dashVariables, DashSpec, newId, Panel, TEMPLATES, usesServiceVar, Variable, VarValues } from "../dashboards/model";
import { PanelBody, ROW_PX, usePanelData } from "../dashboards/panels";
import { ago } from "../util";

export interface Dashboard {
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
  const run = useAction();
  const nav = useNavigate();
  const list = useAsync(() => api.get<Dashboard[]>("/api/dashboards"), []);
  const catalog = useAsync(() => api.get<MetricSeriesInfo[]>("/api/metrics"), []);
  const [creating, setCreating] = useState(false);
  const [importing, setImporting] = useState(false);

  const create = async (name: string, description: string, spec: DashSpec) => {
    const d = await run(() => api.post<Dashboard>("/api/dashboards", { name, description, spec }));
    if (d) nav(`/dashboards/${d.id}`);
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
        <span className="sub">Your own views of metrics and logs: graphs, gauges, stats and bar lists.</span>
        <div className="actions">
          <button onClick={() => setImporting(true)} title="Create a dashboard from exported JSON">
            Import
          </button>
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
      {importing && <ImportDashboard onClose={() => setImporting(false)} onImport={(n, d, spec) => create(n, d, spec)} />}
    </>
  );
}

function ImportDashboard({ onClose, onImport }: { onClose: () => void; onImport: (name: string, description: string, spec: DashSpec) => void }) {
  const [text, setText] = useState("");
  const [err, setErr] = useState<string>();
  const parse = () => {
    try {
      const v = JSON.parse(text);
      const spec: DashSpec = v.spec ?? v;
      if (!Array.isArray(spec.panels)) throw new Error("no panels in this JSON");
      // Fresh ids, so an import never collides with the original.
      spec.panels = spec.panels.map((p: Panel) => ({ ...p, id: newId() }));
      onImport(String(v.name ?? "Imported dashboard"), String(v.description ?? ""), spec);
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  const file = async (f?: File) => f && setText(await f.text());
  return (
    <Modal
      title="Import dashboard"
      onClose={onClose}
      wide
      footer={
        <>
          <button onClick={onClose}>Cancel</button>
          <button className="primary" disabled={!text.trim()} onClick={parse}>
            Import
          </button>
        </>
      }
    >
      <ErrorNote error={err} />
      <p className="small muted">Paste the JSON of an exported dashboard, or choose the file.</p>
      <input type="file" accept="application/json,.json" onChange={(e) => file(e.target.files?.[0])} />
      <textarea className="mono" rows={14} value={text} onChange={(e) => setText(e.target.value)} placeholder='{"name": "...", "spec": {"panels": [...]}}' style={{ width: "100%", marginTop: 8 }} />
    </Modal>
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

/** Values of every variable: from the URL (var-name=a,b), else the default. */
function readVars(variables: Variable[], params: URLSearchParams): VarValues {
  const out: VarValues = {};
  for (const v of variables) {
    // Older links used ?service= for the service picker.
    const raw = params.get("var-" + v.name) ?? (v.name === "service" ? params.get("service") : null);
    out[v.name] = raw !== null ? raw.split(",").filter(Boolean) : (v.default ?? []);
  }
  return out;
}

function useVarOptions(v: Variable, services: string[]) {
  return useAsync(async () => {
    if (v.kind === "service") return services;
    if (v.kind === "custom") return v.values ?? [];
    if (!v.metric || !v.key) return [];
    const attrs = await api.get<{ key: string; values: string[] }[]>(`/api/metrics/attrs${qs({ name: v.metric })}`);
    return attrs.find((a) => a.key === v.key)?.values ?? [];
  }, [v.kind, v.metric, v.key, (v.values ?? []).join(","), services.join(",")]);
}

function VarPicker({ v, value, services, onChange }: { v: Variable; value: string[]; services: string[]; onChange: (vals: string[]) => void }) {
  const opts = useVarOptions(v, services).data ?? [];
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => ref.current && !ref.current.contains(e.target as Node) && setOpen(false);
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [open]);
  const label = v.label || v.name;
  if (!v.multi) {
    return (
      <label className="var-pick">
        <span>{label}</span>
        <select value={value[0] ?? ""} onChange={(e) => onChange(e.target.value ? [e.target.value] : [])} aria-label={label}>
          <option value="">{v.kind === "service" ? "all services" : "all"}</option>
          {[...new Set([...value, ...opts])].map((o) => (
            <option key={o}>{o}</option>
          ))}
        </select>
      </label>
    );
  }
  return (
    <div className="var-pick" ref={ref}>
      <span>{label}</span>
      <button className="small" onClick={() => setOpen(!open)} aria-expanded={open}>
        {value.length === 0 ? "all" : value.length <= 2 ? value.join(", ") : `${value.length} selected`} ▾
      </button>
      {open && (
        <div className="var-menu">
          <label className="check">
            <input type="checkbox" checked={value.length === 0} onChange={() => onChange([])} /> all
          </label>
          {[...new Set([...value, ...opts])].map((o) => (
            <label key={o} className="check">
              <input type="checkbox" checked={value.includes(o)} onChange={(e) => onChange(e.target.checked ? [...value, o] : value.filter((x) => x !== o))} /> {o}
            </label>
          ))}
        </div>
      )}
    </div>
  );
}

function VariablesEditor({ initial, onSave, onClose }: { initial: Variable[]; onSave: (v: Variable[]) => void; onClose: () => void }) {
  const [vars, setVars] = useState<Variable[]>(initial);
  const catalog = useAsync(() => api.get<MetricSeriesInfo[]>("/api/metrics"), []);
  const metricNames = [...new Set((catalog.data ?? []).map((m) => m.name))].sort();
  const set = (i: number, x: Partial<Variable>) => setVars((vs) => vs.map((v, j) => (j === i ? { ...v, ...x } : v)));
  const valid = vars.every((v) => /^[A-Za-z_]\w*$/.test(v.name)) && new Set(vars.map((v) => v.name)).size === vars.length;
  return (
    <Modal
      title="Variables"
      onClose={onClose}
      wide
      footer={
        <>
          <button onClick={onClose}>Cancel</button>
          <button className="primary" disabled={!valid} onClick={() => onSave(vars)}>
            Apply
          </button>
        </>
      }
    >
      <p className="small muted">
        A variable is a picker above the panels. Use <code>$name</code> as a query's service, operation, filter value or log search; several values match any of
        them, none matches everything.
      </p>
      {vars.map((v, i) => (
        <div key={i} className="query-row">
          <div className="row" style={{ gap: 8, flexWrap: "wrap" }}>
            <input value={v.name} onChange={(e) => set(i, { name: e.target.value.trim() })} placeholder="name" className="mono" style={{ width: 120 }} aria-label="Variable name" />
            <input value={v.label ?? ""} onChange={(e) => set(i, { label: e.target.value })} placeholder="label" style={{ width: 120 }} aria-label="Label" />
            <select value={v.kind} onChange={(e) => set(i, { kind: e.target.value as Variable["kind"] })} aria-label="Values from">
              <option value="service">services</option>
              <option value="attr">values of a metric attribute</option>
              <option value="custom">a fixed list</option>
            </select>
            <label className="check small">
              <input type="checkbox" checked={!!v.multi} onChange={(e) => set(i, { multi: e.target.checked })} /> several values
            </label>
            <button className="small ghost" onClick={() => setVars(vars.filter((_, j) => j !== i))} aria-label="Remove variable">
              ✕
            </button>
          </div>
          {v.kind === "attr" && (
            <div className="row" style={{ gap: 8, marginTop: 8 }}>
              <input list="var-metrics" value={v.metric ?? ""} onChange={(e) => set(i, { metric: e.target.value })} placeholder="metric" className="mono" style={{ flex: 1 }} />
              <datalist id="var-metrics">
                {metricNames.map((m) => (
                  <option key={m} value={m} />
                ))}
              </datalist>
              <input value={v.key ?? ""} onChange={(e) => set(i, { key: e.target.value })} placeholder="attribute, e.g. host" className="mono" style={{ flex: 1 }} />
            </div>
          )}
          {v.kind === "custom" && (
            <input
              value={(v.values ?? []).join(", ")}
              onChange={(e) => set(i, { values: e.target.value.split(",").map((x) => x.trim()).filter(Boolean) })}
              placeholder="a, b, c"
              style={{ width: "100%", marginTop: 8 }}
            />
          )}
        </div>
      ))}
      <button className="small" onClick={() => setVars([...vars, { name: `var${vars.length + 1}`, kind: "attr" }])}>
        + Variable
      </button>
    </Modal>
  );
}

function download(name: string, data: unknown) {
  const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = name.replace(/[^\w.-]+/g, "_") + ".json";
  a.click();
  URL.revokeObjectURL(a.href);
}

export function DashboardView() {
  const { id } = useParams();
  const run = useAction();
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const d = useAsync(() => api.get<Dashboard>(`/api/dashboards/${id}`), [id]);
  const catalog = useAsync(() => api.get<MetricSeriesInfo[]>("/api/metrics"), []);
  const [draft, setDraft] = useState<Dashboard | null>(null);
  const [editing, setEditing] = useState<Panel | null>(null);
  const [editVars, setEditVars] = useState(false);
  const [dragging, setDragging] = useState<string | null>(null);
  const [tick, setTick] = useState(0);
  const [folded, setFolded] = useState<Record<string, boolean>>({});

  const dash = draft ?? d.data;
  const spec = dash?.spec;
  const range = pageRange(params, spec?.range ?? "6h");
  const refresh = Number(params.get("refresh") ?? spec?.refresh ?? 0);
  const variables = useMemo(() => (spec ? dashVariables(spec) : []), [spec]);
  const vars = useMemo(() => readVars(variables, params), [variables, params]);
  const showAnn = spec?.annotations !== false;
  const annService = vars.service?.length === 1 ? vars.service[0] : "";
  const ann = useAsync(() => (showAnn ? api.get<Annotation[]>(`/api/annotations${qs({ ...rangeParams(range), service: annService })}`) : Promise.resolve([])), [range, annService, showAnn, tick]);
  const setParam = (k: string, v: string) => {
    const p = new URLSearchParams(params);
    if (k.startsWith("var-")) p.delete("service");
    if (k === "range") {
      p.delete("from");
      p.delete("to");
    }
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

  const setSpec = (x: Partial<DashSpec>) => setDraft((v) => (v ? { ...v, spec: { ...v.spec, ...x } } : v));
  const setPanels = (ps: Panel[]) => setSpec({ panels: ps });
  const savePanel = (p: Panel) => {
    setPanels(panels.some((x) => x.id === p.id) ? panels.map((x) => (x.id === p.id ? p : x)) : [...panels, p]);
    setEditing(null);
  };
  const save = async () => {
    if (!draft) return;
    // The variables' current values become their defaults.
    const vs = (draft.spec.variables ?? []).map((v) => ({ ...v, default: vars[v.name] ?? v.default }));
    const spec = { ...draft.spec, variables: vs, range: range.startsWith("abs:") ? draft.spec.range : range, refresh, service_var: usesServiceVar(draft.spec) ? (vars.service?.[0] ?? "") : undefined };
    const ok = await run(() => api.put(`/api/dashboards/${draft.id}`, { name: draft.name, description: draft.description, spec }).then(() => true), "Dashboard saved");
    if (ok) {
      setDraft(null);
      d.reload();
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
    queries: [{ id: "A", source: "metrics", service: variables.some((v) => v.name === "service") ? "$service" : "", agg: "avg" }],
    options: {},
  });
  const addRow = () => setPanels([...panels, { id: newId(), title: "New row", type: "row", w: 12, h: 1, queries: [], options: {} }]);
  const isFolded = (p: Panel) => folded[p.id] ?? !!p.options.collapsed;

  // Panels below a collapsed row are hidden (not while editing).
  const shown: Panel[] = [];
  let hide = false;
  for (const p of panels) {
    if (p.type === "row") {
      hide = !edit && isFolded(p);
      shown.push(p);
    } else if (!hide) shown.push(p);
  }

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
          <RangeLabel range={range} />
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
              <button onClick={addRow} title="A collapsible section">
                + Row
              </button>
              <button onClick={() => setEditVars(true)}>Variables</button>
              <label className="check small" title="Mark deploys, outages and alarms on graphs">
                <input type="checkbox" checked={showAnn} onChange={(e) => setSpec({ annotations: e.target.checked })} /> annotations
              </label>
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
              <CopyLink label="Link" pin />
              <button onClick={() => download(dash.name, { name: dash.name, description: dash.description, spec: dash.spec })} title="Download as JSON">
                Export
              </button>
              <button onClick={() => setDraft(structuredClone(dash))}>Edit</button>
              <button className="ghost danger" onClick={remove} title="Delete dashboard">
                Delete
              </button>
            </>
          )}
        </div>
      </div>
      {variables.length > 0 && (
        <div className="var-bar">
          {variables.map((v) => (
            <VarPicker key={v.name} v={v} value={vars[v.name] ?? []} services={services} onChange={(vals) => setParam("var-" + v.name, vals.join(","))} />
          ))}
        </div>
      )}
      {edit && <div className="notice" style={{ marginBottom: 12 }}>Editing: drag panels to reorder, use ✎ to change one. Save when done.</div>}
      {panels.length === 0 ? (
        <div className="card">
          <Empty title="This dashboard is empty">{edit ? "Add a panel to start." : "Press Edit, then Add panel."}</Empty>
        </div>
      ) : (
        <div className="dash-grid">
          {shown.map((p) =>
            p.type === "row" ? (
              <div
                key={p.id}
                className={"dash-row" + (dragging === p.id ? " dragging" : "")}
                draggable={edit}
                onDragStart={() => setDragging(p.id)}
                onDragEnd={() => setDragging(null)}
                onDragOver={(e) => edit && e.preventDefault()}
                onDrop={() => dragging && move(dragging, p.id)}
              >
                <button className="ghost" onClick={() => setFolded({ ...folded, [p.id]: !isFolded(p) })} aria-expanded={!isFolded(p)}>
                  {isFolded(p) ? "▸" : "▾"} {p.title || "Row"}
                </button>
                {edit && (
                  <span className="panel-tools">
                    <button className="small ghost" onClick={() => setEditing(p)} title="Edit row" aria-label="Edit row">
                      ✎
                    </button>
                    <button className="small ghost" onClick={() => setPanels(panels.filter((x) => x.id !== p.id))} title="Remove row (keeps its panels)" aria-label="Remove row">
                      ✕
                    </button>
                  </span>
                )}
              </div>
            ) : (
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
                  vars={vars}
                  tick={tick}
                  edit={edit}
                  annotations={ann.data ?? []}
                  onEdit={() => setEditing(p)}
                  onDuplicate={() => setPanels([...panels, { ...structuredClone(p), id: newId(), title: p.title + " (copy)" }])}
                  onRemove={() => setPanels(panels.filter((x) => x.id !== p.id))}
                />
              </div>
            ),
          )}
        </div>
      )}
      {editing && <PanelEditor initial={editing} range={range} vars={vars} variables={variables} onClose={() => setEditing(null)} onSave={savePanel} />}
      {editVars && (
        <VariablesEditor
          initial={spec.variables ?? []}
          onClose={() => setEditVars(false)}
          onSave={(vs) => {
            setSpec({ variables: vs });
            setEditVars(false);
          }}
        />
      )}
    </>
  );
}

function PanelCard(props: {
  panel: Panel;
  range: string;
  vars: VarValues;
  tick: number;
  edit: boolean;
  annotations: Annotation[];
  onEdit: () => void;
  onDuplicate: () => void;
  onRemove: () => void;
}) {
  const { panel: p } = props;
  const { data, error, loading } = usePanelData(p, props.range, props.vars, props.tick);
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
      {error ? <div className="notice bad small">{error}</div> : <PanelBody panel={p} data={data} height={bodyH} annotations={props.annotations} />}
    </>
  );
}
