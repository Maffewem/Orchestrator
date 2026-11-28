import { useState } from "react";
import { HardDrive, Network, RefreshCw, Usb } from "lucide-react";
import { api, type Device, type Disk, type DiskShare, type DiskVolume } from "../api";
import { bytes, pct, usePoll } from "../lib";
import { Badge, Button, Card, CardHeader, ErrorNote, Field, Loading, Meter, Modal, WarnNote, inputClass } from "./ui";
import { confirmDialog, notify } from "./dialogs";
import { waitFor } from "./ContainerTable";
import DockerStorage from "./DockerStorage";
import { BootCard, FlashDialog, ImageLibrary } from "./Rebuild";

type Dialog =
  | { kind: "format"; vol: DiskVolume; disk: Disk }
  | { kind: "partition"; disk: Disk }
  | { kind: "mount"; vol: DiskVolume }
  | { kind: "share" }
  | { kind: "image"; disk: Disk };

const name = (path: string) => path.replace(/^\/dev\//, "");

/** Runs a disk action on the device and waits for the agent's answer. */
async function run(deviceId: string, action: Parameters<typeof api.diskAction>[1], args: Record<string, string>) {
  const r = await waitFor((await api.diskAction(deviceId, action, args)).id);
  if (r.status === "failed") throw new Error(r.error ?? "Failed");
  return r.output ?? "";
}

export default function Disks({ d }: { d: Device }) {
  const list = usePoll(() => api.disks(d.id), 15000, [d.id]);
  const [dialog, setDialog] = useState<Dialog | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const rep = list.data;

  const unmount = async (mountpoint: string) => {
    const ok = await confirmDialog({
      title: `Unmount ${mountpoint}?`,
      message: "Workers that use this folder will lose access to it until it is mounted again. If it was mounted at boot, that entry is removed too.",
      confirmLabel: "Unmount",
      danger: true,
    });
    if (!ok) return;
    setBusy(mountpoint);
    try {
      notify(await run(d.id, "unmount", { mountpoint }), "success");
    } catch (e) {
      notify(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
      list.reload();
    }
  };

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader
          title="Disks"
          subtitle={`Live from ${d.name}${rep?.board ? ` (${rep.board})` : ""}. Format, partition and unmount are admin-only, need the device path typed back, and are written to Activity.`}
          action={
            <div className="flex flex-wrap gap-2">
              <Button icon={<RefreshCw className="size-4" />} onClick={list.reload}>
                Refresh
              </Button>
              <Button icon={<Network className="size-4" />} onClick={() => setDialog({ kind: "share" })}>
                Add network share
              </Button>
            </div>
          }
        />
        <div className="space-y-3 p-5 text-sm">
          <p className="text-neutral-500">
            The system disk, anything that is mounted, and disks used by LVM, RAID or encryption are locked: Orchestrator refuses to change them, even if asked.
          </p>
          {list.error && <ErrorNote>{list.error}</ErrorNote>}
        </div>
      </Card>

      {!rep && !list.error && <Loading />}
      {rep?.disks.length === 0 && <Card><p className="p-5 text-sm text-neutral-500">No disks found on this device.</p></Card>}
      {rep?.disks.map((disk) => (
        <DiskCard key={disk.path} disk={disk} busy={busy} onUnmount={unmount} onDialog={setDialog} />
      ))}

      {rep && (
        <Card>
          <CardHeader title="Network shares" subtitle="SMB and NFS shares mounted on this device." />
          {rep.shares.length === 0 ? (
            <p className="p-5 text-sm text-neutral-500">No network shares are mounted.</p>
          ) : (
            <ul className="divide-y divide-neutral-100">
              {rep.shares.map((s) => (
                <ShareRow key={s.mountpoint} s={s} busy={busy === s.mountpoint} onUnmount={() => unmount(s.mountpoint)} />
              ))}
            </ul>
          )}
        </Card>
      )}

      <DockerStorage d={d} />

      <ImageLibrary />
      <BootCard d={d} />

      {dialog?.kind === "image" && <FlashDialog d={d} disk={dialog.disk} onClose={() => { setDialog(null); list.reload(); }} />}
      {dialog?.kind === "format" && <FormatDialog d={d} {...dialog} onClose={() => { setDialog(null); list.reload(); }} />}
      {dialog?.kind === "partition" && <PartitionDialog d={d} disk={dialog.disk} onClose={() => { setDialog(null); list.reload(); }} />}
      {dialog?.kind === "mount" && <MountDialog d={d} vol={dialog.vol} onClose={() => { setDialog(null); list.reload(); }} />}
      {dialog?.kind === "share" && <ShareDialog d={d} onClose={() => { setDialog(null); list.reload(); }} />}
    </div>
  );
}

function DiskCard({ disk, busy, onUnmount, onDialog }: { disk: Disk; busy: string | null; onUnmount: (mp: string) => void; onDialog: (d: Dialog) => void }) {
  const parts = disk.partitions ?? [];
  const rows = parts.length > 0 ? parts : [disk.volume];
  const meta = [bytes(disk.size_bytes), disk.model, disk.medium === "emmc" ? "eMMC (soldered flash)" : disk.medium === "sd" ? "microSD card" : disk.transport?.toUpperCase(), disk.kind === "loop" ? "loop device (file-backed)" : disk.removable ? "removable" : disk.rotational ? "HDD" : disk.medium ? "" : "SSD/flash"].filter(Boolean);
  return (
    <Card>
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-neutral-200 px-5 py-3.5">
        <div className="flex min-w-0 items-center gap-3">
          {disk.removable ? <Usb className="size-5 shrink-0 text-neutral-500" /> : <HardDrive className="size-5 shrink-0 text-neutral-500" />}
          <div className="min-w-0">
            <h3 className="flex flex-wrap items-center gap-2 text-sm font-semibold">
              <span className="font-mono">{disk.path}</span>
              {disk.system ? <Badge tone="bad">System disk</Badge> : disk.protected ? <Badge tone="warn">In use</Badge> : <Badge tone="ok">Free</Badge>}
            </h3>
            <p className="mt-0.5 truncate text-xs text-neutral-500">{meta.join(" · ")}</p>
          </div>
        </div>
        <div className="flex gap-2">
          <Button disabled={!!disk.protected} title={disk.protected || "Erase the disk and write a gold image from the server onto it"} onClick={() => onDialog({ kind: "image", disk })}>
            Write image…
          </Button>
          <Button disabled={!!disk.protected} title={disk.protected || "Erase the disk and create a new partition table"} onClick={() => onDialog({ kind: "partition", disk })}>
            Partition…
          </Button>
        </div>
      </div>
      {disk.protected && <p className="border-b border-neutral-100 bg-neutral-50 px-5 py-2 text-xs text-neutral-600">🔒 {disk.protected}</p>}
      <div className="overflow-x-auto">
        <table className="w-full min-w-[640px] text-sm">
          <thead>
            <tr className="text-left text-xs text-neutral-500">
              <th className="px-5 py-2 font-medium">Volume</th>
              <th className="px-3 py-2 font-medium">Filesystem</th>
              <th className="px-3 py-2 font-medium">Size</th>
              <th className="px-3 py-2 font-medium">Used</th>
              <th className="px-3 py-2 font-medium">Mounted at</th>
              <th className="px-5 py-2 text-right font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((v) => (
              <VolumeRow key={v.path} v={v} disk={disk} whole={parts.length === 0} busy={busy === v.mountpoint} onUnmount={onUnmount} onDialog={onDialog} />
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

function VolumeRow({ v, disk, whole, busy, onUnmount, onDialog }: { v: DiskVolume; disk: Disk; whole: boolean; busy: boolean; onUnmount: (mp: string) => void; onDialog: (d: Dialog) => void }) {
  const locked = !!v.protected || disk.system || (whole && !!disk.protected && !v.mountpoint);
  return (
    <tr className="border-t border-neutral-100 align-middle">
      <td className="px-5 py-3">
        <div className="font-mono text-xs">{v.path}</div>
        {v.label && <div className="text-xs text-neutral-500">{v.label}</div>}
      </td>
      <td className="px-3 py-3">{v.fstype ? <Badge>{v.fstype}</Badge> : <span className="text-neutral-400">none</span>}</td>
      <td className="px-3 py-3 tabular-nums">{bytes(v.size_bytes)}</td>
      <td className="w-40 px-3 py-3">{v.mountpoint && v.used_bytes ? <Meter value={pct(v.used_bytes, v.size_bytes)} label={bytes(v.used_bytes)} /> : <span className="text-neutral-400">—</span>}</td>
      <td className="px-3 py-3">
        {v.mountpoint ? (
          <span className="flex flex-wrap items-center gap-1.5">
            <span className="font-mono text-xs">{v.mountpoint}</span>
            {v.persistent && <Badge tone="info">at boot</Badge>}
          </span>
        ) : (
          <span className="text-neutral-400">not mounted</span>
        )}
      </td>
      <td className="px-5 py-3">
        <div className="flex justify-end gap-2">
          {v.mountpoint ? (
            <Button size="sm" variant="danger" loading={busy} disabled={locked} title={locked ? v.protected || disk.protected : "Unmount"} onClick={() => onUnmount(v.mountpoint!)}>
              Unmount
            </Button>
          ) : (
            <>
              <Button size="sm" disabled={locked || !v.fstype} title={!v.fstype ? "Format it first" : locked ? disk.protected : "Mount"} onClick={() => onDialog({ kind: "mount", vol: v })}>
                Mount…
              </Button>
              <Button size="sm" variant="danger" disabled={locked} title={locked ? disk.protected || v.protected : "Erase and create a new filesystem"} onClick={() => onDialog({ kind: "format", vol: v, disk })}>
                Format…
              </Button>
            </>
          )}
        </div>
      </td>
    </tr>
  );
}

function ShareRow({ s, busy, onUnmount }: { s: DiskShare; busy: boolean; onUnmount: () => void }) {
  return (
    <li className="flex flex-wrap items-center justify-between gap-3 px-5 py-3 text-sm">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <span className="font-mono text-xs">{s.source}</span>
          <Badge>{s.fstype === "cifs" ? "SMB" : "NFS"}</Badge>
          {s.persistent && <Badge tone="info">at boot</Badge>}
        </div>
        <div className="mt-0.5 text-xs text-neutral-500">
          at <span className="font-mono">{s.mountpoint}</span>
          {s.size_bytes ? ` · ${bytes(s.used_bytes ?? 0)} of ${bytes(s.size_bytes)} used` : ""}
        </div>
      </div>
      <Button size="sm" variant="danger" loading={busy} onClick={onUnmount}>
        Unmount
      </Button>
    </li>
  );
}

/** Shared shell for a dialog that runs one action and closes when it succeeds. */
function useAction(d: Device, onClose: () => void) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const go = async (action: Parameters<typeof api.diskAction>[1], args: Record<string, string>) => {
    setBusy(true);
    setError(null);
    try {
      setDone(await run(d.id, action, args));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return { busy, error, done, go, close: onClose };
}

function Result({ done, onClose }: { done: string; onClose: () => void }) {
  return (
    <>
      <div className="rounded-lg border border-green-200 bg-green-50 px-3 py-2 text-sm text-ok">{done}</div>
      <div className="mt-5 flex justify-end">
        <Button variant="primary" onClick={onClose}>
          Done
        </Button>
      </div>
    </>
  );
}

function Confirm({ path, value, onChange }: { path: string; value: string; onChange: (v: string) => void }) {
  return (
    <Field label={`Type ${path} to confirm`} hint="This cannot be undone.">
      <input className={inputClass + " font-mono"} value={value} onChange={(e) => onChange(e.target.value)} placeholder={path} autoComplete="off" spellCheck={false} />
    </Field>
  );
}

function FormatDialog({ d, vol, disk, onClose }: { d: Device; vol: DiskVolume; disk: Disk; onClose: () => void }) {
  const a = useAction(d, onClose);
  const [fstype, setFstype] = useState("ext4");
  const [label, setLabel] = useState("");
  const [confirm, setConfirm] = useState("");
  return (
    <Modal title={`Format ${vol.path}`} onClose={onClose}>
      {a.done ? (
        <Result done={a.done} onClose={onClose} />
      ) : (
        <div className="space-y-4">
          <WarnNote>
            Everything on <b className="font-mono">{vol.path}</b> ({bytes(vol.size_bytes)}, on {d.name}
            {disk.model ? `, ${disk.model}` : ""}) will be erased.
            {vol.fstype && (
              <>
                {" "}
                It currently holds a <b>{vol.fstype}</b> filesystem{vol.label ? ` labelled "${vol.label}"` : ""}.
              </>
            )}
          </WarnNote>
          <Field label="Filesystem">
            <select className={inputClass} value={fstype} onChange={(e) => setFstype(e.target.value)}>
              <option value="ext4">ext4 (Linux, recommended)</option>
              <option value="xfs">xfs (Linux, large files)</option>
              <option value="exfat">exFAT (Windows, macOS and Linux)</option>
              <option value="vfat">FAT32 (cameras, boot partitions)</option>
            </select>
          </Field>
          <Field label="Label (optional)" hint="Up to 12 letters, digits, - or _.">
            <input className={inputClass} value={label} maxLength={12} onChange={(e) => setLabel(e.target.value)} />
          </Field>
          <Confirm path={vol.path} value={confirm} onChange={setConfirm} />
          {a.error && <ErrorNote>{a.error}</ErrorNote>}
          <div className="flex justify-end gap-2">
            <Button onClick={onClose}>Cancel</Button>
            <Button variant="danger" loading={a.busy} disabled={confirm !== vol.path} onClick={() => a.go("format", { device: vol.path, fstype, label, confirm })}>
              Erase and format
            </Button>
          </div>
        </div>
      )}
    </Modal>
  );
}

function PartitionDialog({ d, disk, onClose }: { d: Device; disk: Disk; onClose: () => void }) {
  const a = useAction(d, onClose);
  const [split, setSplit] = useState(false);
  const [firstGiB, setFirstGiB] = useState("50");
  const [confirm, setConfirm] = useState("");
  const diskGiB = disk.size_bytes / 1024 ** 3;
  const first = Number(firstGiB);
  const firstOk = Number.isFinite(first) && first >= 1 && first < diskGiB;
  // The agent takes sizes in MiB; "rest" means all remaining space.
  const layout = split ? `${Math.round(first * 1024)},rest` : "rest";
  return (
    <Modal title={`Partition ${disk.path}`} onClose={onClose}>
      {a.done ? (
        <Result done={a.done} onClose={onClose} />
      ) : (
        <div className="space-y-4">
          <WarnNote>
            This replaces the partition table of <b className="font-mono">{disk.path}</b> ({bytes(disk.size_bytes)}
            {disk.model ? `, ${disk.model}` : ""}) with a new GPT table. All existing partitions and their data are lost. New partitions have no filesystem; format them afterwards.
          </WarnNote>
          <div className="space-y-2 text-sm">
            <div className="font-medium">How should the disk be divided?</div>
            <label className="flex items-start gap-2">
              <input type="radio" name="layout" className="mt-1" checked={!split} onChange={() => setSplit(false)} />
              <span>One partition using the whole disk</span>
            </label>
            <label className="flex items-start gap-2">
              <input type="radio" name="layout" className="mt-1" checked={split} onChange={() => setSplit(true)} />
              <span>Split into two partitions</span>
            </label>
          </div>
          {split && (
            <Field label="Size of the first partition (GiB)" hint={`The second partition uses all the remaining space. The disk is ${diskGiB.toFixed(1)} GiB.`}>
              <input className={inputClass} type="number" inputMode="decimal" min={1} step="any" value={firstGiB} onChange={(e) => setFirstGiB(e.target.value)} />
            </Field>
          )}
          <p className="text-sm text-neutral-500">
            {split
              ? firstOk
                ? `Result: partition 1 is ${firstGiB} GiB and partition 2 is the remaining ${Math.max(diskGiB - first, 0).toFixed(1)} GiB.`
                : `Enter a size of at least 1 GiB and smaller than the disk (${diskGiB.toFixed(1)} GiB).`
              : "Result: a single partition covering the whole disk."}
          </p>
          <Confirm path={disk.path} value={confirm} onChange={setConfirm} />
          {a.error && <ErrorNote>{a.error}</ErrorNote>}
          <div className="flex justify-end gap-2">
            <Button onClick={onClose}>Cancel</Button>
            <Button variant="danger" loading={a.busy} disabled={confirm !== disk.path || (split && !firstOk)} onClick={() => a.go("partition", { device: disk.path, layout, confirm })}>
              Erase and partition
            </Button>
          </div>
        </div>
      )}
    </Modal>
  );
}

function MountDialog({ d, vol, onClose }: { d: Device; vol: DiskVolume; onClose: () => void }) {
  const a = useAction(d, onClose);
  const [mountpoint, setMountpoint] = useState(`/mnt/${(vol.label || name(vol.path)).replace(/[^A-Za-z0-9._-]/g, "-")}`);
  const [persist, setPersist] = useState(true);
  return (
    <Modal title={`Mount ${vol.path}`} onClose={onClose}>
      {a.done ? (
        <Result done={a.done} onClose={onClose} />
      ) : (
        <div className="space-y-4">
          <Field label="Mount at" hint="An empty folder is created if it doesn't exist. Use it as a host folder for workers.">
            <input className={inputClass + " font-mono"} value={mountpoint} onChange={(e) => setMountpoint(e.target.value)} />
          </Field>
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" className="mt-0.5" checked={persist} onChange={(e) => setPersist(e.target.checked)} />
            <span>
              Mount at boot
              <span className="block text-xs text-neutral-500">Adds a line to /etc/fstab by UUID with <code>nofail</code>, so a missing drive never blocks the device from starting.</span>
            </span>
          </label>
          {a.error && <ErrorNote>{a.error}</ErrorNote>}
          <div className="flex justify-end gap-2">
            <Button onClick={onClose}>Cancel</Button>
            <Button variant="primary" loading={a.busy} disabled={!mountpoint} onClick={() => a.go("mount", { device: vol.path, mountpoint, persist: persist ? "1" : "" })}>
              Mount
            </Button>
          </div>
        </div>
      )}
    </Modal>
  );
}

function ShareDialog({ d, onClose }: { d: Device; onClose: () => void }) {
  const a = useAction(d, onClose);
  const [kind, setKind] = useState<"smb" | "nfs">("smb");
  const [source, setSource] = useState("");
  const [mountpoint, setMountpoint] = useState("/mnt/nas");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [persist, setPersist] = useState(true);
  return (
    <Modal title="Add network share" onClose={onClose}>
      {a.done ? (
        <Result done={a.done} onClose={onClose} />
      ) : (
        <div className="space-y-4">
          <Field label="Type">
            <select className={inputClass} value={kind} onChange={(e) => setKind(e.target.value as "smb" | "nfs")}>
              <option value="smb">SMB / CIFS (Windows, NAS)</option>
              <option value="nfs">NFS</option>
            </select>
          </Field>
          <Field label="Share" hint={kind === "smb" ? "For example //192.168.0.10/media" : "For example 192.168.0.10:/export/media"}>
            <input className={inputClass + " font-mono"} value={source} onChange={(e) => setSource(e.target.value)} />
          </Field>
          <Field label="Mount at">
            <input className={inputClass + " font-mono"} value={mountpoint} onChange={(e) => setMountpoint(e.target.value)} />
          </Field>
          {kind === "smb" && (
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <Field label="Username" hint="Leave empty for a guest share.">
                <input className={inputClass} value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
              </Field>
              <Field label="Password" hint="Stored on the device in a root-only file, never in the dashboard.">
                <input className={inputClass} type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
              </Field>
            </div>
          )}
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={persist} onChange={(e) => setPersist(e.target.checked)} />
            Mount at boot (nofail, after the network is up)
          </label>
          {a.error && <ErrorNote>{a.error}</ErrorNote>}
          <div className="flex justify-end gap-2">
            <Button onClick={onClose}>Cancel</Button>
            <Button variant="primary" loading={a.busy} disabled={!source || !mountpoint} onClick={() => a.go("share", { kind, source, mountpoint, username, password, persist: persist ? "1" : "" })}>
              Mount share
            </Button>
          </div>
        </div>
      )}
    </Modal>
  );
}
