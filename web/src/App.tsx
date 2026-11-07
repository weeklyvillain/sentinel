import { createContext, useContext, useEffect } from "react";
import { NavLink, Route, Routes } from "react-router-dom";
import { api, Alarm, Analysis, Check, Me } from "./api";
import { Icon, Loading, Logo, useAsync } from "./components/ui";
import Overview from "./pages/Overview";
import Logs from "./pages/Logs";
import Issues, { IssueDetail } from "./pages/Issues";
import Metrics from "./pages/Metrics";
import Services from "./pages/Services";
import Alarms from "./pages/Alarms";
import Webhooks from "./pages/Webhooks";
import GitHub from "./pages/GitHub";
import Uptime from "./pages/Uptime";
import AI from "./pages/AI";

const MeCtx = createContext<Me | undefined>(undefined);
export const useMe = () => useContext(MeCtx)!;

export default function App() {
  const me = useAsync(() => api.get<Me>("/api/me"), []);
  useEffect(() => {
    const onUnauth = () => me.reload();
    window.addEventListener("sentinel:unauthorized", onUnauth);
    return () => window.removeEventListener("sentinel:unauthorized", onUnauth);
  }, [me]);

  if (!me.data) return me.error ? <div className="login"><div className="notice bad">{me.error}</div></div> : <Loading />;
  if (!me.data.signed_in) return <Login me={me.data} />;
  return (
    <MeCtx.Provider value={me.data}>
      <Shell />
    </MeCtx.Provider>
  );
}

function Login({ me }: { me: Me }) {
  const params = new URLSearchParams(location.search);
  return (
    <div className="login">
      <div className="card">
        <Logo className="logo" />
        <h1>Sentinel</h1>
        <p className="muted">Error grouping, logs, metrics and alerts for your OpenTelemetry services.</p>
        {params.get("error") && <div className="notice bad">{params.get("error")}</div>}
        {me.github_sign_in ? (
          <a className="btn primary" href="/auth/login" style={{ justifyContent: "center", width: "100%", marginTop: 12, textDecoration: "none" }}>
            <Icon.github /> Sign in with GitHub
          </a>
        ) : (
          <div className="notice warn" style={{ textAlign: "left", marginTop: 12 }}>
            GitHub sign-in is not configured. Set <code>GITHUB_APP_ID</code>, <code>GITHUB_APP_PRIVATE_KEY_FILE</code>, <code>GITHUB_APP_CLIENT_ID</code>{" "}
            and <code>GITHUB_APP_CLIENT_SECRET</code>, plus <code>ALLOWED_USERS</code> or <code>ALLOWED_ORG</code>.
          </div>
        )}
      </div>
    </div>
  );
}

function Shell() {
  const me = useMe();
  const alarms = useAsync(() => api.get<Alarm[]>("/api/alarms"), [], 30000);
  const firing = (alarms.data ?? []).filter((a) => a.enabled && a.state === "firing").length;
  const checks = useAsync(() => api.get<Check[]>("/api/checks"), [], 30000);
  const checksDown = (checks.data ?? []).filter((c) => c.enabled && c.state === "down").length;
  const analyses = useAsync(() => api.get<Analysis[]>("/api/analyses"), [], 15000);
  const analyzing = (analyses.data ?? []).filter((a) => a.status === "queued" || a.status === "running").length;
  const nav = [
    { to: "/", label: "Overview", icon: Icon.overview, end: true },
    { to: "/logs", label: "Logs", icon: Icon.logs },
    { to: "/issues", label: "Issues", icon: Icon.issues },
    { to: "/metrics", label: "Metrics", icon: Icon.metrics },
    { to: "/services", label: "Services", icon: Icon.services },
    { to: "/uptime", label: "Uptime", icon: Icon.uptime, count: checksDown },
    { to: "/alarms", label: "Alarms", icon: Icon.alarms, count: firing },
    { to: "/webhooks", label: "Webhooks", icon: Icon.webhooks },
    { to: "/ai", label: "AI", icon: Icon.ai, count: analyzing },
    { to: "/github", label: "GitHub", icon: Icon.github },
  ];
  const logout = async () => {
    await api.post("/auth/logout");
    location.href = "/";
  };
  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">
          <Logo /> Sentinel
        </div>
        <nav className="nav">
          {nav.map((n) => (
            <NavLink key={n.to} to={n.to} end={n.end}>
              <n.icon />
              {n.label}
              {n.count ? <span className="count">{n.count}</span> : null}
            </NavLink>
          ))}
        </nav>
        <div className="spacer" />
        {me.info.dry_run && <div className="badge warn" style={{ margin: "0 8px 8px" }}>DRY RUN — GitHub calls are logged</div>}
        <div className="user">
          {me.user?.avatar_url ? <img src={me.user.avatar_url} alt="" /> : null}
          <span className="name" title={me.user?.login}>{me.user?.name || me.user?.login}</span>
          {me.auth_mode === "github" && (
            <button className="small ghost" onClick={logout}>
              Sign out
            </button>
          )}
        </div>
      </aside>
      <main className="main">
        <Routes>
          <Route path="/" element={<Overview />} />
          <Route path="/logs" element={<Logs />} />
          <Route path="/issues" element={<Issues />} />
          <Route path="/issues/:id" element={<IssueDetail />} />
          <Route path="/metrics" element={<Metrics />} />
          <Route path="/services" element={<Services />} />
          <Route path="/uptime" element={<Uptime />} />
          <Route path="/alarms" element={<Alarms />} />
          <Route path="/webhooks" element={<Webhooks />} />
          <Route path="/ai" element={<AI />} />
          <Route path="/github" element={<GitHub />} />
          <Route path="*" element={<div className="empty">Page not found</div>} />
        </Routes>
      </main>
    </div>
  );
}
