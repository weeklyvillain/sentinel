import { useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { api, Bucket, Group, SampleEvent, Service } from "../api";
import { bucketKeys, StackedBars } from "../components/charts";
import { Empty, ErrorNote, Icon, Loading, Seg, StatusBadge, useAsync, useToast } from "../components/ui";
import { ago, fmtFull, fmtNum, LEVEL_COLORS } from "../util";
import { Queue } from "./AI";

type Tab = "active" | "pending" | "open" | "resolved" | "ignored" | "all" | "queue";
const TAB_STATUS: Record<Exclude<Tab, "queue">, string> = { active: "open,pending", pending: "pending", open: "open", resolved: "resolved", ignored: "ignored", all: "" };

export default function Issues() {
  const [params, setParams] = useSearchParams();
  const tab = (params.get("tab") as Tab) ?? "active";
  const queue = tab === "queue";
  const service = params.get("service") ?? "";
  const [text, setText] = useState(params.get("q") ?? "");
  const nav = useNavigate();
  const groups = useAsync(
    () =>
      queue
        ? Promise.resolve([] as Group[])
        : api.get<Group[]>(`/api/groups?status=${TAB_STATUS[tab]}&service=${encodeURIComponent(service)}&q=${encodeURIComponent(params.get("q") ?? "")}`),
    [tab, service, params.get("q")],
    15000,
  );
  const svcs = useAsync(() => api.get<Service[]>("/api/services"), []);
  const set = (k: string, v: string) => {
    const p = new URLSearchParams(params);
    if (v) p.set(k, v);
    else p.delete(k);
    setParams(p);
  };

  return (
    <>
      <div className="page-head">
        <h1>Issues</h1>
        <span className="sub">Deduplicated error groups and the GitHub issues filed for them</span>
      </div>
      <div className="card">
        <div className="row">
          <Seg<Tab>
            value={tab}
            onChange={(v) => set("tab", v)}
            options={[
              { value: "active", label: "Active" },
              { value: "pending", label: "Pending" },
              { value: "open", label: "Open" },
              { value: "resolved", label: "Resolved" },
              { value: "ignored", label: "Ignored" },
              { value: "all", label: "All" },
              { value: "queue", label: "AI queue" },
            ]}
          />
          {!queue && (
            <>
              <select value={service} onChange={(e) => set("service", e.target.value)} aria-label="Service">
                <option value="">All services</option>
                {(svcs.data ?? []).map((s) => (
                  <option key={s.name}>{s.name}</option>
                ))}
              </select>
              <form
                className="search"
                onSubmit={(e) => {
                  e.preventDefault();
                  set("q", text);
                }}
              >
                <Icon.search />
                <input value={text} onChange={(e) => setText(e.target.value)} placeholder="Filter by title or fingerprint" aria-label="Filter issues" />
              </form>
            </>
          )}
        </div>
      </div>
      {queue ? (
        <Queue />
      ) : (
        <GroupTable groups={groups} onOpen={(id) => nav(`/issues/${id}`)} />
      )}
    </>
  );
}

function GroupTable({ groups, onOpen }: { groups: { data?: Group[]; error?: string }; onOpen: (id: number) => void }) {
  return (
    <>
      <ErrorNote error={groups.error} />
      <div className="card flush">
        {!groups.data ? (
          <Loading />
        ) : groups.data.length === 0 ? (
          <Empty title="No issues here">Repeated identical errors become one group; groups are filed after the settle window.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Error</th>
                <th>Service</th>
                <th className="num">Events</th>
                <th>Last seen</th>
                <th>Status</th>
                <th>GitHub</th>
              </tr>
            </thead>
            <tbody>
              {groups.data.map((g) => (
                <tr key={g.id} className="clickable" onClick={() => onOpen(g.id)}>
                  <td style={{ maxWidth: 480 }}>
                    <div className="truncate" title={g.title}>
                      {g.kind === "down" && <span className="badge bad" style={{ marginRight: 6 }}>outage</span>}
                      {g.kind === "span" && <span className="badge" style={{ marginRight: 6 }}>span</span>}
                      {g.title}
                    </div>
                    {g.block_reason && g.status === "pending" && <div className="small" style={{ color: "var(--warn)" }}>⚠ {g.block_reason}</div>}
                  </td>
                  <td className="nowrap">{g.service}</td>
                  <td className="num">{fmtNum(g.count)}</td>
                  <td className="nowrap muted" title={fmtFull(g.last_seen)}>
                    {ago(g.last_seen)}
                  </td>
                  <td>
                    <StatusBadge status={g.status} />
                  </td>
                  <td className="nowrap" onClick={(e) => e.stopPropagation()}>
                    {g.issue_url ? (
                      <a href={g.issue_url} target="_blank" rel="noreferrer">
                        {g.repo ? `${g.repo}#${g.issue_number}` : `#${g.issue_number}`}
                      </a>
                    ) : (
                      <span className="faint">—</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}

interface Detail {
  group: Group;
  samples: { id: number; event: SampleEvent }[];
  histogram: Bucket[];
}

export function IssueDetail() {
  const { id } = useParams();
  const toast = useToast();
  const d = useAsync(() => api.get<Detail>(`/api/groups/${id}`), [id], 20000);
  const [sampleIdx, setSampleIdx] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  if (d.error) return <ErrorNote error={d.error} />;
  if (!d.data) return <Loading />;
  const { group: g, samples, histogram } = d.data;
  const idx = sampleIdx ?? samples.length - 1;
  const s = samples[idx]?.event;

  const setStatus = async (status: "resolved" | "ignored" | "open") => {
    setBusy(true);
    try {
      await api.post(`/api/groups/${g.id}/status`, { status });
      const onGH = g.issue_number ? " (and on GitHub)" : "";
      toast(
        status === "resolved"
          ? `Closed${onGH}. If it happens again, the issue is reopened.`
          : status === "ignored"
            ? `Ignored${onGH ? ", closed as not planned on GitHub" : ""}. Sentinel will stay quiet about it.`
            : `Reopened${onGH}`,
      );
      d.reload();
    } catch (e) {
      toast((e as Error).message, true);
    } finally {
      setBusy(false);
    }
  };
  const span = new Date(histogram[histogram.length - 1]?.t ?? g.last_seen).getTime() - new Date(histogram[0]?.t ?? g.first_seen).getTime();
  const bucketMs = histogram.length > 1 ? span / (histogram.length - 1) : 60000;

  return (
    <>
      <div className="page-head">
        <div>
          <div className="small muted">
            <Link to="/issues">Issues</Link> / {g.service}
          </div>
          <h1 style={{ marginTop: 4 }}>{g.title}</h1>
        </div>
        <div className="actions">
          <StatusBadge status={g.status} />
          {g.issue_url && (
            <a className="btn" href={g.issue_url} target="_blank" rel="noreferrer">
              <Icon.github /> {g.repo}#{g.issue_number}
            </a>
          )}
          <Link className="btn" to={`/logs?fingerprint=${g.fingerprint}&range=7d`}>
            <Icon.logs /> View logs
          </Link>
          {(g.status === "open" || g.status === "pending") && (
            <button disabled={busy} onClick={() => setStatus("resolved")} title={g.issue_number ? "Close the GitHub issue as completed" : "Mark as resolved without filing an issue"}>
              {g.issue_number ? "Close issue" : "Resolve"}
            </button>
          )}
          {g.status === "resolved" && (
            <button disabled={busy} onClick={() => setStatus("open")} title={g.issue_number ? "Reopen the GitHub issue" : ""}>
              Reopen
            </button>
          )}
          {g.status === "ignored" ? (
            <button disabled={busy} onClick={() => setStatus("open")}>
              Un-ignore
            </button>
          ) : (
            <button className="danger" disabled={busy} onClick={() => setStatus("ignored")} title={g.issue_number ? "Close the GitHub issue as not planned and stay quiet" : "Stay quiet about this group"}>
              Ignore
            </button>
          )}
        </div>
      </div>
      {g.block_reason && g.status === "pending" && (
        <div className="notice warn" style={{ marginBottom: 16 }}>
          Not filed yet: {g.block_reason}. <Link to="/settings/services">Configure a repository</Link>.
        </div>
      )}
      <div className="grid stats" style={{ marginBottom: 16 }}>
        <div className="card stat">
          <div className="label">Occurrences</div>
          <div className="value">{fmtNum(g.count)}</div>
          <div className="hint">{fmtNum(g.count - g.count_at_last_report)} since last report</div>
        </div>
        <div className="card stat">
          <div className="label">First seen</div>
          <div className="value" style={{ fontSize: 16 }}>{ago(g.first_seen)}</div>
          <div className="hint">{fmtFull(g.first_seen)}</div>
        </div>
        <div className="card stat">
          <div className="label">Last seen</div>
          <div className="value" style={{ fontSize: 16 }}>{ago(g.last_seen)}</div>
          <div className="hint">{fmtFull(g.last_seen)}</div>
        </div>
        <div className="card stat">
          <div className="label">Where</div>
          <div className="value" style={{ fontSize: 16 }}>{g.service}</div>
          <div className="hint">
            {g.env || "no env"} · {g.kind}
          </div>
        </div>
      </div>
      <div className="card">
        <div className="card-head">
          <h2>Occurrences over time</h2>
        </div>
        <StackedBars buckets={histogram} keys={bucketKeys(histogram, ["debug", "info", "warn", "error", "fatal"])} colors={LEVEL_COLORS} bucketMs={bucketMs} height={140} />
      </div>
      <div className="card">
        <div className="card-head">
          <h2>Samples</h2>
          <div className="right">
            <select value={idx} onChange={(e) => setSampleIdx(+e.target.value)} aria-label="Sample">
              {samples.map((sm, i) => (
                <option key={sm.id} value={i}>
                  {i === 0 ? "First" : i === samples.length - 1 ? "Latest" : `#${i + 1}`} — {fmtFull(sm.event.time)}
                </option>
              ))}
            </select>
          </div>
        </div>
        {s ? (
          <div className="stack">
            <div className="row small">
              {s.severity && <span className="badge">{s.severity}</span>}
              {s.span_name && <span className="badge">span {s.span_name}</span>}
              {s.trace_id && (
                <Link to={`/logs?trace_id=${s.trace_id}&range=7d`} className="mono">
                  trace {s.trace_id}
                </Link>
              )}
            </div>
            {s.message && <pre className="code">{s.message}</pre>}
            {(s.exception_type || s.exception_message) && (
              <pre className="code">
                <strong>{s.exception_type}</strong>
                {s.exception_message ? ": " + s.exception_message : ""}
              </pre>
            )}
            {s.stacktrace && <pre className="code">{s.stacktrace}</pre>}
            {s.attributes && Object.keys(s.attributes).length > 0 && (
              <details>
                <summary className="small muted">Attributes ({Object.keys(s.attributes).length})</summary>
                <div className="kv" style={{ marginTop: 8 }}>
                  {Object.entries(s.attributes)
                    .sort(([a], [b]) => a.localeCompare(b))
                    .map(([k, v]) => (
                      <span key={k} style={{ display: "contents" }}>
                        <span className="k">{k}</span>
                        <span className="v">{v}</span>
                        <span />
                      </span>
                    ))}
                </div>
              </details>
            )}
          </div>
        ) : (
          <Empty title="No samples" />
        )}
        <div className="small faint" style={{ marginTop: 10 }}>
          Fingerprint <code>{g.fingerprint}</code>
        </div>
      </div>
    </>
  );
}
