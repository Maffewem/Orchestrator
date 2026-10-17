// Typed client for the Orchestrator control-plane API.

export interface DockerInfo {
  version: string;
  api_version: string;
  containers_running: number;
  containers_total: number;
  images: number;
  error?: string;
}

export interface HostInfo {
  hostname: string;
  os: string;
  platform: string;
  arch: string;
  kernel: string;
  model?: string;
  addresses?: string[];
  cpus: number;
  memory_total: number;
  agent_version: string;
  docker?: DockerInfo;
}

export interface Metrics {
  cpu_percent: number;
  memory_used: number;
  memory_total: number;
  disk_used: number;
  disk_total: number;
  load1: number;
  load5: number;
  load15: number;
  uptime_seconds: number;
}

export interface Port {
  ip?: string;
  private_port: number;
  public_port?: number;
  type: string;
}

export interface Container {
  id: string;
  name: string;
  image: string;
  state: string;
  status: string;
  created: number;
  ports?: Port[];
  labels?: Record<string, string>;
  compose_project?: string;
}

export type Health = "ok" | "warning" | "critical";

export interface Device {
  id: string;
  name: string;
  labels: string[];
  host: HostInfo;
  metrics: Metrics;
  containers: Container[];
  created_at: string;
  last_seen: string;
  status: "online" | "offline";
  health: Health;
  issues: string[];
}

export interface FleetContainer extends Container {
  device_id: string;
  device_name: string;
  device_status: string;
  device_addresses: string[];
  project_id?: string;
  project_name?: string;
}

export interface Command {
  id: string;
  device_id: string;
  action: string;
  target: string;
  status: "pending" | "dispatched" | "succeeded" | "failed";
  output?: string;
  error?: string;
}

export interface Source {
  type: "github" | "image";
  repo?: string;
  branch?: string;
  context?: string;
  dockerfile?: string;
  auto_deploy: boolean;
  image?: string;
}

export interface Runtime {
  ports: string[];
  env: string[];
  command?: string[];
  restart: string;
}

export interface Commit {
  repo: string;
  branch: string;
  sha: string;
  message: string;
  author: string;
  url?: string;
}

export type DeploymentStatus = "queued" | "running" | "succeeded" | "failed";
export type StepStatus = "pending" | "running" | "succeeded" | "failed" | "skipped";

export interface Step {
  index: number;
  name: string;
  status: StepStatus;
  log: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface DeploySpec {
  service: string;
  image: string;
  ports?: string[];
  env?: string[];
  restart?: string;
  build?: { remote: string; dockerfile?: string };
}

export interface Deployment {
  id: string;
  project_id: string;
  device_id: string;
  trigger: "manual" | "push" | "retry" | "poll";
  commit: Commit | null;
  spec: DeploySpec;
  status: DeploymentStatus;
  error?: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  steps?: Step[];
}

export interface Project {
  id: string;
  name: string;
  device_id: string;
  device_name: string;
  source: Source;
  runtime: Runtime;
  created_at: string;
  updated_at: string;
  latest_deployment: Deployment | null;
}

export interface Event {
  id: number;
  device_id?: string;
  level: "info" | "warning" | "error";
  kind: string;
  message: string;
  created_at: string;
}

export interface Overview {
  devices: { total: number; online: number; offline: number; warning: number; critical: number };
  containers: { total: number; running: number; stopped: number };
  recent_events: Event[];
}

export interface MetricSample {
  time: string;
  cpu: number;
  mem: number;
  disk: number;
  load1: number;
}

export interface Settings {
  join_token: string;
  heartbeat_interval_seconds: number;
  version: string;
  github: { connected: boolean; webhook_path: string; webhook_secret: string };
}

export interface GitHubRepo {
  full_name: string;
  default_branch: string;
  private: boolean;
  html_url: string;
}

const TOKEN_KEY = "orchestrator.adminToken";

export function getToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

export function setToken(token: string | null) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* storage unavailable: the session just won't persist */
  }
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

let onUnauthorized: () => void = () => {};
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

export async function request<T>(method: string, path: string, body?: unknown, token = getToken()): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    onUnauthorized();
    throw new ApiError(401, "Your session has expired");
  }
  if (!res.ok) {
    let msg = res.statusText;
    try {
      msg = (await res.json()).error ?? msg;
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, msg);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

export const api = {
  checkToken: (token: string) => request<void>("GET", "/api/auth/check", undefined, token),
  overview: () => request<Overview>("GET", "/api/overview"),
  devices: () => request<Device[]>("GET", "/api/devices"),
  device: (id: string) => request<Device>("GET", `/api/devices/${id}`),
  deviceMetrics: (id: string) => request<MetricSample[]>("GET", `/api/devices/${id}/metrics`),
  updateDevice: (id: string, patch: { name?: string; labels?: string[] }) =>
    request<Device>("PATCH", `/api/devices/${id}`, patch),
  deleteDevice: (id: string) => request<void>("DELETE", `/api/devices/${id}`),
  containers: () => request<FleetContainer[]>("GET", "/api/containers"),
  command: (deviceId: string, action: string, target: string, args?: Record<string, string>) =>
    request<Command>("POST", `/api/devices/${deviceId}/commands`, { action, target, args }),
  getCommand: (id: string) => request<Command>("GET", `/api/commands/${id}`),
  events: (device?: string, limit = 100) =>
    request<Event[]>("GET", `/api/events?limit=${limit}${device ? `&device=${device}` : ""}`),
  projects: () => request<Project[]>("GET", "/api/projects"),
  project: (id: string) => request<Project>("GET", `/api/projects/${id}`),
  createProject: (p: ProjectInput) => request<Project>("POST", "/api/projects", p),
  updateProject: (id: string, p: Partial<ProjectInput>) => request<Project>("PATCH", `/api/projects/${id}`, p),
  deleteProject: (id: string) => request<void>("DELETE", `/api/projects/${id}`),
  deployProject: (id: string) => request<Deployment>("POST", `/api/projects/${id}/deployments`),
  projectDeployments: (id: string) => request<Deployment[]>("GET", `/api/projects/${id}/deployments`),
  deployments: (q: { device?: string; limit?: number } = {}) =>
    request<Deployment[]>("GET", `/api/deployments?limit=${q.limit ?? 50}${q.device ? `&device=${q.device}` : ""}`),
  deployment: (id: string) => request<Deployment>("GET", `/api/deployments/${id}`),
  retryDeployment: (id: string) => request<Deployment>("POST", `/api/deployments/${id}/retry`),
  settings: () => request<Settings>("GET", "/api/settings"),
  setGitHubToken: (token: string) =>
    request<{ connected: boolean; login?: string }>("PUT", "/api/settings/github", { token }),
  rotateJoinToken: () => request<{ join_token: string }>("POST", "/api/settings/join-token/rotate"),
  githubRepos: () => request<GitHubRepo[]>("GET", "/api/github/repos"),
};

export interface ProjectInput {
  name: string;
  device_id: string;
  source: Source;
  runtime: Runtime;
}
