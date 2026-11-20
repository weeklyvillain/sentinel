import { createContext, ReactNode, useCallback, useContext, useEffect, useRef, useState } from "react";
import { Navigate, NavLink, useLocation, useParams } from "react-router-dom";
import { RANGES } from "../util";
import type { GroupStatus } from "../api";

// --- data loading ---

export function useAsync<T>(fn: () => Promise<T>, deps: unknown[], pollMs = 0) {
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState<string | undefined>();
  const [loading, setLoading] = useState(true);
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const seq = useRef(0);
  const reload = useCallback(async () => {
    const my = ++seq.current;
    setLoading(true);
    try {
      const d = await fnRef.current();
      if (my === seq.current) {
        setData(d);
        setError(undefined);
      }
    } catch (e) {
      if (my === seq.current) setError((e as Error).message);
    } finally {
      if (my === seq.current) setLoading(false);
    }
  }, []);
  useEffect(() => {
    reload();
    if (!pollMs) return;
    const t = setInterval(() => document.visibilityState === "visible" && reload(), pollMs);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  return { data, error, loading, reload, setData };
}

/**
 * Runs a change and reports a failure as a toast, so callers skip the
 * try/catch. `ok` is toasted on success (a function gets the result).
 * Resolves to the result, or undefined when it failed.
 */
export function useAction() {
  const toast = useToast();
  return useCallback(
    async <T,>(fn: () => Promise<T>, ok?: string | ((r: T) => string)): Promise<T | undefined> => {
      try {
        const r = await fn();
        if (ok) toast(typeof ok === "function" ? ok(r) : ok);
        return r;
      } catch (e) {
        toast((e as Error).message, true);
        return undefined;
      }
    },
    [toast],
  );
}

// --- toast ---

const ToastCtx = createContext<(msg: string, bad?: boolean) => void>(() => {});
export const useToast = () => useContext(ToastCtx);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toast, setToast] = useState<{ msg: string; bad?: boolean } | null>(null);
  const timer = useRef<number>();
  const show = useCallback((msg: string, bad?: boolean) => {
    setToast({ msg, bad });
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setToast(null), 3500);
  }, []);
  return (
    <ToastCtx.Provider value={show}>
      {children}
      {toast && (
        <div className={"toast" + (toast.bad ? " bad" : "")} role="status">
          {toast.msg}
        </div>
      )}
    </ToastCtx.Provider>
  );
}

// --- primitives ---

export function Modal(props: { title: string; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean | "x" }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && props.onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [props]);
  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && props.onClose()}>
      <div className={"modal" + (props.wide === "x" ? " xwide" : props.wide ? " wide" : "")} role="dialog" aria-label={props.title}>
        <div className="modal-head">
          <h2>{props.title}</h2>
          <button className="ghost icon" onClick={props.onClose} aria-label="Close">
            <Icon.x />
          </button>
        </div>
        <div className="modal-body">{props.children}</div>
        {props.footer && <div className="modal-foot">{props.footer}</div>}
      </div>
    </div>
  );
}

export function Switch(props: { checked: boolean; onChange: (v: boolean) => void; disabled?: boolean; label?: string }) {
  return (
    <label className="switch" title={props.label}>
      <input type="checkbox" checked={props.checked} disabled={props.disabled} aria-label={props.label} onChange={(e) => props.onChange(e.target.checked)} />
      <span />
    </label>
  );
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <div className="title">{title}</div>
      {children && <div>{children}</div>}
    </div>
  );
}

export function ErrorNote({ error }: { error?: string }) {
  if (!error) return null;
  return <div className="notice bad">{error}</div>;
}

export function Loading() {
  return (
    <div className="empty">
      <span className="spinner" />
    </div>
  );
}

export function RangePicker({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <div className="seg" role="group" aria-label="Time range">
      {RANGES.map((r) => (
        <button key={r.value} className={value === r.value ? "on" : ""} onClick={() => onChange(r.value)}>
          {r.label}
        </button>
      ))}
    </div>
  );
}

export function Seg<T extends string>({ value, options, onChange }: { value: T; options: { value: T; label: ReactNode }[]; onChange: (v: T) => void }) {
  return (
    <div className="seg">
      {options.map((o) => (
        <button key={o.value} className={value === o.value ? "on" : ""} onClick={() => onChange(o.value)}>
          {o.label}
        </button>
      ))}
    </div>
  );
}

/**
 * A page made of tabs, one per sub-page, routed as `${base}/${tab.path}`
 * (a tab with path "" lives at `base` itself). The tab comes from the
 * `:tab` route parameter unless the route passes it.
 */
export interface SectionTab {
  path: string;
  label: string;
  element: ReactNode;
  count?: number;
}

const tabURL = (base: string, path: string) => (path ? `${base}/${path}` : base);

export function Section({ title, base, tabs, tab }: { title: string; base: string; tabs: SectionTab[]; tab?: string }) {
  const params = useParams();
  const want = tab ?? params.tab ?? "";
  const current = tabs.find((t) => t.path === want);
  if (!current) return <Navigate to={tabURL(base, tabs[0].path)} replace />;
  return (
    <>
      <div className="page-head section-head">
        <h1>{title}</h1>
      </div>
      <nav className="tabs" aria-label={title}>
        {tabs.map((t) => (
          <NavLink key={t.path} to={tabURL(base, t.path)} end>
            {t.label}
            {t.count ? <span className="count">{t.count}</span> : null}
          </NavLink>
        ))}
      </nav>
      {current.element}
    </>
  );
}

/** Redirects an old URL to its new place, keeping the query string. */
export function Moved({ to }: { to: string }) {
  const { search } = useLocation();
  return <Navigate to={to + search} replace />;
}

/** Milliseconds of a relative range such as "15m", "6h" or "7d". */
export function rangeMs(r: string): number {
  const m = /^(\d+)([mhd])$/.exec(r);
  if (!m) return 3600_000;
  return +m[1] * { m: 60_000, h: 3600_000, d: 86400_000 }[m[2] as "m" | "h" | "d"];
}

/** The current address with a relative ?range= turned into fixed from/to. */
export function pinnedURL(): string {
  const u = new URL(location.href);
  const r = u.searchParams.get("range");
  if (r && !u.searchParams.get("from")) {
    const to = Date.now();
    u.searchParams.delete("range");
    u.searchParams.set("from", String(to - rangeMs(r)));
    u.searchParams.set("to", String(to));
  }
  return u.toString();
}

/**
 * Copies the current address (filters, range and all). With pin, a second
 * button copies it with the time range fixed, so the link shows exactly
 * what you see now instead of "the last hour" whenever it is opened.
 */
export function CopyLink({ label = "Copy link", pin = false }: { label?: string; pin?: boolean }) {
  const toast = useToast();
  const copy = async (url: string, what: string) => {
    try {
      await navigator.clipboard.writeText(url);
      toast(what);
    } catch {
      toast("Could not copy; copy the address bar instead", true);
    }
  };
  return (
    <span className="copy-link">
      <button className="ghost" onClick={() => copy(location.href, "Link copied. It opens this view with the same filters and time range.")} title="Copy a link to this view">
        <Icon.link /> {label}
      </button>
      {pin && (
        <button className="ghost icon" onClick={() => copy(pinnedURL(), "Link copied with this exact time window.")} title="Copy a link to this exact time window" aria-label="Copy a link to this exact time window">
          <Icon.clock />
        </button>
      )}
    </span>
  );
}

/** Query parameters for a range: "abs:<from>-<to>" (ms) or a relative one. */
export function rangeParams(range: string): { range?: string; from?: string; to?: string } {
  const m = /^abs:(\d+)-(\d+)$/.exec(range);
  return m ? { from: m[1], to: m[2] } : { range };
}

/** The range a page shows: fixed from/to in the URL win over ?range=. */
export function pageRange(params: URLSearchParams, def: string): string {
  const from = params.get("from");
  const to = params.get("to");
  if (from && to && /^\d+$/.test(from) && /^\d+$/.test(to)) return `abs:${from}-${to}`;
  return params.get("range") ?? def;
}

export function RangeLabel({ range }: { range: string }) {
  const m = /^abs:(\d+)-(\d+)$/.exec(range);
  if (!m) return null;
  const f = (ms: string) => new Date(+ms).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
  return <span className="badge accent" title="A fixed time window from a shared link">{f(m[1])} – {f(m[2])}</span>;
}

export function StatusBadge({ status }: { status: GroupStatus }) {
  const cls = { pending: "warn", open: "err", resolved: "ok", ignored: "" }[status];
  return <span className={"badge " + cls}>{status}</span>;
}

export function Stat(props: { label: string; value: ReactNode; hint?: ReactNode; tone?: "ok" | "bad" }) {
  return (
    <div className={"card stat" + (props.tone === "bad" ? " bad" : "")}>
      <div className="label">{props.label}</div>
      <div className="value">{props.value}</div>
      {props.hint && <div className="hint">{props.hint}</div>}
    </div>
  );
}

// --- icons (inline, stroke-based) ---

const I = (d: string) => () => (
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    <path d={d} />
  </svg>
);
export const Icon = {
  overview: I("M3 13h8V3H3zM13 21h8V11h-8zM3 21h8v-6H3zM13 3v6h8V3z"),
  logs: I("M4 6h16M4 12h16M4 18h10"),
  issues: I("M12 8v5M12 16h.01M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"),
  metrics: I("M3 3v18h18M7 15l4-4 3 3 5-6"),
  services: I("M5 12h14M5 12a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v4a2 2 0 0 1-2 2M5 12a2 2 0 0 0-2 2v4a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-4a2 2 0 0 0-2-2M7 8h.01M7 16h.01"),
  alarms: I("M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9M13.7 21a2 2 0 0 1-3.4 0"),
  webhooks: I("M18 16.98h-5.99c-1.1 0-1.95.94-2.48 1.9A4 4 0 0 1 2 17c.01-.7.2-1.4.57-2M6 17l3.13-5.78c.53-.97.1-2.18-.5-3.1a4 4 0 1 1 6.89-4.06M12 6l3.13 5.73C15.66 12.7 16.9 13 18 13a4 4 0 0 1 0 8"),
  github: I("M9 19c-5 1.5-5-2.5-7-3m14 6v-3.87a3.37 3.37 0 0 0-.94-2.61c3.14-.35 6.44-1.54 6.44-7A5.44 5.44 0 0 0 20 4.77 5.07 5.07 0 0 0 19.91 1S18.73.65 16 2.48a13.38 13.38 0 0 0-7 0C6.27.65 5.09 1 5.09 1A5.07 5.07 0 0 0 5 4.77a5.44 5.44 0 0 0-1.5 3.78c0 5.42 3.3 6.61 6.44 7A3.37 3.37 0 0 0 9 18.13V22"),
  search: I("M21 21l-4.35-4.35M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16z"),
  uptime: I("M22 12h-4l-3 9L9 3l-3 9H2"),
  dashboards: I("M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z"),
  settings: I("M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6"),
  traces: I("M3 6h10M7 12h10M11 18h10M3 6v0M7 12v0"),
  setup: I("M9 11l3 3L22 4M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"),
  x: I("M18 6 6 18M6 6l12 12"),
  check: I("M20 6 9 17l-5-5"),
  link: I("M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7-7l-1.7 1.7M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7 7l1.7-1.7"),
  clock: I("M12 7v5l3 2M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z"),
  edit: I("M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z"),
  wrench: I("M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.8-3.8a6 6 0 0 1-7.9 7.9l-6.9 6.9a2.1 2.1 0 0 1-3-3l6.9-6.9a6 6 0 0 1 7.9-7.9z"),
  snooze: I("M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"),
  up: I("M18 15l-6-6-6 6"),
  down: I("M6 9l6 6 6-6"),
  right: I("M9 18l6-6-6-6"),
  external: I("M15 3h6v6M10 14 21 3M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"),
  ai: I("M12 3l1.9 5.8L20 10l-6.1 1.2L12 17l-1.9-5.8L4 10l6.1-1.2zM19 15l.8 2.2L22 18l-2.2.8L19 21l-.8-2.2L16 18l2.2-.8z"),
};

/**
 * The mark: a lens with an off-centre pupil, watching. Drawn in
 * currentColor so it follows the ink of whatever it sits on.
 */
export function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" className={className} aria-hidden>
      <rect x="1" y="1" width="22" height="22" rx="6" fill="currentColor" />
      <circle cx="12" cy="12" r="6" fill="none" stroke="var(--on-ink, #fff)" strokeWidth="2" />
      <circle cx="14" cy="10" r="2.25" fill="var(--on-ink, #fff)" />
    </svg>
  );
}
