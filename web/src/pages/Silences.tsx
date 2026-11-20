import { useState } from "react";
import { Alarm, api, Silence } from "../api";
import { Empty, ErrorNote, Loading, Modal, useAction, useAsync } from "../components/ui";
import { useServices } from "../shared";
import { fmtFull, fmtTime } from "../util";

function state(s: Silence): { cls: string; label: string } {
  const now = Date.now();
  if (new Date(s.ends_at).getTime() <= now) return { cls: "", label: "ended" };
  if (new Date(s.starts_at).getTime() > now) return { cls: "accent", label: "scheduled" };
  return { cls: "warn", label: "active" };
}

/** A datetime-local value for a date. */
const local = (d: Date) => new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16);

export default function Silences() {
  const run = useAction();
  const list = useAsync(() => api.get<Silence[]>("/api/silences"), [], 30000);
  const alarms = useAsync(() => api.get<Alarm[]>("/api/alarms"), []);
  const [editing, setEditing] = useState<Partial<Silence> | null>(null);
  const alarmName = (id: number) => alarms.data?.find((a) => a.id === id)?.name ?? `alarm #${id}`;
  const end = async (s: Silence) => {
    await run(() => api.put(`/api/silences/${s.id}`, { ...s, ends_at: new Date().toISOString() }), "Maintenance window ended");
    list.reload();
  };
  const remove = async (s: Silence) => {
    await run(() => api.del(`/api/silences/${s.id}`), "Deleted");
    list.reload();
  };
  return (
    <>
      <div className="page-head">
        <span className="sub">
          During a maintenance window no webhook is notified for the service (or alarm) it covers: alarms, SLOs, outages and new issues. Everything is still
          recorded.
        </span>
        <div className="actions">
          <button className="primary" onClick={() => setEditing({})}>
            + New window
          </button>
        </div>
      </div>
      <ErrorNote error={list.error} />
      <div className="card flush">
        {!list.data ? (
          <Loading />
        ) : list.data.length === 0 ? (
          <Empty title="No maintenance windows">Plan one before a deploy or a database migration so nobody gets paged for it.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Covers</th>
                <th>From</th>
                <th>Until</th>
                <th>State</th>
                <th>Comment</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.data.map((s) => {
                const st = state(s);
                return (
                  <tr key={s.id}>
                    <td>
                      {s.service || "every service"}
                      {s.alarm_id ? <div className="small muted">only {alarmName(s.alarm_id)}</div> : null}
                    </td>
                    <td className="nowrap" title={fmtFull(s.starts_at)}>
                      {fmtTime(s.starts_at, false)}
                    </td>
                    <td className="nowrap" title={fmtFull(s.ends_at)}>
                      {fmtTime(s.ends_at, false)}
                    </td>
                    <td>
                      <span className={"badge " + st.cls}>{st.label}</span>
                    </td>
                    <td className="small">
                      {s.comment}
                      {s.created_by && <span className="faint"> — @{s.created_by}</span>}
                    </td>
                    <td className="nowrap" style={{ textAlign: "right" }}>
                      {st.label === "active" && (
                        <button className="small" onClick={() => end(s)}>
                          End now
                        </button>
                      )}{" "}
                      {st.label !== "ended" && (
                        <button className="small" onClick={() => setEditing(s)}>
                          Edit
                        </button>
                      )}{" "}
                      <button className="small danger" onClick={() => remove(s)}>
                        Delete
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
      {editing && (
        <SilenceForm
          initial={editing}
          alarms={alarms.data ?? []}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            list.reload();
          }}
        />
      )}
    </>
  );
}

function SilenceForm(props: { initial: Partial<Silence>; alarms: Alarm[]; onClose: () => void; onSaved: () => void }) {
  const now = new Date();
  const [s, setS] = useState({
    service: props.initial.service ?? "",
    alarm_id: props.initial.alarm_id ?? 0,
    comment: props.initial.comment ?? "",
    starts: local(props.initial.starts_at ? new Date(props.initial.starts_at) : now),
    ends: local(props.initial.ends_at ? new Date(props.initial.ends_at) : new Date(now.getTime() + 3600_000)),
  });
  const [err, setErr] = useState<string>();
  const svcs = useServices();
  const set = (p: Partial<typeof s>) => setS((x) => ({ ...x, ...p }));
  const quick = (h: number) => set({ starts: local(new Date()), ends: local(new Date(Date.now() + h * 3600_000)) });
  const save = async () => {
    setErr(undefined);
    const body = { service: s.service, alarm_id: s.alarm_id, comment: s.comment, starts_at: new Date(s.starts).toISOString(), ends_at: new Date(s.ends).toISOString() };
    try {
      if (props.initial.id) await api.put(`/api/silences/${props.initial.id}`, body);
      else await api.post("/api/silences", body);
      props.onSaved();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <Modal
      title={props.initial.id ? "Edit maintenance window" : "New maintenance window"}
      onClose={props.onClose}
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" onClick={save}>
            Save
          </button>
        </>
      }
    >
      <ErrorNote error={err} />
      <label className="field">
        Service
        <select value={s.service} onChange={(e) => set({ service: e.target.value })}>
          <option value="">Every service</option>
          {(svcs.data ?? []).map((x) => (
            <option key={x.name}>{x.name}</option>
          ))}
        </select>
      </label>
      <label className="field">
        Only this alarm
        <select value={s.alarm_id} onChange={(e) => set({ alarm_id: +e.target.value })}>
          <option value={0}>Any alarm or event</option>
          {props.alarms.map((a) => (
            <option key={a.id} value={a.id}>
              {a.name}
            </option>
          ))}
        </select>
      </label>
      <div className="row" style={{ gap: 6, margin: "4px 0 10px" }}>
        <span className="small muted">From now for</span>
        {[1, 2, 4, 8, 24].map((h) => (
          <button key={h} className="small" onClick={() => quick(h)}>
            {h}h
          </button>
        ))}
      </div>
      <div className="row">
        <label className="field" style={{ flex: 1 }}>
          From
          <input type="datetime-local" value={s.starts} onChange={(e) => set({ starts: e.target.value })} />
        </label>
        <label className="field" style={{ flex: 1 }}>
          Until
          <input type="datetime-local" value={s.ends} onChange={(e) => set({ ends: e.target.value })} />
        </label>
      </div>
      <label className="field">
        Comment
        <input value={s.comment} onChange={(e) => set({ comment: e.target.value })} placeholder="Database migration" />
      </label>
    </Modal>
  );
}
