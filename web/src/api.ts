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
  /** A Compute Blade's supply, from GPIO23 (only while its LED profile is "blade"). */
  power?: { source: "poe-plus" | "5v"; max_watts: number };
  /** Each address with its interface, subnet, gateway and whether it is on Wi-Fi. */
  network?: NetAddr[];
}

export interface NetAddr {
  interface: string;
  address: string;
  assigned?: "dhcp" | "static";
  prefix?: number;
  gateway?: string;
  wireless?: boolean;
}

/** A worker's own address on the device's LAN (a macvlan network). */
export interface DedicatedIP {
  mode: "dhcp" | "static";
  ip?: string;
  /** Generated once, so a router can reserve an address for it. */
  mac?: string;
  parent?: string;
  /** Spare address that lets the device itself reach the worker. */
  host_ip?: string;
  /** Ports for the copy on this address; empty uses the project's ports. */
  ports?: string[];
  /** 802.1Q VLAN ID (1-4094): the agent creates a tagged sub-interface such as eth0.1005. */
  vlan?: number;
  /** The VLAN's network (static mode); learned from DHCP otherwise. */
  subnet?: string;
  gateway?: string;
}

export interface DedicatedCheck {
  problems?: string[];
  notes?: string[];
  /** "yes" when the VLAN's gateway or DHCP server answered, "no" when nothing did. */
  vlan_reachable?: "yes" | "no";
  offer?: string;
  /** A free address near the top of the network for the device's own access; never saved automatically. */
  suggest_host_ip?: string;
  subnet?: string;
  gateway?: string;
  parent?: string;
  wireless?: boolean;
  mac: string;
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
  temp_c?: number;
  sensors?: { name: string; c: number }[];
  iowait_percent?: number;
  swap_used?: number;
  swap_total?: number;
  oom_kills?: number;
  clock_mhz?: number;
  max_clock_mhz?: number;
  disks?: DiskIO[];
  nets?: NetIO[];
  power?: PowerInfo;
}

export interface DiskIO {
  name: string;
  read_bps: number;
  write_bps: number;
  read_iops: number;
  write_iops: number;
  busy: number;
}

export interface NetIO {
  name: string;
  rx_bps: number;
  tx_bps: number;
  errors?: number;
  wireless?: boolean;
  signal_dbm?: number;
}

export interface PowerInfo {
  raw: number;
  under_voltage?: boolean;
  freq_capped?: boolean;
  throttled?: boolean;
  temp_limit?: boolean;
  since_under_voltage?: boolean;
  since_freq_capped?: boolean;
  since_throttled?: boolean;
  since_temp_limit?: boolean;
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
  /** Docker networks the container is on, with its address and MAC (a macvlan one is a dedicated address). */
  networks?: { name: string; ip?: string; mac?: string }[];
  /** Docker has restarted it repeatedly in the last few minutes, even if it is running right now. */
  unstable?: boolean;
}

export interface DiskVolume {
  path: string;
  size_bytes: number;
  fstype?: string;
  label?: string;
  uuid?: string;
  mountpoint?: string;
  used_bytes?: number;
  persistent?: boolean;
  protected?: string;
}
export interface Disk {
  path: string;
  kind: "disk" | "loop";
  size_bytes: number;
  model?: string;
  serial?: string;
  transport?: string;
  removable?: boolean;
  rotational?: boolean;
  /** "emmc" or "sd" for MMC-bus devices; absent when unknown. */
  medium?: "emmc" | "sd";
  volume: DiskVolume;
  partitions?: DiskVolume[];
  protected?: string;
  system?: boolean;
}
export interface DiskShare {
  source: string;
  mountpoint: string;
  fstype: string;
  size_bytes?: number;
  used_bytes?: number;
  persistent?: boolean;
}
export interface DockerGCPolicy {
  keep: number;
  max_gb: number;
  mode: "off" | "daily" | "weekly";
  time: string;
  weekday: number;
  tz: string;
  dangling: boolean;
  build_cache: boolean;
  cache_days: number;
  next_run?: string;
}
export interface DockerImageVersion { tag: string; id: string; size: number; created: number; in_use: boolean; removable: boolean }
export interface DockerImageGroup { repo: string; worker: string; orphaned: boolean; versions: DockerImageVersion[] }
export interface DockerStorage {
  root_dir: string;
  keep: number;
  images: number;
  images_size: number;
  layers_size: number;
  dangling: number;
  dangling_size: number;
  unused: number;
  unused_size: number;
  workers: DockerImageGroup[];
  build_cache: number;
  build_cache_reclaimable: number;
  reclaimable: number;
}

export interface DiskReport {
  disks: Disk[];
  shares: DiskShare[];
  /** Device model such as "Raspberry Pi Compute Module 5", when the system reports one. */
  board?: string;
}

export type Health = "ok" | "warning" | "critical";

/** A failing container, explained: why it failed, the numbers behind it and what to do about it. */
export interface ContainerIssue {
  name: string;
  id: string;
  kind: "oom" | "killed" | "loop" | "dead";
  message: string;
  cause: string;
  tips?: string[];
  project_id?: string;
  project_name?: string;
  mem_limit?: number;
  mem_used?: number;
  host_mem_total?: number;
  host_mem_used?: number;
  swap_total?: number;
  swap_used?: number;
  /** A memory limit (MiB) worth trying in the project settings. */
  suggest_mb?: number;
  acked?: boolean;
  acked_by?: string;
  restarts?: number;
}

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
  container_issues?: ContainerIssue[];
  /** A change to the device's network, shown until acknowledged. */
  network_notice?: { message: string; at: string };
  proxy: "running" | "stopped" | "none" | "agent_too_old";
  maintenance: boolean;
  maintenance_note?: string;
  maintenance_until?: string;
  agent_link: "https" | "http" | "unknown";
  /** Set while the device flashes its LEDs (RFC 3339). */
  identifying_until?: string;
}

export interface LED {
  name: string;
  role: "activity" | "power" | "status" | "other";
  brightness: number;
  max: number;
  trigger?: string;
  state?: "" | "ok" | "warn" | "crit" | "identify";
}
export type LEDMetric = "temp" | "memory" | "cpu" | "disk";
export interface LEDRule {
  metric: LEDMetric;
  led: string;
  warn: number;
  crit: number;
}
export interface LEDConfig {
  profile: "pi" | "blade";
  enabled: boolean;
  /** Keep every LED of the profile off. */
  stealth?: boolean;
  rules?: LEDRule[];
}
export interface LEDStatus {
  profile: string;
  supported: boolean;
  note?: string;
  leds: LED[];
  config: LEDConfig;
  values?: Partial<Record<LEDMetric, number>>;
}

export interface SSHSettings {
  password_auth: boolean;
  pubkey_auth: boolean;
  root_login: "yes" | "no" | "prohibit-password" | "forced-commands-only";
}
export interface SSHKey {
  user: string;
  type: string;
  fingerprint: string;
  comment?: string;
}
export interface SSHStatus {
  installed: boolean;
  running: boolean;
  service?: string;
  effective: SSHSettings;
  managed: boolean;
  users?: { name: string; uid: number; home: string }[];
  keys?: SSHKey[];
  pending?: { revert_at: string };
  error?: string;
}
export interface SSHCheck {
  warnings: { level: "lockout" | "note"; message: string }[];
  lockout: boolean;
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

export interface CloudflareTarget {
  kind: "pages" | "worker";
  /** Pages project or Worker script; empty means the project name. */
  name?: string;
  /** Pages: directory to upload. Worker: entry module. Empty: detected. */
  output?: string;
  compatibility_date?: string;
}

/** What the monitor last learned from Cloudflare about a project. */
export interface CloudflareStatus {
  kind: "pages" | "worker";
  name: string;
  state: "live" | "building" | "failed" | "missing" | "error";
  detail?: string;
  url?: string;
  /** Where to visit it: the custom domain when it has an active one. */
  default_url?: string;
  domains?: string[];
  latest_id?: string;
  latest_at?: string;
  requests: number;
  errors: number;
  subrequests: number;
  cpu_p50_us: number;
  cpu_p99_us: number;
  /** "ok", "none" (not collected for Pages) or why it could not be read. */
  analytics: string;
  /** Set when Cloudflare also has a resource of the other kind under this name. */
  other_kind?: "pages" | "worker";
  polled_at: string;
}

/** What Cloudflare itself holds for a project: history, previews, domains and read-only settings. */
export interface CloudflareRemote {
  kind: "pages" | "worker";
  name: string;
  fetched_at: string;
  deployments: {
    id: string; label?: string; url?: string; environment: "production" | "preview"; status: "success" | "failure" | "building" | "canceled" | "active";
    created_at: string; branch?: string; commit?: string; message?: string; author?: string; source?: string; live?: boolean; percentage?: number;
  }[];
  previews: { id: string; label: string; url?: string; aliases?: string[]; status: string; created_at: string; commit?: string; message?: string; author?: string }[];
  domains: { host: string; kind: "custom-domain" | "route" | "pages-domain"; status?: string; detail?: string }[];
  settings?: {
    compatibility_date?: string; compatibility_flags?: string[]; usage_model?: string; observability?: boolean;
    production_branch?: string; repo?: string; build_command?: string; output_dir?: string; root_dir?: string;
    /** Workers Builds: how Cloudflare builds this Worker from Git. */
    build?: {
      name?: string; repo?: string; provider?: string; build_command?: string; deploy_command?: string; root_dir?: string;
      branches?: string[]; exclude_branches?: string[]; path_includes?: string[]; path_excludes?: string[]; build_caching?: boolean;
    };
    variables: { name: string; type: string; value?: string; secret?: boolean }[];
    bindings: { name: string; type: string; target?: string }[];
  };
  /** Per section, why it could not be read (usually a missing token permission). */
  notes?: Record<string, string>;
}

/** A Pages project or Worker that already exists in the Cloudflare account. */
export interface CloudflareFound {
  kind: "pages" | "worker";
  name: string;
  url?: string;
  repo?: string;
  branch?: string;
  /** Name of the Orchestrator project that already manages it. */
  imported?: string;
}

export interface Source {
  type: "github" | "image" | "compose" | "cloudflare";
  cloudflare?: CloudflareTarget;
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
  /** Host port that carries web traffic (tunnel, proxy, production URL); empty uses the first TCP port. */
  web_port?: string;
  env: string[];
  command?: string[];
  restart: string;
  /** Restart cap for the "on-failure" policy; 0 or absent is unlimited. */
  max_retries?: number;
  /** Hard memory limit in MiB; 0 or absent is none. */
  memory_mb?: number;
  volumes?: string[];
  labels?: Record<string, string>;
  network?: string;
  /** Own address on the device's network, so the worker can use ports the device holds (80, 443). */
  dedicated?: DedicatedIP | null;
  /** Own-address settings of other devices the worker runs on (replicas), by device id. */
  copy_addresses?: Record<string, DedicatedIP>;
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
  cloudflare?: { kind: "pages" | "worker"; name: string; branch: string; production_branch: string; output?: string };
}

export interface TunnelRoute {
  hostname: string;
  /** A URL such as http://localhost:3000, or project:<name>. */
  service: string;
  /** Set when the route belongs to a project; kept as is when the device page saves routes. */
  project?: string;
  /** Kept in step with a project's domains by the server. */
  managed?: boolean;
}

export interface TunnelView {
  supported: boolean;
  enabled: boolean;
  state?: string;
  tunnel_id?: string;
  via: "" | "proxy";
  routes: TunnelRoute[];
  can_route: boolean;
  warnings?: string[];
  /** cloudflared version running on the device, the newest release, and whether the first is behind. */
  version?: string;
  latest?: string;
  update_available: boolean;
  /** False when the device's agent predates tunnel updates. */
  can_update: boolean;
  auto_update: boolean;
  /** When automatic updates run (the server update schedule); empty when that schedule is off. */
  auto_update_when?: string;
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
  trigger: "manual" | "push" | "retry" | "poll" | "rollback" | "replica" | "migration";
  preview_id?: string;
  commit: Commit | null;
  spec: DeploySpec;
  status: DeploymentStatus;
  error?: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  steps?: Step[];
  /** Cloudflare deployments: the deployment's ID on Cloudflare and the address it is served at. */
  remote_id?: string;
  url?: string;
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

/** An extra copy of a worker on another device (a replica). */
export interface ReplicaInfo {
  device_id: string;
  device: string;
  online: boolean;
  maintenance?: boolean;
  running: boolean;
  container_id?: string;
  state?: string;
  deployment_id?: string;
  deployment_status?: string;
  deployment_error?: string;
  conflicts?: string[];
  url?: string;
  running_url?: string;
  pending?: boolean;
}

export interface Project {
  id: string;
  name: string;
  device_id: string;
  device_name: string;
  replicas?: ReplicaInfo[];
  main_url?: string;
  main_running_url?: string;
  main_pending?: boolean;
  source: Source;
  runtime: Runtime;
  created_at: string;
  updated_at: string;
  latest_deployment: Deployment | null;
  cloudflare?: CloudflareStatus | null;
  /** Host-level options in use (host folder mounts, host networking, Compose files); only admins can change them. */
  host_access?: string[];
  network_access?: string[];
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
  cloudflare: { projects: number; live: number; failing: number; requests: number; errors: number };
  recent_events: Event[];
}

export interface MetricSample {
  time: string;
  cpu: number;
  mem: number;
  disk: number;
  load1: number;
  temp: number;
  iowait?: number;
  swap?: number;
  swap_used?: number;
  oom?: number;
  clock?: number;
  max_clock?: number;
  power?: number;
  has_power?: boolean;
  disks?: DiskIO[];
  nets?: NetIO[];
}

export interface Alert {
  id: string;
  severity: "critical" | "warning" | "info";
  title: string;
  detail?: string;
  link: string;
  at?: string;
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
  status: "active" | "lan" | "tunnel" | "pending" | "mismatch";
  access?: "tunnel";
  detail: string;
  records: DnsRecord[];
}

export interface UrlRow {
  hostname: string;
  enabled: boolean;
  status?: DomainRow["status"];
  detail?: string;
  records?: DnsRecord[];
  tunnel?: boolean;
}

export interface DomainsView {
  lan_only: boolean;
  tunnel_ready: boolean;
  tunnel_via: "" | "proxy";
  base_domain: string;
  production: UrlRow | null;
  preview: UrlRow | null;
  domains: DomainRow[];
  addresses: string[];
  proxy: "ok" | "agent_too_old";
  protected: boolean;
  dns: "manual" | "cloudflare";
  ssl: "http" | "dns";
  /** Problems applying the last change, such as a tunnel hostname Cloudflare refused. */
  warnings?: string[];
}

export interface ConnAddress {
  url: string;
  listed: boolean;
  /** The paired standby's address: announced automatically, not editable in the list. */
  auto?: boolean;
  kind: "ip" | "name" | "loopback";
  assigned?: "dhcp" | "static" | "";
  devices: string[];
}
export interface ConnDevice {
  id: string;
  name: string;
  status: "online" | "offline";
  server: string;
  via: "" | "configured" | "announced" | "discovered";
  recovers: boolean;
  network: { interface?: string; address: string; assigned?: string }[];
}
export interface ConnView {
  host: string;
  addresses: ConnAddress[];
  suggestions: string[];
  devices: ConnDevice[];
  warnings: string[];
  changes: { id: number; level: string; message: string; created_at: string }[];
}

export interface Settings {
  base_domain: string;
  /** Addresses agents are told to know this server by, most preferred first. */
  agent_urls: string[] | null;
  /** Addresses added automatically (the paired standby server). */
  agent_urls_auto?: string[] | null;
  backup_password_set: boolean;
  cloudflare: { connected: boolean; proxied: boolean; deploy: { configured: boolean; account_id: string; own_token: boolean } };
  ssl_mode: "http" | "dns";
  sso: SSOView;
  https: HTTPSView;
  temperature: { warn_c: number; crit_c: number };
  docker_gc: DockerGCPolicy;
  security: { session_hours: number; min_password: number; force_https: boolean; require_https_agents: boolean };
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

// --- Per-worker metrics ---

export type MetricsRange = "1h" | "24h" | "7d" | "30d";

export interface MetricPrefs {
  metrics: string[];
  interval: number;
  retention_days: number;
}

export interface MetricPrefsView {
  key: string;
  cloudflare: boolean;
  prefs: MetricPrefs;
  /** Estimated SQLite bytes this worker's history takes at these settings. */
  storage: number;
}

export interface WorkerSample {
  t: number;
  cpu?: number;
  mem?: number;
  mem_limit?: number;
  net_rx?: number;
  net_tx?: number;
  blk_read?: number;
  blk_write?: number;
  pids?: number;
  throttled?: number;
  restarts?: number;
}

export interface ProxyPoint {
  t: number;
  requests: number;
  c2xx: number;
  c3xx: number;
  c4xx: number;
  c5xx: number;
  bytes?: number;
  p50?: number;
  p95?: number;
}

export interface WorkerMetrics {
  supported: boolean;
  project_id?: string;
  prefs: MetricPrefs;
  range: MetricsRange;
  step: number;
  current?: { at: number; cpu: number; mem: number; mem_limit: number; pids: number; restarts: number; health?: string; started_at: number; host_network?: boolean; oom_killed?: boolean };
  series: WorkerSample[];
  restarts: number;
  volumes: { name: string; size: number }[];
  volumes_at?: string;
  proxy?: { hosts: string[]; series: ProxyPoint[]; totals: ProxyPoint; p99?: number };
  proxy_note?: string;
}

export interface CFPoint {
  t: number;
  requests: number;
  errors: number;
  subrequests: number;
  cpu_p50?: number;
  cpu_p99?: number;
  wall_p50?: number;
  wall_p99?: number;
}

export interface ProjectMetrics {
  kind: "device" | "compose" | "cloudflare";
  range: MetricsRange;
  step: number;
  workers: { device_id: string; device_name: string; name: string; state: string }[];
  deployments: {
    total: number;
    succeeded: number;
    failed: number;
    previews: number;
    rollbacks: number;
    median_seconds?: number;
    p95_seconds?: number;
    series: { t: number; succeeded: number; failed: number }[];
  };
  cloudflare?: {
    kind: "pages" | "worker";
    analytics: string;
    note?: string;
    series: CFPoint[];
    totals: CFPoint;
    by_status: Record<string, number>;
    by_version: Record<string, number>;
  };
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

export interface Migration {
  status: "running" | "done" | "failed";
  step: string;
  error?: string;
  from: string;
  to: string;
  log: string[];
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
  channel?: "stable" | "dev" | "custom";
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
  consolidate: boolean;
  weekly_keep: number;
  estimated_size?: number;
  estimated_total?: number;
  next_run?: string;
  last_run?: string;
}

export interface BackupStorage {
  dir: string;
  backup_bytes: number;
  free_bytes: number;
  total_bytes: number;
  level: "ok" | "warning" | "critical";
  container_bytes: number;
  server_bytes: number;
  other_bytes: number;
  location: { device?: string; device_id?: string; container?: string; host_path?: string; volume?: string; note?: string };
}
export interface BackupOAuthProvider {
  id: "google" | "dropbox" | "onedrive";
  name: string;
  client_id: string;
  secret_set: boolean;
  console: string;
  setup: string;
}

export interface ServerBackupPolicy {
  mode: "off" | "daily" | "weekly";
  time: string;
  weekday: number;
  tz: string;
  keep: number;
  /** Destination ids to copy to; empty means every enabled destination. */
  destinations?: string[];
}
export interface BackupCopy {
  dest_id: string;
  dest: string;
  kind: string;
  status: "ok" | "failed" | "pending" | "missing";
  verified: boolean;
  size?: number;
  at?: string;
  verified_at?: string;
  error?: string;
  note?: string;
}
export interface ServerBackup {
  name: string;
  size: number;
  encrypted: boolean;
  created_at: string;
  /** False when the file is gone from this server but copies remain on destinations. */
  local: boolean;
  sha256?: string;
  wanted: number;
  mirrored: number;
  copies?: BackupCopy[];
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
  challenge?: "dns";
}

export interface BackupDest {
  id: string;
  kind: "s3" | "folder" | "device" | "rclone";
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
  rclone_conf?: string;
  remote?: string;
  /** Backups kept on this destination; 0 or unset follows the schedule. */
  keep?: number;
  last_at?: string;
  last_size?: number;
  last_error?: string;
  /** Where copies land, in words (device and folder, bucket and prefix, remote and folder). */
  location?: string;
  /** Space the copies take; known for folders on the server only. */
  used_bytes?: number;
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

export interface ServerMove {
  phase: "preparing" | "installing" | "verifying" | "ready" | "switched" | "done" | "failed";
  device_id: string;
  device_name: string;
  new_url: string;
  old_url: string;
  port: number;
  error?: string;
  steps: string[];
  started_at: string;
  updated_at: string;
}
export interface MoveCandidate {
  id: string;
  name: string;
  online: boolean;
  eligible: boolean;
  reason?: string;
  addresses: string[];
  arch: string;
}
export interface MoveView {
  move?: ServerMove;
  standby_to?: string;
  moved_from?: { old_url: string; new_url: string; at: string };
  devices: MoveCandidate[];
  on_this_server: number;
  image?: string;
  can_move: boolean;
  cannot_why?: string;
}
export interface MoveResult {
  view: MoveView;
  note?: string;
}

/** Downloads a password-protected copy of this server for moving it by hand. */
async function exportServerCopy(password: string) {
  const res = await fetch("/api/server-move/export", {
    method: "POST",
    headers: { Authorization: `Bearer ${getToken()}`, "Content-Type": "application/json" },
    body: JSON.stringify({ password }),
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
  const url = URL.createObjectURL(await res.blob());
  const a = document.createElement("a");
  a.href = url;
  a.download = "orchestrator-move.db.enc";
  a.click();
  URL.revokeObjectURL(url);
}

export interface ClusterPolicy {
  failover: boolean;
  allowed?: string[];
  fail_back: boolean;
  fail_back_seconds?: number;
  start_empty: boolean;
  replicas?: string[];
}
export interface FailoverEntry {
  id: string;
  project_id: string;
  worker: string;
  kind: "failover" | "failback";
  at: string;
  finished_at?: string;
  from_id: string;
  from_name: string;
  to_id?: string;
  to_name?: string;
  status: "running" | "done" | "failed";
  reason?: string;
  error?: string;
  offline_seconds?: number;
  with_data: boolean;
  duration_seconds?: number;
  warnings?: string[];
}
export interface TargetFit {
  device_id: string;
  device: string;
  fits: boolean;
  problems?: string[];
}
export interface DeviceCapacity {
  device_id: string;
  device: string;
  cores: number;
  mem_free: number;
  disk_free: number;
  workers: number;
  online: boolean;
  can_lose: boolean;
  stranded?: string[];
  reasons?: string[];
}
export interface ClusterView {
  config: { enabled: boolean; grace_seconds: number };
  capacity: { enabled: boolean; survives_one: boolean; devices: DeviceCapacity[] };
  workers: {
    project_id: string;
    name: string;
    device_id: string;
    device: string;
    policy: ClusterPolicy;
    replicas?: ReplicaInfo[];
    url?: string;
    targets?: TargetFit[];
    need?: { cores?: number; mem?: number; disk?: number };
    last_failover?: { from_name: string; to_name: string; at: string; with_data: boolean; home: boolean };
  }[];
}

export interface HAView {
  role: "active" | "standby";
  epoch: number;
  peer_url?: string;
  self_url?: string;
  paired: boolean;
  token?: string;
  last_sync?: string;
  sync_error?: string;
  sync_version?: string;
  ready: boolean;
  standby_last_pull?: string;
  previous?: string;
  version: string;
  can_restart: boolean;
}

export interface GoldImage {
  id: string;
  name: string;
  arch?: string;
  notes?: string;
  format: "raw" | "gz" | "xz" | "zst";
  size: number;
  sha256: string;
  created_by?: string;
  created_at: string;
}

export interface EnrolToken {
  id: string;
  label: string;
  created_at: string;
  created_by?: string;
  expires_at: string;
  max_uses: number;
  uses: number;
  status: "active" | "expired" | "used up" | "revoked";
  devices?: string[];
  /** Only in the response that creates the token. */
  token?: string;
}

export interface BootInfo {
  supported: boolean;
  reason?: string;
  model?: string;
  order?: string;
  config?: string;
}

export const api = {
  serverHA: () => request<HAView>("GET", "/api/server-ha"),
  haPair: () => request<HAView>("POST", "/api/server-ha/pair"),
  haJoin: (b: { active_url: string; token: string; self_url: string }) => request<HAView>("POST", "/api/server-ha/join", b),
  haLeave: () => request<{ status: string }>("POST", "/api/server-ha/leave"),
  haPromote: (activeIsDown: boolean) => request<{ status: string }>("POST", "/api/server-ha/promote", { active_is_down: activeIsDown }),
  cluster: () => request<ClusterView>("GET", "/api/cluster"),
  clusterHistory: (project?: string) => request<FailoverEntry[]>("GET", `/api/cluster/history${project ? `?project=${project}` : ""}`),
  setCluster: (c: { enabled: boolean; grace_seconds: number }) => request<ClusterView["config"]>("PUT", "/api/cluster", c),
  setClusterPolicy: (id: string, p: ClusterPolicy, o?: { confirm?: boolean; on_remove?: "keep" | "stop" | "remove"; delete_volumes?: boolean }) =>
    request<ClusterPolicy>("PUT", `/api/projects/${id}/cluster${o?.confirm ? "?confirm=1" : ""}`, { ...p, on_remove: o?.on_remove, delete_volumes: o?.delete_volumes }),
  retryReplica: (id: string, deviceId: string) => request<unknown>("POST", `/api/projects/${id}/cluster/replicas/${deviceId}/retry`),
  serverMove: () => request<MoveView>("GET", "/api/server-move"),
  startServerMove: (b: { device_id: string; port: number; address: string; old_url: string }) => request<MoveView>("POST", "/api/server-move", b),
  switchServerMove: () => request<MoveView>("POST", "/api/server-move/switch"),
  finishServerMove: () => request<MoveView>("POST", "/api/server-move/finish"),
  cancelServerMove: () => request<MoveResult>("POST", "/api/server-move/cancel"),
  acceptServerMove: () => request<MoveResult>("POST", "/api/server-move/accept"),
  moveServerBack: () => request<MoveResult>("POST", "/api/server-move/back"),
  exportServerCopy,
  backupStorage: () => request<BackupStorage>("GET", "/api/backups/storage"),
  backupOAuth: () => request<BackupOAuthProvider[]>("GET", "/api/backup-oauth"),
  setBackupOAuth: (c: Record<string, { client_id: string; client_secret?: string }>) => request<BackupOAuthProvider[]>("PUT", "/api/backup-oauth", c),
  startBackupOAuth: (provider: string, destId?: string) => request<{ url: string }>("POST", `/api/backup-oauth/${provider}/start`, destId ? { dest_id: destId } : undefined),
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
  restoreServerBackup: (name: string, from?: string) => request<void>("POST", `/api/server-backups/${encodeURIComponent(name)}/restore${from ? `?from=${encodeURIComponent(from)}` : ""}`),
  syncServerBackup: (name: string, destId?: string) => request<void>("POST", `/api/server-backups/${encodeURIComponent(name)}/sync`, destId ? { dest_id: destId } : undefined),
  verifyServerBackup: (name: string) => request<void>("POST", `/api/server-backups/${encodeURIComponent(name)}/verify`),
  downloadServerBackup,
  restoreServerUpload,
  backupPolicy: (deviceId: string, name: string) =>
    request<BackupPolicy>("GET", `/api/devices/${deviceId}/workers/${encodeURIComponent(name)}/backup-policy`),
  setBackupPolicy: (deviceId: string, name: string, p: Omit<BackupPolicy, "next_run" | "last_run">) =>
    request<BackupPolicy>("PUT", `/api/devices/${deviceId}/workers/${encodeURIComponent(name)}/backup-policy`, p),
  updateStatus: () => request<UpdateStatus>("GET", "/api/system/update"),
  checkUpdate: () => request<UpdateStatus>("POST", "/api/system/update/check"),
  applyUpdate: (agents: boolean) => request<UpdateStatus>("POST", `/api/system/update/apply${agents ? "?agents=1" : ""}`),
  setUpdateChannel: (channel: "stable" | "dev", agents: boolean) => request<UpdateStatus>("PUT", "/api/system/update/channel", { channel, agents }),
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
  revokeOtherSessions: () => request<void>("POST", "/api/auth/sessions/revoke-others"),
  users: () => request<User[]>("GET", "/api/users"),
  createUser: (u: { username: string; password: string; role: Role }) => request<User>("POST", "/api/users", u),
  updateUser: (id: string, u: { role?: Role; password?: string; disabled?: boolean }) => request<User>("PATCH", `/api/users/${id}`, u),
  deleteUser: (id: string) => request<void>("DELETE", `/api/users/${id}`),
  setAccess: (id: string, protectedAccess: boolean) =>
    request<DomainsView>("PUT", `/api/projects/${id}/domains/access`, { protected: protectedAccess }),
  overview: () => request<Overview>("GET", "/api/overview"),
  alerts: () => request<Alert[]>("GET", "/api/alerts"),
  devices: () => request<Device[]>("GET", "/api/devices"),
  device: (id: string) => request<Device>("GET", `/api/devices/${id}`),
  deviceMetrics: (id: string) => request<MetricSample[]>("GET", `/api/devices/${id}/metrics`),
  workerMetrics: (deviceId: string, name: string, range: MetricsRange) =>
    request<WorkerMetrics>("GET", `/api/devices/${deviceId}/workers/${encodeURIComponent(name)}/metrics?range=${range}`),
  workerMetricPrefs: (deviceId: string, name: string) =>
    request<MetricPrefsView>("GET", `/api/devices/${deviceId}/workers/${encodeURIComponent(name)}/metrics/prefs`),
  setWorkerMetricPrefs: (deviceId: string, name: string, p: MetricPrefs) =>
    request<MetricPrefsView>("PUT", `/api/devices/${deviceId}/workers/${encodeURIComponent(name)}/metrics/prefs`, p),
  projectMetrics: (id: string, range: MetricsRange) => request<ProjectMetrics>("GET", `/api/projects/${id}/metrics?range=${range}`),
  projectMetricPrefs: (id: string) => request<MetricPrefsView>("GET", `/api/projects/${id}/metrics/prefs`),
  setProjectMetricPrefs: (id: string, p: MetricPrefs) => request<MetricPrefsView>("PUT", `/api/projects/${id}/metrics/prefs`, p),
  updateDevice: (id: string, patch: { name?: string; labels?: string[]; maintenance?: boolean; maintenance_note?: string; maintenance_hours?: number }) =>
    request<Device>("PATCH", `/api/devices/${id}`, patch),
  ackContainerIssue: (id: string, container: string, undo = false) =>
    request<Device>("POST", `/api/devices/${id}/container-alerts/ack`, { container, undo }),
  ackNetworkNotice: (id: string) => request<Device>("POST", `/api/devices/${id}/network-notice/ack`),
  tunnel: (id: string) => request<TunnelView>("GET", `/api/devices/${id}/tunnel`),
  setTunnel: (id: string, token: string) => request<TunnelView>("PUT", `/api/devices/${id}/tunnel`, { token }),
  updateTunnel: (id: string) => request<TunnelView>("POST", `/api/devices/${id}/tunnel/update`),
  setTunnelAutoUpdate: (id: string, enabled: boolean) => request<TunnelView>("PUT", `/api/devices/${id}/tunnel/auto-update`, { enabled }),
  setTunnelVia: (id: string, via: "" | "proxy") => request<TunnelView>("PUT", `/api/devices/${id}/tunnel/via`, { via }),
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
  disks: (deviceId: string) => request<DiskReport>("GET", `/api/devices/${deviceId}/disks`),
  diskAction: (deviceId: string, action: "format" | "partition" | "mount" | "unmount" | "share", args: Record<string, string>) =>
    request<Command>("POST", `/api/devices/${deviceId}/disks/${action}`, args),
  dockerStorage: (deviceId: string) => request<DockerStorage>("GET", `/api/devices/${deviceId}/docker`),
  dockerPrune: (deviceId: string, body: { trim: boolean; dangling: boolean; unused: boolean; build_cache: boolean; cache_days: number }) =>
    request<Command>("POST", `/api/devices/${deviceId}/docker/prune`, { ...body, confirm: "prune" }),
  setDockerGC: (b: Omit<DockerGCPolicy, "next_run">) => request<DockerGCPolicy>("PUT", "/api/settings/docker-gc", b),
  enrolTokens: () => request<EnrolToken[]>("GET", "/api/enrol-tokens"),
  createEnrolToken: (body: { label: string; ttl_hours: number; max_uses: number }) => request<EnrolToken>("POST", "/api/enrol-tokens", body),
  revokeEnrolToken: (id: string) => request<void>("DELETE", `/api/enrol-tokens/${id}`),
  images: () => request<GoldImage[]>("GET", "/api/images"),
  uploadImage: async (file: File, meta: { name: string; arch: string; notes: string }) => {
    const q = new URLSearchParams(meta);
    const res = await fetch(`/api/images?${q}`, { method: "POST", headers: { Authorization: `Bearer ${getToken() ?? ""}` }, body: file });
    if (!res.ok) throw new ApiError(res.status, (await res.json().catch(() => ({}))).error ?? res.statusText);
    return (await res.json()) as GoldImage;
  },
  deleteImage: (id: string) => request<void>("DELETE", `/api/images/${id}`),
  flashImage: (deviceId: string, body: { image: string; device: string; confirm: string; enrol: boolean; name?: string }) =>
    request<Command>("POST", `/api/devices/${deviceId}/images/flash`, body),
  bootInfo: (deviceId: string) => request<BootInfo>("GET", `/api/devices/${deviceId}/boot`),
  setBootOrder: (deviceId: string, order: string, confirm: string) => request<Command>("POST", `/api/devices/${deviceId}/boot`, { order, confirm }),
  getCommand: (id: string) => request<Command>("GET", `/api/commands/${id}`),
  events: (device?: string, limit = 100) =>
    request<Event[]>("GET", `/api/events?limit=${limit}${device ? `&device=${device}` : ""}`),
  projects: () => request<Project[]>("GET", "/api/projects"),
  project: (id: string) => request<Project>("GET", `/api/projects/${id}`),
  createProject: (p: ProjectInput) => request<Project>("POST", "/api/projects", p),
  updateProject: (id: string, p: Partial<ProjectInput>) => request<Project>("PATCH", `/api/projects/${id}`, p),
  deleteProject: (id: string, removeContainer = false) =>
    request<void>("DELETE", `/api/projects/${id}${removeContainer ? "?remove_container=1" : ""}`),
  migrateProject: (id: string, body: { device_id: string; with_data: boolean; stop_source: boolean; from_backup?: boolean }) =>
    request<Migration>("POST", `/api/projects/${id}/migrate`, body),
  checkDedicated: (deviceId: string, body: DedicatedIP & { project_id?: string }) =>
    request<DedicatedCheck>("POST", `/api/devices/${deviceId}/dedicated/check`, body),
  migration: (id: string) => request<Migration>("GET", `/api/projects/${id}/migration`),
  cloudflareStatus: (id: string) => request<CloudflareStatus | null>("GET", `/api/projects/${id}/cloudflare`),
  cloudflareRemote: (id: string) => request<CloudflareRemote | null>("GET", `/api/projects/${id}/cloudflare/remote`),
  setCloudflareKind: (id: string, kind: "pages" | "worker") => request<CloudflareStatus>("PUT", `/api/projects/${id}/cloudflare/kind`, { kind }),
  refreshCloudflare: (id: string) => request<CloudflareStatus>("POST", `/api/projects/${id}/cloudflare/refresh`),
  setCloudflareDeploy: (accountId: string, token?: string) =>
    request<{ pages?: boolean; workers?: boolean }>("PUT", "/api/settings/cloudflare/deploy", { account_id: accountId, token: token ?? "" }),
  cloudflareDiscover: () => request<CloudflareFound[]>("GET", "/api/cloudflare/discover"),
  cloudflareImport: (items: { kind: string; name: string }[]) =>
    request<{ kind: string; name: string; project?: string; error?: string }[]>("POST", "/api/cloudflare/import", { items }),
  deployProject: (id: string, confirm = false) => request<Deployment>("POST", `/api/projects/${id}/deployments${confirm ? "?confirm=1" : ""}`),
  adopt: (deviceId: string, containers: { id: string; name?: string }[], dryRun: boolean) =>
    request<AdoptItem[]>("POST", `/api/devices/${deviceId}/adopt`, { containers, dry_run: dryRun }),
  projectDeployments: (id: string) => request<Deployment[]>("GET", `/api/projects/${id}/deployments`),
  deployments: (q: { device?: string; limit?: number } = {}) =>
    request<Deployment[]>("GET", `/api/deployments?limit=${q.limit ?? 50}${q.device ? `&device=${q.device}` : ""}`),
  deployment: (id: string) => request<Deployment>("GET", `/api/deployments/${id}`),
  cancelDeployment: (id: string) => request<Deployment>("POST", `/api/deployments/${id}/cancel`),
  retryDeployment: (id: string) => request<Deployment>("POST", `/api/deployments/${id}/retry`),
  rollbackDeployment: (id: string) => request<Deployment>("POST", `/api/deployments/${id}/rollback`),
  projectPreviews: (id: string) => request<PreviewsView>("GET", `/api/projects/${id}/previews`),
  setPreviewSettings: (id: string, s: { enabled: boolean; max: number }) =>
    request<PreviewsView>("PUT", `/api/projects/${id}/previews/settings`, s),
  deletePreview: (id: string, pid: string) => request<void>("DELETE", `/api/projects/${id}/previews/${pid}`),
  openStream: (deviceId: string, s: { kind: "logs" | "shell" | "console"; container?: string; tail?: number; cols?: number; rows?: number }) =>
    request<{ id: string }>("POST", `/api/devices/${deviceId}/streams`, s),
  leds: (id: string) => request<LEDStatus>("GET", `/api/devices/${id}/leds`),
  /** Flash the device's LEDs. 0 seconds means until switched off (the agent still stops it after half an hour). */
  identifyDevice: (id: string, seconds = 0) => request<{ until: string }>("POST", `/api/devices/${id}/leds/identify`, { seconds }),
  stopIdentify: (id: string) => request<{ ok: boolean }>("POST", `/api/devices/${id}/leds/identify`, { stop: true }),
  setLEDConfig: (id: string, cfg: LEDConfig) => request<LEDStatus>("PUT", `/api/devices/${id}/leds/config`, cfg),
  sshStatus: (id: string) => request<SSHStatus>("GET", `/api/devices/${id}/ssh`),
  sshCheck: (id: string, s: SSHSettings) => request<SSHCheck>("POST", `/api/devices/${id}/ssh/check`, s),
  sshSet: (id: string, s: SSHSettings & { confirm?: string; trial_seconds?: number }) =>
    request<{ result: string }>("PUT", `/api/devices/${id}/ssh/config`, s),
  sshConfirm: (id: string, revert: boolean) => request<{ result: string }>("POST", `/api/devices/${id}/ssh/confirm`, { revert }),
  sshAddKey: (id: string, user: string, key: string) => request<{ fingerprint: string }>("POST", `/api/devices/${id}/ssh/keys`, { user, key }),
  sshRemoveKey: (id: string, user: string, fingerprint: string, confirm?: string) =>
    request<{ ok: boolean }>("POST", `/api/devices/${id}/ssh/keys/remove`, { user, fingerprint, confirm }),
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
  connectivity: () => request<ConnView>("GET", "/api/connectivity"),
  setAgentURLs: (urls: string[]) => request<{ agent_urls: string[] }>("PUT", "/api/settings/agent-urls", { urls }),
  setSSLMode: (mode: "http" | "dns") => request<{ ssl_mode: string }>("PUT", "/api/settings/ssl", { mode }),
  setTemperature: (b: { warn_c: number; crit_c: number }) => request<Settings["temperature"]>("PUT", "/api/settings/temperature", b),
  setSecurity: (b: { session_hours: number; min_password: number; force_https: boolean; require_https_agents: boolean }) => request<Settings["security"]>("PUT", "/api/settings/security", b),
  createDeviceSetup: (b: { proxy: boolean; tunnel_token?: string; tunnel_via?: "" | "proxy"; maintenance: boolean; maintenance_note?: string }) =>
    request<{ id: string }>("POST", "/api/device-setups", b),
  rotateJoinToken: () => request<{ join_token: string }>("POST", "/api/settings/join-token/rotate"),
  projectDomains: (id: string) => request<DomainsView>("GET", `/api/projects/${id}/domains`),
  setProjectUrls: (id: string, u: { production: boolean; preview: boolean; tunnel?: boolean; preview_tunnel?: boolean }) =>
    request<DomainsView>("PUT", `/api/projects/${id}/domains/urls`, u),
  addDomain: (id: string, d: { hostname: string; type: "domain" | "route"; path?: string; access?: "tunnel" }) =>
    request<DomainsView>("POST", `/api/projects/${id}/domains`, d),
  setDomainAccess: (id: string, did: string, access: "" | "tunnel") => request<DomainsView>("PATCH", `/api/projects/${id}/domains/${did}`, { access }),
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
