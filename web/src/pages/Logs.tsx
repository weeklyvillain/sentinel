import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { api, Bucket, LogRecord, qs, Series } from "../api";
import { bucketKeys, Legend, SeriesChart, StackedBars } from "../components/charts";
import { CopyLink, Empty, ErrorNote, Icon, Modal, MultiSelect, RangePicker, Seg, Switch, useAction, useAsync } from "../components/ui";
import { LogDrawer } from "../components/LogDrawer";
import { newId, Panel } from "../dashboards/model";
import { columnSuggestions, columnValue, messageOf, rememberColumns, rememberedColumns } from "../logjson";
import {
  aggNeedsField,
  filtersToQuery,
  forgetRecent,
  LEVELS,
  levelsFromParams,
  LOG_AGGS,
  recentSearches,
  rememberSearch,
  SEARCH_EXAMPLES,
} from "../logsearch";
import { useServices } from "../shared";
import { fmtFull, fmtNum, fmtTime, LEVEL_COLORS, levelOf } from "../util";
import type { Dashboard } from "./Dashboards";

const PAGE = 100;

interface LogsResp {
  logs: LogRecord[];
  total?: number;
  next?: { before_ts: number; before_id: number };
}

interface SavedSearch {
  id: number;
  name: string;
  params: string;
  created_by?: string;
  uses: number;
}

// Params that describe a search (what a saved search keeps). The time range
// is left out so a saved search runs over the range you are looking at.
const SEARCH_PARAMS = ["q", "level", "service", "attr", "trace_id", "fingerprint", "view", "agg", "field", "by", "col"];

export default function Logs() {
  const [params, setParams] = useSearchParams();
  const run = useAction();
  const q = params.get("q") ?? "";
  const services = params.getAll("service");
  const levels = levelsFromParams(params);
  const range = params.get("range") ?? (params.get("from") ? "" : "1h");
  const from = params.get("from") ?? "";
  const to = params.get("to") ?? "";
  const traceId = params.get("trace_id") ?? "";
  const fingerprint = params.get("fingerprint") ?? "";
  const attrs = params.getAll("attr");
  const view = params.get("view") === "metric" ? "metric" : "volume";
  const agg = params.get("agg") ?? "count";
  const field = params.get("field") ?? "";
  const by = params.get("by") ?? "";

  const [draft, setDraft] = useState(q);
  useEffect(() => setDraft(q), [q]);
  const [live, setLive] = useState(false);
  const [help, setHelp] = useState(false);
  // The open line, by id: in live mode new lines push it down the list.
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const fields = useAsync(() => api.get<Record<string, string>>("/api/logs/message-fields"), []);
  const facets = useAsync(() => api.get<{ attributes: string[] }>("/api/logs/facets"), []);
  const saved = useAsync(() => api.get<SavedSearch[]>("/api/logs/saved"), []);
  const [more, setMore] = useState<LogRecord[]>([]);
  const [next, setNext] = useState<LogsResp["next"]>();
  const [loadingMore, setLoadingMore] = useState(false);
  const [moreError, setMoreError] = useState<string>();

  const filter = { q, service: services, level: levels, range: from ? undefined : range, from, to, trace_id: traceId, fingerprint, attr: attrs };
  const key = qs(filter);

  const svcList = useServices();
  // Live tail pauses while older lines are loaded below, so they never shift.
  const livePoll = live && more.length === 0;
  const logs = useAsync(() => api.get<LogsResp>("/api/logs" + qs({ ...filter, limit: String(PAGE) })), [key, livePoll], livePoll ? 5000 : 0);
  const hist = useAsync(
    () => (view === "volume" ? api.get<{ buckets: Bucket[]; bucket_ms: number }>("/api/logs/histogram" + key) : Promise.resolve(undefined)),
    [key, view, live],
    live ? 10000 : 0,
  );
  const metricKey = qs({ agg, field, by });
  const metric = useAsync(
    () =>
      view === "metric" && (!aggNeedsField(agg) || field)
        ? api.get<{ series: Series[]; bucket_ms: number }>("/api/logs/metric" + qs({ ...filter, agg, field, by }))
        : Promise.resolve(undefined),
    [key, view, metricKey, live],
    live ? 10000 : 0,
  );

  useEffect(() => {
    setMore([]);
    setMoreError(undefined);
    setNext(logs.data?.next);
  }, [logs.data]);

  const update = (patch: Record<string, string | string[] | null>) => {
    const p = new URLSearchParams(params);
    for (const [k, v] of Object.entries(patch)) {
      p.delete(k);
      if (Array.isArray(v)) v.forEach((x) => p.append(k, x));
      else if (v) p.set(k, v);
    }
    setParams(p);
  };

  // --- infinite scroll ---
  const keyRef = useRef(key);
  keyRef.current = key;
  const busy = useRef(false);
  const loadMore = useCallback(async () => {
    if (!next || busy.current) return;
    busy.current = true;
    setLoadingMore(true);
    const forKey = key;
    try {
      const r = await api.get<LogsResp>("/api/logs" + qs({ ...filter, before_ts: String(next.before_ts), before_id: String(next.before_id), limit: String(PAGE) }));
      if (keyRef.current !== forKey) return; // the search changed meanwhile
      setMore((m) => [...m, ...r.logs]);
      setNext(r.next);
      setMoreError(undefined);
    } catch (e) {
      setMoreError((e as Error).message);
    } finally {
      busy.current = false;
      setLoadingMore(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [next, key]);
  const bottom = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = bottom.current;
    if (!el || !next || moreError) return;
    const io = new IntersectionObserver((es) => es[0].isIntersecting && loadMore(), { rootMargin: "600px 0px" });
    io.observe(el);
    return () => io.disconnect();
  }, [next, loadMore, moreError]);

  const rows = useMemo(() => [...(logs.data?.logs ?? []), ...more], [logs.data, more]);
  const addAttr = (k: string, v: string) => update({ attr: [...attrs.filter((a) => !a.startsWith(k + "=")), `${k}=${v}`] });
  const selected = selectedId === null ? -1 : rows.findIndex((r) => r.id === selectedId);
  const open = selected >= 0 ? rows[selected] : undefined;
  const setSelected = (i: number | null) => setSelectedId(i === null ? null : (rows[i]?.id ?? null));
  // A new search or time range closes the drawer.
  useEffect(() => setSelectedId(null), [key]);
  const setField = async (service: string, f: string) => {
    await api.put("/api/logs/message-fields", { service, field: f });
    fields.reload();
  };

  const search = (text: string) => {
    rememberSearch(text);
    update({ q: text });
  };
  const jumpToNewest = () => {
    setMore([]);
    window.scrollTo({ top: 0 });
    logs.reload();
  };

  // --- saved searches ---
  const currentParams = () => {
    const p = new URLSearchParams();
    for (const k of SEARCH_PARAMS) params.getAll(k).forEach((v) => p.append(k, v));
    // Older links filter by min_severity; save the levels it stands for.
    if (!params.has("level")) levels.forEach((l) => p.append("level", l));
    return p.toString();
  };
  const applySaved = (s: SavedSearch) => {
    const p = new URLSearchParams(s.params);
    for (const k of ["range", "from", "to"]) params.getAll(k).forEach((v) => p.append(k, v));
    setParams(p);
    api.post(`/api/logs/saved/${s.id}/use`).then(() => saved.reload(), () => undefined);
  };
  const activeSaved = (saved.data ?? []).find((s) => s.params === currentParams());
  const [saving, setSaving] = useState(false);
  const [toDash, setToDash] = useState(false);

  const svcOptions = (svcList.data ?? []).map((s) => ({ value: s.name }));

  // Extra columns: ?col=… (kept in links and saved searches), else the ones
  // last picked in this browser.
  const [remembered, setRemembered] = useState(rememberedColumns);
  const columns = params.has("col") ? params.getAll("col").filter(Boolean) : remembered;
  const setColumns = (cols: string[]) => {
    rememberColumns(cols);
    setRemembered(cols);
    // An empty ?col= keeps "no columns" from falling back to the remembered ones.
    update({ col: cols.length ? cols : [""] });
  };
  const toggleColumn = (c: string) => setColumns(columns.includes(c) ? columns.filter((x) => x !== c) : [...columns, c]);
  const colOptions = useMemo(() => columnSuggestions(rows).map((value) => ({ value })), [rows]);
  const fieldNames = facets.data?.attributes ?? [];

  return (
    <>
      <div className="page-head">
        <h1>Logs</h1>
        <span className="sub">{logs.data?.total !== undefined ? `${fmtNum(logs.data.total)} matching` : ""}</span>
        <div className="actions">
          <CopyLink pin />
          <label className="check">
            <Switch checked={live} onChange={setLive} label="Live tail" /> Live
          </label>
          <RangePicker value={range} onChange={(r) => update({ range: r, from: null, to: null })} />
        </div>
      </div>

      <div className="card">
        <form
          className="row"
          onSubmit={(e) => {
            e.preventDefault();
            search(draft);
          }}
        >
          <SavedMenu
            saved={saved.data ?? []}
            active={activeSaved}
            onApply={applySaved}
            onRecent={(text) => {
              setDraft(text);
              search(text);
            }}
            onDelete={async (s) => {
              if (!confirm(`Delete the saved search "${s.name}" for everyone?`)) return;
              await run(() => api.del(`/api/logs/saved/${s.id}`), "Saved search deleted");
              saved.reload();
            }}
          />
          <div className="search">
            <Icon.search />
            <input
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder="Search — e.g. level:error service:api timeout -healthcheck"
              aria-label="Search logs"
              spellCheck={false}
            />
          </div>
          <MultiSelect
            label="Level"
            value={levels}
            onChange={(v) => update({ level: v, min_severity: null })}
            options={LEVELS.map((l) => ({ value: l, color: LEVEL_COLORS[l] }))}
          />
          <MultiSelect label="Service" value={services} onChange={(v) => update({ service: v })} options={svcOptions} allLabel="all" empty="No services yet" />
          <MultiSelect label="Columns" value={columns} onChange={setColumns} options={colOptions} allLabel="none" empty="No fields in these lines" freeText />
          <button type="submit" className="primary">
            Search
          </button>
          <button type="button" onClick={() => setSaving(true)} title={activeSaved ? `Saved as "${activeSaved.name}"` : "Save this search"}>
            {activeSaved ? "★ Saved" : "☆ Save"}
          </button>
          <button type="button" className={"ghost small" + (help ? " on" : "")} onClick={() => setHelp(!help)} aria-expanded={help}>
            Syntax
          </button>
        </form>
        <ErrorNote error={logs.error} />
        {help && (
          <SearchHelp
            fields={fieldNames}
            onExample={(text) => {
              setDraft(text);
              search(text);
            }}
            onField={(f) => setDraft((d) => (d.trim() ? d.trim() + " " : "") + f + ":")}
          />
        )}
        {(traceId || fingerprint || attrs.length > 0 || from) && (
          <div className="row" style={{ marginTop: 10 }}>
            <span className="small muted">Filters:</span>
            {from && (
              <span className="chip">
                {fmtTime(new Date(+from).toISOString())} → {to ? fmtTime(new Date(+to).toISOString()) : "now"}
                <button onClick={() => update({ from: null, to: null, range: "1h" })}>×</button>
              </span>
            )}
            {traceId && (
              <span className="chip">
                trace={traceId.slice(0, 12)}…<button onClick={() => update({ trace_id: null })}>×</button>
              </span>
            )}
            {fingerprint && (
              <span className="chip">
                issue group<button onClick={() => update({ fingerprint: null })}>×</button>
              </span>
            )}
            {attrs.map((a) => (
              <span className="chip" key={a}>
                {a}
                <button onClick={() => update({ attr: attrs.filter((x) => x !== a) })}>×</button>
              </span>
            ))}
          </div>
        )}
      </div>

      <div className="card">
        <div className="card-head">
          <Seg
            value={view}
            options={[
              { value: "volume", label: "Volume" },
              { value: "metric", label: "Metric" },
            ]}
            onChange={(v) => update({ view: v === "metric" ? "metric" : null })}
          />
          <div className="right">
            {view === "volume" ? (
              <Legend keys={bucketKeys(hist.data?.buckets ?? [], [...LEVELS])} colors={LEVEL_COLORS} />
            ) : (
              <button className="small" onClick={() => setToDash(true)} disabled={aggNeedsField(agg) && !field}>
                Add to dashboard
              </button>
            )}
          </div>
        </div>
        {view === "volume" ? (
          <>
            {hist.data && (
              <StackedBars
                buckets={hist.data.buckets}
                keys={bucketKeys(hist.data.buckets, [...LEVELS])}
                colors={LEVEL_COLORS}
                bucketMs={hist.data.bucket_ms}
                height={130}
                onBucketClick={(t) => {
                  const s = new Date(t).getTime();
                  update({ from: String(s), to: String(s + hist.data!.bucket_ms), range: null });
                }}
              />
            )}
            <div className="small faint">Click a bar to zoom into that interval. Switch to Metric to chart a statistic of a field.</div>
          </>
        ) : (
          <>
            <MetricControls agg={agg} field={field} by={by} fields={fieldNames} onChange={(x) => update(x)} />
            <ErrorNote error={metric.error} />
            {aggNeedsField(agg) && !field ? (
              <Empty title="Pick a field">A numeric attribute or JSON field, e.g. duration_ms.</Empty>
            ) : metric.data && metric.data.series.length > 0 ? (
              <>
                <SeriesChart series={metric.data.series} bucketMs={metric.data.bucket_ms} height={180} />
                {metric.data.series.length > 1 && (
                  <Legend keys={metric.data.series.map((s) => s.label)} />
                )}
              </>
            ) : (
              metric.data && <Empty title="No data">No matching logs have this field in the range.</Empty>
            )}
          </>
        )}
      </div>

      {live && more.length > 0 && (
        <div className="notice live-paused">
          Live tail is paused while older lines are loaded.{" "}
          <button className="link" onClick={jumpToNewest}>
            Jump to newest
          </button>
        </div>
      )}
      <div className="card flush">
        {rows.length === 0 && !logs.loading ? (
          <Empty title="No matching logs">Try a wider time range or fewer filters.</Empty>
        ) : (
          <table className="logs">
            <thead>
              <tr>
                <th style={{ width: 150 }}>Time</th>
                <th style={{ width: 64 }}>Level</th>
                <th style={{ width: 140 }}>Service</th>
                <th>Message</th>
                {columns.map((c) => (
                  <th key={c} className="log-col">
                    <span className="truncate" title={c}>
                      {c}
                    </span>
                    <button className="ghost small icon" onClick={() => toggleColumn(c)} aria-label={`Remove column ${c}`} title="Remove column">
                      <Icon.x />
                    </button>
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((l, i) => {
                const lvl = levelOf(l.severity);
                const m = messageOf(l.body, fields.data?.[l.service]);
                return (
                  <tr key={l.id} className={"clickable" + (selected === i ? " expanded" : "")} onClick={() => setSelected(i)}>
                    <td className="nowrap mono muted" title={fmtFull(l.time)}>
                      {fmtTime(l.time)}
                    </td>
                    <td>
                      <span className={"lvl " + lvl}>{l.kind === "span" ? "span" : lvl}</span>
                    </td>
                    <td className="truncate" style={{ maxWidth: 140 }}>
                      {l.service}
                    </td>
                    <td className="body">
                      {m.json && (
                        <span className="json-tag" title={m.format === "logfmt" ? "logfmt log line" : "JSON log line"}>
                          {m.format === "logfmt" ? "k=v" : "{}"}
                        </span>
                      )}
                      {m.text.length > 300 ? m.text.slice(0, 300) + "…" : m.text}
                    </td>
                    {columns.map((c) => {
                      const v = columnValue(l, c);
                      return (
                        <td key={c} className="mono log-col-val" title={v}>
                          {v === "" ? <span className="faint">—</span> : v}
                        </td>
                      );
                    })}
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
      <div ref={bottom} className="scroll-end small faint">
        {moreError ? (
          <>
            Couldn't load older lines: {moreError}{" "}
            <button className="link" onClick={() => setMoreError(undefined)}>
              Retry
            </button>
          </>
        ) : next ? (
          loadingMore ? (
            <>
              <span className="spinner" /> Loading older lines…
            </>
          ) : (
            <button className="link" onClick={loadMore}>
              Load older
            </button>
          )
        ) : (
          rows.length > 0 && `End of results · ${fmtNum(rows.length)} lines`
        )}
      </div>
      {open && (
        <LogDrawer
          log={open}
          field={fields.data?.[open.service]}
          onClose={() => setSelected(null)}
          onPrev={selected > 0 ? () => setSelected(selected - 1) : undefined}
          onNext={selected < rows.length - 1 ? () => setSelected(selected + 1) : undefined}
          onSetField={(f) => setField(open.service, f)}
          onFilterAttr={(k, v) => {
            setSelected(null);
            addAttr(k, v);
          }}
          onSearch={(text) => update({ q: `"${text.replace(/"/g, "")}"` })}
          onTrace={() => update({ trace_id: open.trace_id!, range: "7d", from: null, to: null })}
          onSameError={() => update({ fingerprint: open.fingerprint!, range: "7d", from: null, to: null })}
          columns={columns}
          onToggleColumn={toggleColumn}
        />
      )}
      {saving && (
        <SaveSearchModal
          existing={activeSaved}
          onClose={() => setSaving(false)}
          onSave={async (name, overwrite) => {
            const body = { name, params: currentParams() };
            const ok = await run(
              () => (overwrite ? api.put(`/api/logs/saved/${overwrite.id}`, body) : api.post("/api/logs/saved", body)),
              `Saved "${name}"`,
            );
            if (ok) {
              setSaving(false);
              saved.reload();
            }
          }}
        />
      )}
      {toDash && (
        <AddToDashboard
          onClose={() => setToDash(false)}
          panel={() => ({
            id: newId(),
            title: metricTitle(agg, field, by, q),
            type: "timeseries",
            w: 6,
            h: 3,
            queries: [
              {
                id: "A",
                source: "logs",
                q: filtersToQuery({ q, levels, services, attrs, traceId }),
                log_agg: agg as never,
                field: field || undefined,
                split_by: by || undefined,
              },
            ],
            options: { style: agg === "count" ? "bars" : "line", unit: agg === "rate" ? "per_s" : agg === "count" ? "count" : "" },
          })}
        />
      )}
      <p className="small faint">
        Search matches words by prefix in messages and attribute values, plus <code>field:value</code> filters, <code>OR</code>, parentheses and
        comparisons — see <button className="link" onClick={() => setHelp(true)}>Syntax</button>. Groups from <Link to="/issues">Issues</Link> link here.
      </p>
    </>
  );
}

function metricTitle(agg: string, field: string, by: string, q: string): string {
  const a = LOG_AGGS.find((x) => x.value === agg);
  let t = a?.field ? `${a.label} ${field}` : agg === "rate" ? "Log rate" : "Log count";
  if (q) t += ` — ${q.length > 40 ? q.slice(0, 40) + "…" : q}`;
  if (by) t += ` by ${by}`;
  return t;
}

function MetricControls(props: {
  agg: string;
  field: string;
  by: string;
  fields: string[];
  onChange: (x: Record<string, string | null>) => void;
}) {
  const [field, setField] = useState(props.field);
  const [by, setBy] = useState(props.by);
  useEffect(() => setField(props.field), [props.field]);
  useEffect(() => setBy(props.by), [props.by]);
  const needs = aggNeedsField(props.agg);
  return (
    <form
      className="row metric-controls"
      onSubmit={(e) => {
        e.preventDefault();
        props.onChange({ field: field.trim() || null, by: by.trim() || null });
      }}
    >
      <select value={props.agg} onChange={(e) => props.onChange({ agg: e.target.value === "count" ? null : e.target.value })} aria-label="Statistic">
        {LOG_AGGS.map((a) => (
          <option key={a.value} value={a.value}>
            {a.label}
          </option>
        ))}
      </select>
      {needs && (
        <input
          className="mono"
          list="log-fields"
          value={field}
          onChange={(e) => setField(e.target.value)}
          onBlur={() => field !== props.field && props.onChange({ field: field.trim() || null })}
          placeholder="field, e.g. duration_ms"
          aria-label="Field"
          style={{ width: 200 }}
        />
      )}
      <span className="small muted">split by</span>
      <input
        className="mono"
        list="log-split"
        value={by}
        onChange={(e) => setBy(e.target.value)}
        onBlur={() => by !== props.by && props.onChange({ by: by.trim() || null })}
        placeholder="nothing"
        aria-label="Split by"
        style={{ width: 160 }}
      />
      <datalist id="log-fields">
        {props.fields.map((f) => (
          <option key={f} value={f} />
        ))}
      </datalist>
      <datalist id="log-split">
        {["level", "service", "env", ...props.fields].map((f) => (
          <option key={f} value={f} />
        ))}
      </datalist>
      <button type="submit" className="small">
        Apply
      </button>
    </form>
  );
}

function SearchHelp({ fields, onExample, onField }: { fields: string[]; onExample: (q: string) => void; onField: (f: string) => void }) {
  return (
    <div className="search-help">
      <div className="search-help-grid">
        {SEARCH_EXAMPLES.map((e) => (
          <div key={e.q} className="search-help-row">
            <button type="button" className="link mono" onClick={() => onExample(e.q)}>
              {e.q}
            </button>
            <span className="small muted">{e.what}</span>
          </div>
        ))}
      </div>
      <div className="small muted" style={{ marginTop: 8 }}>
        Built-in fields: <code>service</code> <code>level</code> <code>severity</code> <code>env</code> <code>kind</code> <code>trace</code>{" "}
        <code>span</code> <code>message</code>. Anything else is an attribute or a field of a JSON log line (<code>db.table</code> also finds nested{" "}
        <code>{"{db: {table}}"}</code>); prefix with <code>@</code> for an attribute named like a built-in one. Terms are ANDed; <code>OR</code> and{" "}
        <code>NOT</code> are upper case.
      </div>
      {fields.length > 0 && (
        <div className="row" style={{ marginTop: 8, gap: 4 }}>
          <span className="small muted">Fields seen in the last day:</span>
          {fields.slice(0, 60).map((f) => (
            <button key={f} type="button" className="small ghost mono" onClick={() => onField(f)}>
              {f}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function SavedMenu(props: {
  saved: SavedSearch[];
  active?: SavedSearch;
  onApply: (s: SavedSearch) => void;
  onRecent: (q: string) => void;
  onDelete: (s: SavedSearch) => void;
}) {
  const [open, setOpen] = useState(false);
  const [recent, setRecent] = useState<string[]>([]);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    setRecent(recentSearches());
    const close = (e: MouseEvent) => ref.current && !ref.current.contains(e.target as Node) && setOpen(false);
    const key = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", key);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", key);
    };
  }, [open]);
  return (
    <div className="var-pick ms" ref={ref}>
      <button type="button" onClick={() => setOpen(!open)} aria-expanded={open} title="Saved and recent searches">
        <span className="ms-summary">{props.active ? `★ ${props.active.name}` : "Saved"}</span> <span className="faint">▾</span>
      </button>
      {open && (
        <div className="var-menu saved-menu">
          <div className="small muted">Saved searches</div>
          {props.saved.length === 0 && <div className="small faint">None yet — search, then press Save.</div>}
          {props.saved.map((s) => (
            <div key={s.id} className={"ms-row" + (props.active?.id === s.id ? " on" : "")}>
              <button
                type="button"
                className="link saved-name"
                onClick={() => {
                  setOpen(false);
                  props.onApply(s);
                }}
                title={decodeURIComponent(s.params.replace(/\+/g, " "))}
              >
                {s.name}
              </button>
              <span className="faint small" title="Times used">
                {s.uses > 0 ? `${s.uses}×` : ""}
              </span>
              <button type="button" className="ghost small icon" onClick={() => props.onDelete(s)} aria-label={`Delete ${s.name}`}>
                <Icon.x />
              </button>
            </div>
          ))}
          {recent.length > 0 && (
            <>
              <div className="small muted" style={{ marginTop: 6 }}>
                Recent (this browser){" "}
                <button
                  type="button"
                  className="link small"
                  onClick={() => {
                    forgetRecent();
                    setRecent([]);
                  }}
                >
                  clear
                </button>
              </div>
              {recent.map((r) => (
                <button
                  key={r}
                  type="button"
                  className="link mono small saved-name"
                  onClick={() => {
                    setOpen(false);
                    props.onRecent(r);
                  }}
                >
                  {r}
                </button>
              ))}
            </>
          )}
        </div>
      )}
    </div>
  );
}

function SaveSearchModal(props: { existing?: SavedSearch; onClose: () => void; onSave: (name: string, overwrite?: SavedSearch) => void }) {
  const [name, setName] = useState(props.existing?.name ?? "");
  return (
    <Modal
      title={props.existing ? "Update saved search" : "Save search"}
      onClose={props.onClose}
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          {props.existing && <button onClick={() => props.onSave(name.trim())}>Save as new</button>}
          <button className="primary" disabled={!name.trim()} onClick={() => props.onSave(name.trim(), props.existing)}>
            {props.existing ? "Rename" : "Save"}
          </button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (name.trim()) props.onSave(name.trim(), props.existing);
        }}
      >
        <label className="field">
          <span>Name</span>
          <input autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. Checkout errors" maxLength={200} />
        </label>
      </form>
      <p className="small muted">
        Saves the search, levels, services, filters and chart for everyone. The time range is not saved — a saved search runs over the range you are
        viewing. Searches used most are listed first.
      </p>
    </Modal>
  );
}

function AddToDashboard({ onClose, panel }: { onClose: () => void; panel: () => Panel }) {
  const run = useAction();
  const nav = useNavigate();
  const list = useAsync(() => api.get<Dashboard[]>("/api/dashboards"), []);
  const [target, setTarget] = useState<string>("");
  const [title, setTitle] = useState(() => panel().title);
  const [newName, setNewName] = useState("Log metrics");
  useEffect(() => {
    if (!target && list.data) setTarget(list.data[0] ? String(list.data[0].id) : "new");
  }, [list.data, target]);
  const add = async () => {
    const p = { ...panel(), title };
    const done = await run(async () => {
      if (target === "new") {
        const d = await api.post<Dashboard>("/api/dashboards", { name: newName.trim() || "Log metrics", description: "", spec: { panels: [p], range: "6h", refresh: 60 } });
        return d.id;
      }
      const d = await api.get<Dashboard>(`/api/dashboards/${target}`);
      const spec = { ...d.spec, panels: [...(d.spec.panels ?? []), p] };
      await api.put(`/api/dashboards/${d.id}`, { name: d.name, description: d.description, spec });
      return d.id;
    }, "Panel added");
    if (done !== undefined) {
      onClose();
      nav(`/dashboards/${done}`);
    }
  };
  return (
    <Modal
      title="Add to dashboard"
      onClose={onClose}
      footer={
        <>
          <button onClick={onClose}>Cancel</button>
          <button className="primary" onClick={add} disabled={!target || !title.trim()}>
            Add panel
          </button>
        </>
      }
    >
      <label className="field">
        <span>Panel title</span>
        <input value={title} onChange={(e) => setTitle(e.target.value)} />
      </label>
      <label className="field">
        <span>Dashboard</span>
        <select value={target} onChange={(e) => setTarget(e.target.value)}>
          {(list.data ?? []).map((d) => (
            <option key={d.id} value={d.id}>
              {d.name}
            </option>
          ))}
          <option value="new">New dashboard…</option>
        </select>
      </label>
      {target === "new" && (
        <label className="field">
          <span>New dashboard name</span>
          <input value={newName} onChange={(e) => setNewName(e.target.value)} />
        </label>
      )}
      <p className="small muted">
        The panel keeps this search, levels and services as one query, and follows the dashboard's time range. You can also alarm on a log search from{" "}
        <Link to="/alerts/alarms">Alarms</Link>.
      </p>
    </Modal>
  );
}
