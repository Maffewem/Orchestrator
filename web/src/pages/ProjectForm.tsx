import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Box, Cloud, Github, Globe, Layers, Package, Server } from "lucide-react";
import { api, type DedicatedIP, type NodeBuild, type PreviewBuild, type Project, type ProjectInput, type Source } from "../api";
import DedicatedAddress from "../components/DedicatedAddress";
import EnvEditor from "../components/EnvEditor";
import { normalizeEnv } from "../env";
import { Button, ChipInput, ErrorNote, Field, Toggle, inputClass } from "../components/ui";
import { cx, usePoll } from "../lib";
import { useCan } from "../components/auth";
import { confirmDialog } from "../components/dialogs";

const lines = (s: string) => s.split("\n").map((l) => l.trim()).filter(Boolean);

const BUILD_KINDS = [
  { kind: "docker", icon: Box, title: "Dockerfile", sub: "Build the Dockerfile in your repository" },
  { kind: "node", icon: Server, title: "Node.js server", sub: "npm, pnpm or yarn. No Dockerfile needed" },
  { kind: "static", icon: Globe, title: "Static site", sub: "Vite, React, Astro… built, then served by nginx" },
] as const;

function Panel({ title, sub, children }: { title?: string; sub?: string; children: ReactNode }) {
  return (
    <div className="rounded-xl border border-neutral-200 bg-white p-5">
      {title && <h4 className="text-sm font-semibold">{title}</h4>}
      {sub && <p className="mt-0.5 text-xs text-neutral-500">{sub}</p>}
      <div className={title ? "mt-4" : ""}>{children}</div>
    </div>
  );
}

function ToggleRow({ title, sub, checked, onChange }: { title: string; sub: string; checked: boolean; onChange: (v: boolean) => void }) {
  return (
    <div className="flex items-center justify-between gap-6">
      <div>
        <p className="text-sm font-semibold">{title}</p>
        <p className="mt-0.5 text-xs text-neutral-500">{sub}</p>
      </div>
      <Toggle checked={checked} onChange={onChange} label={title} />
    </div>
  );
}

export default function ProjectForm({
  initial,
  submitLabel,
  onSubmit,
  replicaView,
  viewDeviceId,
  onDirty,
}: {
  /** Device of the copy being viewed; its own-address settings are the ones shown. */
  viewDeviceId?: string;
  /** Told whether the form has unsaved edits. */
  onDirty?: (dirty: boolean) => void;
  initial?: Project;
  /** Name of the replica's device when the page is showing a replica rather than the main copy. */
  replicaView?: string;
  submitLabel: string;
  /** previews is set only when the Previews switch differs from what the project has now. */
  onSubmit: (p: ProjectInput, previews?: { enabled: boolean; max: number }) => Promise<void>;
}) {
  const devices = usePoll(api.devices, 0);
  const settings = usePoll(api.settings, 0);
  const connected = settings.data?.github.connected ?? false;
  const repos = usePoll(() => (connected ? api.githubRepos() : Promise.resolve([])), 0, [connected]);

  const [name, setName] = useState(initial?.name ?? "");
  const [deviceId, setDeviceId] = useState(initial?.device_id ?? "");
  const [source, setSource] = useState<Source>(
    initial?.source ?? { type: "github", repo: "", branch: "main", context: "", dockerfile: "", auto_deploy: true },
  );
  const [ports, setPorts] = useState((initial?.runtime.ports ?? []).join("\n"));
  const [webPort, setWebPort] = useState(initial?.runtime.web_port ?? "");
  const [dedicated, setDedicated] = useState<DedicatedIP | null>(initial?.runtime.dedicated ?? null);
  // Each other copy (replica) has its own address settings, by device id.
  const [copyAddrs, setCopyAddrs] = useState<Record<string, DedicatedIP>>(initial?.runtime.copy_addresses ?? {});
  const [env, setEnv] = useState((initial?.runtime.env ?? []).join("\n"));
  const [volumes, setVolumes] = useState((initial?.runtime.volumes ?? []).join("\n"));
  // New workers stop restarting after a few failures instead of looping forever; existing ones keep their policy.
  const [restart, setRestart] = useState(initial ? initial.runtime.restart || "unless-stopped" : "on-failure");
  const [maxRetries, setMaxRetries] = useState(String(initial ? (initial.runtime.max_retries ?? 0) : 5));
  const [memoryMB, setMemoryMB] = useState(String(initial?.runtime.memory_mb ?? 0));
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Unsaved edits are reported so a page can ask before throwing them away.
  const snapshot = JSON.stringify([name, deviceId, source, ports, webPort, dedicated, copyAddrs, env, volumes, restart, maxRetries, memoryMB]);
  const baseline = useRef(snapshot);
  const report = useRef(onDirty);
  report.current = onDirty;
  useEffect(() => {
    report.current?.(snapshot !== baseline.current);
  }, [snapshot]);
  useEffect(() => () => report.current?.(false), []);

  // Cloudflare builds run on the server, so only admins may set them up or change them.
  const isAdmin = useCan("admin");
  const [tab, setTab] = useState<"production" | "preview">("production");
  const saved = usePoll(() => (initial ? api.projectPreviews(initial.id) : Promise.resolve(null)), 0, [initial?.id]);
  const [previewsPick, setPreviewsPick] = useState<boolean | null>(null);
  const previewsNow = saved.data?.settings.enabled ?? false;
  const previewsOn = previewsPick ?? previewsNow;
  const setPreviewsOn = setPreviewsPick;
  const pv: PreviewBuild = source.preview ?? {};
  const setPv = (patch: Partial<PreviewBuild>) => setSource((s) => ({ ...s, preview: { ...s.preview, ...patch } }));
  const kind = source.node ? (source.node.static ? "static" : "node") : "docker";
  const isCF = source.type === "cloudflare";
  const cfTarget = source.cloudflare ?? { kind: "pages" as const };
  const setCF = (patch: Partial<NonNullable<Source["cloudflare"]>>) => setSource((s) => ({ ...s, cloudflare: { ...(s.cloudflare ?? { kind: "pages" as const }), ...patch } }));

  const renamedCF = !!initial && initial.source.type === "cloudflare" && isCF && name.trim() !== initial.name;

  const device = deviceId || devices.data?.[0]?.id || "";
  // The copy whose own-address settings are shown: the main device, or a replica being viewed.
  const viewing = viewDeviceId || device;
  const viewingCopy = viewing !== device;
  const set = (patch: Partial<Source>) => setSource((s) => ({ ...s, ...patch }));
  const node = source.node;
  const setNode = (patch: Partial<NodeBuild>) => setSource((s) => ({ ...s, node: { ...s.node, ...patch } }));

  // Host folder mounts, host networking and Compose files give a container control of the
  // device, so only admins may add or change them. The server enforces this too.
  const hostMounts = lines(volumes).filter((v) => !/^[A-Za-z0-9][A-Za-z0-9_.-]*:/.test(v));
  const keptMounts = new Set((initial?.runtime.volumes ?? []).filter((v) => !/^[A-Za-z0-9][A-Za-z0-9_.-]*:/.test(v)));
  const addsHostMount = hostMounts.some((v) => !keptMounts.has(v));
  const composeChanged =
    source.type === "compose" &&
    (initial?.source.type !== "compose" ||
      (source.compose ?? "") !== (initial.source.compose ?? "") ||
      (source.repo ?? "") !== (initial.source.repo ?? "") ||
      (source.compose_file ?? "") !== (initial.source.compose_file ?? ""));
  const hostBlocked = !isAdmin && (addsHostMount || composeChanged);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    // The Cloudflare project is found by name. Renaming here does not rename it there.
    let src = source;
    if (renamedCF) {
      const keep = initial!.source.cloudflare?.name || initial!.name;
      const ok = await confirmDialog({
        title: "Rename the dashboard project?",
        message: `This only changes the name here. The Cloudflare ${cfTarget.kind === "pages" ? "Pages project" : "Worker"} is still called "${keep}" and cannot be renamed from the dashboard. ` +
          `If the names no longer match, the link breaks and "${keep}" shows up as a new project to import.` +
          (cfTarget.name ? "" : ` To prevent that, the dashboard will keep pointing at "${keep}".`),
        confirmLabel: "Rename",
      });
      if (!ok) return;
      if (!cfTarget.name) src = { ...source, cloudflare: { ...cfTarget, name: keep } };
    }
    if (dedicated && dedicated.vlan && dedicated.mode === "static") {
      if (!dedicated.ip?.trim()) {
        setError("Enter the worker's IP address for its own address on the network, for example 192.168.5.20.");
        return;
      }
      if (!dedicated.subnet?.trim()) {
        setError("Enter the subnet mask for the VLAN (under Own address on the network, Advanced), for example 24 or 255.255.255.0.");
        return;
      }
    }
    setBusy(true);
    try {
      // A typed address for this device to reach the worker is checked on the network again before it is saved.
      const toCheck: [string, DedicatedIP | null | undefined, string | undefined][] = [
        [device, dedicated, initial?.runtime.dedicated?.host_ip],
        ...Object.entries(copyAddrs).map(([id, d]): [string, DedicatedIP, string | undefined] => [id, d, initial?.runtime.copy_addresses?.[id]?.host_ip]),
      ];
      for (const [id, d, was] of toCheck) {
        const dev = devices.data?.find((x) => x.id === id);
        if (d?.host_ip && d.host_ip !== was && dev && dev.status !== "offline" && dev.host.capabilities?.includes("dedicated-ip")) {
          const r = await api.checkDedicated(dev.id, { ...d, project_id: initial?.id });
          if (r.problems?.length) throw new Error(`${dev.name}: ${r.problems.join(". ")}`);
        }
      }
      await onSubmit(
        { name: name.trim(), device_id: isCF ? "" : device, source: src, runtime: { ports: lines(ports), web_port: webPort.trim(), dedicated: isCF || source.type === "compose" ? null : dedicated, copy_addresses: isCF || source.type === "compose" || Object.keys(copyAddrs).length === 0 ? undefined : copyAddrs, env: normalizeEnv(env), restart, max_retries: restart === "on-failure" ? Math.max(0, Math.floor(Number(maxRetries)) || 0) : 0, memory_mb: Math.max(0, Math.floor(Number(memoryMB)) || 0), volumes: lines(volumes) } },
        previewsPick !== null && previewsPick !== previewsNow ? { enabled: previewsPick, max: saved.data?.settings.max || 5 } : undefined,
      );
      baseline.current = snapshot;
      onDirty?.(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} className="space-y-8">
      {!isCF && <p className="rounded-lg bg-neutral-50 px-4 py-2 text-sm text-neutral-600">Changes to the source, ports, address and environment are saved now but only take effect when the worker is redeployed. Redeploy runs the same source again with the new settings; a new commit or version is deployed from the Deployments tab.</p>}
      <fieldset className="space-y-4">
        <legend className="mb-3 text-base font-semibold">Source</legend>
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {[
            { type: "github" as const, icon: Github, title: "GitHub repository", sub: "Build a Dockerfile or Node.js project on the device, redeploy on push" },
            { type: "image" as const, icon: Package, title: "Container image", sub: "Pull an image from Docker Hub or any registry" },
            { type: "compose" as const, icon: Layers, title: "Compose stack", sub: "Paste a docker-compose file that runs several services together" },
            { type: "cloudflare" as const, icon: Cloud, title: "Cloudflare", sub: "Build here, deploy to Cloudflare Pages or Workers. No device needed" },
          ]
            .filter((o) => o.type !== "cloudflare" || isAdmin || source.type === "cloudflare")
            .filter((o) => o.type !== "compose" || isAdmin || source.type === "compose")
            .map((o) => (
            <button
              type="button"
              key={o.type}
              onClick={() =>
                set(
                  o.type === "cloudflare"
                    ? { type: o.type, cloudflare: source.cloudflare ?? { kind: "pages" }, node: undefined, repo: source.repo?.trim(), branch: source.branch || "main", auto_deploy: true }
                    : { type: o.type, cloudflare: undefined, node: source.type === "cloudflare" ? undefined : source.node },
                )
              }
              className={cx(
                "rounded-xl border p-4 text-left transition-colors",
                source.type === o.type ? "border-primary bg-blue-50/50 ring-1 ring-primary" : "border-neutral-200 bg-white hover:bg-neutral-50",
              )}
            >
              <o.icon className="mb-2 size-5" />
              <p className="text-sm font-medium">{o.title}</p>
              <p className="mt-0.5 text-xs text-neutral-500">{o.sub}</p>
            </button>
          ))}
        </div>

        {source.type === "github" || isCF ? (
          <div className="space-y-6">
            <div className="grid gap-4 sm:grid-cols-2">
              <Field
                label="Repository"
                hint={connected ? "Repositories your GitHub token can see." : "Public repositories work without it. Connect GitHub in Settings for private repositories."}
              >
                {connected && repos.data?.length ? (
                  <select
                    className={inputClass}
                    value={source.repo}
                    onChange={(e) => {
                      const r = repos.data!.find((x) => x.full_name === e.target.value);
                      set({ repo: e.target.value, branch: r?.default_branch ?? source.branch });
                      if (!name && r) setName(r.full_name.split("/")[1].toLowerCase().replace(/[^a-z0-9-]/g, "-"));
                    }}
                  >
                    <option value="">Choose a repository…</option>
                    {repos.data.map((r) => (
                      <option key={r.full_name} value={r.full_name}>
                        {r.full_name}
                        {r.private ? " (private)" : ""}
                      </option>
                    ))}
                  </select>
                ) : (
                  <input className={inputClass} placeholder="owner/repository" value={source.repo} onChange={(e) => set({ repo: e.target.value })} />
                )}
              </Field>
              <Field label="Production branch">
                <input className={inputClass} value={source.branch} onChange={(e) => set({ branch: e.target.value })} />
              </Field>
            </div>

            {isCF && (
              <div>
                <p className="mb-1.5 text-sm font-medium">Where does it go?</p>
                <div className="grid gap-3 sm:grid-cols-2">
                  {[
                    { kind: "pages" as const, icon: Globe, title: "Cloudflare Pages", sub: "A static site or front end: the build output folder is uploaded" },
                    { kind: "worker" as const, icon: Server, title: "Cloudflare Worker", sub: "Server-side code on Cloudflare's edge: one bundled JavaScript module" },
                  ].map((k) => (
                    <button
                      type="button"
                      key={k.kind}
                      onClick={() => setCF({ kind: k.kind })}
                      className={cx(
                        "rounded-xl border p-3.5 text-left transition-colors",
                        cfTarget.kind === k.kind ? "border-primary bg-blue-50/50 ring-1 ring-primary" : "border-neutral-200 bg-white hover:bg-neutral-50",
                      )}
                    >
                      <k.icon className="mb-2 size-5" />
                      <p className="text-sm font-medium">{k.title}</p>
                      <p className="mt-0.5 text-xs text-neutral-500">{k.sub}</p>
                    </button>
                  ))}
                </div>
              </div>
            )}
            {!isCF && (
            <div>
              <p className="mb-1.5 text-sm font-medium">What are you building?</p>
              <div className="grid gap-3 sm:grid-cols-3">
                {BUILD_KINDS.map((k) => (
                  <button
                    type="button"
                    key={k.kind}
                    onClick={() => set({ node: k.kind === "docker" ? undefined : { ...source.node, static: k.kind === "static" } })}
                    className={cx(
                      "rounded-xl border p-3.5 text-left transition-colors",
                      kind === k.kind ? "border-primary bg-blue-50/50 ring-1 ring-primary" : "border-neutral-200 bg-white hover:bg-neutral-50",
                    )}
                  >
                    <k.icon className="mb-2 size-5" />
                    <p className="text-sm font-medium">{k.title}</p>
                    <p className="mt-0.5 text-xs text-neutral-500">{k.sub}</p>
                  </button>
                ))}
              </div>
            </div>
            )}

            <div className="space-y-4">
              <div className="flex items-center gap-3">
                <h3 className="text-sm font-semibold">Builds</h3>
                <div className="inline-flex rounded-lg bg-neutral-100 p-0.5 text-sm" role="tablist">
                  {(["production", "preview"] as const).map((t) => (
                    <button
                      type="button"
                      role="tab"
                      aria-selected={tab === t}
                      key={t}
                      onClick={() => setTab(t)}
                      className={cx("rounded-md px-3 py-1 font-medium transition-colors", tab === t ? "bg-white shadow-sm" : "text-neutral-500 hover:text-neutral-800")}
                    >
                      {t === "production" ? "Production" : "Previews"}
                    </button>
                  ))}
                </div>
              </div>

              {tab === "production" ? (
                <>
                  <Panel>
                    <ToggleRow
                      title="Disable builds"
                      sub="Stop deploying automatically when commits are pushed to the production branch. You can still deploy by hand."
                      checked={!source.auto_deploy}
                      onChange={(off) => set({ auto_deploy: !off })}
                    />
                  </Panel>
                  {isCF ? (
                    <Panel title="Build configuration" sub="Runs on this server in a fresh checkout. Anything left empty is detected.">
                      <div className="grid gap-4 sm:grid-cols-2">
                        <Field label="Install command" hint="Empty: npm ci, pnpm or yarn, chosen from your lock file.">
                          <input className={inputClass} placeholder="npm ci" value={node?.install ?? ""} onChange={(e) => setNode({ install: e.target.value })} />
                        </Field>
                        <Field label="Build command" hint="Empty: npm run build, if package.json has a build script.">
                          <input className={inputClass} placeholder="npm run build" value={node?.build ?? ""} onChange={(e) => setNode({ build: e.target.value })} />
                        </Field>
                        <Field
                          label={cfTarget.kind === "pages" ? "Build output directory" : "Worker entry file"}
                          hint={cfTarget.kind === "pages" ? "Empty: the first of dist, build, out, public or _site." : "A single bundled ES module. Empty: dist/index.js, then src/index.js."}
                        >
                          <input className={inputClass} placeholder={cfTarget.kind === "pages" ? "dist" : "dist/index.js"} value={cfTarget.output ?? ""} onChange={(e) => setCF({ output: e.target.value })} />
                        </Field>
                        <Field label="Root directory" hint="Folder that holds package.json. Empty: the repo root.">
                          <input className={inputClass} placeholder="/" value={source.context} onChange={(e) => set({ context: e.target.value })} />
                        </Field>
                        <Field label={cfTarget.kind === "pages" ? "Pages project name" : "Worker name"} hint="Empty: the project name. Created on first deploy if it doesn't exist.">
                          <input className={inputClass} placeholder={name || "my-site"} value={cfTarget.name ?? ""} onChange={(e) => setCF({ name: e.target.value })} />
                        </Field>
                        {cfTarget.kind === "worker" && (
                          <Field label="Compatibility date" hint="Empty: the day of each deployment.">
                            <input className={inputClass} placeholder="2025-01-31" value={cfTarget.compatibility_date ?? ""} onChange={(e) => setCF({ compatibility_date: e.target.value })} />
                          </Field>
                        )}
                      </div>
                    </Panel>
                  ) : (
                  <Panel title="Build configuration">
                    <div className="grid gap-4 sm:grid-cols-2">
                      {node && (
                        <Field label="Build command" hint="Empty: npm run build, if package.json has a build script.">
                          <input className={inputClass} placeholder="npm run build" value={node.build ?? ""} onChange={(e) => setNode({ build: e.target.value })} />
                        </Field>
                      )}
                      {node && !node.static && (
                        <Field label="Deploy command" hint="Runs in the container. Your app must listen on $PORT. Empty: npm start.">
                          <input className={inputClass} placeholder="npm start" value={node.start ?? ""} onChange={(e) => setNode({ start: e.target.value })} />
                        </Field>
                      )}
                      {!node && (
                        <Field label="Dockerfile" hint="Relative to the root directory. Must be a Dockerfile, not docker-compose.yml. Empty: Dockerfile.">
                          <input className={inputClass} placeholder="Dockerfile" value={source.dockerfile} onChange={(e) => set({ dockerfile: e.target.value })} />
                        </Field>
                      )}
                      <Field label="Root directory" hint={node ? "Folder that holds package.json. Empty: the repo root." : "Folder that holds the Dockerfile. Empty: the repo root."}>
                        <input className={inputClass} placeholder="/" value={source.context} onChange={(e) => set({ context: e.target.value })} />
                      </Field>
                    </div>
                  </Panel>
                  )}
                  <Panel title="Build watch paths" sub="Only commits that change a matching file trigger a build.">
                    <div className="grid gap-4 sm:grid-cols-2">
                      <Field as="div" label="Include paths">
                        <ChipInput value={source.include ?? []} onChange={(v) => set({ include: v })} placeholder="* (all files)" />
                      </Field>
                      <Field as="div" label="Exclude paths">
                        <ChipInput value={source.exclude ?? []} onChange={(v) => set({ exclude: v })} placeholder="node_modules/**, .git/" />
                      </Field>
                    </div>
                  </Panel>
                  {node && (
                    <details className="rounded-xl border border-neutral-200 bg-white">
                      <summary className="cursor-pointer select-none px-5 py-3 text-sm font-medium">Advanced</summary>
                      <div className="grid gap-4 border-t border-neutral-200 p-5 sm:grid-cols-2">
                        <Field label="Install command" hint="Empty: npm ci, pnpm or yarn, chosen from your lock file.">
                          <input className={inputClass} placeholder="npm ci" value={node.install ?? ""} onChange={(e) => setNode({ install: e.target.value })} />
                        </Field>
                        <Field label="Node version" hint="Empty: from .nvmrc or engines.node, else 22.">
                          <input className={inputClass} placeholder="22" value={node.version ?? ""} onChange={(e) => setNode({ version: e.target.value })} />
                        </Field>
                        {node.static && (
                          <Field label="Build output directory" hint="Empty: the first of dist, build, out or public.">
                            <input className={inputClass} placeholder="dist" value={node.output ?? ""} onChange={(e) => setNode({ output: e.target.value })} />
                          </Field>
                        )}
                      </div>
                    </details>
                  )}
                </>
              ) : (
                <>
                  <Panel>
                    <ToggleRow
                      title="Builds for Preview branches"
                      sub={isCF ? (cfTarget.kind === "pages" ? "Deploy every open pull request from this repository into the production branch as a Pages preview deployment." : "Deploy every open pull request from this repository into the production branch as its own Worker, removed when the pull request closes.") : "Build and serve every open pull request from this repository into the production branch on its own port."}
                      checked={previewsOn}
                      onChange={setPreviewsOn}
                    />
                  </Panel>
                  <div className={cx("space-y-4", !previewsOn && "opacity-50")}>
                    <p className="text-xs text-neutral-500">Anything left empty uses the Production setting shown as its placeholder.</p>
                    <Panel title="Build configuration">
                      <div className="grid gap-4 sm:grid-cols-2">
                        {(node || isCF) && (
                          <Field label="Build command">
                            <input className={inputClass} placeholder={node?.build || "npm run build"} value={pv.build ?? ""} onChange={(e) => setPv({ build: e.target.value })} />
                          </Field>
                        )}
                        {node && !node.static && !isCF && (
                          <Field label="Preview command">
                            <input className={inputClass} placeholder={node.start || "npm start"} value={pv.start ?? ""} onChange={(e) => setPv({ start: e.target.value })} />
                          </Field>
                        )}
                        <Field label="Root directory">
                          <input className={inputClass} placeholder={source.context || "/"} value={pv.context ?? ""} onChange={(e) => setPv({ context: e.target.value })} />
                        </Field>
                      </div>
                    </Panel>
                    <Panel title="Build watch paths">
                      <div className="grid gap-4 sm:grid-cols-2">
                        <Field as="div" label="Include paths">
                          <ChipInput value={pv.include ?? []} onChange={(v) => setPv({ include: v })} placeholder={source.include?.join(", ") || "* (all files)"} />
                        </Field>
                        <Field as="div" label="Exclude paths">
                          <ChipInput value={pv.exclude ?? []} onChange={(v) => setPv({ exclude: v })} placeholder={source.exclude?.join(", ") || "node_modules/**, .git/"} />
                        </Field>
                      </div>
                    </Panel>
                  </div>
                </>
              )}
            </div>
          </div>
        ) : source.type === "compose" ? (
          <div className="space-y-4">
            <div className="inline-flex rounded-lg bg-neutral-100 p-0.5 text-sm" role="tablist">
              {([false, true] as const).map((fromRepo) => (
                <button
                  type="button"
                  role="tab"
                  key={String(fromRepo)}
                  aria-selected={!!source.repo === fromRepo}
                  onClick={() => set(fromRepo ? { repo: source.repo || " ", branch: source.branch || "main", compose_file: source.compose_file || "docker-compose.yml", auto_deploy: true } : { repo: "", auto_deploy: false })}
                  className={cx("rounded-md px-3 py-1 font-medium transition-colors", !!source.repo === fromRepo ? "bg-white shadow-sm" : "text-neutral-500 hover:text-neutral-800")}
                >
                  {fromRepo ? "From GitHub" : "Paste YAML"}
                </button>
              ))}
            </div>
            {source.repo ? (
              <>
                <div className="grid gap-4 sm:grid-cols-3">
                  <Field label="Repository" hint="Public repositories work without it. Connect GitHub in Settings for private repositories.">
                    <input className={inputClass} placeholder="owner/repository" value={source.repo.trim()} onChange={(e) => set({ repo: e.target.value || " " })} />
                  </Field>
                  <Field label="Branch">
                    <input className={inputClass} value={source.branch ?? ""} onChange={(e) => set({ branch: e.target.value })} />
                  </Field>
                  <Field label="Compose file" hint="Path in the repository.">
                    <input className={inputClass} placeholder="docker-compose.yml" value={source.compose_file ?? ""} onChange={(e) => set({ compose_file: e.target.value })} />
                  </Field>
                </div>
                <Panel>
                  <ToggleRow
                    title="Redeploy on push"
                    sub="Read the compose file again and update the stack whenever the branch changes. Services with build: are built from a checkout of the repository on the device (needs git)."
                    checked={source.auto_deploy}
                    onChange={(on) => set({ auto_deploy: on })}
                  />
                </Panel>
                <Field as="div" label="Only for changes to" hint="Include or exclude paths; empty means any change redeploys.">
                  <ChipInput value={source.include ?? []} onChange={(v) => set({ include: v })} placeholder="* (all files)" />
                </Field>
              </>
            ) : null}
          {!source.repo && (<Field
            label="docker-compose.yml"
            hint="Use ${VAR} or ${VAR:?message} for values you set under Environment variables below, such as ${TUNNEL_TOKEN:?needed}. Relative bind mounts (./data) are not supported; use named volumes."
          >
            <textarea
              className={cx(inputClass, "font-mono text-xs")}
              rows={16}
              spellCheck={false}
              placeholder={"services:\n  app:\n    image: nginx:latest\n    ports: [\"8080:80\"]"}
              value={source.compose ?? ""}
              onChange={(e) => set({ compose: e.target.value })}
            />
          </Field>)}
          </div>
        ) : (
          <Field label="Image" hint="For example nginx:1.27, ghcr.io/owner/app:latest or pihole/pihole.">
            <input className={inputClass} placeholder="nginx:latest" value={source.image} onChange={(e) => set({ image: e.target.value })} />
          </Field>
        )}
      </fieldset>

      <fieldset className="space-y-4">
        <legend className="mb-3 text-base font-semibold">Project</legend>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Project name" hint={source.type === "compose" ? "Lowercase letters, digits and dashes. Also the Compose project name." : "Lowercase letters, digits and dashes. Also used as the container name."}>
            <input className={inputClass} placeholder="my-service" value={name} onChange={(e) => setName(e.target.value)} />
            {renamedCF && (
              <p className="mt-1.5 text-sm text-amber-700">
                Changing this doesn't rename the Cloudflare project. Keep the Cloudflare name below as it is, or the link breaks and the old project appears as a new one to import.
              </p>
            )}
          </Field>
          {!isCF && (<Field label={replicaView ? "Device (main copy)" : "Device"} hint={replicaView ? "You are viewing a replica. This is the main copy's device, and settings apply to every copy." : undefined}>
            <select className={inputClass} value={device} onChange={(e) => setDeviceId(e.target.value)}>
              {devices.data?.length === 0 && <option value="">No devices registered yet</option>}
              {devices.data?.map((d) => (
                <option key={d.id} value={d.id} disabled={d.maintenance && d.id !== initial?.device_id}>
                  {d.name} ({d.host.arch}
                  {d.status === "offline" ? ", offline" : ""}
                  {d.maintenance ? ", maintenance" : ""})
                </option>
              ))}
            </select>
          </Field>)}
        </div>
      </fieldset>

      <fieldset className="space-y-4">
        <legend className="mb-3 text-base font-semibold">{source.type === "compose" ? "Environment" : isCF ? "Build environment" : "Runtime"}</legend>
        {source.type !== "compose" && !isCF && (<>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Ports" hint={node ? "host:container. Empty: 8080:80 for a static site, 3000:3000 for a Node server (it gets PORT set to the container port)." : "One per line: host:container, e.g. 8080:80 or 53:53/udp."}>
            <textarea className={cx(inputClass, "font-mono")} rows={3} value={ports} onChange={(e) => setPorts(e.target.value)} />
          </Field>
          <Field label="Web port" hint="The host port that serves the website when there are several. The Cloudflare Tunnel, domains and the production URL send traffic here. Empty: the first TCP port.">
            <input className={inputClass} inputMode="numeric" placeholder="e.g. 9080" value={webPort} onChange={(e) => setWebPort(e.target.value)} />
          </Field>
          <Field label="Restart policy">
            <select className={inputClass} value={restart} onChange={(e) => setRestart(e.target.value)}>
              <option value="unless-stopped">Unless stopped</option>
              <option value="always">Always</option>
              <option value="on-failure">On failure</option>
              <option value="no">Never</option>
            </select>
          </Field>
          {restart === "on-failure" && (
            <Field label="Maximum restarts" hint="Docker gives up after this many failed restarts in a row and leaves the container stopped, instead of looping forever. 0 means no limit.">
              <input className={inputClass} type="number" inputMode="numeric" min={0} value={maxRetries} onChange={(e) => setMaxRetries(e.target.value)} />
            </Field>
          )}
          <Field label="Memory limit (MiB)" hint="Docker kills the container if it uses more (and, with swap off, it never swaps). 0 means no limit. A worker killed for running out of memory needs a higher limit or more RAM on the device. Raising it applies to the running container straight away.">
            <input className={inputClass} type="number" inputMode="numeric" min={0} value={memoryMB} onChange={(e) => setMemoryMB(e.target.value)} />
          </Field>
        </div>
        <DedicatedAddress
          value={viewingCopy ? (copyAddrs[viewing] ?? null) : dedicated}
          onChange={(v) => {
            if (!viewingCopy) return setDedicated(v);
            setCopyAddrs((m) => {
              const next = { ...m };
              if (v) next[viewing] = v;
              else delete next[viewing];
              return next;
            });
          }}
          device={devices.data?.find((d) => d.id === viewing)}
          projectId={initial?.id}
          canEdit={isAdmin}
          copyLabel={viewingCopy || replicaView ? `${devices.data?.find((d) => d.id === viewing)?.name ?? ""} (${viewingCopy ? "replica" : "main"})` : undefined}
        />
        <Field label="Volumes" hint="One per line: name:/path for a named volume, /host/path:/path for a folder, add :ro for read-only. Existing data stays when the container is recreated.">
          <textarea className={cx(inputClass, "font-mono")} rows={3} value={volumes} onChange={(e) => setVolumes(e.target.value)} />
        </Field>
        {!isAdmin && (
          <p className="text-sm text-neutral-500">
            Only administrators can mount host folders (a /path source), because that gives the container access to the device itself. Named volumes are fine.
          </p>
        )}
        </>)}
        <Field as="div" label="Environment variables" hint={isCF ? "Set while the build runs, so VITE_* style variables are baked into the site. They are not set on a deployed Worker." : source.type === "compose" ? "Fills the ${VAR} references in the compose file. They are not passed to the containers unless the file lists them under environment." : node?.static ? "Passed to the build, so VITE_* style variables are baked into the site." : "Edit line by line, paste a whole .env, or import a file."}>
          <EnvEditor value={env} onChange={setEnv} />
        </Field>
      </fieldset>

      {error && <ErrorNote>{error}</ErrorNote>}
      {hostBlocked && (
        <p className="text-sm text-neutral-500">
          Only administrators can add or change host folder mounts and Compose files, because they give containers control of the device. Remove the host folder
          {source.type === "compose" ? " or leave the Compose file as it was" : ""} to save.
        </p>
      )}
      {isCF && !isAdmin && (
        <p className="text-sm text-neutral-500">Only administrators can change Cloudflare projects, because their builds run on the Orchestrator server itself.</p>
      )}
      <div className="flex justify-end">
        <Button variant="primary" loading={busy} disabled={(!device && !isCF) || (isCF && !isAdmin) || hostBlocked}>
          {submitLabel}
        </Button>
      </div>
    </form>
  );
}
