import { useEffect, useState } from "react";
import { Area, AreaChart, Bar, BarChart, CartesianGrid, Line, LineChart, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api, qs } from "../api";
import { colorFor, fmtTime, tickFormatter } from "../util";
import { formatValue, Panel, PanelData, PanelOptions, reduceSeries, resolveQueries, toneColor, toneFor } from "./model";

export const ROW_PX = 110;

const axis = { stroke: "var(--faint)", fontSize: 11, tickLine: false, axisLine: false } as const;
const tooltip = {
  contentStyle: { background: "var(--panel)", border: "1px solid var(--border)", borderRadius: 8, fontSize: 12, color: "var(--text)" },
  labelStyle: { color: "var(--muted)", marginBottom: 4 },
  cursor: { stroke: "var(--border)" },
};

/** Fetches a panel's data for the range; refetches when tick changes. */
export function usePanelData(panel: Panel, range: string, service: string, tick: number) {
  const [data, setData] = useState<PanelData | null>(null);
  const [error, setError] = useState<string>();
  const [loading, setLoading] = useState(false);
  const body = JSON.stringify({ queries: resolveQueries(panel.queries, service), formula: panel.formula || "" });
  useEffect(() => {
    if (panel.type === "text" || panel.queries.length === 0) return;
    let live = true;
    setLoading(true);
    api
      .post<PanelData>(`/api/panel/query${qs({ range })}`, JSON.parse(body))
      .then((d) => {
        if (live) {
          setData(d);
          setError(undefined);
        }
      })
      .catch((e) => live && setError((e as Error).message))
      .finally(() => live && setLoading(false));
    return () => {
      live = false;
    };
  }, [body, range, tick, panel.type]);
  return { data, error, loading };
}

export function PanelBody({ panel, data, height }: { panel: Panel; data: PanelData | null; height: number }) {
  const o = panel.options;
  if (panel.type === "text") return <div className="panel-text">{o.text || ""}</div>;
  if (!data) return null;
  if (data.series.length === 0) return <div className="panel-empty">No data in this range</div>;
  switch (panel.type) {
    case "stat":
      return <Stats data={data} o={o} height={height} />;
    case "gauge":
      return <Gauges data={data} o={o} height={height} />;
    case "bar":
      return <BarList data={data} o={o} />;
    default:
      return <TimeSeries data={data} o={o} height={height} />;
  }
}

function TimeSeries({ data, o, height }: { data: PanelData; o: PanelOptions; height: number }) {
  const labels = data.series.map((s) => s.label);
  const rows: Record<string, number | string | null>[] = [];
  data.series.forEach((s, si) =>
    s.points.forEach((p, i) => {
      rows[i] = rows[i] ?? { t: p.t };
      rows[i][labels[si]] = p.v;
    }),
  );
  const fmt = (v: number) => formatValue(v, o.unit, o.decimals);
  const style = o.style ?? "line";
  const common = { data: rows, margin: { top: 6, right: 8, left: 0, bottom: 0 } };
  const children = [
    <CartesianGrid key="g" stroke="var(--border)" vertical={false} />,
    <XAxis key="x" dataKey="t" tickFormatter={tickFormatter(data.bucket_ms)} {...axis} minTickGap={40} />,
    <YAxis key="y" {...axis} tickFormatter={(v) => formatValue(v, o.unit)} width={64} domain={[o.min ?? "auto", o.max ?? "auto"]} />,
    <Tooltip key="tt" {...tooltip} labelFormatter={(t) => fmtTime(String(t))} formatter={(v: number, n: string) => [fmt(v), n]} />,
    o.warn !== undefined ? <ReferenceLine key="w" y={o.warn} stroke="var(--warn)" strokeDasharray="4 4" /> : null,
    o.bad !== undefined ? <ReferenceLine key="b" y={o.bad} stroke="var(--bad)" strokeDasharray="4 4" /> : null,
  ];
  const stack = o.stack ? "s" : undefined;
  const color = (l: string) => colorFor(l, labels);
  return (
    <div style={{ height }}>
      <ResponsiveContainer width="100%" height="100%">
        {style === "bars" ? (
          <BarChart {...common}>
            {children}
            {labels.map((l) => (
              <Bar key={l} dataKey={l} stackId={stack} fill={color(l)} isAnimationActive={false} />
            ))}
          </BarChart>
        ) : style === "area" ? (
          <AreaChart {...common}>
            {children}
            {labels.map((l) => (
              <Area key={l} type="monotone" dataKey={l} stackId={stack} stroke={color(l)} fill={color(l)} fillOpacity={0.18} strokeWidth={1.6} dot={false} connectNulls isAnimationActive={false} />
            ))}
          </AreaChart>
        ) : (
          <LineChart {...common}>
            {children}
            {labels.map((l) => (
              <Line key={l} type="monotone" dataKey={l} stroke={color(l)} strokeWidth={1.6} dot={false} connectNulls isAnimationActive={false} />
            ))}
          </LineChart>
        )}
      </ResponsiveContainer>
    </div>
  );
}

function Sparkline({ points, color }: { points: { t: string; v: number | null }[]; color: string }) {
  return (
    <ResponsiveContainer width="100%" height="100%">
      <AreaChart data={points} margin={{ top: 2, right: 0, left: 0, bottom: 0 }}>
        <Area type="monotone" dataKey="v" stroke={color} fill={color} fillOpacity={0.15} strokeWidth={1.4} dot={false} connectNulls isAnimationActive={false} />
      </AreaChart>
    </ResponsiveContainer>
  );
}

function Stats({ data, o, height }: { data: PanelData; o: PanelOptions; height: number }) {
  const many = data.series.length > 1;
  return (
    <div className="stat-grid" style={{ height }}>
      {data.series.slice(0, 12).map((s) => {
        const v = reduceSeries(s, o.reduce);
        const tone = toneFor(v, o);
        return (
          <div key={s.label} className="stat-cell">
            {many && <div className="stat-label" title={s.label}>{s.label}</div>}
            <div className="stat-value" style={{ color: tone ? toneColor[tone] : undefined }}>
              {formatValue(v, o.unit, o.decimals)}
            </div>
            <div className="stat-spark">
              <Sparkline points={s.points} color={toneColor[tone]} />
            </div>
          </div>
        );
      })}
    </div>
  );
}

/** A 240° arc gauge. */
export function Gauge({ value, o, label, size }: { value: number | null; o: PanelOptions; label?: string; size: number }) {
  const min = o.min ?? 0;
  const max = o.max ?? 100;
  const frac = value === null ? 0 : Math.max(0, Math.min(1, (value - min) / (max - min || 1)));
  const tone = toneFor(value, o);
  const r = 40;
  const cx = 50;
  const cy = 52;
  const start = (150 * Math.PI) / 180;
  const sweep = (240 * Math.PI) / 180;
  const pt = (a: number) => [cx + r * Math.cos(a), cy + r * Math.sin(a)];
  const arc = (a0: number, a1: number) => {
    const [x0, y0] = pt(a0);
    const [x1, y1] = pt(a1);
    return `M ${x0} ${y0} A ${r} ${r} 0 ${a1 - a0 > Math.PI ? 1 : 0} 1 ${x1} ${y1}`;
  };
  const marks = [o.warn, o.bad].filter((t): t is number => t !== undefined && t > min && t < max);
  return (
    <div className="gauge" style={{ width: size }}>
      <svg viewBox="0 0 100 86" width={size} height={size * 0.86} role="img" aria-label={`${label ?? ""} ${formatValue(value, o.unit, o.decimals)}`}>
        <path d={arc(start, start + sweep)} stroke="var(--border)" strokeWidth={9} fill="none" strokeLinecap="round" />
        {value !== null && frac > 0 && <path d={arc(start, start + sweep * frac)} stroke={toneColor[tone]} strokeWidth={9} fill="none" strokeLinecap="round" />}
        {marks.map((t) => {
          const a = start + sweep * ((t - min) / (max - min));
          const [x0, y0] = [cx + (r - 7) * Math.cos(a), cy + (r - 7) * Math.sin(a)];
          const [x1, y1] = [cx + (r + 7) * Math.cos(a), cy + (r + 7) * Math.sin(a)];
          return <line key={t} x1={x0} y1={y0} x2={x1} y2={y1} stroke={t === o.bad ? "var(--bad)" : "var(--warn)"} strokeWidth={1.5} />;
        })}
        <text x={cx} y={cy + 4} textAnchor="middle" fontSize={15} fontWeight={700} fill="var(--text)">
          {formatValue(value, o.unit, o.decimals)}
        </text>
        <text x={cx} y={84} textAnchor="middle" fontSize={8} fill="var(--faint)">
          {formatValue(min, o.unit, 0)} – {formatValue(max, o.unit, 0)}
        </text>
      </svg>
      {label && (
        <div className="gauge-label" title={label}>
          {label}
        </div>
      )}
    </div>
  );
}

function Gauges({ data, o, height }: { data: PanelData; o: PanelOptions; height: number }) {
  const n = Math.min(data.series.length, 8);
  const many = n > 1;
  const size = Math.max(70, Math.min(height - (many ? 22 : 0), many ? 150 : 210));
  return (
    <div className="gauge-row" style={{ height }}>
      {data.series.slice(0, 8).map((s) => (
        <Gauge key={s.label} value={reduceSeries(s, o.reduce)} o={o} label={many ? s.label : undefined} size={size} />
      ))}
    </div>
  );
}

function BarList({ data, o }: { data: PanelData; o: PanelOptions }) {
  const rows = data.series
    .map((s) => ({ label: s.label, v: reduceSeries(s, o.reduce) }))
    .filter((r) => r.v !== null)
    .sort((a, b) => (b.v ?? 0) - (a.v ?? 0));
  const max = o.max ?? Math.max(...rows.map((r) => r.v ?? 0), 0);
  return (
    <div className="bar-list">
      {rows.map((r) => {
        const tone = toneFor(r.v, o);
        return (
          <div key={r.label} className="bar-row">
            <span className="bar-label" title={r.label}>
              {r.label}
            </span>
            <span className="bar-track">
              <span className="bar-fill" style={{ width: `${max > 0 ? Math.max(1, ((r.v ?? 0) / max) * 100) : 0}%`, background: toneColor[tone] }} />
            </span>
            <span className="bar-value">{formatValue(r.v, o.unit, o.decimals)}</span>
          </div>
        );
      })}
    </div>
  );
}
