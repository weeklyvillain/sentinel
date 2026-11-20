import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import { ErrorNote, Loading, Seg, useAction, useAsync, useToast } from "../components/ui";
import { useServices } from "../shared";
import { ago } from "../util";

// Settings → Integrations: how telemetry gets into Sentinel. Apps and
// browsers use a public key per service (Sentry SDKs, Grafana Faro, the
// Datadog Browser SDK, plain JSON over HTTP); servers use OpenTelemetry with
// the ingest token.

interface ClientKey {
  id: number;
  key: string;
  service: string;
  environment?: string;
  origins?: string[];
  created_at: string;
  dsn: string;
  last_seen?: string;
}

interface IntegrationsView {
  otlp: { endpoint: string; token_from_env: boolean };
  sentry: ClientKey[];
}

function useCopy() {
  const toast = useToast();
  return async (s: string, what: string) => {
    try {
      await navigator.clipboard.writeText(s);
      toast(`${what} copied`);
    } catch {
      toast("Could not copy; select the text instead", true);
    }
  };
}

export default function Integrations() {
  const v = useAsync(() => api.get<IntegrationsView>("/api/integrations"), [], 15000);
  const [selected, setSelected] = useState<number>();
  if (!v.data) return v.error ? <ErrorNote error={v.error} /> : <Loading />;
  const keys = v.data.sentry;
  const key = keys.find((k) => k.id === selected) ?? keys[keys.length - 1];
  return (
    <>
      <div className="page-head">
        <span className="sub">Send telemetry the way you already do: Sentinel speaks OpenTelemetry, the Sentry SDKs, Grafana Faro, the Datadog Browser SDK and plain JSON.</span>
      </div>
      <Keys v={v.data} selected={key?.id} onSelect={setSelected} onChange={v.reload} />
      {key && <Setup k={key} />}
      <OTLP v={v.data} />
    </>
  );
}

function Keys({ v, selected, onSelect, onChange }: { v: IntegrationsView; selected?: number; onSelect: (id: number) => void; onChange: () => void }) {
  const run = useAction();
  const copy = useCopy();
  const services = useServices();
  const [service, setService] = useState("");
  const [env, setEnv] = useState("");
  const [editing, setEditing] = useState<number>();
  const [origins, setOrigins] = useState("");

  const add = async (e: React.FormEvent) => {
    e.preventDefault();
    const r = await run(() => api.post<IntegrationsView>("/api/integrations/sentry", { service: service.trim(), environment: env.trim() }), "Key created");
    if (r) {
      setService("");
      setEnv("");
      const last = r.sentry[r.sentry.length - 1];
      if (last) onSelect(last.id);
      onChange();
    }
  };
  const rotate = async (k: ClientKey) => {
    if (!confirm(`Replace ${k.service}'s key? Apps using the old key or DSN are refused until you update them.`)) return;
    if (await run(() => api.post(`/api/integrations/sentry/${k.id}/rotate`), "Key replaced")) onChange();
  };
  const remove = async (k: ClientKey) => {
    if (!confirm(`Delete ${k.service}'s key? Apps using it are refused from now on.`)) return;
    if (await run(() => api.del(`/api/integrations/sentry/${k.id}`), "Key deleted")) onChange();
  };
  const saveOrigins = async (k: ClientKey) => {
    const list = origins
      .split(/[\s,]+/)
      .map((o) => o.trim())
      .filter(Boolean);
    if (await run(() => api.put(`/api/integrations/sentry/${k.id}`, { origins: list }), "Allowed origins saved")) {
      setEditing(undefined);
      onChange();
    }
  };

  return (
    <div className="card">
      <div className="card-head">
        <h2>App and browser keys</h2>
      </div>
      <p className="small muted">
        A key per service, for SDKs in your apps and in the browser. Keys are public by design (they ship in your frontend code), so they can only send
        telemetry. Limit browser use to your own sites with allowed origins.
      </p>
      {v.sentry.length > 0 && (
        <table style={{ marginBottom: 14 }}>
          <thead>
            <tr>
              <th>Service</th>
              <th>Allowed origins</th>
              <th>Last event</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {v.sentry.map((k) => (
              <tr key={k.id} className={"clickable" + (selected === k.id ? " expanded" : "")} onClick={() => onSelect(k.id)}>
                <td>
                  <strong>{k.service}</strong>
                  <div className="small faint">{k.environment ? `default environment: ${k.environment}` : `key #${k.id}`}</div>
                </td>
                <td onClick={(e) => e.stopPropagation()} style={{ maxWidth: 340 }}>
                  {editing === k.id ? (
                    <div className="row" style={{ flexWrap: "nowrap" }}>
                      <input
                        className="mono"
                        autoFocus
                        value={origins}
                        onChange={(e) => setOrigins(e.target.value)}
                        placeholder="https://shop.example.com, https://*.example.com"
                        style={{ minWidth: 0, flex: 1 }}
                      />
                      <button className="small primary" onClick={() => saveOrigins(k)}>
                        Save
                      </button>
                      <button className="small" onClick={() => setEditing(undefined)}>
                        Cancel
                      </button>
                    </div>
                  ) : (
                    <button
                      className="link small"
                      onClick={() => {
                        setEditing(k.id);
                        setOrigins((k.origins ?? []).join(", "));
                      }}
                    >
                      {k.origins?.length ? k.origins.join(", ") : "any site (set origins)"}
                    </button>
                  )}
                </td>
                <td className="small muted nowrap">{k.last_seen ? ago(k.last_seen) : "never"}</td>
                <td className="nowrap" style={{ textAlign: "right" }} onClick={(e) => e.stopPropagation()}>
                  <button className="small" onClick={() => copy(k.dsn, "Sentry DSN")}>
                    Copy DSN
                  </button>{" "}
                  <button className="small" onClick={() => rotate(k)}>
                    Replace key
                  </button>{" "}
                  <button className="small danger" onClick={() => remove(k)}>
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form className="row" onSubmit={add} style={{ alignItems: "flex-end" }}>
        <label className="field" style={{ margin: 0 }}>
          Service
          <input list="integration-services" value={service} onChange={(e) => setService(e.target.value)} placeholder="web-frontend" style={{ width: 200 }} />
          <datalist id="integration-services">
            {(services.data ?? []).map((s) => (
              <option key={s.name} value={s.name} />
            ))}
          </datalist>
        </label>
        <label className="field" style={{ margin: 0 }}>
          Default environment <span className="faint">(optional)</span>
          <input value={env} onChange={(e) => setEnv(e.target.value)} placeholder="production" style={{ width: 160 }} />
        </label>
        <button className="primary" disabled={!service.trim()}>
          Create key
        </button>
      </form>
    </div>
  );
}

type Tool = "sentry" | "faro" | "datadog" | "http";
type Lang = "javascript" | "python" | "go" | "env";

function ingestURL(k: ClientKey, kind: string) {
  const u = new URL(k.dsn);
  return `${u.protocol}//${u.host}/ingest/${k.id}/${k.key}/${kind}`;
}

function sentrySnippet(lang: Lang, k: ClientKey) {
  const e = k.environment || "production";
  switch (lang) {
    case "javascript":
      return `import * as Sentry from "@sentry/browser"; // or @sentry/node, @sentry/react, …

Sentry.init({
  dsn: "${k.dsn}",
  environment: "${e}",
  integrations: [Sentry.browserTracingIntegration()],
  tracesSampleRate: 0.2, // transactions become traces
  enableLogs: true,      // Sentry logs become log lines
  // Continue the trace in your backends: Sentry sends sentry-trace,
  // OpenTelemetry services read the W3C traceparent. Send both to your API:
  tracePropagationTargets: [/^https:\/\/api\.example\.com/],
  propagateTraceparent: true,
});`;
    case "python":
      return `import sentry_sdk

sentry_sdk.init(
    dsn="${k.dsn}",
    environment="${e}",
    traces_sample_rate=0.2,
    enable_logs=True,
    # If the brotli package is installed, Sentinel needs gzip instead:
    # _experiments={"transport_compression_algo": "gzip"},
)`;
    case "go":
      return `sentry.Init(sentry.ClientOptions{
\tDsn:              "${k.dsn}",
\tEnvironment:      "${e}",
\tEnableTracing:    true,
\tTracesSampleRate: 0.2,
})`;
    default:
      return `SENTRY_DSN=${k.dsn}
SENTRY_ENVIRONMENT=${e}`;
  }
}

const TOOLS: { value: Tool; label: string; what: string }[] = [
  {
    value: "sentry",
    label: "Sentry SDK",
    what: "Keep the Sentry SDK and swap the DSN. Errors become issues, transactions traces, Sentry logs log lines; replays, profiles and sessions are dropped.",
  },
  {
    value: "faro",
    label: "Grafana Faro",
    what: "Grafana's open-source frontend SDK: console logs, uncaught errors and rejections, Web Vitals (as metrics, per page), page events and, with its tracing package, page loads and fetches as spans that continue in your backends.",
  },
  {
    value: "datadog",
    label: "Datadog Browser Logs",
    what: "Keep @datadog/browser-logs and point its proxy at Sentinel. Logs, console and network errors arrive with their view, user and error details.",
  },
  {
    value: "http",
    label: "HTTP / JSON",
    what: "Anything that can POST JSON: fetch, navigator.sendBeacon, logger transports (pino, winston, loglevel), Vector, Fluent Bit. One object, an array, or one per line.",
  },
];

function toolSnippet(tool: Tool, lang: Lang, k: ClientKey): string {
  const e = k.environment || "production";
  switch (tool) {
    case "sentry":
      return sentrySnippet(lang, k);
    case "faro":
      return `import { initializeFaro, getWebInstrumentations } from "@grafana/faro-web-sdk";
import { TracingInstrumentation } from "@grafana/faro-web-tracing";

initializeFaro({
  url: "${ingestURL(k, "faro")}",
  app: { name: "${k.service}", version: "1.0.0", environment: "${e}" },
  instrumentations: [
    ...getWebInstrumentations(),
    // Page loads and fetches become spans, and a W3C traceparent header goes
    // to your API so its spans join the same trace. List your API's origins:
    new TracingInstrumentation({
      instrumentationOptions: { propagateTraceHeaderCorsUrls: [/^https:\/\/api\.example\.com/] },
    }),
  ],
});`;
    case "datadog":
      return `import { datadogLogs } from "@datadog/browser-logs";

datadogLogs.init({
  clientToken: "unused", // required by the SDK, ignored by Sentinel
  proxy: "${ingestURL(k, "datadog")}",
  service: "${k.service}",
  env: "${e}",
  forwardErrorsToLogs: true,
  forwardConsoleLogs: ["error", "warn"],
  sessionSampleRate: 100,
});

datadogLogs.logger.info("Button clicked", { cart: 3 });`;
    case "http":
      return `// From the browser (also on page unload):
navigator.sendBeacon("${ingestURL(k, "logs")}",
  JSON.stringify({ message: "checkout opened", level: "info", user: { id: 7 } }));

// Or with fetch; errors with a stack become issues:
fetch("${ingestURL(k, "logs")}", {
  method: "POST",
  body: JSON.stringify({ message: err.message, level: "error",
    error: { name: err.name, message: err.message, stack: err.stack } }),
  keepalive: true,
});

# From a shell or a log shipper:
curl -X POST "${ingestURL(k, "logs")}" -d '{"msg":"deploy done","level":"info"}'`;
  }
}

function Setup({ k }: { k: ClientKey }) {
  const copy = useCopy();
  const [tool, setTool] = useState<Tool>("sentry");
  const [lang, setLang] = useState<Lang>("javascript");
  const t = TOOLS.find((x) => x.value === tool)!;
  const code = toolSnippet(tool, lang, k);
  return (
    <div className="card">
      <div className="card-head">
        <h2>
          Send from <span className="mono">{k.service}</span>
        </h2>
      </div>
      <div className="row" style={{ marginBottom: 10 }}>
        <Seg<Tool> value={tool} onChange={setTool} options={TOOLS.map((x) => ({ value: x.value, label: x.label }))} />
        {tool === "sentry" && (
          <Seg<Lang>
            value={lang}
            onChange={setLang}
            options={[
              { value: "javascript", label: "JavaScript" },
              { value: "python", label: "Python" },
              { value: "go", label: "Go" },
              { value: "env", label: "Env" },
            ]}
          />
        )}
      </div>
      <p className="small muted" style={{ margin: "0 0 8px" }}>
        {t.what}
      </p>
      <div className="row" style={{ marginBottom: 6 }}>
        <span className="small faint">
          {tool === "sentry" ? "Every Sentry SDK takes a DSN, so Java, .NET, PHP, Ruby and Rust work the same." : "Events show up under this service."}
        </span>
        <button className="small" style={{ marginLeft: "auto" }} onClick={() => copy(code, "Snippet")}>
          Copy
        </button>
      </div>
      <pre className="code">{code}</pre>
    </div>
  );
}

function OTLP({ v }: { v: IntegrationsView }) {
  const copy = useCopy();
  const access = useAsync(() => api.get<{ ingest_token: string }>("/api/access"), []);
  const token = access.data?.ingest_token ?? "<ingest token>";
  const env = `OTEL_EXPORTER_OTLP_ENDPOINT=${v.otlp.endpoint}
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer%20${token}
OTEL_SERVICE_NAME=<your service>`;
  return (
    <div className="card">
      <div className="card-head">
        <h2>OpenTelemetry (servers)</h2>
        <span className="badge">OTLP/HTTP</span>
      </div>
      <p className="small muted">
        Logs, traces and metrics over OTLP/HTTP (protobuf or JSON) at <code>{v.otlp.endpoint}/v1/logs</code>, <code>/v1/traces</code> and{" "}
        <code>/v1/metrics</code>, with the secret ingest token from <Link to="/settings/access">Settings → Access</Link>. Keep it on servers; browsers use a
        key above. Every OpenTelemetry SDK reads these:
      </p>
      <div className="row" style={{ marginBottom: 6 }}>
        <button className="small" style={{ marginLeft: "auto" }} onClick={() => copy(env, "Settings")}>
          Copy
        </button>
      </div>
      <pre className="code">{env}</pre>
      <p className="small faint" style={{ marginTop: 8 }}>
        Or run the OpenTelemetry Collector on each server with <code>deploy/otel-collector-app-server.yaml</code>: it also sends the server's own CPU,
        memory and disk, and buffers when Sentinel is unreachable.
      </p>
    </div>
  );
}
