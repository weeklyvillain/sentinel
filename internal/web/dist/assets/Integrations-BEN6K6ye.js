import{r as p,j as e,L as C}from"./vendor-react-5Xfo6IyM.js";import{u as N,E as P,L as D,g as h,j as _,k as R,d as A,o as b,h as K}from"./index-CHEjdEYH.js";function v(){const n=K();return async(a,t)=>{try{await navigator.clipboard.writeText(a),n(`${t} copied`)}catch{n("Could not copy; select the text instead",!0)}}}function z(){const n=N(()=>h.get("/api/integrations"),[],15e3),[a,t]=p.useState();if(!n.data)return n.error?e.jsx(P,{error:n.error}):e.jsx(D,{});const o=n.data.sentry,r=o.find(l=>l.id===a)??o[o.length-1];return e.jsxs(e.Fragment,{children:[e.jsx("div",{className:"page-head",children:e.jsx("span",{className:"sub",children:"Send telemetry the way you already do: Sentinel speaks OpenTelemetry, the Sentry SDKs, Grafana Faro, the Datadog Browser SDK and plain JSON."})}),e.jsx(k,{v:n.data,selected:r==null?void 0:r.id,onSelect:t,onChange:n.reload}),r&&e.jsx(F,{k:r}),e.jsx(J,{v:n.data})]})}function k({v:n,selected:a,onSelect:t,onChange:o}){const r=_(),l=v(),g=R(),[m,d]=p.useState(""),[x,f]=p.useState(""),[T,y]=p.useState(),[j,S]=p.useState(""),O=async s=>{s.preventDefault();const c=await r(()=>h.post("/api/integrations/sentry",{service:m.trim(),environment:x.trim()}),"Key created");if(c){d(""),f("");const i=c.sentry[c.sentry.length-1];i&&t(i.id),o()}},E=async s=>{confirm(`Replace ${s.service}'s key? Apps using the old key or DSN are refused until you update them.`)&&await r(()=>h.post(`/api/integrations/sentry/${s.id}/rotate`),"Key replaced")&&o()},L=async s=>{confirm(`Delete ${s.service}'s key? Apps using it are refused from now on.`)&&await r(()=>h.del(`/api/integrations/sentry/${s.id}`),"Key deleted")&&o()},$=async s=>{const c=j.split(/[\s,]+/).map(i=>i.trim()).filter(Boolean);await r(()=>h.put(`/api/integrations/sentry/${s.id}`,{origins:c}),"Allowed origins saved")&&(y(void 0),o())};return e.jsxs("div",{className:"card",children:[e.jsx("div",{className:"card-head",children:e.jsx("h2",{children:"App and browser keys"})}),e.jsx("p",{className:"small muted",children:"A key per service, for SDKs in your apps and in the browser. Keys are public by design (they ship in your frontend code), so they can only send telemetry. Limit browser use to your own sites with allowed origins."}),n.sentry.length>0&&e.jsxs("table",{style:{marginBottom:14},children:[e.jsx("thead",{children:e.jsxs("tr",{children:[e.jsx("th",{children:"Service"}),e.jsx("th",{children:"Allowed origins"}),e.jsx("th",{children:"Last event"}),e.jsx("th",{})]})}),e.jsx("tbody",{children:n.sentry.map(s=>{var c;return e.jsxs("tr",{className:"clickable"+(a===s.id?" expanded":""),onClick:()=>t(s.id),children:[e.jsxs("td",{children:[e.jsx("strong",{children:s.service}),e.jsx("div",{className:"small faint",children:s.environment?`default environment: ${s.environment}`:`key #${s.id}`})]}),e.jsx("td",{onClick:i=>i.stopPropagation(),style:{maxWidth:340},children:T===s.id?e.jsxs("div",{className:"row",style:{flexWrap:"nowrap"},children:[e.jsx("input",{className:"mono",autoFocus:!0,value:j,onChange:i=>S(i.target.value),placeholder:"https://shop.example.com, https://*.example.com",style:{minWidth:0,flex:1}}),e.jsx("button",{className:"small primary",onClick:()=>$(s),children:"Save"}),e.jsx("button",{className:"small",onClick:()=>y(void 0),children:"Cancel"})]}):e.jsx("button",{className:"link small",onClick:()=>{y(s.id),S((s.origins??[]).join(", "))},children:(c=s.origins)!=null&&c.length?s.origins.join(", "):"any site (set origins)"})}),e.jsx("td",{className:"small muted nowrap",children:s.last_seen?A(s.last_seen):"never"}),e.jsxs("td",{className:"nowrap",style:{textAlign:"right"},onClick:i=>i.stopPropagation(),children:[e.jsx("button",{className:"small",onClick:()=>l(s.dsn,"Sentry DSN"),children:"Copy DSN"})," ",e.jsx("button",{className:"small",onClick:()=>E(s),children:"Replace key"})," ",e.jsx("button",{className:"small danger",onClick:()=>L(s),children:"Delete"})]})]},s.id)})})]}),e.jsxs("form",{className:"row",onSubmit:O,style:{alignItems:"flex-end"},children:[e.jsxs("label",{className:"field",style:{margin:0},children:["Service",e.jsx("input",{list:"integration-services",value:m,onChange:s=>d(s.target.value),placeholder:"web-frontend",style:{width:200}}),e.jsx("datalist",{id:"integration-services",children:(g.data??[]).map(s=>e.jsx("option",{value:s.name},s.name))})]}),e.jsxs("label",{className:"field",style:{margin:0},children:["Default environment ",e.jsx("span",{className:"faint",children:"(optional)"}),e.jsx("input",{value:x,onChange:s=>f(s.target.value),placeholder:"production",style:{width:160}})]}),e.jsx("button",{className:"primary",disabled:!m.trim(),children:"Create key"})]})]})}function u(n,a){const t=new URL(n.dsn);return`${t.protocol}//${t.host}/ingest/${n.id}/${n.key}/${a}`}function I(n,a){const t=a.environment||"production";switch(n){case"javascript":return`import * as Sentry from "@sentry/browser"; // or @sentry/node, @sentry/react, …

Sentry.init({
  dsn: "${a.dsn}",
  environment: "${t}",
  integrations: [Sentry.browserTracingIntegration()],
  tracesSampleRate: 0.2, // transactions become traces
  enableLogs: true,      // Sentry logs become log lines
  // Continue the trace in your backends: Sentry sends sentry-trace,
  // OpenTelemetry services read the W3C traceparent. Send both to your API:
  tracePropagationTargets: [/^https://api.example.com/],
  propagateTraceparent: true,
});`;case"python":return`import sentry_sdk

sentry_sdk.init(
    dsn="${a.dsn}",
    environment="${t}",
    traces_sample_rate=0.2,
    enable_logs=True,
    # If the brotli package is installed, Sentinel needs gzip instead:
    # _experiments={"transport_compression_algo": "gzip"},
)`;case"go":return`sentry.Init(sentry.ClientOptions{
	Dsn:              "${a.dsn}",
	Environment:      "${t}",
	EnableTracing:    true,
	TracesSampleRate: 0.2,
})`;default:return`SENTRY_DSN=${a.dsn}
SENTRY_ENVIRONMENT=${t}`}}const w=[{value:"sentry",label:"Sentry SDK",what:"Keep the Sentry SDK and swap the DSN. Errors become issues, transactions traces, Sentry logs log lines; replays, profiles and sessions are dropped."},{value:"faro",label:"Grafana Faro",what:"Grafana's open-source frontend SDK: console logs, uncaught errors and rejections, Web Vitals (as metrics, per page), page events and, with its tracing package, page loads and fetches as spans that continue in your backends."},{value:"datadog",label:"Datadog Browser Logs",what:"Keep @datadog/browser-logs and point its proxy at Sentinel. Logs, console and network errors arrive with their view, user and error details."},{value:"http",label:"HTTP / JSON",what:"Anything that can POST JSON: fetch, navigator.sendBeacon, logger transports (pino, winston, loglevel), Vector, Fluent Bit. One object, an array, or one per line."}];function B(n,a,t){const o=t.environment||"production";switch(n){case"sentry":return I(a,t);case"faro":return`import { initializeFaro, getWebInstrumentations } from "@grafana/faro-web-sdk";
import { TracingInstrumentation } from "@grafana/faro-web-tracing";

initializeFaro({
  url: "${u(t,"faro")}",
  app: { name: "${t.service}", version: "1.0.0", environment: "${o}" },
  instrumentations: [
    ...getWebInstrumentations(),
    // Page loads and fetches become spans, and a W3C traceparent header goes
    // to your API so its spans join the same trace. List your API's origins:
    new TracingInstrumentation({
      instrumentationOptions: { propagateTraceHeaderCorsUrls: [/^https://api.example.com/] },
    }),
  ],
});`;case"datadog":return`import { datadogLogs } from "@datadog/browser-logs";

datadogLogs.init({
  clientToken: "unused", // required by the SDK, ignored by Sentinel
  proxy: "${u(t,"datadog")}",
  service: "${t.service}",
  env: "${o}",
  forwardErrorsToLogs: true,
  forwardConsoleLogs: ["error", "warn"],
  sessionSampleRate: 100,
});

datadogLogs.logger.info("Button clicked", { cart: 3 });`;case"http":return`// From the browser (also on page unload):
navigator.sendBeacon("${u(t,"logs")}",
  JSON.stringify({ message: "checkout opened", level: "info", user: { id: 7 } }));

// Or with fetch; errors with a stack become issues:
fetch("${u(t,"logs")}", {
  method: "POST",
  body: JSON.stringify({ message: err.message, level: "error",
    error: { name: err.name, message: err.message, stack: err.stack } }),
  keepalive: true,
});

# From a shell or a log shipper:
curl -X POST "${u(t,"logs")}" -d '{"msg":"deploy done","level":"info"}'`}}function F({k:n}){const a=v(),[t,o]=p.useState("sentry"),[r,l]=p.useState("javascript"),g=w.find(d=>d.value===t),m=B(t,r,n);return e.jsxs("div",{className:"card",children:[e.jsx("div",{className:"card-head",children:e.jsxs("h2",{children:["Send from ",e.jsx("span",{className:"mono",children:n.service})]})}),e.jsxs("div",{className:"row",style:{marginBottom:10},children:[e.jsx(b,{value:t,onChange:o,options:w.map(d=>({value:d.value,label:d.label}))}),t==="sentry"&&e.jsx(b,{value:r,onChange:l,options:[{value:"javascript",label:"JavaScript"},{value:"python",label:"Python"},{value:"go",label:"Go"},{value:"env",label:"Env"}]})]}),e.jsx("p",{className:"small muted",style:{margin:"0 0 8px"},children:g.what}),e.jsxs("div",{className:"row",style:{marginBottom:6},children:[e.jsx("span",{className:"small faint",children:t==="sentry"?"Every Sentry SDK takes a DSN, so Java, .NET, PHP, Ruby and Rust work the same.":"Events show up under this service."}),e.jsx("button",{className:"small",style:{marginLeft:"auto"},onClick:()=>a(m,"Snippet"),children:"Copy"})]}),e.jsx("pre",{className:"code",children:m})]})}function J({v:n}){var l;const a=v(),o=((l=N(()=>h.get("/api/access"),[]).data)==null?void 0:l.ingest_token)??"<ingest token>",r=`OTEL_EXPORTER_OTLP_ENDPOINT=${n.otlp.endpoint}
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer%20${o}
OTEL_SERVICE_NAME=<your service>`;return e.jsxs("div",{className:"card",children:[e.jsxs("div",{className:"card-head",children:[e.jsx("h2",{children:"OpenTelemetry (servers)"}),e.jsx("span",{className:"badge",children:"OTLP/HTTP"})]}),e.jsxs("p",{className:"small muted",children:["Logs, traces and metrics over OTLP/HTTP (protobuf or JSON) at ",e.jsxs("code",{children:[n.otlp.endpoint,"/v1/logs"]}),", ",e.jsx("code",{children:"/v1/traces"})," and"," ",e.jsx("code",{children:"/v1/metrics"}),", with the secret ingest token from ",e.jsx(C,{to:"/settings/access",children:"Settings → Access"}),". Keep it on servers; browsers use a key above. Every OpenTelemetry SDK reads these:"]}),e.jsx("div",{className:"row",style:{marginBottom:6},children:e.jsx("button",{className:"small",style:{marginLeft:"auto"},onClick:()=>a(r,"Settings"),children:"Copy"})}),e.jsx("pre",{className:"code",children:r}),e.jsxs("p",{className:"small faint",style:{marginTop:8},children:["Or run the OpenTelemetry Collector on each server with ",e.jsx("code",{children:"deploy/otel-collector-app-server.yaml"}),": it also sends the server's own CPU, memory and disk, and buffers when Sentinel is unreachable."]})]})}export{z as default};
