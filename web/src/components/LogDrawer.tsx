import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { LogRecord, qs } from "../api";
import { flatten, messageOf, valueText } from "../logjson";
import { fmtFull, levelOf } from "../util";
import { Icon, useToast } from "./ui";

// LogDrawer shows one log line: its message, every field of a JSON body
// (pick one as the service's message), the OpenTelemetry attributes and the
// raw body.

export interface LogDrawerProps {
  log: LogRecord;
  field?: string; // the service's chosen message field
  onClose: () => void;
  onPrev?: () => void;
  onNext?: () => void;
  onSetField: (field: string) => Promise<void>;
  onFilterAttr: (k: string, v: string) => void;
  onSearch: (text: string) => void;
  onTrace?: () => void;
  onSameError?: () => void;
}

export function LogDrawer(p: LogDrawerProps) {
  const { log } = p;
  const toast = useToast();
  const msg = messageOf(log.body, p.field);
  const fields = useMemo(() => (msg.json ? flatten(msg.json) : []), [msg.json]);
  const [find, setFind] = useState("");
  const [busy, setBusy] = useState(false);
  const lvl = levelOf(log.severity);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.target as HTMLElement)?.tagName === "INPUT") return;
      if (e.key === "Escape") p.onClose();
      if ((e.key === "ArrowDown" || e.key === "j") && p.onNext) {
        e.preventDefault();
        p.onNext();
      }
      if ((e.key === "ArrowUp" || e.key === "k") && p.onPrev) {
        e.preventDefault();
        p.onPrev();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [p]);

  const copy = async (s: string, what: string) => {
    try {
      await navigator.clipboard.writeText(s);
      toast(`${what} copied`);
    } catch {
      toast("Could not copy; select the text instead", true);
    }
  };
  const setField = async (f: string) => {
    setBusy(true);
    try {
      await p.onSetField(f);
    } finally {
      setBusy(false);
    }
  };
  const raw = msg.json && msg.format === "json" ? JSON.stringify(msg.json, null, 2) : log.body;
  const shown = find ? fields.filter(([k, v]) => (k + " " + valueText(v)).toLowerCase().includes(find.toLowerCase())) : fields;

  return (
    <div className="drawer-overlay" onMouseDown={(e) => e.target === e.currentTarget && p.onClose()}>
      <aside className="drawer" role="dialog" aria-label="Log line">
        <div className="drawer-head">
          <span className={"lvl " + lvl}>{log.kind === "span" ? "span" : lvl}</span>
          <strong className="truncate">{log.service}</strong>
          {log.env && <span className="badge">{log.env}</span>}
          <span className="small muted nowrap">{fmtFull(log.time)}</span>
          <div className="right">
            <button className="ghost small icon" onClick={p.onPrev} disabled={!p.onPrev} aria-label="Newer line" title="Newer line (↑)">
              <Icon.up />
            </button>
            <button className="ghost small icon" onClick={p.onNext} disabled={!p.onNext} aria-label="Older line" title="Older line (↓)">
              <Icon.down />
            </button>
            <button className="ghost icon" onClick={p.onClose} aria-label="Close">
              <Icon.x />
            </button>
          </div>
        </div>

        <div className="drawer-body">
          <section>
            <div className="drawer-label">Message</div>
            <pre className="drawer-message">{msg.text}</pre>
            {msg.json && (
              <div className="small muted row" style={{ gap: 6 }}>
                {msg.field ? (
                  <>
                    From <code>{msg.field}</code>
                    {p.field ? ` (chosen for ${log.service})` : " (found automatically)"}.
                  </>
                ) : p.field ? (
                  <>
                    This line has no <code>{p.field}</code> field, so the whole object is shown.
                  </>
                ) : (
                  "No message field found, so the whole object is shown. Pick one below."
                )}
                {p.field && (
                  <button className="link small" disabled={busy} onClick={() => setField("")}>
                    Use the default
                  </button>
                )}
              </div>
            )}
            <div className="row" style={{ marginTop: 10 }}>
              {p.onTrace && log.trace_id && (
                <button className="small" onClick={p.onTrace}>
                  Show trace {log.trace_id.slice(0, 8)}…
                </button>
              )}
              {p.onSameError && log.fingerprint && (
                <button className="small" onClick={p.onSameError}>
                  Same error
                </button>
              )}
              <button className="small" onClick={() => copy(msg.text, "Message")}>
                Copy message
              </button>
            </div>
          </section>

          {msg.json && (
            <section>
              <div className="drawer-label row">
                Fields <span className="faint">{fields.length}</span>
                {fields.length > 10 && (
                  <input className="drawer-find" value={find} onChange={(e) => setFind(e.target.value)} placeholder="Find a field" aria-label="Find a field" />
                )}
              </div>
              <div className="field-list">
                {shown.map(([k, v]) => {
                  const text = valueText(v);
                  const isMsg = msg.field === k;
                  return (
                    <div key={k} className={"field-row" + (isMsg ? " is-message" : "")}>
                      <span className="fk" title={k}>
                        {k}
                      </span>
                      <span className={"fv" + (typeof v === "string" ? "" : " typed")}>{text}</span>
                      <span className="fa">
                        {isMsg ? (
                          <span className="badge accent">message</span>
                        ) : (
                          <button className="link small" disabled={busy} onClick={() => setField(k)} title={`Show ${k} as the message for ${log.service}`}>
                            use as message
                          </button>
                        )}
                        {text.length > 0 && text.length < 200 && (
                          <button className="link small" onClick={() => p.onSearch(text)} title="Search logs for this value">
                            search
                          </button>
                        )}
                      </span>
                    </div>
                  );
                })}
                {shown.length === 0 && <div className="small muted">No field matches.</div>}
              </div>
            </section>
          )}

          {Object.keys(log.attrs ?? {}).length > 0 && (
            <section>
              <div className="drawer-label">
                Attributes <span className="faint">{Object.keys(log.attrs!).length}</span>
              </div>
              <div className="field-list">
                {Object.entries(log.attrs!)
                  .sort(([a], [b]) => a.localeCompare(b))
                  .map(([k, v]) => (
                    <div key={k} className="field-row">
                      <span className="fk" title={k}>
                        {k}
                      </span>
                      <span className="fv">{v}</span>
                      <span className="fa">
                        <button className="link small" onClick={() => p.onFilterAttr(k, v)}>
                          filter
                        </button>
                        {v.length < 200 && (
                          <Link className="small" to={`/traces${qs({ range: "24h", attr: `${k}=${v}` })}`} title="Traces with this value (last 24 hours)">
                            traces
                          </Link>
                        )}
                      </span>
                    </div>
                  ))}
                {log.trace_id && (
                  <div className="field-row">
                    <span className="fk">trace_id</span>
                    <span className="fv">{log.trace_id}</span>
                    <span className="fa" />
                  </div>
                )}
              </div>
            </section>
          )}

          <section>
            <div className="drawer-label row">
              {msg.format === "json" ? "JSON" : msg.format === "logfmt" ? "Raw (logfmt)" : "Raw"}
              <button className="link small" onClick={() => copy(raw, msg.format === "json" ? "JSON" : "Line")} style={{ marginLeft: "auto" }}>
                copy
              </button>
            </div>
            <pre className="code drawer-raw">{raw}</pre>
          </section>
        </div>
      </aside>
    </div>
  );
}
