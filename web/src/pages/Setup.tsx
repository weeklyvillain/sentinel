import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import { ErrorNote, Loading, useAction, useAsync } from "../components/ui";

// The setup guide: every step of getting Sentinel working, checked against
// what is configured, one after the other.

export interface SetupStep {
  id: string;
  title: string;
  why: string;
  state: "done" | "todo" | "warn";
  detail: string;
  optional?: boolean;
  skipped?: boolean;
  link?: string;
  link_label?: string;
  env?: string[];
}

export interface SetupState {
  steps: SetupStep[];
  remaining: number;
  public_url: string;
  ingest_auth: boolean;
}

export default function Setup() {
  const st = useAsync(() => api.get<SetupState>("/api/setup"), [], 15000);
  const run = useAction();
  const [open, setOpen] = useState<string>();
  if (!st.data) return st.error ? <ErrorNote error={st.error} /> : <Loading />;
  const steps = st.data.steps;
  const done = steps.filter((s) => s.state === "done").length;
  const current = steps.find((s) => s.state !== "done" && !s.skipped)?.id;
  const shown = open ?? current;
  const skip = async (s: SetupStep, skip: boolean) => {
    if (await run(() => api.post("/api/setup/skip", { id: s.id, skip }))) st.reload();
  };
  return (
    <>
      <div className="page-head">
        <h1>Setup</h1>
        <span className="sub">Everything Sentinel needs, step by step. Each step checks itself; come back any time.</span>
      </div>
      <div className="card" style={{ marginBottom: 16 }}>
        <div className="row">
          <strong>
            {done} of {steps.length} done
          </strong>
          <span className="small muted">
            {st.data.remaining === 0 ? "All set: every step is done or skipped." : `${st.data.remaining} to go.`}
          </span>
        </div>
        <div className="setup-bar" aria-hidden>
          <div style={{ width: `${(done / steps.length) * 100}%` }} />
        </div>
      </div>
      <ol className="setup-steps">
        {steps.map((s, i) => (
          <li key={s.id} className={"card setup-step " + (s.skipped ? "skipped" : s.state) + (shown === s.id ? " open" : "")}>
            <button className="setup-head" onClick={() => setOpen(shown === s.id ? "" : s.id)} aria-expanded={shown === s.id}>
              <span className="setup-num">{s.state === "done" ? "✓" : s.state === "warn" ? "!" : i + 1}</span>
              <span className="setup-title">
                {s.title}
                {s.optional && <span className="badge">optional</span>}
                {s.skipped && <span className="badge">skipped</span>}
              </span>
              <span className="small muted setup-state">{s.state === "done" ? "Done" : s.state === "warn" ? "Needs attention" : s.skipped ? "" : "To do"}</span>
            </button>
            {shown === s.id && (
              <div className="setup-body">
                <p className="muted">{s.why}</p>
                <p>{s.detail}</p>
                {s.id === "telemetry" && <TelemetryHelp url={st.data!.public_url} auth={st.data!.ingest_auth} />}
                {s.env && s.env.length > 0 && s.state !== "done" && (
                  <p className="small muted">
                    Set on the server in <code>deploy/.env</code> (then <code>docker compose up -d</code>): {s.env.map((e, j) => (
                      <span key={e}>
                        {j > 0 && ", "}
                        <code>{e}</code>
                      </span>
                    ))}
                    .
                  </p>
                )}
                <div className="row" style={{ marginTop: 10 }}>
                  {s.link && (
                    <Link className="btn primary" to={s.link}>
                      Open {s.link_label}
                    </Link>
                  )}
                  {s.state !== "done" &&
                    (s.skipped ? (
                      <button onClick={() => skip(s, false)}>Don't skip</button>
                    ) : (
                      <button onClick={() => skip(s, true)}>Skip this step</button>
                    ))}
                </div>
              </div>
            )}
          </li>
        ))}
      </ol>
    </>
  );
}

function TelemetryHelp({ url, auth }: { url: string; auth: boolean }) {
  const base = url.replace(/\/$/, "");
  return (
    <>
      <p className="small">
        On each app server, install <strong>otelcol-contrib</strong> with <code>deploy/otel-collector-app-server.yaml</code> from the repository and set:
      </p>
      <pre className="code">
        {`SENTINEL_ENDPOINT=${base}\n` +
          (auth ? "SENTINEL_INGEST_TOKEN=<INGEST_TOKEN from deploy/.env>\n" : "") +
          "HOST_SERVICE_NAME=<this server's name>"}
      </pre>
      <p className="small muted">
        Point your apps' OpenTelemetry SDKs at the collector (<code>http://localhost:4318</code>). Services appear on the Overview once data arrives.
      </p>
    </>
  );
}
