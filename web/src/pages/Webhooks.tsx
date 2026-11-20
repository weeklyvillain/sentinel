import { Fragment, useEffect, useRef, useState } from "react";
import { api, Delivery, Webhook, WebhookStyle } from "../api";
import { Empty, ErrorNote, Loading, Modal, Seg, Switch, useAsync, useToast, useAction } from "../components/ui";
import { DiscordPreview, SlackPreview } from "../components/WebhookPreview";
import { ago, fmtFull } from "../util";

const EVENT_HELP: Record<string, string> = {
  "issue.created": "A new GitHub issue was filed",
  "issue.regression": "A closed issue was reopened because the error came back",
  "issue.updated": "An update comment was posted (at most once per update interval)",
  "fix.requested": "The fix workflow was asked to fix an issue",
  "fix.opened": "The fix workflow opened a pull request for an issue",
  "fix.merged": "A fix pull request was merged",
  "service.down": "A service stopped sending telemetry",
  "service.recovered": "A down service is sending again",
  "check.down": "An uptime check started failing",
  "check.recovered": "A failing uptime check passes again",
};

interface HooksResp {
  webhooks: Webhook[];
  event_types: string[];
  preview_events: string[];
  placeholders: string[];
}

export default function Webhooks() {
  const toast = useToast();
  const run = useAction();
  const hooks = useAsync(() => api.get<HooksResp>("/api/webhooks"), []);
  const [editing, setEditing] = useState<Partial<Webhook> | null>(null);
  const [log, setLog] = useState<Webhook | null>(null);

  const test = async (h: Webhook) => {
    const d = await run(() => api.post<Delivery>(`/api/webhooks/${h.id}/test`));
    if (d) toast(d.error ? `Test failed: ${d.error}` : `Delivered (HTTP ${d.status}, ${d.duration_ms}ms)`, !!d.error);
  };
  const toggle = async (h: Webhook, enabled: boolean) => {
    await api.put(`/api/webhooks/${h.id}`, { ...h, enabled });
    hooks.reload();
  };
  const remove = async (h: Webhook) => {
    if (!confirm(`Delete webhook "${h.name}"?`)) return;
    await api.del(`/api/webhooks/${h.id}`);
    hooks.reload();
  };

  return (
    <>
      <div className="page-head">
        <span className="sub">Send issue and outage events to Slack, Discord or any HTTP endpoint, styled per service with a live preview. Alarms choose their own webhooks.</span>
        <div className="actions">
          <button className="primary" onClick={() => setEditing({})}>
            + New webhook
          </button>
        </div>
      </div>
      <ErrorNote error={hooks.error} />
      <div className="card flush">
        {!hooks.data ? (
          <Loading />
        ) : hooks.data.webhooks.length === 0 ? (
          <Empty title="No webhooks" />
        ) : (
          <table>
            <thead>
              <tr>
                <th style={{ width: 50 }}>On</th>
                <th>Name</th>
                <th>Format</th>
                <th>Events</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {hooks.data.webhooks.map((h) => (
                <tr key={h.id}>
                  <td>
                    <Switch checked={h.enabled} onChange={(v) => toggle(h, v)} label="Enabled" />
                  </td>
                  <td>
                    <div style={{ fontWeight: 600 }}>
                      {h.name} {h.has_secret && <span className="badge" title="Requests are signed with X-Sentinel-Signature">signed</span>}
                    </div>
                    <div className="small faint mono truncate" style={{ maxWidth: 360 }}>
                      {safeHost(h.url)}
                    </div>
                  </td>
                  <td>
                    <span className="badge">{h.format}</span>
                    {h.format !== "json" && <span className="faint small"> {h.style?.rich ? "card" : "text"}</span>}
                  </td>
                  <td className="small">{h.events.length ? h.events.join(", ") : <span className="faint">alarms only</span>}</td>
                  <td className="nowrap" style={{ textAlign: "right" }}>
                    <button className="small" onClick={() => test(h)}>
                      Send test
                    </button>{" "}
                    <button className="small" onClick={() => setLog(h)}>
                      Deliveries
                    </button>{" "}
                    <button className="small" onClick={() => setEditing(h)}>
                      Edit
                    </button>{" "}
                    <button className="small danger" onClick={() => remove(h)}>
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      <div className="card">
        <h2>Payload</h2>
        <p className="small muted" style={{ marginTop: 0 }}>
          <code>json</code> webhooks receive a POST with headers <code>X-Sentinel-Event</code>, <code>X-Sentinel-Delivery</code> and, with a secret,{" "}
          <code>X-Sentinel-Signature: sha256=HMAC_SHA256(secret, body)</code>. Failed deliveries (network errors, 429, 5xx) are retried twice.
        </p>
        <pre className="code">{`{
  "event": "issue.created",
  "timestamp": "2026-11-07T10:01:05Z",
  "service": "checkout",
  "message": "New issue for checkout: Payment provider timeouts (34 occurrences)",
  "group": { "id": 1, "fingerprint": "ca3f…", "kind": "log", "title": "…", "count": 34, "env": "prod" },
  "issue": { "repo": "acme/checkout", "number": 42, "url": "https://github.com/acme/checkout/issues/42" },
  "alarm": null
}`}</pre>
      </div>
      {editing && (
        <WebhookForm
          initial={editing}
          eventTypes={hooks.data?.event_types ?? []}
          previewEvents={hooks.data?.preview_events ?? ["issue.created"]}
          placeholders={hooks.data?.placeholders ?? []}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            hooks.reload();
            toast("Webhook saved");
          }}
        />
      )}
      {log && <DeliveryLog hook={log} onClose={() => setLog(null)} />}
    </>
  );
}

function safeHost(u: string) {
  try {
    const x = new URL(u);
    return x.host + (x.pathname.length > 1 ? x.pathname.slice(0, 12) + "…" : "");
  } catch {
    return u;
  }
}

const DEFAULT_COLORS = { color_problem: "#d83c3e", color_recovery: "#2d9d58", color_info: "#4f7fd9" } as const;

const EVENT_LABEL: Record<string, string> = {
  "issue.created": "New issue",
  "issue.regression": "Regression",
  "issue.updated": "Issue updated",
  "service.down": "Service down",
  "service.recovered": "Service recovered",
  "check.down": "Uptime check down",
  "check.recovered": "Uptime check recovered",
  "alarm.firing": "Alarm firing",
  "alarm.resolved": "Alarm resolved",
  "slo.burn": "SLO burning",
  "slo.recovered": "SLO recovered",
  "fix.requested": "Fix requested",
  "fix.opened": "Fix opened",
  "fix.merged": "Fix merged",
  test: "Test notification",
};

const PLACEHOLDER_HELP: Record<string, string> = {
  icon: "event emoji",
  title: "event name, e.g. New issue",
  message: "the full message",
  service: "service name",
  event: "event type, e.g. issue.created",
  link: "link to the GitHub issue",
  issue: "repo#number",
  issue_url: "issue URL",
  time: "UTC time",
};

const MENTION_HELP: Record<string, string> = {
  discord: "Role <@&ROLE_ID>, user <@USER_ID>, @here or @everyone.",
  slack: "<!channel>, <!here>, a user <@U0123ABC> or a group <!subteam^S0123ABC>.",
};

function defaultTemplate(format: string, rich: boolean) {
  if (rich) return "{message}";
  return format === "discord" ? "{icon} **Sentinel** — {message} {link}" : "{icon} *Sentinel* — {message} {link}";
}

function WebhookForm(props: {
  initial: Partial<Webhook>;
  eventTypes: string[];
  previewEvents: string[];
  placeholders: string[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const [h, setH] = useState<Partial<Webhook>>({
    name: "",
    url: "",
    format: "json",
    events: ["issue.created", "service.down", "service.recovered"],
    enabled: true,
    ...props.initial,
    // New webhooks default to cards; existing ones keep what they had.
    style: { rich: !props.initial.id, ...props.initial.style },
  });
  const [secret, setSecret] = useState("");
  const [clearSecret, setClearSecret] = useState(false);
  const [err, setErr] = useState<string>();
  const set = (p: Partial<Webhook>) => setH((x) => ({ ...x, ...p }));
  const style = h.style!;
  const setStyle = (p: Partial<WebhookStyle>) => setH((x) => ({ ...x, style: { ...x.style!, ...p } }));
  const fmt = h.format!;
  const chat = fmt === "slack" || fmt === "discord";

  const onURL = (url: string) => {
    // Pick the format from well-known webhook hosts.
    let format = h.format;
    if (/^https:\/\/(canary\.|ptb\.)?(discord|discordapp)\.com\/api\/webhooks\//.test(url)) format = "discord";
    else if (/^https:\/\/hooks\.slack\.com\//.test(url)) format = "slack";
    set({ url, format });
  };

  const save = async () => {
    setErr(undefined);
    const body = { name: h.name, url: h.url, format: h.format, events: h.events, enabled: h.enabled, style: h.style, secret: secret || undefined, clear_secret: clearSecret };
    try {
      if (h.id) await api.put(`/api/webhooks/${h.id}`, body);
      else await api.post("/api/webhooks", body);
      props.onSaved();
    } catch (e) {
      setErr((e as Error).message);
    }
  };

  const tplRef = useRef<HTMLTextAreaElement>(null);
  const insert = (name: string) => {
    const el = tplRef.current;
    const cur = style.template || defaultTemplate(fmt, style.rich);
    const focused = el !== null && el === document.activeElement && !!style.template;
    const at = focused ? el!.selectionStart : cur.length;
    const end = focused ? el!.selectionEnd : cur.length;
    const tok = `{${name}}`;
    setStyle({ template: cur.slice(0, at) + tok + cur.slice(end) });
    requestAnimationFrame(() => {
      el?.focus();
      el?.setSelectionRange(at + tok.length, at + tok.length);
    });
  };

  return (
    <Modal
      title={h.id ? `Edit ${props.initial.name}` : "New webhook"}
      onClose={props.onClose}
      wide="x"
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" onClick={save} disabled={!h.name || !h.url}>
            Save
          </button>
        </>
      }
    >
      <ErrorNote error={err} />
      <div className="wh-editor">
        <div className="wh-form">
          <label className="field">
            Name
            <input value={h.name} onChange={(e) => set({ name: e.target.value })} placeholder="#ops-alerts" autoFocus />
          </label>
          <label className="field">
            URL
            <input value={h.url} onChange={(e) => onURL(e.target.value)} placeholder="https://discord.com/api/webhooks/…" className="mono" />
          </label>
          <div className="field">
            Format
            <div>
              <Seg
                value={fmt}
                onChange={(format) => set({ format })}
                options={[
                  { value: "discord", label: "Discord" },
                  { value: "slack", label: "Slack" },
                  { value: "json", label: "JSON (generic)" },
                ]}
              />
            </div>
          </div>

          {chat && (
            <fieldset className="wh-section">
              <legend>Message</legend>
              <div className="field">
                Layout
                <div>
                  <Seg
                    value={style.rich ? "card" : "text"}
                    onChange={(v) => setStyle({ rich: v === "card" })}
                    options={[
                      { value: "card", label: fmt === "discord" ? "Embed" : "Attachment card" },
                      { value: "text", label: "Plain text" },
                    ]}
                  />
                </div>
              </div>
              <label className="field">
                <span>
                  {style.rich ? "Card text" : "Message text"} <span className="help">· empty uses the default</span>
                </span>
                <textarea ref={tplRef} rows={3} className="mono" value={style.template ?? ""} placeholder={defaultTemplate(fmt, style.rich)} onChange={(e) => setStyle({ template: e.target.value })} />
              </label>
              <div className="wh-chips" aria-label="Insert a placeholder">
                {props.placeholders.map((p) => (
                  <button key={p} type="button" className="small" title={PLACEHOLDER_HELP[p]} onClick={() => insert(p)}>
                    {`{${p}}`}
                  </button>
                ))}
              </div>
              <div className="wh-row">
                <label className="field">
                  Display name
                  <input value={style.username ?? ""} onChange={(e) => setStyle({ username: e.target.value })} placeholder={fmt === "discord" ? "as set in Discord" : "as set in Slack"} />
                </label>
                {fmt === "slack" && (
                  <label className="field">
                    Icon emoji
                    <input value={style.icon_emoji ?? ""} onChange={(e) => setStyle({ icon_emoji: e.target.value })} placeholder=":rotating_light:" className="mono" />
                  </label>
                )}
              </div>
              <label className="field">
                {fmt === "slack" ? "Icon URL" : "Avatar URL"}
                <input value={style.avatar_url ?? ""} onChange={(e) => setStyle({ avatar_url: e.target.value })} placeholder="https://…/avatar.png" className="mono" />
                {fmt === "slack" && <span className="help">Webhooks of newer Slack apps always post as the app and ignore the name and icon.</span>}
              </label>
              <div className="wh-row">
                <label className="field">
                  Mention
                  <input value={style.mention ?? ""} onChange={(e) => setStyle({ mention: e.target.value })} placeholder={fmt === "discord" ? "<@&123456789012345678>" : "<!here>"} className="mono" />
                  <span className="help">{MENTION_HELP[fmt]}</span>
                </label>
                <label className="field">
                  Mention on
                  <select value={style.mention_on || "problems"} onChange={(e) => setStyle({ mention_on: e.target.value as WebhookStyle["mention_on"] })} disabled={!style.mention}>
                    <option value="problems">Problems only</option>
                    <option value="all">Every message</option>
                  </select>
                </label>
              </div>
              {style.rich && (
                <>
                  <div className="field">
                    Card colors
                    <div className="wh-colors">
                      {(
                        [
                          ["color_problem", "Problem"],
                          ["color_recovery", "Recovery"],
                          ["color_info", "Info"],
                        ] as const
                      ).map(([k, label]) => (
                        <span key={k} className="wh-color">
                          <input type="color" aria-label={`${label} color`} value={style[k] || DEFAULT_COLORS[k]} onChange={(e) => setStyle({ [k]: e.target.value })} />
                          <span>{label}</span>
                          {style[k] && (
                            <button type="button" className="ghost small" onClick={() => setStyle({ [k]: "" })} title="Back to the default color">
                              reset
                            </button>
                          )}
                        </span>
                      ))}
                    </div>
                  </div>
                  <div className="wh-row">
                    <label className="field">
                      Footer
                      <input value={style.footer ?? ""} onChange={(e) => setStyle({ footer: e.target.value })} placeholder="Sentinel" />
                    </label>
                    <label className="check wh-check">
                      <input type="checkbox" checked={!style.hide_fields} onChange={(e) => setStyle({ hide_fields: !e.target.checked })} /> Show detail fields
                    </label>
                  </div>
                </>
              )}
            </fieldset>
          )}

          <fieldset className="wh-section">
            <legend>Delivery</legend>
            <label className="field">
              Signing secret
              <input type="password" value={secret} onChange={(e) => setSecret(e.target.value)} placeholder={h.has_secret ? "•••••• (unchanged)" : "optional"} autoComplete="new-password" />
              {h.has_secret && (
                <label className="check small">
                  <input type="checkbox" checked={clearSecret} onChange={(e) => setClearSecret(e.target.checked)} /> remove the existing secret
                </label>
              )}
            </label>
            <div className="field">
              Events
              {props.eventTypes.map((ev) => (
                <label key={ev} className="check" style={{ fontWeight: 400 }}>
                  <input
                    type="checkbox"
                    checked={h.events!.includes(ev)}
                    onChange={(e) => set({ events: e.target.checked ? [...h.events!, ev] : h.events!.filter((x) => x !== ev) })}
                  />
                  <code>{ev}</code> <span className="faint small">{EVENT_HELP[ev]}</span>
                </label>
              ))}
            </div>
            <label className="check">
              <input type="checkbox" checked={h.enabled} onChange={(e) => set({ enabled: e.target.checked })} /> Enabled
            </label>
          </fieldset>
        </div>
        <LivePreview format={fmt} style={style} events={props.previewEvents} subscribed={h.events ?? []} />
      </div>
    </Modal>
  );
}

function LivePreview({ format, style, events, subscribed }: { format: string; style: WebhookStyle; events: string[]; subscribed: string[] }) {
  const [event, setEvent] = useState(() => subscribed.find((e) => events.includes(e)) ?? "issue.created");
  const [view, setView] = useState<"preview" | "payload">("preview");
  const [payload, setPayload] = useState<unknown>();
  const [err, setErr] = useState<string>();
  const key = JSON.stringify({ format, style, event });
  useEffect(() => {
    let cancelled = false;
    const t = setTimeout(async () => {
      try {
        const r = await api.post<{ payload: unknown }>("/api/webhooks/preview", JSON.parse(key));
        if (!cancelled) {
          setPayload(r.payload);
          setErr(undefined);
        }
      } catch (e) {
        if (!cancelled) setErr((e as Error).message);
      }
    }, 150);
    return () => {
      cancelled = true;
      clearTimeout(t);
    };
  }, [key]);
  const chat = format === "slack" || format === "discord";
  return (
    <div className="wh-preview">
      <div className="wh-preview-head">
        <span className="wh-preview-title">Live preview</span>
        <select value={event} onChange={(e) => setEvent(e.target.value)} aria-label="Example event">
          {events.map((e) => (
            <option key={e} value={e}>
              {(EVENT_LABEL[e] ?? e) + (subscribed.includes(e) ? " ✓" : "")}
            </option>
          ))}
        </select>
        {chat && (
          <Seg
            value={view}
            onChange={setView}
            options={[
              { value: "preview", label: format === "discord" ? "Discord" : "Slack" },
              { value: "payload", label: "JSON" },
            ]}
          />
        )}
      </div>
      {err && <div className="small wh-preview-err">{err}</div>}
      {payload === undefined ? (
        <Loading />
      ) : !chat || view === "payload" ? (
        <pre className="code wh-json">{JSON.stringify(payload, null, 2)}</pre>
      ) : format === "discord" ? (
        <DiscordPreview payload={payload as Parameters<typeof DiscordPreview>[0]["payload"]} />
      ) : (
        <SlackPreview payload={payload as Parameters<typeof SlackPreview>[0]["payload"]} />
      )}
      <p className="small faint" style={{ margin: 0 }}>
        Rendered from the exact payload Sentinel sends, for an example event (✓ = subscribed). Alarms and SLOs pick their webhooks themselves.
      </p>
    </div>
  );
}

function DeliveryLog({ hook, onClose }: { hook: Webhook; onClose: () => void }) {
  const d = useAsync(() => api.get<Delivery[]>(`/api/webhooks/${hook.id}/deliveries`), [hook.id], 10000);
  const [open, setOpen] = useState<number | null>(null);
  return (
    <Modal title={`Deliveries — ${hook.name}`} onClose={onClose} wide>
      {!d.data ? (
        <Loading />
      ) : d.data.length === 0 ? (
        <Empty title="Nothing delivered yet" />
      ) : (
        <table>
          <thead>
            <tr>
              <th>When</th>
              <th>Event</th>
              <th>Result</th>
              <th className="num">Time</th>
              <th className="num">Attempts</th>
            </tr>
          </thead>
          <tbody>
            {d.data.map((x) => (
              <Fragment key={x.id}>
                <tr className="clickable" onClick={() => setOpen(open === x.id ? null : x.id)}>
                  <td className="nowrap" title={fmtFull(x.time)}>
                    {ago(x.time)}
                  </td>
                  <td className="mono small">{x.event}</td>
                  <td>{x.error ? <span className="badge bad">{x.error}</span> : <span className="badge ok">{x.status}</span>}</td>
                  <td className="num">{x.duration_ms}ms</td>
                  <td className="num">{x.attempt}</td>
                </tr>
                {open === x.id && (
                  <tr>
                    <td colSpan={5}>
                      <pre className="code">{pretty(x.payload)}</pre>
                    </td>
                  </tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
    </Modal>
  );
}

function pretty(s?: string) {
  try {
    return JSON.stringify(JSON.parse(s ?? ""), null, 2);
  } catch {
    return s;
  }
}
