import { useMemo, useState } from "react";
import { api, MetricSeriesInfo as MetricSeries, qs } from "../api";
import { Modal, useAsync } from "../components/ui";
import { Panel, PanelQuery, PanelType, UNITS } from "./model";
import { PanelBody, ROW_PX, usePanelData } from "./panels";

const TYPES: { value: PanelType; label: string; hint: string }[] = [
  { value: "timeseries", label: "Graph", hint: "Values over time" },
  { value: "stat", label: "Stat", hint: "One big number with a sparkline" },
  { value: "gauge", label: "Gauge", hint: "A value within a range" },
  { value: "bar", label: "Bar list", hint: "Compare series, largest first" },
  { value: "text", label: "Text", hint: "A note" },
];

const AGGS = [
  { value: "avg", label: "average" },
  { value: "last", label: "last value" },
  { value: "max", label: "max" },
  { value: "min", label: "min" },
  { value: "sum", label: "sum" },
  { value: "rate", label: "rate per second (counters)" },
  { value: "count", label: "count of points" },
];

const LETTERS = "ABCDEF";

export function PanelEditor({ initial, range, service, onSave, onClose }: { initial: Panel; range: string; service: string; onSave: (p: Panel) => void; onClose: () => void }) {
  const [p, setP] = useState<Panel>(initial);
  const set = (x: Partial<Panel>) => setP((v) => ({ ...v, ...x }));
  const setO = (x: Partial<Panel["options"]>) => setP((v) => ({ ...v, options: { ...v.options, ...x } }));
  const setQ = (i: number, x: Partial<PanelQuery>) => setP((v) => ({ ...v, queries: v.queries.map((q, j) => (j === i ? { ...q, ...x } : q)) }));
  const catalog = useAsync(() => api.get<MetricSeries[]>("/api/metrics"), []);
  const [tick, setTick] = useState(0);
  const [preview, setPreview] = useState(p);
  const pd = usePanelData(preview, range, service, tick);
  const refresh = () => {
    setPreview(p);
    setTick((t) => t + 1);
  };

  const services = useMemo(() => [...new Set((catalog.data ?? []).map((m) => m.service))].sort(), [catalog.data]);
  const addQuery = () => {
    const id = LETTERS.split("").find((l) => !p.queries.some((q) => q.id === l));
    if (!id) return;
    set({ queries: [...p.queries, { id, source: "metrics", service: p.queries[0]?.service ?? "", agg: "avg" }] });
  };
  const num = (s: string) => (s.trim() === "" ? undefined : Number(s));
  const o = p.options;
  const previewH = Math.max(2, p.h) * ROW_PX - 40;

  return (
    <Modal
      title={initial.title ? `Edit panel: ${initial.title}` : "New panel"}
      onClose={onClose}
      wide="x"
      footer={
        <>
          <button onClick={onClose}>Cancel</button>
          <button className="primary" onClick={() => onSave(p)}>
            Apply
          </button>
        </>
      }
    >
      <div className="panel-editor">
        <div className="pe-form">
          <div className="form-grid">
            <label className="field">
              Title
              <input value={p.title} onChange={(e) => set({ title: e.target.value })} placeholder="CPU used" autoFocus />
            </label>
            <label className="field">
              Type
              <select value={p.type} onChange={(e) => set({ type: e.target.value as PanelType })}>
                {TYPES.map((t) => (
                  <option key={t.value} value={t.value}>
                    {t.label} — {t.hint}
                  </option>
                ))}
              </select>
            </label>
          </div>

          {p.type === "text" ? (
            <label className="field" style={{ marginTop: 12 }}>
              Text
              <textarea rows={6} value={o.text ?? ""} onChange={(e) => setO({ text: e.target.value })} />
            </label>
          ) : (
            <>
              <h4 className="pe-h">Queries</h4>
              {p.queries.map((q, i) => (
                <QueryRow
                  key={q.id}
                  q={q}
                  services={services}
                  catalog={catalog.data ?? []}
                  serviceVar={service}
                  onChange={(x) => setQ(i, x)}
                  onRemove={p.queries.length > 1 ? () => set({ queries: p.queries.filter((_, j) => j !== i) }) : undefined}
                />
              ))}
              <div className="row" style={{ gap: 8 }}>
                {p.queries.length < LETTERS.length && (
                  <button className="small" onClick={addQuery}>
                    + Query
                  </button>
                )}
              </div>
              <label className="field" style={{ marginTop: 12 }}>
                Formula <span className="help">optional, e.g. 100 * (1 - A / B). When set, only the formula is shown.</span>
                <input className="mono" value={p.formula ?? ""} onChange={(e) => set({ formula: e.target.value })} placeholder="A / B * 100" />
              </label>

              <h4 className="pe-h">Display</h4>
              <div className="form-grid three">
                <label className="field">
                  Unit
                  <select value={o.unit ?? ""} onChange={(e) => setO({ unit: e.target.value as never })}>
                    {UNITS.map((u) => (
                      <option key={u.value} value={u.value}>
                        {u.label}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="field">
                  Decimals
                  <input type="number" min={0} max={6} value={o.decimals ?? ""} onChange={(e) => setO({ decimals: num(e.target.value) })} placeholder="auto" />
                </label>
                {p.type === "timeseries" ? (
                  <label className="field">
                    Style
                    <select value={o.style ?? "line"} onChange={(e) => setO({ style: e.target.value as never })}>
                      <option value="line">Lines</option>
                      <option value="area">Area</option>
                      <option value="bars">Bars</option>
                    </select>
                  </label>
                ) : (
                  <label className="field">
                    Value
                    <select value={o.reduce ?? "last"} onChange={(e) => setO({ reduce: e.target.value as never })}>
                      <option value="last">Latest</option>
                      <option value="avg">Average over range</option>
                      <option value="max">Max over range</option>
                      <option value="min">Min over range</option>
                      <option value="sum">Total over range</option>
                    </select>
                  </label>
                )}
                {(p.type === "gauge" || p.type === "timeseries" || p.type === "bar") && (
                  <>
                    <label className="field">
                      Min
                      <input type="number" value={o.min ?? ""} onChange={(e) => setO({ min: num(e.target.value) })} placeholder={p.type === "gauge" ? "0" : "auto"} />
                    </label>
                    <label className="field">
                      Max
                      <input type="number" value={o.max ?? ""} onChange={(e) => setO({ max: num(e.target.value) })} placeholder={p.type === "gauge" ? "100" : "auto"} />
                    </label>
                  </>
                )}
                {p.type === "timeseries" && (
                  <label className="check" style={{ alignSelf: "end" }}>
                    <input type="checkbox" checked={!!o.stack} onChange={(e) => setO({ stack: e.target.checked })} /> Stack series
                  </label>
                )}
                <label className="field">
                  Warning at
                  <input type="number" value={o.warn ?? ""} onChange={(e) => setO({ warn: num(e.target.value) })} />
                </label>
                <label className="field">
                  Critical at
                  <input type="number" value={o.bad ?? ""} onChange={(e) => setO({ bad: num(e.target.value) })} />
                </label>
                <label className="check" style={{ alignSelf: "end" }}>
                  <input type="checkbox" checked={!!o.below} onChange={(e) => setO({ below: e.target.checked })} /> Low values are bad
                </label>
              </div>
            </>
          )}
          <h4 className="pe-h">Size</h4>
          <div className="row" style={{ gap: 12 }}>
            <label className="field" style={{ flex: 1 }}>
              Width
              <select value={p.w} onChange={(e) => set({ w: Number(e.target.value) })}>
                {[3, 4, 6, 8, 12].map((w) => (
                  <option key={w} value={w}>
                    {w === 12 ? "Full" : `${w}/12`}
                  </option>
                ))}
              </select>
            </label>
            <label className="field" style={{ flex: 1 }}>
              Height
              <select value={p.h} onChange={(e) => set({ h: Number(e.target.value) })}>
                {[1, 2, 3, 4, 5].map((h) => (
                  <option key={h} value={h}>
                    {h} row{h > 1 ? "s" : ""}
                  </option>
                ))}
              </select>
            </label>
          </div>
        </div>

        <div className="pe-preview">
          <div className="row" style={{ marginBottom: 8 }}>
            <b className="small">{p.title || "Preview"}</b>
            <button className="small" style={{ marginLeft: "auto" }} onClick={refresh}>
              {pd.loading ? "Loading…" : "Run"}
            </button>
          </div>
          <div className="card" style={{ minHeight: previewH + 20 }}>
            {pd.error ? <div className="notice bad">{pd.error}</div> : <PanelBody panel={preview} data={pd.data} height={previewH} />}
          </div>
          <p className="small faint">Press Run to preview your changes. Range: {range}.</p>
        </div>
      </div>
    </Modal>
  );
}

function QueryRow({
  q,
  services,
  catalog,
  serviceVar,
  onChange,
  onRemove,
}: {
  q: PanelQuery;
  services: string[];
  catalog: MetricSeries[];
  serviceVar: string;
  onChange: (x: Partial<PanelQuery>) => void;
  onRemove?: () => void;
}) {
  const svc = q.service === "$service" ? serviceVar : q.service ?? "";
  const metrics = useMemo(() => [...new Set(catalog.filter((m) => !svc || m.service === svc).map((m) => m.name))].sort(), [catalog, svc]);
  const kind = catalog.find((m) => m.name === q.metric && (!svc || m.service === svc))?.kind;
  const attrs = useAsync(
    () => (q.metric ? api.get<{ key: string; values: string[] }[]>(`/api/metrics/attrs${qs({ name: q.metric, service: svc })}`) : Promise.resolve([])),
    [q.metric, svc],
  );
  const keys = ["service", ...(attrs.data ?? []).map((a) => a.key)];
  const filters = q.filters ?? [];
  const setFilter = (i: number, x: Partial<{ key: string; op: "=" | "!="; value: string }>) =>
    onChange({ filters: filters.map((f, j) => (j === i ? { ...f, ...x } : f)) });

  return (
    <div className="query-row">
      <div className="row" style={{ gap: 8, marginBottom: 8 }}>
        <span className="query-id">{q.id}</span>
        <select value={q.source} onChange={(e) => onChange({ source: e.target.value as never })} style={{ width: 110 }}>
          <option value="metrics">Metric</option>
          <option value="logs">Log count</option>
        </select>
        <select value={q.service ?? ""} onChange={(e) => onChange({ service: e.target.value })} style={{ flex: 1, minWidth: 140 }} aria-label="Service">
          <option value="">All services</option>
          <option value="$service">$service (dashboard picker)</option>
          {services.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </select>
        <input value={q.alias ?? ""} onChange={(e) => onChange({ alias: e.target.value })} placeholder="Name" style={{ width: 110 }} aria-label="Series name" />
        <label className="check small" title="Use only in the formula">
          <input type="checkbox" checked={!!q.hidden} onChange={(e) => onChange({ hidden: e.target.checked })} /> hide
        </label>
        {onRemove && (
          <button className="small ghost" onClick={onRemove} aria-label={`Remove query ${q.id}`}>
            ✕
          </button>
        )}
      </div>
      {q.source === "logs" ? (
        <div className="row" style={{ gap: 8 }}>
          <input value={q.q ?? ""} onChange={(e) => onChange({ q: e.target.value })} placeholder='Search, e.g. timeout -"health"' style={{ flex: 1 }} />
          <select value={q.min_severity ?? 0} onChange={(e) => onChange({ min_severity: Number(e.target.value) })}>
            <option value={0}>All levels</option>
            <option value={13}>Warn and up</option>
            <option value={17}>Error and up</option>
          </select>
          <select value={q.split_by ?? ""} onChange={(e) => onChange({ split_by: e.target.value as never })}>
            <option value="">One series</option>
            <option value="level">By level</option>
            <option value="service">By service</option>
          </select>
        </div>
      ) : (
        <>
          <div className="row" style={{ gap: 8 }}>
            <input list={`metrics-${q.id}`} className="mono" value={q.metric ?? ""} onChange={(e) => onChange({ metric: e.target.value })} placeholder="metric name" style={{ flex: 2, minWidth: 180 }} aria-label="Metric" />
            <datalist id={`metrics-${q.id}`}>
              {metrics.map((m) => (
                <option key={m} value={m} />
              ))}
            </datalist>
            <select value={q.agg ?? "avg"} onChange={(e) => onChange({ agg: e.target.value as never })} style={{ flex: 1 }} aria-label="Aggregation">
              {AGGS.map((a) => (
                <option key={a.value} value={a.value}>
                  {a.label}
                </option>
              ))}
            </select>
            <select value={q.combine ?? ""} onChange={(e) => onChange({ combine: e.target.value as never })} style={{ width: 130 }} aria-label="Combine series" title="How series in a group are combined">
              <option value="">combine: auto</option>
              <option value="sum">combine: sum</option>
              <option value="avg">combine: avg</option>
              <option value="max">combine: max</option>
              <option value="min">combine: min</option>
            </select>
          </div>
          {kind && (
            <div className="small faint" style={{ marginTop: 4 }}>
              {kind}
              {q.metric?.endsWith("_total") && q.agg !== "rate" ? " · looks like a counter: use rate per second" : ""}
            </div>
          )}
          <div className="row" style={{ gap: 6, marginTop: 8, flexWrap: "wrap" }}>
            <span className="small muted">Split by</span>
            {keys.map((k) => {
              const on = (q.group_by ?? []).includes(k);
              return (
                <button
                  key={k}
                  className={"small" + (on ? " primary" : "")}
                  onClick={() => onChange({ group_by: on ? (q.group_by ?? []).filter((x) => x !== k) : [...(q.group_by ?? []), k] })}
                >
                  {k}
                </button>
              );
            })}
          </div>
          {filters.map((f, i) => {
            const values = attrs.data?.find((a) => a.key === f.key)?.values ?? [];
            return (
              <div key={i} className="row" style={{ gap: 6, marginTop: 6 }}>
                <select value={f.key} onChange={(e) => setFilter(i, { key: e.target.value })} style={{ flex: 1 }}>
                  {[f.key, ...keys.filter((k) => k !== f.key && k !== "service")].map((k) => (
                    <option key={k} value={k}>
                      {k || "attribute"}
                    </option>
                  ))}
                </select>
                <select value={f.op} onChange={(e) => setFilter(i, { op: e.target.value as never })} style={{ width: 60 }}>
                  <option value="=">=</option>
                  <option value="!=">≠</option>
                </select>
                <input list={`vals-${q.id}-${i}`} value={f.value} onChange={(e) => setFilter(i, { value: e.target.value })} placeholder="value (a|b for several)" style={{ flex: 1 }} />
                <datalist id={`vals-${q.id}-${i}`}>
                  {values.map((v) => (
                    <option key={v} value={v} />
                  ))}
                </datalist>
                <button className="small ghost" onClick={() => onChange({ filters: filters.filter((_, j) => j !== i) })} aria-label="Remove filter">
                  ✕
                </button>
              </div>
            );
          })}
          <button className="small ghost" style={{ marginTop: 6 }} onClick={() => onChange({ filters: [...filters, { key: keys[1] ?? "", op: "=", value: "" }] })}>
            + Filter
          </button>
        </>
      )}
    </div>
  );
}
