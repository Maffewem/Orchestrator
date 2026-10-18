import { useState, type FormEvent } from "react";
import { Github, Package } from "lucide-react";
import { api, type NodeBuild, type Project, type ProjectInput, type Source } from "../api";
import EnvEditor from "../components/EnvEditor";
import { Button, ErrorNote, Field, inputClass } from "../components/ui";
import { cx, usePoll } from "../lib";

const lines = (s: string) => s.split("\n").map((l) => l.trim()).filter(Boolean);

export default function ProjectForm({
  initial,
  submitLabel,
  onSubmit,
}: {
  initial?: Project;
  submitLabel: string;
  onSubmit: (p: ProjectInput) => Promise<void>;
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
  const [env, setEnv] = useState((initial?.runtime.env ?? []).join("\n"));
  const [restart, setRestart] = useState(initial?.runtime.restart || "unless-stopped");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const device = deviceId || devices.data?.[0]?.id || "";
  const set = (patch: Partial<Source>) => setSource((s) => ({ ...s, ...patch }));
  const node = source.node;
  const setNode = (patch: Partial<NodeBuild>) => setSource((s) => ({ ...s, node: { ...s.node, ...patch } }));

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await onSubmit({ name: name.trim(), device_id: device, source, runtime: { ports: lines(ports), env: lines(env), restart } });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} className="space-y-8">
      <fieldset className="space-y-4">
        <legend className="mb-3 text-base font-semibold">Source</legend>
        <div className="grid grid-cols-2 gap-3">
          {[
            { type: "github" as const, icon: Github, title: "GitHub repository", sub: "Build a Dockerfile or Node.js project on the device, redeploy on push" },
            { type: "image" as const, icon: Package, title: "Container image", sub: "Pull an image from Docker Hub or any registry" },
          ].map((o) => (
            <button
              type="button"
              key={o.type}
              onClick={() => set({ type: o.type })}
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

        {source.type === "github" ? (
          <div className="grid gap-4 sm:grid-cols-2">
            <Field
              label="Repository"
              hint={connected ? "Repositories your GitHub token can see." : "Public repos work as-is. Connect GitHub in Settings for private repos."}
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
            <Field label="Build with" hint="Node.js projects need no Dockerfile: install, build and run are generated.">
              <select
                className={inputClass}
                value={source.node ? (source.node.static ? "static" : "node") : "docker"}
                onChange={(e) => {
                  const v = e.target.value;
                  set({ node: v === "docker" ? undefined : { ...source.node, static: v === "static" } });
                }}
              >
                <option value="docker">Dockerfile</option>
                <option value="node">Node.js server (npm, pnpm or yarn)</option>
                <option value="static">Node.js static site (Vite, React, Astro…)</option>
              </select>
            </Field>
            <Field label="Root directory" hint={node ? "Folder that holds package.json. Leave empty for the repo root." : "Folder that holds the Dockerfile. Leave empty for the repo root."}>
              <input className={inputClass} placeholder="/" value={source.context} onChange={(e) => set({ context: e.target.value })} />
            </Field>
            {node ? (
              <>
                <Field label="Install command" hint="Empty: npm ci, pnpm or yarn, chosen from your lock file.">
                  <input className={inputClass} placeholder="npm ci" value={node.install ?? ""} onChange={(e) => setNode({ install: e.target.value })} />
                </Field>
                <Field label="Build command" hint="Empty: npm run build, if package.json has a build script.">
                  <input className={inputClass} placeholder="npm run build" value={node.build ?? ""} onChange={(e) => setNode({ build: e.target.value })} />
                </Field>
                {node.static ? (
                  <Field label="Build output directory" hint="Empty: the first of dist, build, out or public.">
                    <input className={inputClass} placeholder="dist" value={node.output ?? ""} onChange={(e) => setNode({ output: e.target.value })} />
                  </Field>
                ) : (
                  <Field label="Start command" hint="Empty: npm start. Your app must listen on $PORT (the container port below).">
                    <input className={inputClass} placeholder="npm start" value={node.start ?? ""} onChange={(e) => setNode({ start: e.target.value })} />
                  </Field>
                )}
                <Field label="Node version" hint="Empty: from .nvmrc or engines.node, else 22.">
                  <input className={inputClass} placeholder="22" value={node.version ?? ""} onChange={(e) => setNode({ version: e.target.value })} />
                </Field>
              </>
            ) : (
              <Field label="Dockerfile" hint="Relative to the root directory.">
                <input className={inputClass} placeholder="Dockerfile" value={source.dockerfile} onChange={(e) => set({ dockerfile: e.target.value })} />
              </Field>
            )}
            <label className="flex items-center gap-2 text-sm sm:col-span-2">
              <input type="checkbox" className="size-4 accent-primary" checked={source.auto_deploy} onChange={(e) => set({ auto_deploy: e.target.checked })} />
              Deploy automatically when commits are pushed to the production branch
            </label>
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
          <Field label="Project name" hint="Lowercase letters, digits and dashes. Also used as the container name.">
            <input className={inputClass} placeholder="my-service" value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Device">
            <select className={inputClass} value={device} onChange={(e) => setDeviceId(e.target.value)}>
              {devices.data?.length === 0 && <option value="">No devices registered yet</option>}
              {devices.data?.map((d) => (
                <option key={d.id} value={d.id}>
                  {d.name} ({d.host.arch}
                  {d.status === "offline" ? ", offline" : ""})
                </option>
              ))}
            </select>
          </Field>
        </div>
      </fieldset>

      <fieldset className="space-y-4">
        <legend className="mb-3 text-base font-semibold">Runtime</legend>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Ports" hint={node ? "host:container. Empty: 8080:80 for a static site, 3000:3000 for a Node server (it gets PORT set to the container port)." : "One per line: host:container, e.g. 8080:80 or 53:53/udp."}>
            <textarea className={cx(inputClass, "font-mono")} rows={3} value={ports} onChange={(e) => setPorts(e.target.value)} />
          </Field>
          <Field label="Restart policy">
            <select className={inputClass} value={restart} onChange={(e) => setRestart(e.target.value)}>
              <option value="unless-stopped">Unless stopped</option>
              <option value="always">Always</option>
              <option value="on-failure">On failure</option>
              <option value="no">Never</option>
            </select>
          </Field>
        </div>
        <Field as="div" label="Environment variables" hint={node?.static ? "Passed to the build, so VITE_* style variables are baked into the site." : "Edit line by line, paste a whole .env, or import a file."}>
          <EnvEditor value={env} onChange={setEnv} />
        </Field>
      </fieldset>

      {error && <ErrorNote>{error}</ErrorNote>}
      <div className="flex justify-end">
        <Button variant="primary" loading={busy} disabled={!device}>
          {submitLabel}
        </Button>
      </div>
    </form>
  );
}
