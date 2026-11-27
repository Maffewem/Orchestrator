import { useRef, useState } from "react";
import { Link } from "react-router";
import { Trash2, Upload } from "lucide-react";
import { api, type Device, type Disk } from "../api";
import { bytes, usePoll } from "../lib";
import { Badge, Button, Card, CardHeader, ErrorNote, Field, Modal, WarnNote, inputClass } from "./ui";
import { confirmDialog, notify } from "./dialogs";
import { waitFor } from "./ContainerTable";

/** The server's gold images: raw disk images (optionally .gz, .xz or .zst) that devices can write onto a spare disk. */
export function ImageLibrary() {
  const list = usePoll(() => api.images(), 30000, []);
  const file = useRef<HTMLInputElement>(null);
  const [name, setName] = useState("");
  const [arch, setArch] = useState("");
  const [notes, setNotes] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const upload = async () => {
    const f = file.current?.files?.[0];
    if (!f) return;
    setBusy(true);
    setError(null);
    try {
      await api.uploadImage(f, { name: name.trim() || f.name, arch, notes });
      setName("");
      setNotes("");
      if (file.current) file.current.value = "";
      list.reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const remove = async (id: string, label: string) => {
    if (!(await confirmDialog({ title: `Delete image "${label}"?`, message: "Devices can no longer write it. Disks already written are not affected.", confirmLabel: "Delete", danger: true }))) return;
    try {
      await api.deleteImage(id);
      list.reload();
    } catch (e) {
      notify(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <Card>
      <CardHeader title="Gold images" subtitle="Standard builds kept on this server. Write one onto a free disk with “Write image…”, then move the disk into the machine to rebuild." />
      <div className="space-y-4 p-5 text-sm">
        {list.error && <ErrorNote>{list.error}</ErrorNote>}
        {list.data?.length === 0 && <p className="text-neutral-500">No images yet.</p>}
        {list.data && list.data.length > 0 && (
          <ul className="divide-y divide-neutral-100">
            {list.data.map((im) => (
              <li key={im.id} className="flex flex-wrap items-center justify-between gap-2 py-2">
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-2 font-medium">
                    {im.name} {im.arch && <Badge>{im.arch}</Badge>} <Badge>{im.format}</Badge>
                  </div>
                  <p className="truncate text-xs text-neutral-500">
                    {bytes(im.size)} · sha256 {im.sha256.slice(0, 12)}… · added by {im.created_by ?? "an admin"}
                    {im.notes ? ` · ${im.notes}` : ""}
                  </p>
                </div>
                <Button size="sm" icon={<Trash2 className="size-4" />} onClick={() => remove(im.id, im.name)}>
                  Delete
                </Button>
              </li>
            ))}
          </ul>
        )}
        <div className="grid gap-3 border-t border-neutral-100 pt-4 sm:grid-cols-2">
          <Field label="Image file" hint="A raw .img, or .img.gz / .xz / .zst. Uploaded through the browser, so large images are better placed with a smaller test image first.">
            <input ref={file} type="file" className={inputClass} />
          </Field>
          <Field label="Name">
            <input className={inputClass} value={name} onChange={(e) => setName(e.target.value)} placeholder="Raspberry Pi OS Lite, agent preinstalled" />
          </Field>
          <Field label="Built for">
            <select className={inputClass} value={arch} onChange={(e) => setArch(e.target.value)}>
              <option value="">Not specified</option>
              <option value="arm64">arm64 (Pi 4, CM4, Pi 5, CM5)</option>
              <option value="armv7">armv7 (32-bit Pi)</option>
              <option value="amd64">amd64</option>
            </select>
          </Field>
          <Field label="Notes (optional)">
            <input className={inputClass} value={notes} maxLength={500} onChange={(e) => setNotes(e.target.value)} />
          </Field>
        </div>
        {error && <ErrorNote>{error}</ErrorNote>}
        <Button icon={<Upload className="size-4" />} loading={busy} onClick={upload}>
          Upload image
        </Button>
      </div>
    </Card>
  );
}

/** Writes a gold image onto a whole free disk of this device, optionally with first-boot registration. */
export function FlashDialog({ d, disk, onClose }: { d: Device; disk: Disk; onClose: () => void }) {
  const images = usePoll(() => api.images(), 60000, []);
  const [image, setImage] = useState("");
  const [enrol, setEnrol] = useState(true);
  const [name, setName] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [queued, setQueued] = useState(false);
  const chosen = images.data?.find((i) => i.id === (image || images.data?.[0]?.id));

  const go = async () => {
    if (!chosen) return;
    setBusy(true);
    setError(null);
    try {
      const c = await api.flashImage(d.id, { image: chosen.id, device: disk.path, confirm, enrol, name: name.trim() || undefined });
      setQueued(true);
      // Short quick failures (a guard refusing the disk) are shown here; a long write carries on in Activity.
      waitFor(c.id).then((r) => r.status === "failed" && setError(r.error ?? "Failed")).catch(() => undefined);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title={`Write an image to ${disk.path}`} onClose={onClose}>
      {queued && !error ? (
        <div className="space-y-4">
          <div className="rounded-lg border border-green-200 bg-green-50 px-3 py-2 text-sm text-ok">
            Queued. {d.name} is downloading and writing the image. Large images take a while: follow the result in{" "}
            <Link className="underline" to="/activity">Activity</Link>.
          </div>
          <div className="flex justify-end"><Button variant="primary" onClick={onClose}>Done</Button></div>
        </div>
      ) : (
        <div className="space-y-4">
          <WarnNote>
            Everything on <b className="font-mono">{disk.path}</b> ({bytes(disk.size_bytes)}
            {disk.model ? `, ${disk.model}` : ""}) on {d.name} will be erased. The disk is checked again on the device first: the system disk and anything in use are refused.
          </WarnNote>
          {images.data?.length === 0 ? (
            <p className="text-sm text-neutral-500">Upload a gold image first.</p>
          ) : (
            <Field label="Image">
              <select className={inputClass} value={chosen?.id ?? ""} onChange={(e) => setImage(e.target.value)}>
                {images.data?.map((i) => (
                  <option key={i.id} value={i.id}>{i.name} ({bytes(i.size)}, {i.format})</option>
                ))}
              </select>
            </Field>
          )}
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" className="mt-1" checked={enrol} onChange={(e) => setEnrol(e.target.checked)} />
            <span>
              Register the rebuilt machine automatically on first boot
              <span className="block text-xs text-neutral-500">Adds a one-time job to the new system that installs the agent with this server’s join token (root-only file, deleted afterwards). Needs an ext4 root partition.</span>
            </span>
          </label>
          {enrol && (
            <Field label="Device name (optional)" hint="Letters, digits, . _ -. Re-using the name of an offline device keeps its identity.">
              <input className={inputClass} value={name} onChange={(e) => setName(e.target.value)} />
            </Field>
          )}
          <Field label={`Type ${disk.path} to confirm`} hint="This cannot be undone.">
            <input className={inputClass + " font-mono"} value={confirm} onChange={(e) => setConfirm(e.target.value)} placeholder={disk.path} autoComplete="off" spellCheck={false} />
          </Field>
          {error && <ErrorNote>{error}</ErrorNote>}
          <div className="flex justify-end gap-2">
            <Button onClick={onClose}>Cancel</Button>
            <Button variant="danger" loading={busy} disabled={!chosen || confirm !== disk.path} onClick={go}>
              Erase and write image
            </Button>
          </div>
        </div>
      )}
    </Modal>
  );
}

const presets: [string, string][] = [
  ["SD card, then USB, then network (Pi default)", "0xf241"],
  ["NVMe, then SD card, then USB", "0xf416"],
  ["Network first, then SD card (for rebuilds)", "0xf12"],
  ["USB first, then SD card", "0xf14"],
];

/** Raspberry Pi boot order, staged with rpi-eeprom-config and applied on the next reboot. */
export function BootCard({ d }: { d: Device }) {
  const [info, setInfo] = useState<Awaited<ReturnType<typeof api.bootInfo>> | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [order, setOrder] = useState(presets[0][1]);
  const [confirm, setConfirm] = useState("");
  const [done, setDone] = useState<string | null>(null);

  const load = async () => {
    setBusy(true);
    setError(null);
    try {
      setInfo(await api.bootInfo(d.id));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const apply = async () => {
    setBusy(true);
    setError(null);
    try {
      const r = await waitFor((await api.setBootOrder(d.id, order, confirm)).id);
      if (r.status === "failed") throw new Error(r.error ?? "Failed");
      setDone(r.output ?? "Done");
      setConfirm("");
      setInfo(await api.bootInfo(d.id));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader title="Boot order" subtitle="Raspberry Pi 4, CM4, Pi 5 and CM5 only. Changes the bootloader EEPROM and takes effect after the next reboot." action={<Button loading={busy && !info} onClick={load}>{info ? "Refresh" : "Read boot order"}</Button>} />
      <div className="space-y-3 p-5 text-sm">
        {error && <ErrorNote>{error}</ErrorNote>}
        {info && !info.supported && <p className="text-neutral-500">{info.reason}</p>}
        {info?.supported && (
          <>
            <p>
              {info.model} · current <span className="font-mono">BOOT_ORDER={info.order ?? "default"}</span>
            </p>
            <Field label="New boot order" hint="Each digit is a source tried right to left: 1 SD, 2 network, 4 USB, 6 NVMe, f restart. A wrong order can leave the Pi unable to boot; keep a way to recover it (rpiboot or a spare SD card).">
              <select className={inputClass} value={order} onChange={(e) => { setOrder(e.target.value); setConfirm(""); }}>
                {presets.map(([label, v]) => (
                  <option key={v} value={v}>{label} ({v})</option>
                ))}
              </select>
            </Field>
            <Field label={`Type ${order} to confirm`}>
              <input className={inputClass + " font-mono"} value={confirm} onChange={(e) => setConfirm(e.target.value)} placeholder={order} autoComplete="off" spellCheck={false} />
            </Field>
            {done && <div className="rounded-lg border border-green-200 bg-green-50 px-3 py-2 text-ok">{done}</div>}
            <Button variant="danger" loading={busy} disabled={confirm.toLowerCase() !== order} onClick={apply}>
              Stage boot order
            </Button>
          </>
        )}
      </div>
    </Card>
  );
}
