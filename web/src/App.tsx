import { lazy, Suspense, useEffect } from "react";
import { NavLink, Route, Routes } from "react-router-dom";
import { api, Me } from "./api";
import type { SetupState } from "./pages/Setup";
import { Bootstrap, Welcome } from "./pages/Welcome";
import { AuthLayout } from "./components/Showcase";
import { Icon, Loading, Logo, Moved, Section, SectionTab, useAsync } from "./components/ui";
import { MeCtx, SharedData, useMe, useStatus } from "./shared";

// Each page is its own chunk, loaded the first time it is opened.
const Overview = lazy(() => import("./pages/Overview"));
const Logs = lazy(() => import("./pages/Logs"));
const Issues = lazy(() => import("./pages/Issues"));
const Traces = lazy(() => import("./pages/Traces"));
const TraceView = lazy(() => import("./pages/Traces").then((m) => ({ default: m.TraceView })));
const SLOs = lazy(() => import("./pages/SLOs"));
const Silences = lazy(() => import("./pages/Silences"));
const IssueDetail = lazy(() => import("./pages/Issues").then((m) => ({ default: m.IssueDetail })));
const Metrics = lazy(() => import("./pages/Metrics"));
const Services = lazy(() => import("./pages/Services"));
const Alarms = lazy(() => import("./pages/Alarms"));
const Webhooks = lazy(() => import("./pages/Webhooks"));
const GitHub = lazy(() => import("./pages/GitHub"));
const Fixer = lazy(() => import("./pages/Fixer"));
const Setup = lazy(() => import("./pages/Setup"));
const Access = lazy(() => import("./pages/Access"));
const Integrations = lazy(() => import("./pages/Integrations"));
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
  if (!me.data.signed_in) return <SignedOut />;
  return (
    <MeCtx.Provider value={me.data}>
      <Shell />
    </MeCtx.Provider>
  );
}

// SignedOut is the first-start setup until Sentinel has its GitHub App and
// an admin, then the GitHub sign-in.
function SignedOut() {
  const boot = useAsync(() => api.get<Bootstrap>("/api/bootstrap"), []);
  if (!boot.data) return boot.error ? <div className="login"><div className="notice bad">{boot.error}</div></div> : <Loading />;
  if (boot.data.setup) return <Welcome boot={boot.data} onChange={boot.reload} />;
  return <Login />;
}

function Login() {
  const params = new URLSearchParams(location.search);
  return (
    <AuthLayout>
      <h1>Welcome back</h1>
      <p className="auth-sub">Sign in to see your services' errors, logs, alarms and fixes.</p>
      {params.get("error") && <div className="notice bad">{params.get("error")}</div>}
      <a className="btn github" href="/auth/login">
        <Icon.github /> Continue with GitHub
      </a>
      <p className="auth-note">Only people allowed under Settings → Access can sign in.</p>
    </AuthLayout>
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
  const setup = useAsync(() => api.get<SetupState>("/api/setup"), [], 60000);
  const setupLeft = setup.data?.remaining ?? 0;
  // Counts are neutral; only something actually broken (a firing alarm,
  // a failing check) is red.
  type NavItem = { to: string; label: string; icon: () => JSX.Element; end?: boolean; count?: number; bad?: boolean };
  const nav: NavItem[] = [
    { to: "/", label: "Overview", icon: Icon.overview, end: true },
    { to: "/issues", label: "Issues", icon: Icon.issues, count: analyzing },
    { to: "/logs", label: "Logs", icon: Icon.logs },
    { to: "/traces", label: "Traces", icon: Icon.traces },
    { to: "/dashboards", label: "Dashboards", icon: Icon.dashboards },
    { to: "/alerts", label: "Alerts", icon: Icon.alarms, count: firing + checksDown, bad: true },
  ];
  const navBottom: NavItem[] = [
    { to: "/setup", label: "Setup", icon: Icon.setup, count: setupLeft },
    { to: "/settings", label: "Settings", icon: Icon.settings },
  ];
  const link = (n: NavItem) => (
    <NavLink key={n.to} to={n.to} end={n.end}>
      <n.icon />
      {n.label}
      {n.count ? <span className={"count" + (n.bad ? " bad" : "")}>{n.count}</span> : null}
    </NavLink>
  );
  const dashTabs: SectionTab[] = [
    { path: "", label: "Dashboards", element: <Dashboards /> },
    { path: "explore", label: "Explore metrics", element: <Metrics /> },
  ];
  const alertTabs: SectionTab[] = [
    { path: "alarms", label: "Alarms", element: <Alarms />, count: firing },
    { path: "uptime", label: "Uptime", element: <Uptime />, count: checksDown },
    { path: "slos", label: "SLOs", element: <SLOs /> },
    { path: "silences", label: "Maintenance", element: <Silences /> },
  ];
  const settingsTabs: SectionTab[] = [
    { path: "services", label: "Services", element: <Services /> },
    { path: "integrations", label: "Integrations", element: <Integrations /> },
    { path: "access", label: "Access", element: <Access /> },
    { path: "github", label: "GitHub", element: <GitHub /> },
    { path: "fixer", label: "Fixer", element: <Fixer /> },
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
          <Logo /> <span>Sentinel</span>
        </div>
        <nav className="nav" aria-label="Main">
          {nav.map(link)}
          <div className="spacer" />
          <div className="nav-sep" />
          {navBottom.map(link)}
        </nav>
        {me.info.dry_run && (
          <div className="env-flag" title="Dry run: GitHub calls are logged instead of made">
            <span className="dot warn" style={{ animation: "none" }} />
            <span>Dry run · GitHub calls are logged</span>
          </div>
        )}
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
            <Route path="/traces" element={<Traces />} />
            <Route path="/traces/:id" element={<TraceView />} />
            <Route path="/dashboards" element={<Section title="Dashboards" base="/dashboards" tab="" tabs={dashTabs} />} />
            <Route path="/dashboards/explore" element={<Section title="Dashboards" base="/dashboards" tab="explore" tabs={dashTabs} />} />
            <Route path="/dashboards/:id" element={<DashboardView />} />
            <Route path="/alerts/:tab?" element={<Section title="Alerts" base="/alerts" tabs={alertTabs} />} />
            <Route path="/settings/:tab?" element={<Section title="Settings" base="/settings" tabs={settingsTabs} />} />
            <Route path="/setup" element={<Setup />} />
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
