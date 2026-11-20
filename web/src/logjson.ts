// JSON log lines: a body that is a JSON object is shown by one of its fields
// (the service's chosen message field, or the usual names), and its fields
// are listed in the log drawer.

export type JSONObject = Record<string, unknown>;

// DEFAULT_MESSAGE_FIELDS are tried in order when a service has no field set.
export const DEFAULT_MESSAGE_FIELDS = ["message", "msg", "@message", "log", "event", "text", "error.message", "err"];

const cache = new Map<string, JSONObject | null>();

// parseJSONLog returns the body as an object, or null when it is not one.
export function parseJSONLog(body: string): JSONObject | null {
  const t = body.trim();
  if (t.length < 2 || t[0] !== "{" || t[t.length - 1] !== "}") return null;
  const hit = cache.get(body);
  if (hit !== undefined) return hit;
  let out: JSONObject | null = null;
  try {
    const v = JSON.parse(t);
    if (v && typeof v === "object" && !Array.isArray(v)) out = v as JSONObject;
  } catch {
    out = null;
  }
  if (cache.size > 5000) cache.clear();
  cache.set(body, out);
  return out;
}

// getPath reads a dot path ("error.message"); a key that itself contains
// dots ("http.status") is found too.
export function getPath(obj: JSONObject, path: string): unknown {
  if (path in obj) return obj[path];
  const parts = path.split(".");
  let cur: unknown = obj;
  for (let i = 0; i < parts.length; i++) {
    if (!cur || typeof cur !== "object") return undefined;
    const rec = cur as JSONObject;
    // Longest key first: {"http.status": 500} under "req".
    let found = false;
    for (let j = parts.length; j > i; j--) {
      const k = parts.slice(i, j).join(".");
      if (k in rec) {
        cur = rec[k];
        i = j - 1;
        found = true;
        break;
      }
    }
    if (!found) return undefined;
  }
  return cur;
}

export function valueText(v: unknown): string {
  if (v === null) return "null";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return JSON.stringify(v);
}

// messageOf is what the log list shows for a line: for JSON, the chosen
// field, else the first usual field, else the whole object; plain text as is.
export function messageOf(body: string, field?: string): { text: string; json: JSONObject | null; field?: string } {
  const json = parseJSONLog(body);
  if (!json) return { text: body, json: null };
  if (field) {
    const v = getPath(json, field);
    if (v !== undefined && v !== null && v !== "") return { text: valueText(v), json, field };
  } else {
    // Automatically, only plain values count: {"event": {...}} is not a message.
    for (const f of DEFAULT_MESSAGE_FIELDS) {
      const v = getPath(json, f);
      if (typeof v === "string" ? v !== "" : typeof v === "number" || typeof v === "boolean") return { text: valueText(v), json, field: f };
    }
  }
  return { text: JSON.stringify(json), json };
}

// flatten lists every leaf as a dot path, in the object's order. Arrays of
// plain values stay one field; arrays of objects are indexed.
export function flatten(obj: JSONObject, prefix = "", out: [string, unknown][] = []): [string, unknown][] {
  for (const [k, v] of Object.entries(obj)) {
    const path = prefix ? `${prefix}.${k}` : k;
    if (v && typeof v === "object" && !Array.isArray(v) && Object.keys(v).length > 0) {
      flatten(v as JSONObject, path, out);
    } else if (Array.isArray(v) && v.some((x) => x && typeof x === "object")) {
      v.forEach((x, i) => {
        if (x && typeof x === "object" && !Array.isArray(x)) flatten(x as JSONObject, `${path}.${i}`, out);
        else out.push([`${path}.${i}`, x]);
      });
    } else {
      out.push([path, v]);
    }
  }
  return out;
}
