// Writing a log search: what can be typed at the cursor (autocomplete), and
// fields that look mistyped. Pure functions; the search box wires them up.

import { LEVELS } from "./logsearch";

/** The search language's own fields, with what they mean. */
export const BUILTIN_FIELDS: { name: string; detail: string }[] = [
  { name: "service", detail: "the service that sent the line" },
  { name: "level", detail: "debug, info, warn, error, fatal (level>=warn for and above)" },
  { name: "message", detail: "search only the message text" },
  { name: "env", detail: "deployment environment" },
  { name: "kind", detail: "log or span" },
  { name: "has", detail: "lines that have a field, e.g. has:user.id" },
  { name: "trace", detail: "a trace id" },
  { name: "span", detail: "a span id" },
  { name: "severity", detail: "severity number 1–24 (supports > < …)" },
];

const BUILTIN_NAMES = new Set([...BUILTIN_FIELDS.map((f) => f.name), "lvl", "sev", "msg", "body", "exists", "trace_id", "span_id", "fingerprint"]);

// Fields whose values are free text or ids: nothing useful to list.
const NO_VALUES = new Set(["message", "msg", "body", "trace", "trace_id", "span", "span_id", "fingerprint", "severity", "sev"]);

export interface FieldValue {
  value: string;
  count: number;
}

export interface Suggestion {
  /** Replaces the text from Completion.from to Completion.to. */
  insert: string;
  label: string;
  detail?: string;
  kind: "field" | "value" | "keyword" | "recent";
  /** Keep the list open after picking it (a field name, then its values). */
  more?: boolean;
  /** Run the search right away (a recent search). */
  run?: boolean;
}

export interface Completion {
  from: number;
  to: number;
  items: Suggestion[];
  /** Set when values of a field are wanted: the caller fetches them. */
  valuesOf?: { field: string; prefix: string };
}

export interface CompleteCtx {
  fields: string[];
  services: string[];
  recent?: string[];
  values?: { field: string; values: FieldValue[] };
}

const quote = (v: string) => (/^[\w.\-/@:*]+$/.test(v) && !v.startsWith("//") ? v : `"${v.replace(/"/g, '\\"')}"`);

const FIELD_OP = /^(@?[A-Za-z_][\w.\-@]*)(>=|<=|!=|:|=|>|<)([\s\S]*)$/;

/** What could be typed at the cursor. */
export function complete(text: string, cursor: number, ctx: CompleteCtx): Completion {
  // The token under the cursor: from the last space or parenthesis outside quotes.
  let start = 0;
  let inQuote = false;
  for (let i = 0; i < cursor; i++) {
    const ch = text[i];
    if (ch === '"' && text[i - 1] !== "\\") inQuote = !inQuote;
    else if (!inQuote && /[\s()]/.test(ch)) start = i + 1;
  }
  let end = cursor;
  if (!inQuote) while (end < text.length && !/[\s()]/.test(text[end])) end++;
  else {
    const close = text.indexOf('"', cursor);
    end = close < 0 ? text.length : close + 1;
  }
  let tok = text.slice(start, cursor);
  if (tok.startsWith("-")) {
    tok = tok.slice(1);
    start++;
  }
  const none: Completion = { from: start, to: end, items: [] };

  if (!text.trim()) {
    const recent = (ctx.recent ?? []).slice(0, 5).map((r): Suggestion => ({ insert: r, label: r, detail: "recent", kind: "recent", run: true }));
    const fields = BUILTIN_FIELDS.slice(0, 3).map((f): Suggestion => ({ insert: f.name + ":", label: f.name, detail: f.detail, kind: "field", more: true }));
    return { from: 0, to: text.length, items: [...recent, ...fields] };
  }

  const m = FIELD_OP.exec(tok);
  if (m) {
    const [, name, op] = m;
    let raw = m[3];
    let from = start + name.length + op.length;
    // field:>500 is field>500; nothing to suggest for numbers.
    const lead = /^[<>=]+/.exec(raw)?.[0] ?? "";
    raw = raw.slice(lead.length);
    from += lead.length;
    if (raw.startsWith("//")) return none; // a URL
    const prefix = raw.replace(/^"/, "").replace(/"$/, "");
    const lname = name.replace(/^@/, "").toLowerCase();
    const builtin = !name.startsWith("@");
    const pick = (vals: { v: string; detail?: string }[], quoteIt = true): Completion => ({
      from,
      to: end,
      items: vals
        .filter((x) => x.v.toLowerCase().startsWith(prefix.toLowerCase()) && x.v !== prefix)
        .slice(0, 12)
        .map((x) => ({ insert: (quoteIt ? quote(x.v) : x.v) + " ", label: x.v, detail: x.detail, kind: "value" })),
    });
    if (builtin && (lname === "level" || lname === "lvl")) {
      return pick(LEVELS.map((l) => ({ v: l, detail: op === ">=" || lead === ">=" ? `${l} and above` : undefined })));
    }
    if (builtin && lname === "service" && ctx.services.length) return pick(ctx.services.map((v) => ({ v })));
    if (builtin && lname === "kind") return pick([{ v: "log" }, { v: "span" }]);
    if (builtin && (lname === "has" || lname === "exists")) return pick(ctx.fields.map((v) => ({ v })), false);
    if (op === ">" || op === "<" || op === ">=" || op === "<=" || lead) return none;
    if (builtin && NO_VALUES.has(lname)) return none;
    const vals = ctx.values && ctx.values.field === name ? ctx.values.values : [];
    const c = pick(vals.map((v) => ({ v: v.value, detail: v.count ? `${v.count.toLocaleString()} recent lines` : undefined })));
    if (!prefix) c.items.push({ insert: "* ", label: "*", detail: "the field is present", kind: "value" });
    return { ...c, valuesOf: { field: name, prefix } };
  }

  if (inQuote || !/^@?[\w.\-@]*$/.test(tok)) return none;

  const items: Suggestion[] = [];
  if (tok && "OR".startsWith(tok) && text.slice(0, start).trim()) items.push({ insert: "OR ", label: "OR", detail: "either side matches", kind: "keyword" });
  if (tok && "NOT".startsWith(tok)) items.push({ insert: "NOT ", label: "NOT", detail: "exclude what follows (or prefix with -)", kind: "keyword" });
  const t = tok.replace(/^@/, "").toLowerCase();
  const rank = (n: string) => (n.toLowerCase().startsWith(t) ? 0 : t.length > 1 && n.toLowerCase().includes(t) ? 1 : -1);
  const forced = tok.startsWith("@");
  const builtins = forced ? [] : BUILTIN_FIELDS.filter((f) => rank(f.name) >= 0).sort((a, b) => rank(a.name) - rank(b.name));
  const attrs = ctx.fields.filter((f) => rank(f) >= 0).sort((a, b) => rank(a) - rank(b) || a.localeCompare(b));
  for (const f of builtins) items.push({ insert: f.name + ":", label: f.name, detail: f.detail, kind: "field", more: true });
  for (const f of attrs) {
    if (BUILTIN_NAMES.has(f.toLowerCase()) || forced) {
      items.push({ insert: "@" + f + ":", label: "@" + f, detail: "attribute (named like a built-in field)", kind: "field", more: true });
    } else items.push({ insert: f + ":", label: f, detail: "field seen in recent logs", kind: "field", more: true });
  }
  // A word that no field name starts with is most likely text to search for.
  if (tok && !items.some((i) => i.kind === "keyword" || i.label.replace(/^@/, "").toLowerCase().startsWith(t))) return none;
  // Typing a field's full name: offer the next step (its values), not itself.
  if (items.length === 1 && items[0].label === tok) return none;
  return { from: start, to: end, items: items.slice(0, 12) };
}

function editDistance(a: string, b: string): number {
  const d = Array.from({ length: b.length + 1 }, (_, i) => i);
  for (let i = 1; i <= a.length; i++) {
    let prev = d[0];
    d[0] = i;
    for (let j = 1; j <= b.length; j++) {
      const tmp = d[j];
      d[j] = Math.min(d[j] + 1, d[j - 1] + 1, prev + (a[i - 1] === b[j - 1] ? 0 : 1));
      prev = tmp;
    }
  }
  return d[b.length];
}

export interface FieldHint {
  field: string;
  /** A known field it was probably meant to be. */
  suggestion?: string;
}

/**
 * Fields used in a search that no recent line has (most likely a typo, as
 * field names are case-sensitive), each with the closest known one.
 */
export function unknownFields(q: string, fields: string[]): FieldHint[] {
  if (!fields.length) return [];
  const known = new Set(fields);
  const bare = q.replace(/"(?:\\"|[^"])*"?/g, '""');
  const out: FieldHint[] = [];
  const seen = new Set<string>();
  for (const m of bare.matchAll(/(?:^|[\s(-])(@?[A-Za-z_][\w.\-@]*)(?:>=|<=|!=|:|=|>|<)(?!\/\/)(?=\S)/g)) {
    const raw = m[1];
    const name = raw.replace(/^@/, "");
    const forced = raw.startsWith("@");
    if (seen.has(raw) || known.has(name) || (!forced && BUILTIN_NAMES.has(name.toLowerCase()))) continue;
    seen.add(raw);
    const candidates = [...fields, ...(forced ? [] : BUILTIN_FIELDS.map((f) => f.name))];
    let best: string | undefined;
    let bestD = Infinity;
    for (const c of candidates) {
      const dist = c.toLowerCase() === name.toLowerCase() ? 0 : editDistance(name.toLowerCase(), c.toLowerCase());
      if (dist < bestD) [best, bestD] = [c, dist];
    }
    const limit = Math.max(1, Math.min(3, Math.floor(name.length / 3)));
    out.push({ field: raw, suggestion: best !== undefined && bestD <= limit ? (forced ? "@" : "") + best : undefined });
  }
  return out;
}

/** Renames a field wherever q uses it as a field (outside quotes is assumed). */
export function renameField(q: string, from: string, to: string): string {
  const esc = from.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return q.replace(new RegExp(`(^|[\\s(-])${esc}(?=>=|<=|!=|:|=|>|<)`, "g"), `$1${to}`);
}
