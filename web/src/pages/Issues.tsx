import { useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Alias, api, Bucket, Group, LogRecord, qs, Release, SampleEvent, TagSummary } from "../api";
import { bucketKeys, StackedBars } from "../components/charts";
import { CopyLink, Empty, ErrorNote, Icon, Loading, Modal, Seg, StatusBadge, useAsync, useAction } from "../components/ui";
import { useServices } from "../shared";
import { ago, fmtFull, fmtNum, fmtTime, LEVEL_COLORS, levelOf } from "../util";
import { Queue } from "./AI";
import { FixerRuns } from "./FixerRuns";

type Tab = "active" | "pending" | "open" | "resolved" | "ignored" | "all" | "releases" | "queue" | "fixer";
const TAB_STATUS: Record<Exclude<Tab, "queue" | "releases" | "fixer">, string> = { active: "open,pending", pending: "pending", open: "open", resolved: "resolved", ignored: "ignored", all: "" };

const SORTS = [
  { value: "", label: "Last seen" },
  { value: "first_seen", label: "First seen" },
  { value: "count", label: "Events" },
  { value: "users", label: "Users" },
];

export default function Issues() {
  const [params, setParams] = useSearchParams();
  const tab = (params.get("tab") as Tab) ?? "active";
  const listTab = tab !== "queue" && tab !== "releases" && tab !== "fixer";
  const service = params.get("service") ?? "";
  const env = params.get("env") ?? "";
  const release = params.get("release") ?? "";
  const isNew = params.get("new") === "1";
  const sort = params.get("sort") ?? "";
  const [text, setText] = useState(params.get("q") ?? "");
  const nav = useNavigate();
  const groups = useAsync(
    () =>
      listTab
        ? api.get<Group[]>(
            `/api/groups${qs({
              status: TAB_STATUS[tab as keyof typeof TAB_STATUS],
              service,
              env,
              q: params.get("q") ?? "",
              sort,
              release: isNew ? "" : release,
              first_release: isNew ? release : "",
            })}`,
          )
        : Promise.resolve([] as Group[]),
    [tab, service, env, release, isNew, sort, params.get("q")],
    15000,
  );
  const envs = useAsync(() => api.get<string[]>("/api/environments"), []);
  const releases = useAsync(() => api.get<Release[]>(`/api/releases${qs({ service })}`), [service]);
  const svcs = useServices();
  const set = (k: string, v: string) => {
    const p = new URLSearchParams(params);
    if (v) p.set(k, v);
    else p.delete(k);
    setParams(p);
  };
  const versions = [...new Set((releases.data ?? []).map((r) => r.version))];

  return (
    <>
      <div className="page-head">
        <h1>Issues</h1>
        <span className="sub">Deduplicated error groups and the GitHub issues filed for them</span>
        <div className="actions">
          <CopyLink />
        </div>
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
              { value: "releases", label: "Releases" },
              { value: "queue", label: "AI queue" },
              { value: "fixer", label: "Fixer" },
            ]}
          />
          {tab !== "queue" && tab !== "fixer" && (
            <select value={service} onChange={(e) => set("service", e.target.value)} aria-label="Service">
              <option value="">All services</option>
              {(svcs.data ?? []).map((s) => (
                <option key={s.name}>{s.name}</option>
              ))}
            </select>
          )}
          {listTab && (
            <>
              <select value={env} onChange={(e) => set("env", e.target.value)} aria-label="Environment">
                <option value="">All environments</option>
                {(envs.data ?? []).map((e) => (
                  <option key={e}>{e}</option>
                ))}
              </select>
              <select value={release} onChange={(e) => set("release", e.target.value)} aria-label="Release">
                <option value="">All releases</option>
                {versions.map((v) => (
                  <option key={v}>{v}</option>
                ))}
              </select>
              {release && (
                <label className="small" title="Only errors that first appeared in this release">
                  <input type="checkbox" checked={isNew} onChange={(e) => set("new", e.target.checked ? "1" : "")} /> new in {release}
                </label>
              )}
              <select value={sort} onChange={(e) => set("sort", e.target.value)} aria-label="Sort by">
                {SORTS.map((s) => (
                  <option key={s.value} value={s.value}>
                    Sort: {s.label}
                  </option>
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
      {tab === "queue" ? (
        <Queue />
      ) : tab === "fixer" ? (
        <FixerRuns />
      ) : tab === "releases" ? (
        <Releases releases={releases} onOpen={(r) => setParams(new URLSearchParams({ tab: "all", service: r.service, release: r.version, new: "1" }))} />
      ) : (
        <GroupTable groups={groups} onOpen={(id) => nav(`/issues/${id}`)} onChanged={groups.reload} />
      )}
    </>
  );
}

function Releases({ releases, onOpen }: { releases: { data?: Release[]; error?: string }; onOpen: (r: Release) => void }) {
  return (
    <>
      <ErrorNote error={releases.error} />
      <div className="card flush">
        {!releases.data ? (
          <Loading />
        ) : releases.data.length === 0 ? (
          <Empty title="No releases yet">
            Set the resource attribute <code>service.version</code> in your services; every version they send from shows up here with the errors it introduced.
          </Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Release</th>
                <th>Service</th>
                <th>Environment</th>
                <th>First seen</th>
                <th>Last seen</th>
                <th className="num">New issues</th>
                <th className="num">Regressions</th>
                <th className="num">Error events</th>
              </tr>
            </thead>
            <tbody>
              {releases.data.map((r) => (
                <tr key={r.service + r.version} className="clickable" onClick={() => onOpen(r)} title="Issues first seen in this release">
                  <td className="mono">{r.version}</td>
                  <td>{r.service}</td>
                  <td className="muted">{r.env || "—"}</td>
                  <td className="nowrap" title={fmtFull(r.first_seen)}>
                    {fmtTime(r.first_seen, false)}
                  </td>
                  <td className="nowrap muted">{ago(r.last_seen)}</td>
                  <td className="num">{r.new_issues ? <span className="badge bad">{r.new_issues}</span> : "0"}</td>
                  <td className="num">{r.regressions ? <span className="badge warn">{r.regressions}</span> : "0"}</td>
                  <td className="num">{fmtNum(r.events)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}

function KindBadge({ g }: { g: Group }) {
  if (g.kind === "down") return <span className="badge bad" style={{ marginRight: 6 }}>outage</span>;
  if (g.kind === "perf") return <span className="badge warn" style={{ marginRight: 6 }}>performance</span>;
  if (g.kind === "span") return <span className="badge" style={{ marginRight: 6 }}>span</span>;
  return null;
}

const FIX: Record<NonNullable<Group["fix_state"]>, { cls: string; label: string; help: string }> = {
  queued: { cls: "accent", label: "fix queued", help: "Waiting for Sentinel's fixer to pick it up" },
  running: { cls: "accent", label: "fixing", help: "Sentinel's fixer is running Claude Code on the repository" },
  no_fix: { cls: "warn", label: "no fix found", help: "Claude Code looked into it but found no fix it was confident about" },
  failed: { cls: "bad", label: "fix failed", help: "The fixer could not finish (clone, Claude Code or push failed)" },
  requested: { cls: "accent", label: "fix requested", help: "The fix workflow was started; waiting for its pull request" },
  pr_open: { cls: "accent", label: "fix PR open", help: "A pull request that fixes this is waiting for review" },
  merged: { cls: "ok", label: "fix merged", help: "The fix was merged" },
  pr_closed: { cls: "", label: "fix PR closed", help: "The fix pull request was closed without merging" },
  no_pr: { cls: "warn", label: "no fix PR", help: "No pull request appeared: the workflow is missing, failed, or found no confident fix (see the issue's comments)" },
};

const VERDICT: Record<NonNullable<Group["triage_verdict"]>, { cls: string; label: string }> = {
  fix: { cls: "bad", label: "worth fixing" },
  investigate: { cls: "warn", label: "needs a person" },
  ignore: { cls: "", label: "noise" },
};

function TriageBadge({ g }: { g: Group }) {
  if (!g.triage_verdict) return null;
  const v = VERDICT[g.triage_verdict];
  return (
    <span className={"badge " + v.cls} title={g.triage_reason}>
      {v.label}
      {g.triage_severity ? ` · ${g.triage_severity}` : ""}
    </span>
  );
}

function FixBadge({ g }: { g: Group }) {
  if (!g.fix_state) return null;
  const f = FIX[g.fix_state];
  const badge = (
    <span className={"badge " + f.cls} title={f.help}>
      🤖 {f.label}
      {g.fix_pr_number ? ` #${g.fix_pr_number}` : ""}
    </span>
  );
  return g.fix_pr_url ? (
    <a href={g.fix_pr_url} target="_blank" rel="noreferrer" onClick={(e) => e.stopPropagation()}>
      {badge}
    </a>
  ) : (
    badge
  );
}

function snoozeText(g: Group): string {
  const parts: string[] = [];
  if (g.snooze_until) parts.push("until " + fmtTime(g.snooze_until, false));
  if (g.snooze_count) parts.push(`until ${fmtNum(g.snooze_count)} events`);
  if (g.snooze_release) parts.push("until a new release");
  return parts.length ? "snoozed " + parts.join(" or ") : "";
}

function GroupTable({ groups, onOpen, onChanged }: { groups: { data?: Group[]; error?: string }; onOpen: (id: number) => void; onChanged: () => void }) {
  const [sel, setSel] = useState<number[]>([]);
  const [merging, setMerging] = useState(false);
  const toggle = (id: number) => setSel((s) => (s.includes(id) ? s.filter((x) => x !== id) : [...s, id]));
  const selected = (groups.data ?? []).filter((g) => sel.includes(g.id));
  return (
    <>
      <ErrorNote error={groups.error} />
      {sel.length > 0 && (
        <div className="notice" style={{ marginBottom: 12, display: "flex", gap: 10, alignItems: "center" }}>
          {sel.length} selected
          <button className="small" disabled={sel.length < 2} onClick={() => setMerging(true)} title="Treat these as one error from now on">
            Merge…
          </button>
          <button className="small ghost" onClick={() => setSel([])}>
            Clear
          </button>
        </div>
      )}
      <div className="card flush">
        {!groups.data ? (
          <Loading />
        ) : groups.data.length === 0 ? (
          <Empty title="No issues here">Repeated identical errors become one group; groups are filed after the settle window.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th style={{ width: 28 }} />
                <th>Error</th>
                <th>Service</th>
                <th>Release</th>
                <th className="num">Events</th>
                <th className="num">Users</th>
                <th>Last seen</th>
                <th>Status</th>
                <th>GitHub</th>
              </tr>
            </thead>
            <tbody>
              {groups.data.map((g) => (
                <tr key={g.id} className="clickable" onClick={() => onOpen(g.id)}>
                  <td onClick={(e) => e.stopPropagation()}>
                    {g.kind !== "down" && <input type="checkbox" checked={sel.includes(g.id)} onChange={() => toggle(g.id)} aria-label={`Select ${g.title}`} />}
                  </td>
                  <td style={{ maxWidth: 440 }}>
                    <div className="truncate" title={g.title}>
                      <KindBadge g={g} />
                      {g.title}
                    </div>
                    {g.block_reason && g.status === "pending" && <div className="small" style={{ color: "var(--warn)" }}>⚠ {g.block_reason}</div>}
                    {g.status === "ignored" && snoozeText(g) && <div className="small muted">💤 {snoozeText(g)}</div>}
                    {g.triage_verdict && (
                      <div className="small" title={g.triage_reason}>
                        <TriageBadge g={g} />
                      </div>
                    )}
                  </td>
                  <td className="nowrap">{g.service}</td>
                  <td className="nowrap mono small muted" title={g.first_release ? `first seen in ${g.first_release}` : ""}>
                    {g.last_release || "—"}
                  </td>
                  <td className="num">{fmtNum(g.count)}</td>
                  <td className="num">{g.users ? fmtNum(g.users) : <span className="faint">—</span>}</td>
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
                    {g.fix_state && (
                      <div>
                        <FixBadge g={g} />
                      </div>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {merging && (
        <MergeDialog
          groups={selected}
          onClose={() => setMerging(false)}
          onDone={() => {
            setMerging(false);
            setSel([]);
            onChanged();
          }}
        />
      )}
    </>
  );
}

function MergeDialog({ groups, onClose, onDone }: { groups: Group[]; onClose: () => void; onDone: () => void }) {
  const run = useAction();
  const [target, setTarget] = useState(groups.find((g) => g.issue_number)?.id ?? groups[0]?.id);
  const services = new Set(groups.map((g) => g.service));
  const merge = async () => {
    const ok = await run(
      () => api.post("/api/groups/merge", { target, sources: groups.filter((g) => g.id !== target).map((g) => g.id) }),
      "Merged. The other issues were closed with a link to this one.",
    );
    if (ok) onDone();
  };
  return (
    <Modal
      title={`Merge ${groups.length} issues`}
      onClose={onClose}
      footer={
        <>
          <button onClick={onClose}>Cancel</button>
          <button className="primary" disabled={services.size > 1} onClick={merge}>
            Merge
          </button>
        </>
      }
    >
      {services.size > 1 ? (
        <div className="notice warn">Only errors of the same service can be merged.</div>
      ) : (
        <>
          <p className="small muted">
            Events of all of them count toward the one you keep. Their GitHub issues are closed as not planned with a link to it. You can split them off again
            from the issue page.
          </p>
          {groups.map((g) => (
            <label key={g.id} className="row small" style={{ gap: 8, margin: "6px 0" }}>
              <input type="radio" checked={target === g.id} onChange={() => setTarget(g.id)} />
              <span className="truncate" style={{ maxWidth: 420 }}>
                {g.title}
              </span>
              <span className="faint">
                {fmtNum(g.count)} events{g.issue_number ? ` · #${g.issue_number}` : ""}
              </span>
            </label>
          ))}
        </>
      )}
    </Modal>
  );
}

interface Detail {
  group: Group;
  samples: { id: number; event: SampleEvent }[];
  histogram: Bucket[];
  aliases: Alias[];
}

const SNOOZES = [
  { label: "for 1 hour", body: { for: "1h" } },
  { label: "for 24 hours", body: { for: "24h" } },
  { label: "for 7 days", body: { for: "7d" } },
  { label: "until 100 more events", body: { count: 100 } },
  { label: "until 1,000 more events", body: { count: 1000 } },
  { label: "until the next release", body: { release: true } },
];

export function IssueDetail() {
  const { id } = useParams();
  const run = useAction();
  const d = useAsync(() => api.get<Detail>(`/api/groups/${id}`), [id], 20000);
  const tags = useAsync(() => api.get<TagSummary[]>(`/api/groups/${id}/tags`), [id], 60000);
  const [sampleIdx, setSampleIdx] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const nav = useNavigate();
  if (d.error) return <ErrorNote error={d.error} />;
  if (!d.data) return <Loading />;
  const { group: g, samples, histogram, aliases } = d.data;
  const idx = sampleIdx ?? samples.length - 1;
  const sm = samples[idx];
  const s = sm?.event;

  const setStatus = async (status: "resolved" | "ignored" | "open") => {
    setBusy(true);
    const onGH = g.issue_number ? " (and on GitHub)" : "";
    await run(
      () => api.post(`/api/groups/${g.id}/status`, { status }),
      status === "resolved"
        ? `Closed${onGH}. If it happens again, the issue is reopened.`
        : status === "ignored"
          ? `Ignored${onGH ? ", closed as not planned on GitHub" : ""}. Sentinel will stay quiet about it.`
          : `Reopened${onGH}`,
    );
    d.reload();
    setBusy(false);
  };
  const snooze = async (i: number) => {
    if (i < 0) return;
    setBusy(true);
    await run(() => api.post(`/api/groups/${g.id}/snooze`, SNOOZES[i].body), `Snoozed ${SNOOZES[i].label}. It comes back by itself then.`);
    d.reload();
    setBusy(false);
  };
  const fix = async () => {
    setBusy(true);
    await run(() => api.post(`/api/groups/${g.id}/fix`), "Fix requested: the workflow in the repository will open a pull request");
    d.reload();
    setBusy(false);
  };
  const canFix =
    (g.kind === "log" || g.kind === "span") &&
    g.status === "open" &&
    !!g.issue_number &&
    !["requested", "pr_open", "queued", "running"].includes(g.fix_state ?? "");
  const unmerge = async (a: Alias) => {
    const ng = await run(() => api.post<Group>(`/api/groups/${g.id}/unmerge`, { fingerprint: a.fingerprint }), "Split off into its own issue");
    if (ng) nav(`/issues/${ng.id}`);
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
          <h1 style={{ marginTop: 4 }}>
            <KindBadge g={g} />
            {g.title}
          </h1>
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
          {canFix && (
            <button
              className="primary"
              disabled={busy}
              onClick={fix}
              title="Add the fix label to the GitHub issue: the repository's workflow runs Claude Code, which opens a pull request"
            >
              🤖 {g.fix_state ? "Try fixing again" : "Fix it"}
            </button>
          )}
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
            <>
              <select value="-1" disabled={busy} onChange={(e) => snooze(+e.target.value)} aria-label="Snooze" title="Ignore for a while; it comes back by itself">
                <option value="-1">💤 Snooze…</option>
                {SNOOZES.map((o, i) => (
                  <option key={i} value={i}>
                    {o.label}
                  </option>
                ))}
              </select>
              <button className="danger" disabled={busy} onClick={() => setStatus("ignored")} title={g.issue_number ? "Close the GitHub issue as not planned and stay quiet" : "Stay quiet about this group"}>
                Ignore
              </button>
            </>
          )}
        </div>
      </div>
      {g.block_reason && g.status === "pending" && (
        <div className="notice warn" style={{ marginBottom: 16 }}>
          Not filed yet: {g.block_reason}. <Link to="/settings/services">Configure a repository</Link>.
        </div>
      )}
      {g.triage_verdict && (
        <div className={"notice" + (g.triage_verdict === "fix" ? " warn" : "")} style={{ marginBottom: 16, display: "flex", gap: 10, alignItems: "baseline" }}>
          <TriageBadge g={g} />
          <span className="small">{g.triage_reason}</span>
        </div>
      )}
      {g.fix_state && (
        <div className={"notice" + (g.fix_state === "no_pr" ? " warn" : "")} style={{ marginBottom: 16, display: "flex", gap: 10, alignItems: "center" }}>
          <FixBadge g={g} />
          <span className="small">
            {FIX[g.fix_state].help}
            {g.fix_state === "running" && g.fix_started_at ? ` · started ${ago(g.fix_started_at)}` : g.fix_requested_at && ` · requested ${ago(g.fix_requested_at)}`}
            {(g.fix_attempts ?? 0) > 1 && ` · attempt ${g.fix_attempts}`}
            {g.fix_summary && (
              <details style={{ marginTop: 6 }}>
                <summary className="small muted">{g.fix_state === "failed" ? "Error" : "What Claude found"}</summary>
                <pre className="code" style={{ whiteSpace: "pre-wrap", maxHeight: 300 }}>{g.fix_summary}</pre>
              </details>
            )}
          </span>
          {g.fix_pr_url && (
            <a className="btn small" href={g.fix_pr_url} target="_blank" rel="noreferrer" style={{ marginLeft: "auto" }}>
              <Icon.github /> Review pull request #{g.fix_pr_number}
            </a>
          )}
        </div>
      )}
      {g.status === "ignored" && snoozeText(g) && (
        <div className="notice" style={{ marginBottom: 16 }}>
          💤 This issue is {snoozeText(g)}. It is reopened when that happens.
        </div>
      )}
      <div className="grid stats" style={{ marginBottom: 16 }}>
        <div className="card stat">
          <div className="label">Occurrences</div>
          <div className="value">{fmtNum(g.count)}</div>
          <div className="hint">{fmtNum(g.count - g.count_at_last_report)} since last report</div>
        </div>
        <div className="card stat">
          <div className="label">Users affected</div>
          <div className="value">{g.users ? fmtNum(g.users) : "—"}</div>
          <div className="hint">{g.users ? "distinct user ids" : "no user.id / enduser.id attribute"}</div>
        </div>
        <div className="card stat">
          <div className="label">First seen</div>
          <div className="value" style={{ fontSize: 16 }}>{ago(g.first_seen)}</div>
          <div className="hint">
            {fmtFull(g.first_seen)}
            {g.first_release && (
              <>
                {" "}
                · in <span className="mono">{g.first_release}</span>
              </>
            )}
          </div>
        </div>
        <div className="card stat">
          <div className="label">Last seen</div>
          <div className="value" style={{ fontSize: 16 }}>{ago(g.last_seen)}</div>
          <div className="hint">
            {fmtFull(g.last_seen)}
            {g.last_release && (
              <>
                {" "}
                · in <span className="mono">{g.last_release}</span>
              </>
            )}
          </div>
        </div>
        <div className="card stat">
          <div className="label">Where</div>
          <div className="value" style={{ fontSize: 16 }}>{g.service}</div>
          <div className="hint">
            {g.env || "no env"} · {g.kind}
            {g.regressed_release && <> · regressed in {g.regressed_release}</>}
          </div>
        </div>
      </div>
      <div className="grid" style={{ gridTemplateColumns: "minmax(0, 2fr) minmax(0, 1fr)", gap: 16, alignItems: "start" }}>
        <div className="card" style={{ margin: 0 }}>
          <div className="card-head">
            <h2>Occurrences over time</h2>
          </div>
          <StackedBars buckets={histogram} keys={bucketKeys(histogram, ["debug", "info", "warn", "error", "fatal"])} colors={LEVEL_COLORS} bucketMs={bucketMs} height={160} />
        </div>
        <Tags tags={tags.data} />
      </div>
      <div className="card" style={{ marginTop: 16 }}>
        <div className="card-head">
          <h2>Samples</h2>
          <div className="right">
            <select value={idx} onChange={(e) => setSampleIdx(+e.target.value)} aria-label="Sample">
              {samples.map((x, i) => (
                <option key={x.id} value={i}>
                  {i === 0 ? "First" : i === samples.length - 1 ? "Latest" : `#${i + 1}`} — {fmtFull(x.event.time)}
                </option>
              ))}
            </select>
          </div>
        </div>
        {s ? (
          <div className="stack">
            <div className="row small">
              {s.severity && <span className="badge">{s.severity}</span>}
              {s.release && <span className="badge mono">release {s.release}</span>}
              {s.span_name && <span className="badge">span {s.span_name}</span>}
              {s.trace_id && (
                <>
                  <Link to={`/traces/${s.trace_id}`} className="mono">
                    trace {s.trace_id}
                  </Link>
                  <Link to={`/logs?trace_id=${s.trace_id}&range=7d`} className="small">
                    (logs)
                  </Link>
                </>
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
            <Breadcrumbs groupId={g.id} sampleId={sm.id} />
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
      {aliases.length > 0 && (
        <div className="card">
          <div className="card-head">
            <h2>Merged into this issue</h2>
          </div>
          <table>
            <thead>
              <tr>
                <th>Error</th>
                <th className="num">Events</th>
                <th>Merged</th>
                <th>Its issue</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {aliases.map((a) => (
                <tr key={a.fingerprint}>
                  <td className="truncate" style={{ maxWidth: 420 }} title={a.fingerprint}>
                    {a.title}
                  </td>
                  <td className="num">{fmtNum(a.count)}</td>
                  <td className="muted nowrap">{ago(a.merged_at)}</td>
                  <td>
                    {a.issue_url ? (
                      <a href={a.issue_url} target="_blank" rel="noreferrer">
                        #{a.issue_number}
                      </a>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td>
                    <button className="small" onClick={() => unmerge(a)} title="Split this error off into its own issue again">
                      Unmerge
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

const TAG_LABELS: Record<string, string> = { status_code: "status code" };

function Tags({ tags }: { tags?: TagSummary[] }) {
  return (
    <div className="card" style={{ margin: 0 }}>
      <div className="card-head">
        <h2>Tags</h2>
      </div>
      {!tags ? (
        <Loading />
      ) : tags.length === 0 ? (
        <div className="small muted">No tags yet. Tags come from the environment, release, user, host and HTTP attributes of the events.</div>
      ) : (
        <div className="tag-list">
          {tags.map((t) => (
            <div key={t.key} className="tag-block">
              <div className="tag-key">
                {TAG_LABELS[t.key] ?? t.key}
                <span className="faint"> · {t.distinct >= 50 && t.key !== "user" ? "50+" : fmtNum(t.distinct)} value{t.distinct === 1 ? "" : "s"}</span>
              </div>
              {t.top.slice(0, t.key === "user" ? 3 : 5).map((v) => {
                const pct = t.total ? (100 * v.count) / t.total : 0;
                return (
                  <div key={v.value} className="tag-row" title={`${v.value}: ${v.count} events, last ${ago(v.last_seen)}`}>
                    <span className="tag-bar" style={{ width: `${Math.max(2, pct)}%` }} />
                    <span className="tag-val">{v.value}</span>
                    <span className="tag-pct">{pct >= 1 ? Math.round(pct) : "<1"}%</span>
                  </div>
                );
              })}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function Breadcrumbs({ groupId, sampleId }: { groupId: number; sampleId: number }) {
  const b = useAsync(() => api.get<{ logs: LogRecord[]; source: string }>(`/api/groups/${groupId}/breadcrumbs?sample=${sampleId}`), [groupId, sampleId]);
  if (!b.data || b.data.logs.length <= 1) return null;
  return (
    <details open>
      <summary className="small muted">
        Breadcrumbs — {b.data.logs.length} log lines {b.data.source === "trace" ? "of this trace" : "of this service in the minute before"}
      </summary>
      <div className="crumbs">
        {b.data.logs.map((l) => (
          <div key={l.id} className={"crumb " + levelOf(l.severity)}>
            <span className="crumb-t">{new Date(l.time).toLocaleTimeString([], { hour12: false })}</span>
            <span className="crumb-l" style={{ color: LEVEL_COLORS[levelOf(l.severity)] }}>
              {levelOf(l.severity)}
            </span>
            {b.data!.source === "trace" && <span className="crumb-s">{l.service}</span>}
            <span className="crumb-b">{l.body}</span>
          </div>
        ))}
      </div>
    </details>
  );
}
