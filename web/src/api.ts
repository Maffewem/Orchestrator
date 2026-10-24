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
  in_container?: boolean;
  capabilities?: string[];
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
  /** Named Docker volumes mounted in the container (not bind mounts). */
  volumes?: string[];
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
  proxy: "running" | "stopped" | "none" | "agent_too_old";
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

export interface NodeBuild {
  install?: string;
  build?: string;
  start?: string;
  version?: string;
  static?: boolean;
  output?: string;
}

export interface PreviewBuild {
  context?: string;
  build?: string;
  start?: string;
  include?: string[];
  exclude?: string[];
}

export interface Source {
  type: "github" | "image" | "compose";
  repo?: string;
  branch?: string;
  context?: string;
  dockerfile?: string;
  node?: NodeBuild;
  auto_deploy: boolean;
  include?: string[];
  exclude?: string[];
  preview?: PreviewBuild;
  image?: string;
  /** Compose source: the docker-compose YAML. */
  compose?: string;
  /** Compose source from GitHub: path of the compose file in repo. */
  compose_file?: string;
}

export interface Runtime {
  ports: string[];
  env: string[];
  command?: string[];
  restart: string;
  volumes?: string[];
  labels?: Record<string, string>;
  network?: string;
  /** Name of the container, when it differs from the project name (adopted containers). */
  container?: string;
  /** Created from a container that already existed; it keeps running until the first deployment. */
  adopted?: boolean;
  /** Settings of an adopted container that a redeploy would not carry over. */
  unsupported?: string[];
}

export interface AdoptItem {
  container_id: string;
  container_name: string;
  compose_project?: string;
  status: "ready" | "adopted" | "skipped" | "failed";
  reason?: string;
  project_name?: string;
  project_id?: string;
  image?: string;
  ports?: string[];
  volumes?: string[];
  env_count: number;
  warnings?: string[];
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
  compose?: { yaml: string; env?: string[] };
}

export interface TunnelRoute {
  hostname: string;
  /** A URL such as http://localhost:3000, or project:<name>. */
  service: string;
  /** Set when the route belongs to a project; kept as is when the device page saves routes. */
  project?: string;
}

export interface TunnelView {
  supported: boolean;
  enabled: boolean;
  state?: string;
  tunnel_id?: string;
  routes: TunnelRoute[];
  can_route: boolean;
  warnings?: string[];
}

export interface ProjectTunnelView extends TunnelView {
  device_id: string;
  /** TCP host ports the project publishes, including a stack's services. */
  ports: number[];
}

export interface Deployment {
  id: string;
  project_id: string;
  device_id: string;
  trigger: "manual" | "push" | "retry" | "poll" | "rollback";
  preview_id?: string;
  commit: Commit | null;
  spec: DeploySpec;
  status: DeploymentStatus;
  error?: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  steps?: Step[];
}

export interface Preview {
  id: string;
  project_id: string;
  branch: string;
  slug: string;
  pr: number;
  title: string;
  author: string;
  sha: string;
  host_port: number;
  deployment_id: string;
  created_at: string;
  updated_at: string;
  url: string;
  status: DeploymentStatus;
  error?: string;
}

export interface PreviewsView {
  settings: { enabled: boolean; max: number };
  previews: Preview[];
  supported: boolean;
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

export type Role = "viewer" | "operator" | "admin";

export interface Me {
  username: string;
  role: Role;
  token_login: boolean;
}

export interface LoginResult extends Me {
  token: string;
}

export interface User {
  id: string;
  username: string;
  role: Role;
  disabled: boolean;
  created_at: string;
}

export interface DnsRecord {
  type: string;
  name: string;
  value: string;
}

export interface DomainRow {
  id?: string;
  hostname: string;
  type: "domain" | "route";
  path?: string;
  zone: string;
  status: "active" | "pending" | "mismatch";
  detail: string;
  records: DnsRecord[];
}

export interface UrlRow {
  hostname: string;
  enabled: boolean;
  status?: DomainRow["status"];
  detail?: string;
  records?: DnsRecord[];
}

export interface DomainsView {
  base_domain: string;
  production: UrlRow | null;
  preview: UrlRow | null;
  domains: DomainRow[];
  addresses: string[];
  proxy: "ok" | "agent_too_old";
  protected: boolean;
  dns: "manual" | "cloudflare";
  ssl: "http" | "dns";
}

export interface Settings {
  base_domain: string;
  backup_password_set: boolean;
  cloudflare: { connected: boolean; proxied: boolean };
  ssl_mode: "http" | "dns";
  sso: SSOView;
  https: HTTPSView;
  security: { session_hours: number; min_password: number; force_https: boolean };
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

export async function request<T>(method: string, path: string, body?: unknown, token: string | null | undefined = getToken()): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401 && token) {
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

export interface StreamChunk {
  data: Uint8Array;
  next: number;
  closed: boolean;
  error?: string;
}

async function fetchStreamOutput(id: string, from: number, signal: AbortSignal): Promise<StreamChunk> {
  const token = getToken();
  const res = await fetch(`/api/streams/${id}/output?from=${from}`, { signal, headers: token ? { Authorization: `Bearer ${token}` } : {} });
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
  const j = (await res.json()) as { data?: string; next: number; closed?: boolean; error?: string };
  const bin = atob(j.data ?? "");
  const data = Uint8Array.from(bin, (c) => c.charCodeAt(0));
  return { data, next: j.next, closed: !!j.closed, error: j.error };
}

export interface Backup {
  id: string;
  device_id: string;
  device_name: string;
  container_name: string;
  image: string;
  source: "manual" | "scheduled";
  status: "pending" | "complete" | "failed";
  size: number;
  error?: string;
  encrypted: boolean;
  created_at: string;
}

/** Downloads a backup archive through an authenticated request and saves it. */
async function downloadBackup(b: Backup) {
  const res = await fetch(`/api/backups/${b.id}/download`, { headers: { Authorization: `Bearer ${getToken()}` } });
  if (!res.ok) {
    let msg = "Could not download the backup";
    try {
      msg = (await res.json()).error ?? msg;
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, msg);
  }
  const url = URL.createObjectURL(await res.blob());
  const a = document.createElement("a");
  a.href = url;
  a.download = `${b.container_name}-${b.created_at.slice(0, 19).replace(/[-:T]/g, "")}.tar.gz`;
  a.click();
  URL.revokeObjectURL(url);
}

export interface UpdateSchedule {
  mode: "off" | "daily" | "weekly";
  time: string;
  weekday: number;
  tz: string;
  agents: boolean;
}

export interface AgentProgress {
  state: "queued" | "downloading" | "restarting" | "done" | "failed";
  message: string;
  to: string;
  at: string;
}

export interface UpdateStatus {
  version: string;
  image?: string;
  can_update: boolean;
  reason?: string;
  busy?: "checking" | "updating";
  updating: boolean;
  stage?: "pulling" | "handover" | "restarting";
  check?: { checked_at: string; version?: string; newer: boolean; error?: string };
  result?: { at: string; ok: boolean; message: string };
  schedule: UpdateSchedule;
  next_run?: string;
  agents: { device_id: string; name: string; version: string; arch: string; online: boolean; in_container: boolean; outdated: boolean; can_update: boolean; progress?: AgentProgress }[];
}

export interface BackupPolicy {
  mode: "off" | "daily" | "weekly";
  time: string;
  weekday: number;
  tz: string;
  keep: number;
  next_run?: string;
  last_run?: string;
}

export interface ServerBackupPolicy {
  mode: "off" | "daily" | "weekly";
  time: string;
  weekday: number;
  tz: string;
  keep: number;
}
export interface ServerBackup {
  name: string;
  size: number;
  encrypted: boolean;
  created_at: string;
}
export interface SSOConfig {
  github: { enabled: boolean; client_id: string; client_secret?: string; orgs: string[]; default_role: string };
  oidc: { enabled: boolean; name: string; issuer: string; client_id: string; client_secret?: string; domains: string[]; default_role: string; group_claim: string; admin_group: string; operator_group: string };
  hide_password: boolean;
}
export type SSOView = SSOConfig & { github_secret_set: boolean; oidc_secret_set: boolean };

export interface HTTPSView {
  mode: "off" | "upload" | "auto";
  host: string;
  addr: string;
  hsts: boolean;
  listening: boolean;
  error?: string;
  cert_set: boolean;
  subject?: string;
  expires?: string;
}

export interface BackupDest {
  id: string;
  kind: "s3" | "folder" | "device";
  name: string;
  enabled: boolean;
  endpoint?: string;
  region?: string;
  bucket?: string;
  prefix?: string;
  access_key?: string;
  secret_key?: string;
  secret_set?: boolean;
  path?: string;
  device_id?: string;
  last_at?: string;
  last_error?: string;
}
export interface ServerBackups {
  destinations: BackupDest[];
  policy: ServerBackupPolicy;
  next_run?: string;
  backups: ServerBackup[];
  password_set: boolean;
  dir: string;
}

/** Downloads a server backup exactly as stored (protected ones stay protected). */
async function downloadServerBackup(name: string) {
  const res = await fetch(`/api/server-backups/${encodeURIComponent(name)}/download`, { headers: { Authorization: `Bearer ${getToken()}` } });
  if (!res.ok) throw new ApiError(res.status, "Could not download the backup");
  const url = URL.createObjectURL(await res.blob());
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

/** Restores the server from an uploaded backup file. */
async function restoreServerUpload(file: File, encrypted: boolean) {
  const res = await fetch(`/api/server-backups/restore${encrypted ? "?encrypted=1" : ""}`, {
    method: "POST",
    headers: { Authorization: `Bearer ${getToken()}` },
    body: file,
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      msg = (await res.json()).error ?? msg;
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, msg);
  }
}

export const api = {
  setBackupDests: (d: BackupDest[]) => request<ServerBackups>("PUT", "/api/server-backups/destinations", d),
  testBackupDest: (id: string) => request<{ status: string }>("POST", `/api/server-backups/destinations/${encodeURIComponent(id)}/test`),
  ssoProviders: () => request<{ providers: { id: string; name: string }[]; hide_password: boolean }>("GET", "/api/auth/sso/providers", undefined, null),
  ssoExchange: (code: string) => request<LoginResult>("POST", "/api/auth/sso/exchange", { code }, null),
  setSSO: (c: SSOConfig) => request<SSOView>("PUT", "/api/settings/sso", c),
  setHTTPS: (b: { mode: string; host: string; addr: string; hsts: boolean; cert_pem?: string; key_pem?: string }) => request<HTTPSView>("PUT", "/api/settings/https", b),
  serverBackups: () => request<ServerBackups>("GET", "/api/server-backups"),
  createServerBackup: () => request<ServerBackup>("POST", "/api/server-backups"),
  setServerBackupPolicy: (p: ServerBackupPolicy) => request<ServerBackups>("PUT", "/api/server-backups/policy", p),
  deleteServerBackup: (name: string) => request<void>("DELETE", `/api/server-backups/${encodeURIComponent(name)}`),
  restoreServerBackup: (name: string) => request<void>("POST", `/api/server-backups/${encodeURIComponent(name)}/restore`),
  downloadServerBackup,
  restoreServerUpload,
  backupPolicy: (deviceId: string, name: string) =>
    request<BackupPolicy>("GET", `/api/devices/${deviceId}/workers/${encodeURIComponent(name)}/backup-policy`),
  setBackupPolicy: (deviceId: string, name: string, p: Omit<BackupPolicy, "next_run" | "last_run">) =>
    request<BackupPolicy>("PUT", `/api/devices/${deviceId}/workers/${encodeURIComponent(name)}/backup-policy`, p),
  updateStatus: () => request<UpdateStatus>("GET", "/api/system/update"),
  checkUpdate: () => request<UpdateStatus>("POST", "/api/system/update/check"),
  applyUpdate: (agents: boolean) => request<UpdateStatus>("POST", `/api/system/update/apply${agents ? "?agents=1" : ""}`),
  updateAgents: (device?: string) =>
    request<{ queued: number }>("POST", `/api/system/update/agents${device ? `?device=${device}` : ""}`),
  setUpdateSchedule: (s: UpdateSchedule) => request<UpdateStatus>("PUT", "/api/system/update/schedule", s),
  backups: () => request<Backup[]>("GET", "/api/backups"),
  createBackup: (deviceId: string, containerId: string) =>
    request<Backup>("POST", `/api/devices/${deviceId}/containers/${containerId}/backup`),
  restoreBackup: (id: string) => request<Command>("POST", `/api/backups/${id}/restore`),
  deleteBackup: (id: string) => request<void>("DELETE", `/api/backups/${id}`),
  downloadBackup,
  me: (token?: string) => request<Me>("GET", "/api/auth/me", undefined, token),
  authStatus: () => request<{ setup_required: boolean }>("GET", "/api/auth/status", undefined, null),
  login: (username: string, password: string) => request<LoginResult>("POST", "/api/auth/login", { username, password }, null),
  setup: (admin_token: string, username: string, password: string) =>
    request<LoginResult>("POST", "/api/auth/setup", { admin_token, username, password }, null),
  changePassword: (current: string, next: string) => request<void>("POST", "/api/auth/password", { current, new: next }),
  logout: () => request<void>("POST", "/api/auth/logout"),
  users: () => request<User[]>("GET", "/api/users"),
  createUser: (u: { username: string; password: string; role: Role }) => request<User>("POST", "/api/users", u),
  updateUser: (id: string, u: { role?: Role; password?: string; disabled?: boolean }) => request<User>("PATCH", `/api/users/${id}`, u),
  deleteUser: (id: string) => request<void>("DELETE", `/api/users/${id}`),
  setAccess: (id: string, protectedAccess: boolean) =>
    request<DomainsView>("PUT", `/api/projects/${id}/domains/access`, { protected: protectedAccess }),
  overview: () => request<Overview>("GET", "/api/overview"),
  devices: () => request<Device[]>("GET", "/api/devices"),
  device: (id: string) => request<Device>("GET", `/api/devices/${id}`),
  deviceMetrics: (id: string) => request<MetricSample[]>("GET", `/api/devices/${id}/metrics`),
  updateDevice: (id: string, patch: { name?: string; labels?: string[] }) =>
    request<Device>("PATCH", `/api/devices/${id}`, patch),
  tunnel: (id: string) => request<TunnelView>("GET", `/api/devices/${id}/tunnel`),
  setTunnel: (id: string, token: string) => request<TunnelView>("PUT", `/api/devices/${id}/tunnel`, { token }),
  setTunnelRoutes: (id: string, routes: TunnelRoute[]) => request<TunnelView>("PUT", `/api/devices/${id}/tunnel/routes`, { routes }),
  projectTunnel: (id: string) => request<ProjectTunnelView>("GET", `/api/projects/${id}/tunnel`),
  setProjectTunnel: (id: string, routes: { hostname: string; port: number }[]) =>
    request<ProjectTunnelView>("PUT", `/api/projects/${id}/tunnel`, { routes }),
  stackAction: (projectId: string, action: "start" | "stop" | "down", volumes = false) =>
    request<Command>("POST", `/api/projects/${projectId}/stack`, { action, volumes }),
  enableProxy: (id: string) => request<void>("POST", `/api/devices/${id}/proxy`),
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
  deleteProject: (id: string, removeContainer = false) =>
    request<void>("DELETE", `/api/projects/${id}${removeContainer ? "?remove_container=1" : ""}`),
  deployProject: (id: string, confirm = false) => request<Deployment>("POST", `/api/projects/${id}/deployments${confirm ? "?confirm=1" : ""}`),
  adopt: (deviceId: string, containers: { id: string; name?: string }[], dryRun: boolean) =>
    request<AdoptItem[]>("POST", `/api/devices/${deviceId}/adopt`, { containers, dry_run: dryRun }),
  projectDeployments: (id: string) => request<Deployment[]>("GET", `/api/projects/${id}/deployments`),
  deployments: (q: { device?: string; limit?: number } = {}) =>
    request<Deployment[]>("GET", `/api/deployments?limit=${q.limit ?? 50}${q.device ? `&device=${q.device}` : ""}`),
  deployment: (id: string) => request<Deployment>("GET", `/api/deployments/${id}`),
  retryDeployment: (id: string) => request<Deployment>("POST", `/api/deployments/${id}/retry`),
  rollbackDeployment: (id: string) => request<Deployment>("POST", `/api/deployments/${id}/rollback`),
  projectPreviews: (id: string) => request<PreviewsView>("GET", `/api/projects/${id}/previews`),
  setPreviewSettings: (id: string, s: { enabled: boolean; max: number }) =>
    request<PreviewsView>("PUT", `/api/projects/${id}/previews/settings`, s),
  deletePreview: (id: string, pid: string) => request<void>("DELETE", `/api/projects/${id}/previews/${pid}`),
  openStream: (deviceId: string, s: { kind: "logs" | "shell"; container: string; tail?: number; cols?: number; rows?: number }) =>
    request<{ id: string }>("POST", `/api/devices/${deviceId}/streams`, s),
  streamOutput: (id: string, from: number, signal: AbortSignal) =>
    fetchStreamOutput(id, from, signal),
  streamInput: (id: string, input: { data?: string; cols?: number; rows?: number }) =>
    request<void>("POST", `/api/streams/${id}/input`, {
      data: input.data === undefined ? undefined : btoa(String.fromCharCode(...new TextEncoder().encode(input.data))),
      cols: input.cols,
      rows: input.rows,
    }),
  closeStream: (id: string) => request<void>("DELETE", `/api/streams/${id}`),
  settings: () => request<Settings>("GET", "/api/settings"),
  setGitHubToken: (token: string) =>
    request<{ connected: boolean; login?: string }>("PUT", "/api/settings/github", { token }),
  setCloudflare: (b: { token?: string; proxied?: boolean }) =>
    request<{ connected: boolean; zones?: string[]; proxied?: boolean }>("PUT", "/api/settings/cloudflare", { token: b.token ?? "", proxied: b.proxied }),
  setSSLMode: (mode: "http" | "dns") => request<{ ssl_mode: string }>("PUT", "/api/settings/ssl", { mode }),
  setSecurity: (b: { session_hours: number; min_password: number; force_https: boolean }) => request<Settings["security"]>("PUT", "/api/settings/security", b),
  rotateJoinToken: () => request<{ join_token: string }>("POST", "/api/settings/join-token/rotate"),
  projectDomains: (id: string) => request<DomainsView>("GET", `/api/projects/${id}/domains`),
  setProjectUrls: (id: string, u: { production: boolean; preview: boolean }) =>
    request<DomainsView>("PUT", `/api/projects/${id}/domains/urls`, u),
  addDomain: (id: string, d: { hostname: string; type: "domain" | "route"; path?: string }) =>
    request<DomainsView>("POST", `/api/projects/${id}/domains`, d),
  deleteDomain: (id: string, did: string) => request<void>("DELETE", `/api/projects/${id}/domains/${did}`),
  setBackupPassword: (password: string) => request<{ backup_password_set: boolean }>("PUT", "/api/settings/backup-password", { password }),
  setBaseDomain: (base_domain: string) => request<{ base_domain: string }>("PUT", "/api/settings/domain", { base_domain }),
  githubRepos: () => request<GitHubRepo[]>("GET", "/api/github/repos"),
};

export interface ProjectInput {
  name: string;
  device_id: string;
  source: Source;
  runtime: Runtime;
}
