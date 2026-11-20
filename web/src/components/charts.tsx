import { Area, AreaChart, Bar, BarChart, CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import type { Bucket, Series } from "../api";
import { colorFor, fmtNum, fmtTime, tickFormatter } from "../util";

const axis = { stroke: "var(--faint)", fontSize: 11, tickLine: false, axisLine: false } as const;

function tooltipStyle() {
  return {
    contentStyle: { background: "var(--panel)", border: "1px solid var(--border)", borderRadius: 6, fontSize: 12, color: "var(--text)", boxShadow: "var(--pop-shadow)", padding: "8px 10px" },
    labelStyle: { color: "var(--muted)", marginBottom: 4 },
    itemStyle: { color: "var(--text)", padding: 0 },
    cursor: { fill: "var(--panel-2)" },
  };
}

/** Stacked bars of counts per bucket (log volume by level, errors by service). */
export function StackedBars(props: {
  buckets: Bucket[];
  keys: string[];
  colors?: Record<string, string>;
  bucketMs: number;
  height?: number;
  onBucketClick?: (t: string) => void;
}) {
  const data = props.buckets.map((b) => ({ t: b.t, ...b.counts }));
  return (
    <ResponsiveContainer width="100%" height={props.height ?? 180}>
      <BarChart
        data={data}
        margin={{ top: 4, right: 4, left: -16, bottom: 0 }}
        barCategoryGap={1}
        onClick={(e) => e && e.activeLabel && props.onBucketClick?.(String(e.activeLabel))}
        style={{ cursor: props.onBucketClick ? "pointer" : undefined }}
      >
        <CartesianGrid vertical={false} stroke="var(--border)" />
        <XAxis dataKey="t" tickFormatter={tickFormatter(props.bucketMs)} {...axis} minTickGap={40} />
        <YAxis {...axis} allowDecimals={false} tickFormatter={(v) => fmtNum(v)} width={48} />
        <Tooltip {...tooltipStyle()} labelFormatter={(t) => fmtTime(String(t))} formatter={(v: number, n: string) => [fmtNum(v), n]} />
        {props.keys.map((k) => (
          <Bar key={k} dataKey={k} stackId="a" fill={props.colors?.[k] ?? colorFor(k, props.keys)} stroke="var(--panel)" strokeWidth={1} isAnimationActive={false} />
        ))}
      </BarChart>
    </ResponsiveContainer>
  );
}

/** Line/area chart for metric series. */
export function SeriesChart(props: { series: Series[]; bucketMs: number; unit?: string; height?: number; area?: boolean }) {
  const labels = props.series.map((s) => s.label);
  const rows: Record<string, unknown>[] = [];
  props.series.forEach((s, si) => {
    s.points.forEach((p, i) => {
      rows[i] = rows[i] ?? { t: p.t };
      rows[i][labels[si]] = p.v;
    });
  });
  const fmt = (v: number) => fmtNum(v) + (props.unit && props.unit !== "1" ? " " + props.unit : "");
  const common = {
    data: rows,
    margin: { top: 4, right: 8, left: -8, bottom: 0 },
  };
  const children = [
    <CartesianGrid key="g" vertical={false} stroke="var(--border)" />,
    <XAxis key="x" dataKey="t" tickFormatter={tickFormatter(props.bucketMs)} {...axis} minTickGap={40} />,
    <YAxis key="y" {...axis} tickFormatter={(v) => fmtNum(v)} width={56} />,
    <Tooltip key="tt" {...tooltipStyle()} labelFormatter={(t) => fmtTime(String(t))} formatter={(v: number, n: string) => [fmt(v), n]} />,
  ];
  if (props.area && labels.length === 1) {
    const c = colorFor(labels[0], labels);
    return (
      <ResponsiveContainer width="100%" height={props.height ?? 200}>
        <AreaChart {...common}>
          <defs>
            <linearGradient id={"g-" + labels[0].replace(/\W/g, "")} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={c} stopOpacity={0.18} />
              <stop offset="100%" stopColor={c} stopOpacity={0} />
            </linearGradient>
          </defs>
          {children}
          <Area type="monotone" dataKey={labels[0]} stroke={c} strokeWidth={2} fill={`url(#g-${labels[0].replace(/\W/g, "")})`} connectNulls isAnimationActive={false} />
        </AreaChart>
      </ResponsiveContainer>
    );
  }
  return (
    <ResponsiveContainer width="100%" height={props.height ?? 200}>
      <LineChart {...common}>
        {children}
        {labels.map((l) => (
          <Line key={l} type="monotone" dataKey={l} stroke={colorFor(l, labels)} strokeWidth={2} dot={false} connectNulls isAnimationActive={false} />
        ))}
      </LineChart>
    </ResponsiveContainer>
  );
}

export function Legend({ keys, colors }: { keys: string[]; colors?: Record<string, string> }) {
  return (
    <div className="legend">
      {keys.map((k) => (
        <span key={k}>
          <i style={{ background: colors?.[k] ?? colorFor(k, keys) }} />
          {k || "(none)"}
        </span>
      ))}
    </div>
  );
}

/** Keys present in any bucket, in a stable order. */
export function bucketKeys(buckets: Bucket[], preferred?: string[]): string[] {
  const set = new Set<string>();
  buckets.forEach((b) => Object.keys(b.counts).forEach((k) => set.add(k)));
  if (preferred) return preferred.filter((k) => set.has(k));
  return [...set].sort();
}
