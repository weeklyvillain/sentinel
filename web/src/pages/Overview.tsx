import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { api, Check, Overview as OverviewT } from "../api";
import { bucketKeys, Legend, StackedBars } from "../components/charts";
import { Empty, ErrorNote, Loading, RangePicker, Stat, StatusBadge, useAsync } from "../components/ui";
import { ago, fmtNum, LEVEL_COLORS, serviceUp } from "../util";

const LEVELS = ["debug", "info", "warn", "error", "fatal"];

export default function Overview() {
  const [params, setParams] = useSearchParams();
  const range = params.get("range") ?? "24h";
  const nav = useNavigate();
  const { data, error } = useAsync(() => api.get<OverviewT>(`/api/overview?range=${range}`), [range], 30000);
  const checks = useAsync(() => api.get<Check[]>("/api/checks"), [], 30000);
  const activeChecks = (checks.data ?? []).filter((c) => c.enabled);
  const checksDown = activeChecks.filter((c) => c.state === "down");

  return (
    <>
      <div className="page-head">
        <h1>Overview</h1>
        <div className="actions">
          <RangePicker value={range} onChange={(r) => setParams({ range: r })} />
        </div>
      </div>
      <ErrorNote error={error} />
      {!data ? (
        <Loading />
      ) : (
        <div className="stack">
          <div className="grid stats">
            <Stat
              label="Services"
              value={`${data.services.up}/${data.services.total - data.services.never_seen}`}
              hint={data.services.down ? `${data.services.down} down` : "all reporting"}
              tone={data.services.down ? "bad" : "ok"}
            />
            <Stat label="Open issues" value={fmtNum(data.groups.open ?? 0)} hint={`${data.groups.pending ?? 0} pending`} />
            <Stat label={`Errors (${range})`} value={fmtNum(data.errors)} hint={`${fmtNum(data.logs)} log lines`} tone={data.errors ? "bad" : undefined} />
            <Stat label={`New issues (${range})`} value={fmtNum(data.issues_in_range)} />
            <Stat label="Alarms firing" value={data.firing_alarms.length} tone={data.firing_alarms.length ? "bad" : "ok"} />
            {activeChecks.length > 0 && (
              <Stat
                label="Uptime checks"
                value={`${activeChecks.length - checksDown.length}/${activeChecks.length}`}
                hint={checksDown.length ? `down: ${checksDown.map((c) => c.name).join(", ")}` : "all passing"}
                tone={checksDown.length ? "bad" : "ok"}
              />
            )}
          </div>

          {data.firing_alarms.length > 0 && (
            <div className="notice bad">
              <strong>Firing:</strong>{" "}
              {data.firing_alarms.map((a, i) => (
                <span key={a.id}>
                  {i > 0 && ", "}
                  <Link to="/alarms">{a.name}</Link> ({fmtNum(a.last_value)})
                </span>
              ))}
            </div>
          )}

          <div className="grid two">
            <div className="card">
              <div className="card-head">
                <h2>Errors by service</h2>
                <div className="right">
                  <Legend keys={bucketKeys(data.error_series)} />
                </div>
              </div>
              {data.errors === 0 ? (
                <Empty title="No errors in this range" />
              ) : (
                <StackedBars
                  buckets={data.error_series}
                  keys={bucketKeys(data.error_series)}
                  bucketMs={data.bucket_ms}
                  onBucketClick={(t) => nav(`/logs?min_severity=17&from=${new Date(t).getTime()}&to=${new Date(t).getTime() + data.bucket_ms}`)}
                />
              )}
            </div>
            <div className="card">
              <div className="card-head">
                <h2>Log volume</h2>
                <div className="right">
                  <Legend keys={bucketKeys(data.volume_series, LEVELS)} colors={LEVEL_COLORS} />
                </div>
              </div>
              {data.logs === 0 ? (
                <Empty title="No logs yet">Point an OpenTelemetry exporter at {"<sentinel>"}:4318.</Empty>
              ) : (
                <StackedBars buckets={data.volume_series} keys={bucketKeys(data.volume_series, LEVELS)} colors={LEVEL_COLORS} bucketMs={data.bucket_ms} />
              )}
            </div>
          </div>

          <div className="grid two">
            <div className="card flush">
              <div className="card-head" style={{ padding: "14px 16px 0" }}>
                <h2>Recent issues</h2>
                <div className="right">
                  <Link to="/issues">All issues →</Link>
                </div>
              </div>
              {data.recent_groups.length === 0 ? (
                <Empty title="Nothing yet">Error groups appear here as soon as errors arrive.</Empty>
              ) : (
                <table>
                  <tbody>
                    {data.recent_groups.map((g) => (
                      <tr key={g.id} className="clickable" onClick={() => nav(`/issues/${g.id}`)}>
                        <td style={{ maxWidth: 360 }}>
                          <div className="truncate" title={g.title}>{g.title}</div>
                          <div className="small faint">
                            {g.service} · {ago(g.last_seen)}
                          </div>
                        </td>
                        <td className="num">{fmtNum(g.count)}</td>
                        <td className="nowrap">
                          <StatusBadge status={g.status} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
            <div className="card flush">
              <div className="card-head" style={{ padding: "14px 16px 0" }}>
                <h2>Services</h2>
                <div className="right">
                  <Link to="/services">Manage →</Link>
                </div>
              </div>
              {data.service_list.length === 0 ? (
                <Empty title="No services yet" />
              ) : (
                <table>
                  <tbody>
                    {data.service_list.map((s) => {
                      const st = serviceUp(s);
                      return (
                        <tr key={s.name}>
                          <td>
                            <span className="row">
                              <span className={"dot " + (st === "up" ? "ok" : st === "down" ? "bad" : "")} />
                              <Link to={`/logs?service=${encodeURIComponent(s.name)}`}>{s.name}</Link>
                            </span>
                          </td>
                          <td className="muted small">{s.env}</td>
                          <td className="muted small nowrap">{st === "never" ? "never seen" : st === "down" ? `down since ${ago(s.down_since)}` : ago(s.last_seen)}</td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              )}
            </div>
          </div>
        </div>
      )}
    </>
  );
}
