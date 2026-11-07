export const RANGES = [
  { value: "15m", label: "15m" },
  { value: "1h", label: "1h" },
  { value: "6h", label: "6h" },
  { value: "24h", label: "24h" },
  { value: "7d", label: "7d" },
];

export function levelOf(sev: number): "debug" | "info" | "warn" | "error" | "fatal" {
  if (sev >= 21) return "fatal";
  if (sev >= 17) return "error";
  if (sev >= 13) return "warn";
  if (sev >= 9 || sev === 0) return "info";
  return "debug";
}

export const LEVEL_COLORS: Record<string, string> = {
  debug: "#9aa3b2",
  info: "#6b74e8",
  warn: "#e0a030",
  error: "#e05252",
  fatal: "#9b5de5",
};

// Categorical palette for services / series.
export const PALETTE = ["#6366f1", "#14b8a6", "#f59e0b", "#ef4444", "#8b5cf6", "#06b6d4", "#84cc16", "#ec4899", "#f97316", "#64748b"];

export function colorFor(key: string, keys: string[]): string {
  const i = keys.indexOf(key);
  return PALETTE[(i < 0 ? hash(key) : i) % PALETTE.length];
}

function hash(s: string): number {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0;
  return Math.abs(h);
}

const zeroTime = (s?: string) => !s || s.startsWith("0001-");

export function fmtTime(s?: string, withSeconds = true): string {
  if (zeroTime(s)) return "—";
  const d = new Date(s!);
  const today = new Date();
  const sameDay = d.toDateString() === today.toDateString();
  const time = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: withSeconds ? "2-digit" : undefined });
  return sameDay ? time : d.toLocaleDateString([], { month: "short", day: "numeric" }) + " " + time;
}

export function fmtFull(s?: string): string {
  if (zeroTime(s)) return "—";
  return new Date(s!).toLocaleString();
}

export function ago(s?: string): string {
  if (zeroTime(s)) return "never";
  const sec = Math.round((Date.now() - new Date(s!).getTime()) / 1000);
  if (sec < 5) return "just now";
  if (sec < 60) return `${sec}s ago`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ago`;
  if (sec < 86400) return `${Math.floor(sec / 3600)}h ago`;
  return `${Math.floor(sec / 86400)}d ago`;
}

export function fmtNum(n: number | null | undefined, digits = 3): string {
  if (n === null || n === undefined || Number.isNaN(n)) return "—";
  const a = Math.abs(n);
  if (a >= 1e9) return (n / 1e9).toFixed(1) + "B";
  if (a >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (a >= 1e4) return (n / 1e3).toFixed(1) + "k";
  if (Number.isInteger(n)) return n.toLocaleString();
  return Number(n.toPrecision(digits)).toLocaleString();
}

export function fmtDuration(s: number): string {
  if (s % 86400 === 0) return `${s / 86400}d`;
  if (s % 3600 === 0) return `${s / 3600}h`;
  if (s % 60 === 0) return `${s / 60}m`;
  return `${s}s`;
}

export function tickFormatter(bucketMs: number) {
  return (t: string | number) => {
    const d = new Date(t);
    if (bucketMs >= 3600_000 * 3) return d.toLocaleDateString([], { month: "short", day: "numeric" }) + " " + d.getHours() + "h";
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  };
}

export function serviceUp(s: { last_seen: string; down: boolean }): "up" | "down" | "never" {
  if (zeroTime(s.last_seen)) return "never";
  return s.down ? "down" : "up";
}
