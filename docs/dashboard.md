# Dashboard

Sentinel serves a dashboard on `SENTINEL_HTTP_ADDR` (default `:8080`, separate
from the OTLP port):

- **Overview** – services up/down, open issues, errors by service, log volume
  by level, firing alarms, and a setup checklist until everything is set up.
- **Logs** – full-text search over every log line (and error span), with
  multi-select level and service dropdowns, time range, trace id, attributes
  and "same error"; a clickable volume histogram, live tail, and older lines
  that load as you scroll. **Saved searches** are shared and listed most-used
  first (the time range isn't saved); recent searches are kept per browser.
  The **Metric** view charts count, rate, unique, sum, avg, min, max or
  p50–p99 of a field of the matching lines, optionally split by a field, and
  adds it to a dashboard as a panel. Query syntax (also used by log panels and
  log-count alarms):

  | Syntax | Meaning |
  | --- | --- |
  | `timeout "connection refused" -healthcheck` | words (prefix-matched, ANDed), exact phrases, exclusions |
  | `service:api level:error` | field filters |
  | `level>=warn`, `duration_ms>500` | comparisons (`>` `>=` `<` `<=`, or `field:>500`) |
  | `status!=200` | not equal (a missing field counts as not equal) |
  | `http.route:/orders/*` | `*` wildcard, case-insensitive |
  | `user.id:*`, `has:user.id` | the field is present |
  | `(timeout OR refused) -service:web` | `OR`, parentheses, `NOT` / `-` |
  | `message:"slow query"` | search only the message, not attributes |

  Built-in fields: `service`, `level`, `severity`, `env`, `kind`, `trace`,
  `span`, `fingerprint`, `message`. Any other name is an attribute or a field
  of a JSON log line (`db.table` also matches nested `{"db": {"table": …}}`);
  prefix it with `@` for an attribute named like a built-in field.
  logfmt lines are parsed for display only, so their fields can't be filtered
  yet.
- **Traces** – every span is stored (see `TRACE_SAMPLE_RATE`); search traces
  by service, operation, minimum duration or errors, and open one as a
  waterfall across services with each span's attributes, events and log
  lines. **Performance** lists every operation (entry spans: server, consumer
  and root spans) with requests/s, error rate and p50/p95/p99, from per-minute
  statistics that count every span even when traces are sampled.
- **Issues** – error groups by status with their GitHub issue, occurrence
  chart, first/latest samples with stack traces, and the reason a group is
  still pending (e.g. no repository configured). Filter by service,
  environment and release ("new in this release"), sort by last/first seen,
  events or users. An issue shows its tag breakdown, users affected, the
  releases it appeared in and **breadcrumbs**: the log lines of the sample's
  trace (or of its service in the minute before). **Ignore** stays quiet
  forever; **Snooze** ignores it for a while, until N more events or until
  the next release, then reopens it if it happened meanwhile (or marks it
  resolved, so a later event is a regression). **Merge** a few issues of the
  same service into one: their events and future events count there and
  their GitHub issues are closed with a link; **Unmerge** splits one off
  again and reopens its issue. **Releases** lists every version with the new
  issues and regressions it brought. The **AI queue** view lists the model
  analyses waiting, running, done or failed, with a retry for failed ones.
- **Dashboards** – your own graphs, gauges, stats, bar lists, tables and
  heatmaps over metrics, logs and request statistics (rate, errors, latency
  percentiles per service or operation), with a panel editor, formulas and
  templates. Histogram metrics also give p50/p75/p90/p95/p99 and heatmaps of
  their buckets. **Variables** are pickers above the panels (services,
  values of a metric attribute, or a fixed list, optionally several at once)
  used as `$name` in queries. Deploys (a new `service.version`), outages and
  alarms are marked on graphs. **Rows** group panels into collapsible
  sections; dashboards export to and import from JSON. The range and variable
  values are in the URL; the ⏱ link button pins the exact time window.
  **Explore metrics** charts every OTLP metric a service reports (gauges
  as-is, cumulative counters as per-interval deltas, histograms as interval
  means), with avg/sum/max/min/count and per-attribute split;
  **+ Dashboard** on a chart saves it as a panel.
- **Alerts**
  - **Alarms** – rules evaluated every 15s: *log count* (matching text, level,
    service, window) and *metric threshold* (aggregate or histogram
    percentile over a window). With **Fire after**, the condition must hold
    that long (the alarm is *pending* until then) so short blips don't page.
    Alarms that change state in the same evaluation and notify the same
    webhooks are sent as one message. A preview shows the current value;
    state changes are kept as history.
    Older *service down* alarms keep working; for new ones, watch the service
    instead and subscribe a webhook to `service.down`.
  - **Uptime** – HTTP checks run from the Sentinel server (see [Deployment → Uptime checks](deployment.md#3-uptime-checks)).
  - **SLOs** – objectives over request statistics (availability: requests
    that did not fail; latency: requests faster than a threshold) or uptime
    checks, over 1–90 days. Shows the current level, error budget left and
    burn rates. Webhooks chosen on the SLO get `slo.burn` when the budget
    burns 14.4× too fast over 1h and 5m, or 6× over 6h and 30m, and
    `slo.recovered` after. Uptime SLOs only see probe results kept by
    `METRIC_RETENTION`.
  - **Maintenance** – windows during which no webhook is notified for a
    service (or one alarm): alarms, SLOs, outages and issue events.
    Everything is still recorded.
- **Settings**
  - **Services** – discovered automatically from `service.name`; add a service
    ahead of time, choose which repository its issues go to and extra labels.
    The **Issues** switch turns issue creation off per service: its errors are
    still stored and grouped, but nothing is filed, updated or reopened on
    GitHub until it is switched back on. The **Watch** switch flags the
    service as down when it goes quiet (see [Service down](how-it-works.md)).
  - **GitHub** – install the GitHub App, sync repositories, authorize the
    repositories Sentinel may create issues in, and pick a default repository.
  - **Webhooks** – Slack, Discord or generic JSON targets for `issue.created`,
    `issue.regression`, `issue.updated`, `service.down`, `service.recovered`,
    `check.down`, `check.recovered`, `fix.requested`, `fix.opened`, `fix.merged`
    (alarm and SLO notifications go to the webhooks chosen on each alarm or
    SLO; a grouped alarm message lists them all in `alarms`). JSON
    deliveries carry `X-Sentinel-Event` and, with a secret,
    `X-Sentinel-Signature: sha256=<HMAC-SHA256(secret, body)>`. Failures (network,
    429, 5xx) are retried twice and every delivery is logged in the UI.
    Slack and Discord messages can be styled per webhook: a card (Discord
    embed / Slack attachment with colors per problem, recovery and info event,
    detail fields and a footer) or plain text, a message template with
    `{icon}`, `{title}`, `{message}`, `{service}`, `{event}`, `{link}`,
    `{issue}`, `{issue_url}` and `{time}`, a display name and avatar (or Slack
    icon emoji), and a mention (`<@&role>`, `<!here>`, …) for problems or
    every message. The editor shows a live preview of the exact payload for
    any example event.
  - **AI** – provider, model and timeout for issue analysis, with a test button.

Logs and metric points are kept for `LOG_RETENTION` / `METRIC_RETENTION`
(default 7 days), stored spans for `TRACE_RETENTION` (default 3 days), and
pruned every 10 minutes. Request statistics follow `METRIC_RETENTION`. Everything is redacted before
it is stored.
