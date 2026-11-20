import { Icon, Logo } from "./ui";

// The right half of the sign-in and setup screens: a drawing of what
// Sentinel does (dashboards, alarms, logs, automated fixes). Decorative and
// made up; it never shows real data.

const LATENCY = [38, 41, 36, 44, 40, 47, 43, 52, 49, 58, 54, 61, 92, 118, 104, 71, 63, 57, 60, 55, 52, 56, 50, 48];

function Chart() {
  const w = 300;
  const h = 86;
  const max = 130;
  const x = (i: number) => (i / (LATENCY.length - 1)) * w;
  const y = (v: number) => h - (v / max) * h;
  const line = LATENCY.map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(" ");
  return (
    <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className="sc-chart">
      {[0.25, 0.5, 0.75].map((f) => (
        <line key={f} x1="0" x2={w} y1={h * f} y2={h * f} className="sc-grid" />
      ))}
      <line x1="0" x2={w} y1={y(90)} y2={y(90)} className="sc-threshold" />
      <path d={`${line} L${w},${h} L0,${h} Z`} className="sc-area" />
      <path d={line} className="sc-line" />
      <circle cx={x(13)} cy={y(118)} r="3.5" className="sc-dot" />
    </svg>
  );
}

export function Showcase() {
  return (
    <aside className="showcase" aria-hidden>
      <div className="sc-intro">
        <h2>From error to pull request</h2>
        <p>Sentinel watches your services' logs, traces and metrics, files one GitHub issue per problem, and lets Claude Code fix it.</p>
      </div>
      <div className="sc-stack">
        <div className="sc-card sc-dash">
          <div className="sc-head">
            <Icon.dashboards />
            <span>checkout · p95 latency</span>
            <span className="sc-num">118 ms</span>
          </div>
          <Chart />
        </div>

        <div className="sc-card sc-alarm">
          <span className="sc-pulse" />
          <div>
            <div className="sc-title">p95 latency above 90 ms</div>
            <div className="sc-meta">checkout · firing for 4 min · webhook sent</div>
          </div>
          <span className="badge bad">firing</span>
        </div>

        <div className="sc-card sc-logs">
          <div className="sc-head">
            <Icon.logs />
            <span>Logs</span>
          </div>
          <div className="sc-log">
            <span className="sc-t">12:04:31</span>
            <span className="lvl info">info</span> order 81723 created
          </div>
          <div className="sc-log">
            <span className="sc-t">12:04:32</span>
            <span className="lvl error">error</span> payment capture failed: statement timeout
          </div>
          <div className="sc-log">
            <span className="sc-t">12:04:32</span>
            <span className="lvl warn">warn</span> retrying capture (2/3)
          </div>
        </div>

        <div className="sc-card sc-fix">
          <div className="sc-head">
            <Icon.github />
            <span>Fixer opened a pull request</span>
          </div>
          <div className="sc-title">Retry payment capture on statement timeout</div>
          <div className="sc-meta">
            <span className="badge ok">open</span> Fixes #136 · <span className="sc-add">+24</span> <span className="sc-del">−6</span> · tests pass
          </div>
        </div>
      </div>
      <ul className="sc-points">
        <li>
          <Icon.issues /> Errors grouped into one issue each
        </li>
        <li>
          <Icon.alarms /> Alarms, uptime checks and SLOs
        </li>
        <li>
          <Icon.ai /> Automated fixes with Claude Code
        </li>
      </ul>
    </aside>
  );
}

// AuthLayout is the split screen: the sign-in or setup panel on the left,
// the showcase on the right (hidden on narrow screens).
export function AuthLayout({ children, wide }: { children: React.ReactNode; wide?: boolean }) {
  return (
    <div className="auth">
      <main className="auth-main">
        <div className="auth-brand">
          <Logo /> Sentinel
        </div>
        <div className={"auth-body" + (wide ? " wide" : "")}>{children}</div>
        <div className="auth-foot">{location.host}</div>
      </main>
      <Showcase />
    </div>
  );
}
