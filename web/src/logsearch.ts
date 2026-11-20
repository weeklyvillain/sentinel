// Helpers for the Logs page search: levels, recent searches, and turning the
// page's filters into one query in the search language (for dashboards).

export const LEVELS = ["debug", "info", "warn", "error", "fatal"] as const;

// The highest severity number of each level.
const LEVEL_MAX: Record<string, number> = { debug: 8, info: 12, warn: 16, error: 20, fatal: 24 };

/** The checked levels: ?level=…, or the levels at or above ?min_severity (older links). */
export function levelsFromParams(params: URLSearchParams): string[] {
  const lv = params.getAll("level").flatMap((l) => l.split(",")).filter(Boolean);
  if (lv.length) return lv;
  const min = Number(params.get("min_severity") ?? "");
  if (!min) return [];
  return LEVELS.filter((l) => LEVEL_MAX[l] >= min);
}

export const LOG_AGGS: { value: string; label: string; field: boolean }[] = [
  { value: "count", label: "Count", field: false },
  { value: "rate", label: "Rate (per second)", field: false },
  { value: "unique", label: "Unique values of", field: true },
  { value: "sum", label: "Sum of", field: true },
  { value: "avg", label: "Average of", field: true },
  { value: "min", label: "Min of", field: true },
  { value: "max", label: "Max of", field: true },
  { value: "p50", label: "p50 of", field: true },
  { value: "p90", label: "p90 of", field: true },
  { value: "p95", label: "p95 of", field: true },
  { value: "p99", label: "p99 of", field: true },
];

export const aggNeedsField = (agg: string) => LOG_AGGS.find((a) => a.value === agg)?.field ?? false;

const quote = (v: string) => (/^[\w.\-/@:*]+$/.test(v) && !v.startsWith("//") ? v : `"${v.replace(/"/g, '\\"')}"`);
const anyOf = (field: string, vals: string[]) =>
  vals.length === 1 ? `${field}:${quote(vals[0])}` : `(${vals.map((v) => `${field}:${quote(v)}`).join(" OR ")})`;

/** All of the page's filters as one search, e.g. for a dashboard panel or alarm. */
export function filtersToQuery(f: { q: string; levels: string[]; services: string[]; attrs: string[]; traceId?: string }): string {
  const parts: string[] = [];
  if (f.services.length) parts.push(anyOf("service", f.services));
  if (f.levels.length) parts.push(anyOf("level", f.levels));
  for (const a of f.attrs) {
    const i = a.indexOf("=");
    if (i > 0) parts.push(`@${a.slice(0, i)}:${quote(a.slice(i + 1))}`);
  }
  if (f.traceId) parts.push(`trace:${f.traceId}`);
  if (f.q.trim()) parts.push(f.q.includes(" OR ") ? `(${f.q.trim()})` : f.q.trim());
  return parts.join(" ");
}

// --- recent searches (this browser only) ---

const RECENT_KEY = "sentinel.logs.recent";

export function recentSearches(): string[] {
  try {
    const v = JSON.parse(localStorage.getItem(RECENT_KEY) ?? "[]");
    return Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
  } catch {
    return [];
  }
}

export function rememberSearch(q: string) {
  q = q.trim();
  if (!q) return;
  try {
    localStorage.setItem(RECENT_KEY, JSON.stringify([q, ...recentSearches().filter((x) => x !== q)].slice(0, 8)));
  } catch {
    // storage unavailable: nothing to remember
  }
}

export function forgetRecent() {
  try {
    localStorage.removeItem(RECENT_KEY);
  } catch {
    // ignore
  }
}

/** Examples for the syntax help; clicking one runs it. */
export const SEARCH_EXAMPLES: { q: string; what: string }[] = [
  { q: `timeout "connection refused" -healthcheck`, what: "words (prefix), exact phrases, exclusions" },
  { q: `level:error service:api`, what: "field filters" },
  { q: `level>=warn`, what: "this level and above" },
  { q: `(timeout OR refused) -service:web`, what: "OR, parentheses and NOT" },
  { q: `duration_ms>500`, what: "numeric comparison (> >= < <=)" },
  { q: `http.route:/orders/*`, what: "* wildcard, case-insensitive" },
  { q: `user.id:*`, what: "the field is present (also has:user.id)" },
  { q: `status!=200`, what: "not equal (missing counts as not equal)" },
  { q: `message:"slow query"`, what: "search only the message, not attributes" },
];
