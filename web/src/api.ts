// Typed client for the Sentinel dashboard API.

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (method !== "GET") headers["X-Sentinel-Request"] = "1";
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, {
    method,
    headers,
    credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let data: unknown = undefined;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    /* non-JSON */
  }
  if (!res.ok) {
    const msg = (data as { error?: string } | undefined)?.error ?? (text || res.statusText);
    if (res.status === 401) window.dispatchEvent(new Event("sentinel:unauthorized"));
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

export const api = {
  get: <T,>(path: string) => request<T>("GET", path),
  post: <T,>(path: string, body?: unknown) => request<T>("POST", path, body ?? {}),
  put: <T,>(path: string, body?: unknown) => request<T>("PUT", path, body ?? {}),
  patch: <T,>(path: string, body?: unknown) => request<T>("PATCH", path, body ?? {}),
  del: <T,>(path: string) => request<T>("DELETE", path),
};

export function qs(params: Record<string, string | number | boolean | undefined | null | string[]>): string {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === "" || v === false) continue;
    if (Array.isArray(v)) v.forEach((x) => x && u.append(k, x));
    else u.set(k, String(v));
  }
  const s = u.toString();
  return s ? "?" + s : "";
}

// --- types (mirror the Go structs) ---

export interface Me {
  auth_mode: "github" | "none";
  github_sign_in: boolean;
  signed_in: boolean;
  github_app: boolean;
  user?: { login: string; name?: string; avatar_url?: string };
  info: {
    dry_run: boolean;
    token_mode: boolean;
    env_default_repo?: string;
    ai_provider: string;
    log_retention: string;
    metric_retention: string;
    down_after: string;
    otlp_addr: string;
  };
}

export type GroupStatus = "pending" | "open" | "resolved" | "ignored";

export interface Group {
  id: number;
  fingerprint: string;
  service: string;
  env?: string;
  kind: "log" | "span" | "down";
  title: string;
  first_seen: string;
  last_seen: string;
  count: number;
  count_at_last_report: number;
  status: GroupStatus;
  issue_number?: number;
  issue_url?: string;
  repo?: string;
  block_reason?: string;
  last_reported_at?: string;
}

export interface SampleEvent {
  kind: string;
  service: string;
  environment?: string;
  time: string;
  severity?: string;
  message?: string;
  exception_type?: string;
  exception_message?: string;
  stacktrace?: string;
  span_name?: string;
  trace_id?: string;
  span_id?: string;
  attributes?: Record<string, string>;
}

export interface Service {
  name: string;
  env?: string;
  last_seen: string;
  down: boolean;
  down_since?: string;
  repo?: string;
  labels?: string[];
  managed: boolean;
  description?: string;
  /** File GitHub issues for this service. */
  issues: boolean;
  /** Flag the service as down when it goes quiet (added on the Uptime page). */
  watch: boolean;
  active_groups?: number;
}

export interface LogRecord {
  id: number;
  time: string;
  service: string;
  env?: string;
  kind: "log" | "span";
  severity: number;
  severity_text?: string;
  body: string;
  trace_id?: string;
  span_id?: string;
  attrs?: Record<string, string>;
  fingerprint?: string;
}

export interface Bucket {
  t: string;
  counts: Record<string, number>;
}

export interface MetricSeriesInfo {
  service: string;
  name: string;
  unit?: string;
  kind: string;
  description?: string;
  last_seen: string;
}

export interface Series {
  label: string;
  points: { t: string; v: number | null }[];
}

export interface Repo {
  full_name: string;
  installation_id: number;
  private: boolean;
  allowed: boolean;
}

export interface Installation {
  id: number;
  account: string;
  account_type: string;
  repo_selection: string;
  updated_at: string;
}

export interface GitHubStatus {
  mode: "app" | "token" | "none";
  dry_run: boolean;
  installations: Installation[];
  repos: Repo[];
  default_repo: string;
  env_default_repo?: string;
  setup_url: string;
  callback_url: string;
  app_id?: number;
  app_slug?: string;
}

export type AlarmKind = "log_count" | "metric" | "service_down";

export interface Alarm {
  id: number;
  name: string;
  enabled: boolean;
  kind: AlarmKind;
  service: string;
  query: string;
  min_severity: number;
  metric: string;
  agg: string;
  op: string;
  threshold: number;
  window_s: number;
  webhook_ids: number[];
  state: "ok" | "firing";
  last_value: number;
  last_eval?: string;
  last_changed?: string;
}

export interface AlarmEvent {
  id: number;
  alarm_id: number;
  time: string;
  state: string;
  value: number;
  message: string;
}

export interface Webhook {
  id: number;
  name: string;
  url: string;
  format: "json" | "slack" | "discord";
  events: string[];
  enabled: boolean;
  has_secret: boolean;
  created_at: string;
}

export interface Delivery {
  id: number;
  webhook_id: number;
  event: string;
  time: string;
  status: number;
  error?: string;
  duration_ms: number;
  attempt: number;
  payload?: string;
}

export interface Overview {
  from: string;
  to: string;
  bucket_ms: number;
  services: { total: number; up: number; down: number; never_seen: number };
  groups: Partial<Record<GroupStatus, number>>;
  issues_in_range: number;
  logs: number;
  errors: number;
  error_series: Bucket[];
  volume_series: Bucket[];
  recent_groups: Group[];
  firing_alarms: Alarm[];
  service_list: Service[];
}

export interface AISettings {
  provider: "anthropic" | "openai" | "none";
  model: string;
  base_url: string;
  timeout_s: number;
  has_api_key?: boolean;
  env_api_key?: boolean;
}

export interface AIPull {
  model: string;
  status: string;
  completed: number;
  total: number;
  done: boolean;
  error?: string;
  started_at: string;
}

export interface AIState {
  settings: AISettings;
  provider_name: string;
  pull?: AIPull | null;
}

export interface AIModel {
  name: string;
  size?: number;
}

export interface AIDraft {
  title: string;
  summary: string;
  likely_cause: string;
  next_steps: string[];
}

export interface AITestResult {
  provider: string;
  took_ms: number;
  draft?: AIDraft;
  error?: string;
}

export interface Analysis {
  id: number;
  group_id: number;
  status: "queued" | "running" | "done" | "failed";
  provider?: string;
  queued_at: string;
  started_at?: string;
  finished_at?: string;
  error?: string;
  draft?: string;
  service: string;
  group_title: string;
  group_status: GroupStatus;
  issue_url?: string;
}

export interface CheckStats {
  probes: number;
  failures: number;
  uptime_pct: number;
  avg_latency_ms: number;
}

export interface Check {
  id: number;
  name: string;
  url: string;
  method: "GET" | "HEAD";
  interval_s: number;
  timeout_s: number;
  expect_status: string;
  body_contains: string;
  service: string;
  failures_before_down: number;
  enabled: boolean;
  state: "pending" | "up" | "down";
  consecutive_failures: number;
  down_since?: string;
  last_checked?: string;
  last_status: number;
  last_latency_ms: number;
  last_error?: string;
  stats_24h?: CheckStats;
}

export interface CheckResult {
  t: string;
  ok: boolean;
  status: number;
  latency_ms: number;
  error?: string;
}
