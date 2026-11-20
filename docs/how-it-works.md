# How Sentinel works


1. **Ingest** – `POST /v1/logs`, `/v1/traces`, `/v1/metrics` accept protobuf
   (`application/x-protobuf`) or JSON (`application/json`), optionally gzip
   compressed, and reply with an empty `Export*ServiceResponse`.
   - Error **logs**: `severity_number >= MIN_SEVERITY` (default 17 = ERROR), an
     error-like `severity_text` (`error`, `fatal`, `critical`, `panic`, …), or any
     `exception.*` attribute.
   - Error **spans**: status `ERROR`, or an `exception` span event.
   - Every log line is stored (redacted) for search; error spans are stored as
     log lines too. **Metrics** are stored for charts and alarms. Every signal
     is a heartbeat for its `service.name`.
2. **Redaction** – bearer/basic tokens, `password=` / `secret=` / `api_key=` /
   `token=` style values, AWS keys, JWTs, GitHub/Slack tokens and URL
   credentials are replaced with `[REDACTED]` *before* anything is stored, sent
   to a model or posted to GitHub. Attributes whose key looks secret
   (`authorization`, `cookie`, `*password*`, …) are dropped wholesale.
3. **Grouping** – `fingerprint = sha256(service, kind, exception.type, X)`
   where X is the top 5 stack frames with line numbers, hex addresses,
   goroutine ids and Go argument lists stripped, or (with no stack) the message
   with UUIDs, hex ids, IPs, emails, numbers and quoted strings replaced by
   placeholders. Set the attribute `sentinel.fingerprint` on a log/span to
   choose the group yourself.
4. **Reporter** (every `TICK_INTERVAL`):
   - A *pending* group whose first event is older than `SETTLE_WINDOW` and that
     has at least `MIN_EVENTS` events gets an issue. Before creating it, GitHub
     is searched for the hidden marker `<!-- sentinel:fingerprint=… -->`, so a
     lost database does not create duplicates.
   - Open groups with new events get **at most one** update comment per
     `UPDATE_INTERVAL` (new count, total, latest sample).
   - If a human closed the issue as *completed* and the error comes back, it is
     reopened with a **Regression** comment. Closed as *not planned* → the group
     is ignored and Sentinel stays quiet.
   - At most `MAX_ISSUES_PER_HOUR` new issues per rolling hour; the rest stay
     pending until there is room.
5. **Releases and tags** – the resource attribute `service.version` is the
   release. Each group remembers the release it was first and last seen in,
   and the release a resolved error came back in (shown in the issue and the
   regression comment). Per group Sentinel also counts the values of a few
   tags: environment, release, user (`enduser.id`, `user.id`, …), host,
   instance, route, method, status code, region, OS, browser and runtime.
   The number of distinct users is the "users affected" figure.
6. **Performance issues** – from traces Sentinel files *performance* issues
   (kind `perf`) through the same pipeline: **N+1 queries** (a span with at
   least `PERF_N_PLUS_ONE` identical database calls directly under it),
   **slow endpoints** (an entry operation whose p95 over 15 minutes is above
   `PERF_SLOW_P95`, with at least 20 requests) and **latency regressions**
   (p95 over the last hour more than twice the day before, and at least
   100 ms worse). Slow-endpoint and regression issues link an example trace.
7. **Service down** – a *watched* service (the **Watch** switch under
   **Settings → Services**) that was seen and then sends nothing for `DOWN_AFTER` gets a
   synthetic `down` event. Services that are not watched are never flagged for
   being quiet. The `down` event goes through the same
   grouping (down groups skip the settle window and `MIN_EVENTS`). After a
   Sentinel restart there is a grace period of `DOWN_AFTER` before anything can
   be flagged. When the service sends again, Sentinel comments the outage
   length and closes the issue; a later outage reopens the same issue.
