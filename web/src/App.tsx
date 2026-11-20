import { lazy, Suspense, useEffect } from "react";
import { NavLink, Route, Routes } from "react-router-dom";
import { api, Me } from "./api";
import { Icon, Loading, Logo, Moved, Section, SectionTab, useAsync } from "./components/ui";
import { MeCtx, SharedData, useMe, useStatus } from "./shared";

// Each page is its own chunk, loaded the first time it is opened.
const Overview = lazy(() => import("./pages/Overview"));
const Logs = lazy(() => import("./pages/Logs"));
const Issues = lazy(() => import("./pages/Issues"));
const IssueDetail = lazy(() => import("./pages/Issues").then((m) => ({ default: m.IssueDetail })));
const Metrics = lazy(() => import("./pages/Metrics"));
const Services = lazy(() => import("./pages/Services"));
const Alarms = lazy(() => import("./pages/Alarms"));
const Webhooks = lazy(() => import("./pages/Webhooks"));
const GitHub = lazy(() => import("./pages/GitHub"));
const Uptime = lazy(() => import("./pages/Uptime"));
const AI = lazy(() => import("./pages/AI"));
const Dashboards = lazy(() => import("./pages/Dashboards"));
const DashboardView = lazy(() => import("./pages/Dashboards").then((m) => ({ default: m.DashboardView })));

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
  return (
    <SharedData>
      <ShellBody />
    </SharedData>
  );
}

function ShellBody() {
  const me = useMe();
  const status = useStatus().data;
  const firing = status?.firing_alarms.length ?? 0;
  const checksDown = status?.checks.down.length ?? 0;
  const analyzing = status?.analyzing ?? 0;
  const nav = [
    { to: "/", label: "Overview", icon: Icon.overview, end: true },
    { to: "/logs", label: "Logs", icon: Icon.logs },
    { to: "/issues", label: "Issues", icon: Icon.issues, count: analyzing },
    { to: "/dashboards", label: "Dashboards", icon: Icon.dashboards },
    { to: "/alerts", label: "Alerts", icon: Icon.alarms, count: firing + checksDown },
    { to: "/settings", label: "Settings", icon: Icon.settings },
  ];
  const dashTabs: SectionTab[] = [
    { path: "", label: "Dashboards", element: <Dashboards /> },
    { path: "explore", label: "Explore metrics", element: <Metrics /> },
  ];
  const alertTabs: SectionTab[] = [
    { path: "alarms", label: "Alarms", element: <Alarms />, count: firing },
    { path: "uptime", label: "Uptime", element: <Uptime />, count: checksDown },
  ];
  const settingsTabs: SectionTab[] = [
    { path: "services", label: "Services", element: <Services /> },
    { path: "github", label: "GitHub", element: <GitHub /> },
    { path: "webhooks", label: "Webhooks", element: <Webhooks /> },
    { path: "ai", label: "AI", element: <AI /> },
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
        <Suspense fallback={<Loading />}>
          <Routes>
            <Route path="/" element={<Overview />} />
            <Route path="/logs" element={<Logs />} />
            <Route path="/issues" element={<Issues />} />
            <Route path="/issues/:id" element={<IssueDetail />} />
            <Route path="/dashboards" element={<Section title="Dashboards" base="/dashboards" tab="" tabs={dashTabs} />} />
            <Route path="/dashboards/explore" element={<Section title="Dashboards" base="/dashboards" tab="explore" tabs={dashTabs} />} />
            <Route path="/dashboards/:id" element={<DashboardView />} />
            <Route path="/alerts/:tab?" element={<Section title="Alerts" base="/alerts" tabs={alertTabs} />} />
            <Route path="/settings/:tab?" element={<Section title="Settings" base="/settings" tabs={settingsTabs} />} />
            {/* Old addresses, kept working for bookmarks and links in issues. */}
            <Route path="/metrics" element={<Moved to="/dashboards/explore" />} />
            <Route path="/alarms" element={<Moved to="/alerts/alarms" />} />
            <Route path="/uptime" element={<Moved to="/alerts/uptime" />} />
            <Route path="/services" element={<Moved to="/settings/services" />} />
            <Route path="/github" element={<Moved to="/settings/github" />} />
            <Route path="/webhooks" element={<Moved to="/settings/webhooks" />} />
            <Route path="/ai" element={<Moved to="/settings/ai" />} />
            <Route path="*" element={<div className="empty">Page not found</div>} />
          </Routes>
        </Suspense>
      </main>
    </div>
  );
}
