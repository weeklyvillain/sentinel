import type { Series } from "../api";

export type PanelType = "timeseries" | "stat" | "gauge" | "bar" | "table" | "heatmap" | "text" | "row";
export type Unit = "" | "percent" | "bytes" | "bytes/s" | "seconds" | "ms" | "per_s" | "count";
export type Reduce = "last" | "avg" | "max" | "min" | "sum";

export interface AttrFilter {
  key: string;
  op: "=" | "!=";
  value: string;
}

export interface PanelQuery {
  id: string; // A, B, ...
  source: "metrics" | "logs" | "spans";
  alias?: string;
  hidden?: boolean;
  // metrics
  metric?: string;
  service?: string; // may be "$service"
  agg?: "avg" | "min" | "max" | "sum" | "last" | "count" | "rate" | "p50" | "p75" | "p90" | "p95" | "p99" | "heatmap";
  combine?: "" | "sum" | "avg" | "min" | "max";
  group_by?: string[];
  filters?: AttrFilter[];
  limit?: number;
  // logs
  q?: string;
  min_severity?: number;
  /** "" | level | service | env | any attribute or JSON field */
  split_by?: string;
  /** What to compute from the matching lines (default count). */
  log_agg?: "count" | "rate" | "unique" | "sum" | "avg" | "min" | "max" | "p50" | "p90" | "p95" | "p99";
  /** The field for log_agg other than count/rate, e.g. duration_ms. */
  field?: string;
  // spans (request statistics from traces)
  operation?: string;
  stat?: "rate" | "count" | "errors" | "error_rate" | "avg" | "p50" | "p90" | "p95" | "p99" | "heatmap";
}

export interface PanelOptions {
  unit?: Unit;
  decimals?: number;
  min?: number;
  max?: number;
  /** Colour from warn / bad; with below=true, low values are bad (free disk). */
  warn?: number;
  bad?: number;
  below?: boolean;
  style?: "line" | "area" | "bars";
  stack?: boolean;
  reduce?: Reduce;
  text?: string;
  /** Rows: start collapsed. */
  collapsed?: boolean;
}

export interface Panel {
  id: string;
  title: string;
  type: PanelType;
  w: number; // of 12 columns
  h: number; // rows of 110px
  queries: PanelQuery[];
  formula?: string;
  options: PanelOptions;
}

/**
 * A dashboard variable: a picker above the panels whose value replaces
 * "$name" in queries (service, operation, filter values, log search).
 */
export interface Variable {
  name: string;
  label?: string;
  kind: "service" | "attr" | "custom";
  /** attr: the values of this attribute of this metric. */
  metric?: string;
  key?: string;
  /** custom: a fixed list. */
  values?: string[];
  multi?: boolean;
  default?: string[];
}

export type VarValues = Record<string, string[]>;

export interface DashSpec {
  panels: Panel[];
  range?: string;
  refresh?: number; // seconds, 0 = off
  /** Default for "$service" in queries; set when the dashboard uses it. */
  service_var?: string;
  variables?: Variable[];
  /** Mark deploys, outages and alarms on graphs (default on). */
  annotations?: boolean;
}

export interface PanelData {
  series: (Series & { query: string })[];
  bucket_ms: number;
}

export const newId = () => Math.random().toString(36).slice(2, 10);

export function usesServiceVar(spec: DashSpec): boolean {
  return spec.panels.some((p) => p.queries.some((q) => q.service === "$service"));
}

/**
 * The dashboard's variables. Older dashboards only had "$service" with
 * service_var as its value; that becomes a service variable.
 */
export function dashVariables(spec: DashSpec): Variable[] {
  const vars = [...(spec.variables ?? [])];
  if (usesServiceVar(spec) && !vars.some((v) => v.name === "service")) {
    vars.unshift({ name: "service", kind: "service", default: spec.service_var ? [spec.service_var] : [] });
  }
  return vars;
}

const VAR = /^\$([A-Za-z_][\w]*)$/;

function sub(v: string | undefined, vars: VarValues, join: string): string | undefined {
  if (!v) return v;
  const m = VAR.exec(v);
  if (m && vars[m[1]] !== undefined) return vars[m[1]].join(join);
  return v.replace(/\$([A-Za-z_]\w*)/g, (all, n) => (vars[n] !== undefined ? vars[n].join(" ") : all));
}

/** A copy of the queries with "$name" variables replaced by their values. */
export function resolveQueries(qs: PanelQuery[], vars: VarValues): PanelQuery[] {
  return qs.map((q) => {
    const out: PanelQuery = { ...q };
    // A service is a single value: the first one picked.
    if (q.service) out.service = sub(q.service, Object.fromEntries(Object.entries(vars).map(([k, v]) => [k, v.slice(0, 1)])), "");
    if (q.operation) out.operation = sub(q.operation, Object.fromEntries(Object.entries(vars).map(([k, v]) => [k, v.slice(0, 1)])), "");
    if (q.q) out.q = sub(q.q, vars, " ");
    if (q.filters) {
      // Several values match any of them; no value drops the filter (all).
      out.filters = q.filters.flatMap((f) => {
        const m = VAR.exec(f.value);
        if (m && vars[m[1]] !== undefined) return vars[m[1]].length ? [{ ...f, value: vars[m[1]].join("|") }] : [];
        return [{ ...f, value: sub(f.value, vars, "|") ?? "" }];
      });
    }
    return out;
  });
}

export function reduceSeries(s: Series, how: Reduce = "last"): number | null {
  const vals = s.points.map((p) => p.v).filter((v): v is number => v !== null && v !== undefined);
  if (!vals.length) return null;
  switch (how) {
    case "avg":
      return vals.reduce((a, b) => a + b, 0) / vals.length;
    case "max":
      return Math.max(...vals);
    case "min":
      return Math.min(...vals);
    case "sum":
      return vals.reduce((a, b) => a + b, 0);
    default:
      return vals[vals.length - 1];
  }
}

export type Tone = "ok" | "warn" | "bad" | "";

export function toneFor(v: number | null, o: PanelOptions): Tone {
  if (v === null || (o.warn === undefined && o.bad === undefined)) return "";
  const past = (t?: number) => t !== undefined && t !== null && (o.below ? v <= t : v >= t);
  if (past(o.bad)) return "bad";
  if (past(o.warn)) return "warn";
  return "ok";
}

export const toneColor: Record<Tone, string> = { ok: "var(--ok)", warn: "var(--warn)", bad: "var(--bad)", "": "var(--accent)" };

// --- formatting ----------------------------------------------------------

function trim(n: number, decimals?: number): string {
  const d = decimals ?? (Math.abs(n) >= 100 ? 0 : Math.abs(n) >= 10 ? 1 : 2);
  return n.toFixed(d).replace(/\.0+$|(\.\d*?)0+$/, "$1");
}

function scaled(n: number, base: number, units: string[], decimals?: number): string {
  let i = 0;
  while (Math.abs(n) >= base && i < units.length - 1) {
    n /= base;
    i++;
  }
  return `${trim(n, decimals)} ${units[i]}`;
}

export function formatValue(v: number | null | undefined, unit: Unit = "", decimals?: number): string {
  if (v === null || v === undefined || Number.isNaN(v)) return "—";
  switch (unit) {
    case "percent":
      return `${trim(v, decimals ?? 1)}%`;
    case "bytes":
      return scaled(v, 1024, ["B", "KiB", "MiB", "GiB", "TiB"], decimals);
    case "bytes/s":
      return scaled(v, 1024, ["B/s", "KiB/s", "MiB/s", "GiB/s"], decimals);
    case "seconds":
      if (Math.abs(v) < 1) return `${trim(v * 1000, decimals)} ms`;
      if (Math.abs(v) < 120) return `${trim(v, decimals)} s`;
      return `${trim(v / 60, decimals)} min`;
    case "ms":
      return Math.abs(v) < 1000 ? `${trim(v, decimals)} ms` : `${trim(v / 1000, decimals)} s`;
    case "per_s":
      return `${scaled(v, 1000, ["", "k", "M", "G"], decimals).trim()}/s`;
    default:
      return scaled(v, 1000, ["", "k", "M", "G", "T"], decimals).trim();
  }
}

export const UNITS: { value: Unit; label: string }[] = [
  { value: "", label: "Number" },
  { value: "percent", label: "Percent (0–100)" },
  { value: "bytes", label: "Bytes" },
  { value: "bytes/s", label: "Bytes per second" },
  { value: "seconds", label: "Seconds" },
  { value: "ms", label: "Milliseconds" },
  { value: "per_s", label: "Per second" },
  { value: "count", label: "Count" },
];

// --- templates -------------------------------------------------------------

const p = (x: Omit<Panel, "id">): Panel => ({ ...x, id: newId() });
const m = (id: string, metric: string, more: Partial<PanelQuery> = {}): PanelQuery => ({
  id,
  source: "metrics",
  metric,
  service: "$service",
  agg: "avg",
  ...more,
});

export interface Template {
  name: string;
  description: string;
  /** Pick a service that has this metric for $service. */
  needs?: string;
  spec: () => DashSpec;
}

export const TEMPLATES: Template[] = [
  {
    name: "Host overview",
    description: "CPU, memory, disk, load and network of a server (node_* metrics from Alloy or node_exporter).",
    needs: "node_cpu_seconds_total",
    spec: () => ({
      range: "6h",
      refresh: 60,
      service_var: "",
      panels: [
        p({
          title: "CPU used",
          type: "gauge",
          w: 3,
          h: 2,
          queries: [
            m("A", "node_cpu_seconds_total", { agg: "rate", filters: [{ key: "mode", op: "!=", value: "idle" }], hidden: true }),
            m("B", "node_cpu_seconds_total", { agg: "rate", hidden: true }),
          ],
          formula: "100 * A / B",
          options: { unit: "percent", min: 0, max: 100, warn: 70, bad: 90 },
        }),
        p({
          title: "Memory used",
          type: "gauge",
          w: 3,
          h: 2,
          queries: [m("A", "node_memory_MemAvailable_bytes", { agg: "last", hidden: true }), m("B", "node_memory_MemTotal_bytes", { agg: "last", hidden: true })],
          formula: "100 * (1 - A / B)",
          options: { unit: "percent", min: 0, max: 100, warn: 80, bad: 92 },
        }),
        p({
          title: "Disk used",
          type: "gauge",
          w: 3,
          h: 2,
          queries: [
            m("A", "node_filesystem_avail_bytes", { agg: "last", filters: [{ key: "mountpoint", op: "=", value: "/" }], hidden: true }),
            m("B", "node_filesystem_size_bytes", { agg: "last", filters: [{ key: "mountpoint", op: "=", value: "/" }], hidden: true }),
          ],
          formula: "100 * (1 - A / B)",
          options: { unit: "percent", min: 0, max: 100, warn: 80, bad: 90 },
        }),
        p({
          title: "Load (1 min)",
          type: "stat",
          w: 3,
          h: 2,
          queries: [m("A", "node_load1", { agg: "last" })],
          options: { decimals: 2, reduce: "last" },
        }),
        p({
          title: "CPU by mode",
          type: "timeseries",
          w: 6,
          h: 3,
          queries: [m("A", "node_cpu_seconds_total", { agg: "rate", group_by: ["mode"], filters: [{ key: "mode", op: "!=", value: "idle" }] })],
          options: { unit: "", style: "area", stack: true, decimals: 2 },
        }),
        p({
          title: "Memory",
          type: "timeseries",
          w: 6,
          h: 3,
          queries: [
            m("A", "node_memory_MemTotal_bytes", { alias: "total" }),
            m("B", "node_memory_MemAvailable_bytes", { alias: "available" }),
          ],
          options: { unit: "bytes", style: "line" },
        }),
        p({
          title: "Network",
          type: "timeseries",
          w: 6,
          h: 3,
          queries: [
            m("A", "node_network_receive_bytes_total", { agg: "rate", alias: "in", filters: [{ key: "device", op: "!=", value: "lo" }] }),
            m("B", "node_network_transmit_bytes_total", { agg: "rate", alias: "out", filters: [{ key: "device", op: "!=", value: "lo" }] }),
          ],
          options: { unit: "bytes/s", style: "line" },
        }),
        p({
          title: "Disk space used by mountpoint",
          type: "bar",
          w: 6,
          h: 3,
          queries: [
            m("A", "node_filesystem_avail_bytes", { agg: "last", group_by: ["mountpoint"], hidden: true }),
            m("B", "node_filesystem_size_bytes", { agg: "last", group_by: ["mountpoint"], hidden: true }),
          ],
          formula: "100 * (1 - A / B)",
          options: { unit: "percent", max: 100, warn: 80, bad: 90 },
        }),
      ],
    }),
  },
  {
    name: "Service overview",
    description: "Request rate, errors, latency and logs of a service, from OpenTelemetry HTTP and runtime metrics.",
    spec: () => ({
      range: "6h",
      refresh: 60,
      service_var: "",
      panels: [
        p({
          title: "Errors and warnings",
          type: "timeseries",
          w: 6,
          h: 3,
          queries: [{ id: "A", source: "logs", service: "$service", min_severity: 13, split_by: "level" }],
          options: { style: "bars", stack: true },
        }),
        p({
          title: "Log lines",
          type: "stat",
          w: 3,
          h: 2,
          queries: [{ id: "A", source: "logs", service: "$service" }],
          options: { reduce: "sum" },
        }),
        p({
          title: "Error log lines",
          type: "stat",
          w: 3,
          h: 2,
          queries: [{ id: "A", source: "logs", service: "$service", min_severity: 17 }],
          options: { reduce: "sum", warn: 1, bad: 25 },
        }),
        p({
          title: "Notes",
          type: "text",
          w: 6,
          h: 1,
          queries: [],
          options: { text: "Add panels for this service's own metrics with Edit → Add panel." },
        }),
      ],
    }),
  },
];
