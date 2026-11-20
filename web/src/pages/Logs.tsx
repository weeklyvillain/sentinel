import { useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { api, Bucket, LogRecord, qs } from "../api";
import { bucketKeys, Legend, StackedBars } from "../components/charts";
import { CopyLink, Empty, ErrorNote, Icon, RangePicker, Switch, useAsync } from "../components/ui";
import { LogDrawer } from "../components/LogDrawer";
import { messageOf } from "../logjson";
import { useServices } from "../shared";
import { fmtFull, fmtNum, fmtTime, LEVEL_COLORS, levelOf } from "../util";

const LEVELS = ["debug", "info", "warn", "error", "fatal"];
const SEVERITIES = [
  { v: "", label: "All levels" },
  { v: "5", label: "Debug+" },
  { v: "9", label: "Info+" },
  { v: "13", label: "Warn+" },
  { v: "17", label: "Error+" },
  { v: "21", label: "Fatal" },
];

interface LogsResp {
  logs: LogRecord[];
  total?: number;
  next?: { before_ts: number; before_id: number };
}

export default function Logs() {
  const [params, setParams] = useSearchParams();
  const q = params.get("q") ?? "";
  const services = params.getAll("service");
  const minSev = params.get("min_severity") ?? "";
  const range = params.get("range") ?? (params.get("from") ? "" : "1h");
  const from = params.get("from") ?? "";
  const to = params.get("to") ?? "";
  const traceId = params.get("trace_id") ?? "";
  const fingerprint = params.get("fingerprint") ?? "";
  const attrs = params.getAll("attr");

  const [draft, setDraft] = useState(q);
  useEffect(() => setDraft(q), [q]);
  const [live, setLive] = useState(false);
  // The open line, by id: in live mode new lines push it down the list.
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const fields = useAsync(() => api.get<Record<string, string>>("/api/logs/message-fields"), []);
  const [more, setMore] = useState<LogRecord[]>([]);
  const [next, setNext] = useState<LogsResp["next"]>();
  const [loadingMore, setLoadingMore] = useState(false);

  const filter = { q, service: services, min_severity: minSev, range: from ? undefined : range, from, to, trace_id: traceId, fingerprint, attr: attrs };
  const key = qs(filter);

  const svcList = useServices();
  const logs = useAsync(() => api.get<LogsResp>("/api/logs" + qs({ ...filter, limit: "100" })), [key], live ? 5000 : 0);
  const hist = useAsync(() => api.get<{ buckets: Bucket[]; bucket_ms: number }>("/api/logs/histogram" + key), [key], live ? 10000 : 0);

  useEffect(() => {
    setMore([]);
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

  const loadMore = async () => {
    if (!next) return;
    setLoadingMore(true);
    try {
      const r = await api.get<LogsResp>("/api/logs" + qs({ ...filter, before_ts: String(next.before_ts), before_id: String(next.before_id), limit: "100" }));
      setMore((m) => [...m, ...r.logs]);
      setNext(r.next);
    } finally {
      setLoadingMore(false);
    }
  };

  const rows = useMemo(() => [...(logs.data?.logs ?? []), ...more], [logs.data, more]);
  const toggleService = (name: string) =>
    update({ service: services.includes(name) ? services.filter((s) => s !== name) : [...services, name] });
  const addAttr = (k: string, v: string) => update({ attr: [...attrs.filter((a) => !a.startsWith(k + "=")), `${k}=${v}`] });
  const selected = selectedId === null ? -1 : rows.findIndex((r) => r.id === selectedId);
  const open = selected >= 0 ? rows[selected] : undefined;
  const setSelected = (i: number | null) => setSelectedId(i === null ? null : (rows[i]?.id ?? null));
  // A new search or time range closes the drawer.
  useEffect(() => setSelectedId(null), [key]);
  const setField = async (service: string, field: string) => {
    await api.put("/api/logs/message-fields", { service, field });
    fields.reload();
  };

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
            update({ q: draft });
          }}
        >
          <div className="search">
            <Icon.search />
            <input
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder='Search text and attributes — e.g. timeout "connection refused" -healthcheck'
              aria-label="Search logs"
            />
          </div>
          <select value={minSev} onChange={(e) => update({ min_severity: e.target.value })} aria-label="Minimum level">
            {SEVERITIES.map((s) => (
              <option key={s.v} value={s.v}>
                {s.label}
              </option>
            ))}
          </select>
          <button type="submit" className="primary">
            Search
          </button>
        </form>
        <div className="row" style={{ marginTop: 10 }}>
          <span className="small muted">Services:</span>
          {(svcList.data ?? []).map((s) => (
            <button key={s.name} className={"small" + (services.includes(s.name) ? " primary" : "")} onClick={() => toggleService(s.name)}>
              {s.name}
            </button>
          ))}
          {(svcList.data ?? []).length === 0 && <span className="small faint">none yet</span>}
        </div>
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
          <h2>Volume</h2>
          <div className="right">
            <Legend keys={bucketKeys(hist.data?.buckets ?? [], LEVELS)} colors={LEVEL_COLORS} />
          </div>
        </div>
        {hist.data && (
          <StackedBars
            buckets={hist.data.buckets}
            keys={bucketKeys(hist.data.buckets, LEVELS)}
            colors={LEVEL_COLORS}
            bucketMs={hist.data.bucket_ms}
            height={130}
            onBucketClick={(t) => {
              const s = new Date(t).getTime();
              update({ from: String(s), to: String(s + hist.data!.bucket_ms), range: null });
            }}
          />
        )}
        <div className="small faint">Click a bar to zoom into that interval.</div>
      </div>

      <ErrorNote error={logs.error} />
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
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
      {next && (
        <div style={{ textAlign: "center", marginTop: 12 }}>
          <button onClick={loadMore} disabled={loadingMore}>
            {loadingMore ? "Loading…" : "Load older"}
          </button>
        </div>
      )}
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
        />
      )}
      <p className="small faint">
        Search matches words by prefix in messages and attribute values. Use <code>"exact phrase"</code> and <code>-exclude</code>. Groups from{" "}
        <Link to="/issues">Issues</Link> link here.
      </p>
    </>
  );
}
