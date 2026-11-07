import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api, MetricSeriesInfo, qs, Series, Service } from "../api";
import { Legend, SeriesChart } from "../components/charts";
import { Empty, ErrorNote, Icon, Loading, RangePicker, useAsync } from "../components/ui";
import { ago } from "../util";

const AGGS = ["avg", "sum", "max", "min", "count"];

function defaultAgg(kind: string) {
  return kind === "counter" ? "sum" : "avg";
}

export default function Metrics() {
  const [params, setParams] = useSearchParams();
  const service = params.get("service") ?? "";
  const range = params.get("range") ?? "1h";
  const selected = (params.get("m") ?? "").split(",").filter(Boolean);
  const [filter, setFilter] = useState("");

  const svcs = useAsync(() => api.get<Service[]>("/api/services"), []);
  const list = useAsync(() => api.get<MetricSeriesInfo[]>("/api/metrics" + qs({ service })), [service]);

  const set = (patch: Record<string, string>) => {
    const p = new URLSearchParams(params);
    Object.entries(patch).forEach(([k, v]) => (v ? p.set(k, v) : p.delete(k)));
    setParams(p, { replace: true });
  };

  // Pick a service and a few metrics by default.
  useEffect(() => {
    if (!service && svcs.data?.length) {
      const withMetrics = svcs.data.find((s) => s.last_seen && !s.last_seen.startsWith("0001"));
      if (withMetrics) set({ service: withMetrics.name });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [svcs.data]);
  useEffect(() => {
    if (list.data && selected.length === 0 && list.data.length) set({ m: list.data.slice(0, 6).map((m) => m.name).join(",") });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [list.data]);

  const byName = useMemo(() => new Map((list.data ?? []).map((m) => [m.name, m])), [list.data]);
  const toggle = (name: string) =>
    set({ m: (selected.includes(name) ? selected.filter((x) => x !== name) : [...selected, name]).join(",") });
  const shown = (list.data ?? []).filter((m) => m.name.toLowerCase().includes(filter.toLowerCase()));

  return (
    <>
      <div className="page-head">
        <h1>Metrics</h1>
        <span className="sub">OTLP metrics per service (counters shown as per-interval deltas)</span>
        <div className="actions">
          <select value={service} onChange={(e) => set({ service: e.target.value, m: "" })} aria-label="Service">
            <option value="">All services</option>
            {(svcs.data ?? []).map((s) => (
              <option key={s.name}>{s.name}</option>
            ))}
          </select>
          <RangePicker value={range} onChange={(r) => set({ range: r })} />
        </div>
      </div>
      <ErrorNote error={list.error} />
      {!list.data ? (
        <Loading />
      ) : list.data.length === 0 ? (
        <div className="card">
          <Empty title="No metrics yet">
            Add Sentinel to your Collector's <code>metrics</code> pipeline (or set <code>OTEL_EXPORTER_OTLP_ENDPOINT</code>) and charts appear here.
          </Empty>
        </div>
      ) : (
        <div className="metrics-layout">
          <div className="card">
            <div className="search" style={{ minWidth: 0, marginBottom: 8 }}>
              <Icon.search />
              <input value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="Filter metrics" aria-label="Filter metrics" />
            </div>
            <div className="metric-list">
              {shown.map((m) => (
                <label key={m.service + m.name} title={m.description || m.name}>
                  <input type="checkbox" checked={selected.includes(m.name)} onChange={() => toggle(m.name)} />
                  <span className="truncate" style={{ flex: 1 }}>{m.name}</span>
                  <span className="faint small">{m.kind}</span>
                </label>
              ))}
            </div>
          </div>
          <div className="grid charts">
            {selected.length === 0 && (
              <div className="card">
                <Empty title="Pick metrics on the left" />
              </div>
            )}
            {selected.map((name) => (
              <MetricPanel key={service + name + range} service={service} name={name} range={range} info={byName.get(name)} />
            ))}
          </div>
        </div>
      )}
    </>
  );
}

function MetricPanel(props: { service: string; name: string; range: string; info?: MetricSeriesInfo }) {
  const [agg, setAgg] = useState(defaultAgg(props.info?.kind ?? ""));
  const [split, setSplit] = useState(false);
  const q = useAsync(
    () => api.get<{ series: Series[]; bucket_ms: number }>("/api/metrics/query" + qs({ service: props.service, name: props.name, range: props.range, agg, split })),
    [props.service, props.name, props.range, agg, split],
    30000,
  );
  const empty = q.data && q.data.series.every((s) => s.points.every((p) => p.v === null));
  return (
    <div className="card">
      <div className="card-head">
        <div style={{ minWidth: 0 }}>
          <h2 className="truncate mono" style={{ fontSize: 13 }} title={props.name}>
            {props.name}
          </h2>
          <div className="small faint">
            {props.info?.kind} {props.info?.unit && props.info.unit !== "1" ? `· ${props.info.unit}` : ""} · updated {ago(props.info?.last_seen)}
          </div>
        </div>
        <div className="right">
          <select value={agg} onChange={(e) => setAgg(e.target.value)} aria-label="Aggregation">
            {AGGS.map((a) => (
              <option key={a}>{a}</option>
            ))}
          </select>
          <label className="check small" title="One line per attribute set">
            <input type="checkbox" checked={split} onChange={(e) => setSplit(e.target.checked)} /> split
          </label>
        </div>
      </div>
      <ErrorNote error={q.error} />
      {!q.data ? (
        <Loading />
      ) : empty ? (
        <Empty title="No data in this range" />
      ) : (
        <>
          <SeriesChart series={q.data.series} bucketMs={q.data.bucket_ms} unit={props.info?.unit} area={!split} height={190} />
          {split && <Legend keys={q.data.series.map((s) => s.label)} />}
        </>
      )}
    </div>
  );
}
