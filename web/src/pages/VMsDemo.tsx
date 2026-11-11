// DEMO ONLY: a visual mock of how virtual machines could look in the dashboard.
// Everything here is sample data held in this file. There is no libvirt/KVM, no agent support and no API behind it.
import { useState } from "react";
import { Link, Navigate, Route, Routes, useNavigate, useParams } from "react-router";
import { Camera, Cpu, HardDrive, MemoryStick, Monitor, Network, Pause, Play, Plus, Power, RotateCcw, Server, Square, Trash2 } from "lucide-react";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, CardHeader, Field, Meter, PageHeader, StatusDot, Table, Td, inputClass } from "../components/ui";

type VM = {
  id: string; name: string; os: "ubuntu" | "debian" | "windows"; osLabel: string; state: "running" | "stopped" | "paused";
  device: string; deviceId: string; ip: string; ports: string; cpu: number; ram: number; disk: number; cpuUse: number; ramUse: number; diskUse: number; uptime: string; network: string;
};

const vms: VM[] = [
  { id: "web-staging", name: "web-staging", os: "ubuntu", osLabel: "Ubuntu 24.04 LTS", state: "running", device: "portainer-vm", deviceId: "d1", ip: "192.168.0.221", ports: "22, 80, 443", cpu: 2, ram: 4, disk: 40, cpuUse: 18, ramUse: 52, diskUse: 31, uptime: "6d 4h", network: "Bridge br0" },
  { id: "build-runner", name: "build-runner", os: "debian", osLabel: "Debian 12", state: "running", device: "cm4", deviceId: "d2", ip: "192.168.0.233", ports: "22", cpu: 4, ram: 8, disk: 80, cpuUse: 63, ramUse: 71, diskUse: 48, uptime: "2d 11h", network: "Bridge br0" },
  { id: "win11-test", name: "win11-test", os: "windows", osLabel: "Windows 11 Pro", state: "paused", device: "portainer-vm", deviceId: "d1", ip: "192.168.0.240", ports: "3389", cpu: 4, ram: 8, disk: 100, cpuUse: 0, ramUse: 44, diskUse: 62, uptime: "paused", network: "NAT default" },
  { id: "old-nas", name: "old-nas", os: "debian", osLabel: "Debian 12", state: "stopped", device: "claude", deviceId: "d3", ip: "—", ports: "—", cpu: 1, ram: 2, disk: 20, cpuUse: 0, ramUse: 0, diskUse: 12, uptime: "stopped", network: "NAT default" },
];

const tone = (s: VM["state"]) => (s === "running" ? "ok" : s === "paused" ? "warn" : "neutral");

function DemoBanner() {
  return (
    <div className="mb-6 rounded-xl border border-amber-300 bg-amber-50 px-4 py-3 text-sm text-amber-900">
      <strong>Demo only.</strong> Virtual machines are not a working feature yet. This page shows sample data to preview how VMs could fit into the dashboard.
    </div>
  );
}

function OsIcon({ os }: { os: VM["os"] }) {
  const c = os === "ubuntu" ? "bg-orange-500" : os === "debian" ? "bg-rose-600" : "bg-sky-600";
  return <span className={`inline-flex size-6 items-center justify-center rounded-md text-[10px] font-bold text-white ${c}`}>{os === "windows" ? "W" : os === "ubuntu" ? "U" : "D"}</span>;
}

const workers = [
  { name: "api", kind: "Worker", device: "claude", ip: "192.168.0.184", ports: "3000", state: "running", detail: "ghcr.io/maffewem/api" },
  { name: "homepage", kind: "Worker", device: "cm4", ip: "192.168.0.189", ports: "8080", state: "running", detail: "nginx:alpine" },
];

function List() {
  useCrumbs([{ label: "VMs (demo)" }]);
  return (
    <>
      <PageHeader title="Virtual machines" description="Full machines running on your devices, next to your workers and stacks." actions={<Link to="/vms/new"><Button variant="primary" icon={<Plus className="size-4" />}>Create VM</Button></Link>} />
      <DemoBanner />
      <Card className="mb-6">
        <CardHeader title="Workers and stacks (fleet list with VMs)" subtitle="The same list as today, with a Kind column." />
        <Table head={["Name", "Kind", "Device", "IP", "Ports", "State"]}>
          {[...workers.map((w) => ({ ...w, vm: null as VM | null })), ...vms.map((v) => ({ name: v.name, kind: "VM", device: v.device, ip: v.ip, ports: v.ports, state: v.state, detail: v.osLabel, vm: v }))].map((r) => (
            <tr key={r.name} className="border-t border-neutral-100">
              <Td>{r.vm ? <Link to={`/vms/${r.vm.id}`} className="flex items-center gap-2 font-medium text-primary hover:underline"><OsIcon os={r.vm.os} />{r.name}</Link> : <span className="font-medium">{r.name}</span>}<div className="text-xs text-neutral-500">{r.detail}</div></Td>
              <Td><Badge tone={r.vm ? "info" : "neutral"}>{r.kind}</Badge></Td>
              <Td>{r.device}</Td>
              <Td className="font-mono text-xs">{r.ip}</Td>
              <Td className="font-mono text-xs">{r.ports}</Td>
              <Td><span className="flex items-center gap-2"><StatusDot status={r.state} />{r.state}</span></Td>
            </tr>
          ))}
        </Table>
      </Card>
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        {vms.map((v) => (
          <Link key={v.id} to={`/vms/${v.id}`}>
            <Card className="p-4 transition-shadow hover:shadow-md">
              <div className="flex items-center justify-between"><span className="flex items-center gap-2 font-medium"><OsIcon os={v.os} />{v.name}</span><Badge tone={tone(v.state)}>{v.state}</Badge></div>
              <p className="mt-1 text-xs text-neutral-500">{v.osLabel} on {v.device}</p>
              <p className="mt-3 text-sm">{v.cpu} vCPU · {v.ram} GB · {v.disk} GB</p>
              <p className="font-mono text-xs text-neutral-500">{v.ip}</p>
            </Card>
          </Link>
        ))}
      </div>
    </>
  );
}

function Create() {
  useCrumbs([{ label: "VMs (demo)", to: "/vms" }, { label: "Create" }]);
  const nav = useNavigate();
  const [os, setOs] = useState("ubuntu");
  const oses = [["ubuntu", "Ubuntu 24.04 LTS", "Cloud image · ~600 MB"], ["debian", "Debian 12", "Cloud image · ~400 MB"], ["windows", "Windows 11 / Server", "Your ISO · VirtIO drivers added"]];
  return (
    <>
      <PageHeader back={{ to: "/vms", label: "VMs" }} title="Create virtual machine" description="Pick an operating system, size it, and choose which device runs it." />
      <DemoBanner />
      <div className="grid max-w-3xl gap-4">
        <Card>
          <CardHeader title="Operating system" />
          <div className="grid gap-3 p-5 sm:grid-cols-3">
            {oses.map(([k, l, d]) => (
              <button key={k} onClick={() => setOs(k)} className={`rounded-xl border p-4 text-left ${os === k ? "border-primary bg-blue-50" : "border-neutral-200 hover:bg-neutral-50"}`}>
                <OsIcon os={k as VM["os"]} /><p className="mt-2 text-sm font-medium">{l}</p><p className="text-xs text-neutral-500">{d}</p>
              </button>
            ))}
          </div>
        </Card>
        <Card>
          <CardHeader title="Machine" />
          <div className="grid gap-4 p-5 sm:grid-cols-2">
            <Field label="Name"><input className={inputClass} defaultValue="my-new-vm" /></Field>
            <Field label="Device" hint="Only devices with virtualisation are listed."><select className={inputClass}><option>portainer-vm (KVM ready)</option><option>cm4 (KVM ready, ARM)</option></select></Field>
            <Field label="CPU cores"><select className={inputClass} defaultValue="2">{[1, 2, 4, 8].map((n) => <option key={n}>{n}</option>)}</select></Field>
            <Field label="Memory"><select className={inputClass} defaultValue="4 GB">{["1 GB", "2 GB", "4 GB", "8 GB", "16 GB"].map((n) => <option key={n}>{n}</option>)}</select></Field>
            <Field label="Disk size"><input className={inputClass} defaultValue="40 GB" /></Field>
            <Field label="Network"><select className={inputClass}><option>Bridge br0 (own LAN IP)</option><option>NAT (default)</option></select></Field>
          </div>
        </Card>
        <Card>
          <CardHeader title={os === "windows" ? "Install media" : "First boot (cloud-init)"} />
          <div className="grid gap-4 p-5">
            {os === "windows" ? (
              <Field label="ISO"><input className={inputClass} defaultValue="Win11_23H2_English_x64.iso (uploaded)" /></Field>
            ) : (
              <>
                <Field label="Username"><input className={inputClass} defaultValue="matthew" /></Field>
                <Field label="SSH public key"><textarea className={inputClass} rows={2} defaultValue="ssh-ed25519 AAAA… matthew@laptop" /></Field>
              </>
            )}
          </div>
        </Card>
        <div className="flex gap-2"><Button variant="primary" onClick={() => nav("/vms/web-staging")}>Create VM (demo)</Button><Link to="/vms"><Button>Cancel</Button></Link></div>
      </div>
    </>
  );
}

const steps = [["Downloading image", "Ubuntu 24.04 cloud image", "00:41"], ["Creating disk", "40 GB qcow2", "00:02"], ["Applying cloud-init", "user, ssh key, hostname", "00:01"], ["Booting", "KVM, 2 vCPU, 4 GB", "00:09"], ["Verifying", "Got IP 192.168.0.221, SSH reachable", "00:14"]];

function Detail() {
  const { id } = useParams();
  const v = vms.find((x) => x.id === id);
  useCrumbs([{ label: "VMs (demo)", to: "/vms" }, { label: v?.name ?? "VM" }]);
  const [tab, setTab] = useState("overview");
  if (!v) return <Navigate to="/vms" />;
  const tabs = ["overview", "console", "snapshots", "networking", "settings"];
  return (
    <>
      <PageHeader back={{ to: "/vms", label: "VMs" }}
        title={<span className="flex items-center gap-3"><StatusDot status={v.state} />{v.name}<Badge tone="info">VM</Badge></span>}
        description={<>{v.osLabel} on <Link to="/vms/device" className="text-primary hover:underline">{v.device}</Link> · {v.state === "running" ? `up ${v.uptime}` : v.state}</>}
        actions={<>
          <Button icon={<Play className="size-4" />}>Start</Button>
          <Button icon={<Pause className="size-4" />}>Pause</Button>
          <Button icon={<Square className="size-4" />}>Shut down</Button>
          <Button icon={<Power className="size-4" />}>Force off</Button>
        </>} />
      <DemoBanner />
      <div className="mb-6 flex flex-wrap gap-1 rounded-xl border border-neutral-200 bg-neutral-100 p-1 w-fit">
        {tabs.map((t) => <button key={t} onClick={() => setTab(t)} className={`rounded-lg px-3.5 py-1.5 text-sm capitalize ${tab === t ? "border border-neutral-200 bg-white font-medium shadow-sm" : "border border-transparent text-neutral-600"}`}>{t}</button>)}
      </div>
      {tab === "overview" && (
        <div className="grid gap-4 lg:grid-cols-3">
          <Card className="lg:col-span-2">
            <CardHeader title="Latest provisioning" subtitle="Created 6 days ago from the Create VM form" action={<Badge tone="ok">Success</Badge>} />
            <ol className="p-5">
              {steps.map(([s, d, t]) => (
                <li key={s} className="flex items-center gap-3 border-b border-neutral-100 py-2.5 last:border-0"><span className="flex size-5 items-center justify-center rounded-full bg-ok text-[11px] text-white">✓</span><span className="w-44 text-sm font-medium">{s}</span><span className="flex-1 text-sm text-neutral-500">{d}</span><span className="font-mono text-xs text-neutral-400">{t}</span></li>
              ))}
            </ol>
          </Card>
          <Card>
            <CardHeader title="Resources" />
            <div className="grid gap-4 p-5 text-sm">
              <div><p className="mb-1 flex items-center gap-2"><Cpu className="size-4" />{v.cpu} vCPU</p><Meter value={v.cpuUse} label={`${v.cpuUse}%`} /></div>
              <div><p className="mb-1 flex items-center gap-2"><MemoryStick className="size-4" />{v.ram} GB memory</p><Meter value={v.ramUse} label={`${v.ramUse}%`} /></div>
              <div><p className="mb-1 flex items-center gap-2"><HardDrive className="size-4" />{v.disk} GB disk</p><Meter value={v.diskUse} label={`${v.diskUse}%`} /></div>
            </div>
          </Card>
          <Card className="lg:col-span-3">
            <CardHeader title="Access" />
            <dl className="grid gap-4 p-5 text-sm sm:grid-cols-4">
              <div><dt className="text-neutral-500">IP address</dt><dd className="font-mono">{v.ip}</dd></div>
              <div><dt className="text-neutral-500">Open ports</dt><dd className="font-mono">{v.ports}</dd></div>
              <div><dt className="text-neutral-500">Network</dt><dd>{v.network}</dd></div>
              <div><dt className="text-neutral-500">Domain</dt><dd className="text-primary">{v.name}.example.com</dd></div>
            </dl>
          </Card>
        </div>
      )}
      {tab === "console" && (
        <Card>
          <CardHeader title="Console" subtitle={v.os === "windows" ? "Graphical console (VNC in the browser)" : "Serial console in the browser, like the worker shell"} action={<Button size="sm" icon={<Monitor className="size-4" />}>Full screen</Button>} />
          {v.os === "windows" ? (
            <div className="flex h-80 items-center justify-center bg-sky-700 text-white"><div className="text-center"><div className="mx-auto mb-3 grid size-14 grid-cols-2 gap-1"><i className="bg-white/90" /><i className="bg-white/90" /><i className="bg-white/90" /><i className="bg-white/90" /></div>Windows 11 desktop (mock)</div></div>
          ) : (
            <pre className="h-80 overflow-auto bg-neutral-950 p-4 font-mono text-xs leading-5 text-green-300">{`${v.osLabel} ${v.name} ttyS0

${v.name} login: matthew
Welcome to ${v.osLabel} (GNU/Linux 6.8.0-45-generic x86_64)

matthew@${v.name}:~$ ip -4 addr show enp1s0 | grep inet
    inet ${v.ip}/24 brd 192.168.0.255 scope global enp1s0
matthew@${v.name}:~$ uptime
 23:41:02 up ${v.uptime},  1 user,  load average: 0.12, 0.09, 0.05
matthew@${v.name}:~$ █`}</pre>
          )}
        </Card>
      )}
      {tab === "snapshots" && (
        <Card>
          <CardHeader title="Snapshots" action={<Button size="sm" variant="primary" icon={<Camera className="size-4" />}>Take snapshot</Button>} />
          <Table head={["Name", "Taken", "Size", ""]}>
            {[["before-upgrade", "2 days ago", "1.8 GB"], ["fresh-install", "6 days ago", "1.1 GB"]].map(([n, t, s]) => (
              <tr key={n} className="border-t border-neutral-100"><Td className="font-medium">{n}</Td><Td>{t}</Td><Td>{s}</Td><Td><span className="flex justify-end gap-2"><Button size="sm" icon={<RotateCcw className="size-3.5" />}>Restore</Button><Button size="sm" variant="danger" icon={<Trash2 className="size-3.5" />}>Delete</Button></span></Td></tr>
            ))}
          </Table>
        </Card>
      )}
      {tab === "networking" && (
        <Card><CardHeader title="Networking" /><dl className="grid gap-4 p-5 text-sm sm:grid-cols-3"><div><dt className="text-neutral-500">Mode</dt><dd>{v.network}</dd></div><div><dt className="text-neutral-500">MAC</dt><dd className="font-mono">52:54:00:3a:91:0c</dd></div><div><dt className="text-neutral-500">Detected IP</dt><dd className="font-mono">{v.ip}</dd></div></dl></Card>
      )}
      {tab === "settings" && (
        <Card><CardHeader title="Settings" /><div className="grid max-w-md gap-4 p-5"><Field label="vCPUs"><input className={inputClass} defaultValue={v.cpu} /></Field><Field label="Memory (GB)"><input className={inputClass} defaultValue={v.ram} /></Field><label className="flex items-center gap-2 text-sm"><input type="checkbox" defaultChecked /> Start automatically when the device boots</label><div><Button variant="danger" icon={<Trash2 className="size-4" />}>Delete VM</Button></div></div></Card>
      )}
    </>
  );
}

function DevicePreview() {
  useCrumbs([{ label: "Devices" }, { label: "portainer-vm (demo)" }]);
  const mine = vms.filter((v) => v.deviceId === "d1");
  return (
    <>
      <PageHeader back={{ to: "/vms", label: "VMs" }} title={<span className="flex items-center gap-3"><StatusDot status="online" />portainer-vm</span>} description="amd64 · 192.168.0.150 · agent online" />
      <DemoBanner />
      <div className="grid gap-4">
        <Card>
          <CardHeader title="Virtualisation" subtitle="Detected by the agent" action={<Badge tone="ok">KVM ready</Badge>} />
          <dl className="grid gap-4 p-5 text-sm sm:grid-cols-4">
            <div><dt className="text-neutral-500">Hardware virtualisation</dt><dd>Enabled (VT-x)</dd></div>
            <div><dt className="text-neutral-500">libvirt</dt><dd>10.0.0 · running</dd></div>
            <div><dt className="text-neutral-500">Free for VMs</dt><dd>6 cores · 11 GB RAM · 210 GB disk</dd></div>
            <div><dt className="text-neutral-500">Bridge</dt><dd>br0 available</dd></div>
          </dl>
        </Card>
        <Card>
          <CardHeader title="Virtual machines" action={<Link to="/vms/new"><Button size="sm" variant="primary" icon={<Plus className="size-4" />}>Create VM</Button></Link>} />
          <Table head={["Name", "OS", "Resources", "IP", "State"]}>
            {mine.map((v) => (
              <tr key={v.id} className="border-t border-neutral-100"><Td><Link to={`/vms/${v.id}`} className="font-medium text-primary hover:underline">{v.name}</Link></Td><Td>{v.osLabel}</Td><Td>{v.cpu} vCPU · {v.ram} GB · {v.disk} GB</Td><Td className="font-mono text-xs">{v.ip}</Td><Td><Badge tone={tone(v.state)}>{v.state}</Badge></Td></tr>
            ))}
          </Table>
        </Card>
        <Card><CardHeader title="Workers" subtitle="Containers on this device are unchanged" /><p className="flex items-center gap-2 p-5 text-sm text-neutral-500"><Server className="size-4" /><Network className="size-4" /> 3 containers, listed as today.</p></Card>
      </div>
    </>
  );
}

export default function VMsDemoPage() {
  return (
    <Routes>
      <Route index element={<List />} />
      <Route path="new" element={<Create />} />
      <Route path="device" element={<DevicePreview />} />
      <Route path=":id" element={<Detail />} />
    </Routes>
  );
}
