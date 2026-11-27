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
    // A second, separate prompt that restates exactly what is about to be destroyed.
    const ok = await confirmDialog({
      title: `Erase ${disk.path} on ${d.name}?`,
      message: `This permanently erases ${bytes(disk.size_bytes)}${disk.model ? ` (${disk.model})` : ""} and writes "${chosen.name}" to it.${enrol ? " The new system will register itself on first boot." : ""} There is no undo.`,
      confirmLabel: "Yes, erase it",
      danger: true,
    });
    if (!ok) return;
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
              <span className="block text-xs text-neutral-500">Adds a one-time job to the new system that installs the agent with a one-use enrolment token (48 hours, root-only file, deleted afterwards; revocable in Settings). Needs an ext4 root partition.</span>
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

const sources: Record<string, string> = { "1": "SD card", "2": "Network", "4": "USB drive", "6": "NVMe", "7": "HTTP boot", e: "eMMC" };

/** "0xf41" -> ["SD card", "USB drive"]: digits are tried right to left; f means start over. */
export function describeOrder(hex?: string): string[] {
  if (!hex) return [];
  return hex.replace(/^0x/i, "").split("").reverse().filter((c) => c !== "f" && c !== "0").map((c) => sources[c.toLowerCase()] ?? `source ${c}`);
}

const keyOf = (name: string) => Object.entries(sources).find(([, v]) => v === name)?.[0] ?? "1";
/** Ordered source names -> BOOT_ORDER, always ending in "restart" (f) so a failed list retries. */
const toHex = (names: string[]) => "0xf" + [...names].reverse().map(keyOf).join("");

const quick: [string, string[]][] = [
  ["SD card, then USB, then network", ["SD card", "USB drive", "Network"]],
  ["NVMe, then SD card, then USB", ["NVMe", "SD card", "USB drive"]],
  ["Network first, then SD card (for rebuilds)", ["Network", "SD card"]],
];

/** Raspberry Pi boot order, staged with rpi-eeprom-config and applied on the next reboot. */
export function BootCard({ d }: { d: Device }) {
  const [info, setInfo] = useState<Awaited<ReturnType<typeof api.bootInfo>> | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [list, setList] = useState<string[]>(quick[0][1]);
  const order = toHex(list);
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
              {info.model} · current setting <span className="font-mono">{info.order ?? "default"}</span>
            </p>
            <p className="text-neutral-500">Tried in this order: {describeOrder(info.order).join(" → ") || "default"} (then starts over).</p>
            <Field label="Start from a common setup">
              <select className={inputClass} value="" onChange={(e) => { if (e.target.value) { setList(quick[Number(e.target.value)][1]); setConfirm(""); } }}>
                <option value="">Choose…</option>
                {quick.map(([label], i) => (
                  <option key={label} value={i}>{label}</option>
                ))}
              </select>
            </Field>
            <Field label="New boot order" hint="The Pi tries each in turn and boots the first that works. A wrong order can leave it unable to boot; keep a way to recover it (rpiboot or a spare SD card).">
              <ol className="space-y-1.5">
                {list.map((n, i) => (
                  <li key={n} className="flex items-center gap-2">
                    <span className="w-5 text-neutral-500">{i + 1}.</span>
                    <span className="flex-1">{n}</span>
                    <Button size="sm" disabled={i === 0} onClick={() => { const l = [...list]; [l[i - 1], l[i]] = [l[i], l[i - 1]]; setList(l); setConfirm(""); }}>↑</Button>
                    <Button size="sm" disabled={i === list.length - 1} onClick={() => { const l = [...list]; [l[i + 1], l[i]] = [l[i], l[i + 1]]; setList(l); setConfirm(""); }}>↓</Button>
                    <Button size="sm" disabled={list.length === 1} onClick={() => { setList(list.filter((x) => x !== n)); setConfirm(""); }}>Remove</Button>
                  </li>
                ))}
              </ol>
              {Object.values(sources).filter((n) => !list.includes(n)).length > 0 && (
                <select className={inputClass + " mt-2"} value="" onChange={(e) => { if (e.target.value) { setList([...list, e.target.value]); setConfirm(""); } }}>
                  <option value="">Add a boot source…</option>
                  {Object.values(sources).filter((n) => !list.includes(n)).map((n) => (
                    <option key={n} value={n}>{n}</option>
                  ))}
                </select>
              )}
            </Field>
            <Field label={`Type ${d.name} to confirm`} hint={`Sets the boot order to ${list.join(" → ")}.`}>
              <input className={inputClass + " font-mono"} value={confirm} onChange={(e) => setConfirm(e.target.value)} placeholder={d.name} autoComplete="off" spellCheck={false} />
            </Field>
            {done && <div className="rounded-lg border border-green-200 bg-green-50 px-3 py-2 text-ok">{done}</div>}
            <Button variant="danger" loading={busy} disabled={confirm !== d.name} onClick={apply}>
              Stage boot order
            </Button>
          </>
        )}
      </div>
    </Card>
  );
}

function timeLeft(iso: string): string {
  const ms = new Date(iso).getTime() - Date.now();
  if (ms <= 0) return "expired";
  const h = Math.round(ms / 3600000);
  if (h < 1) return `in ${Math.max(1, Math.round(ms / 60000))} min`;
  if (h < 48) return `in ${h} h`;
  return `in ${Math.round(h / 24)} days`;
}

const lifetimes: [string, number][] = [["1 hour", 1], ["1 day", 24], ["1 week", 168], ["30 days", 720]];

/** Scoped enrolment tokens: they expire, have a use limit and can be revoked. For cards, disks and bundles that carry a credential. */
export function EnrolTokens() {
  const list = usePoll(() => api.enrolTokens(), 30000, []);
  const [label, setLabel] = useState("");
  const [hours, setHours] = useState(24);
  const [uses, setUses] = useState(1);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [showEnded, setShowEnded] = useState(false);

  const create = async () => {
    setBusy(true);
    setError(null);
    try {
      const t = await api.createEnrolToken({ label, ttl_hours: hours, max_uses: uses });
      setCreated(t.token ?? null);
      setCopied(false);
      setLabel("");
      list.reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const revoke = async (id: string, l: string) => {
    if (!(await confirmDialog({ title: `Revoke "${l}"?`, message: "Devices that have not registered with it yet can no longer do so.", confirmLabel: "Revoke", danger: true }))) return;
    await api.revokeEnrolToken(id);
    list.reload();
  };
  const copy = async () => {
    if (!created) return;
    try {
      await navigator.clipboard.writeText(created);
      setCopied(true);
    } catch {
      /* the token stays selectable below */
    }
  };

  const active = list.data?.filter((t) => t.status === "active") ?? [];
  const ended = list.data?.filter((t) => t.status !== "active") ?? [];
  const row = (t: NonNullable<typeof list.data>[number]) => (
    <li key={t.id} className="flex flex-wrap items-center justify-between gap-3 py-2.5">
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-2 font-medium">
          {t.label}
          {t.status !== "active" && <Badge tone={t.status === "revoked" ? "bad" : "neutral"}>{t.status}</Badge>}
        </div>
        <p className="text-xs text-neutral-500">
          {t.uses} of {t.max_uses} {t.max_uses === 1 ? "device" : "devices"} enrolled · {t.status === "active" ? `expires ${timeLeft(t.expires_at)}` : `ended ${new Date(t.expires_at).toLocaleDateString()}`}
          {t.devices?.length ? ` · ${t.devices.join(", ")}` : ""}
        </p>
      </div>
      {t.status === "active" && (
        <Button size="sm" onClick={() => revoke(t.id, t.label)}>
          Revoke
        </Button>
      )}
    </li>
  );

  return (
    <div className="space-y-5 p-5 text-sm">
      <WarnNote>
        An enrolment token lets a device join this server without the join token. Anyone holding a card, drive or disk that contains one can enrol a device until the token expires, runs out of uses or is revoked. Keep that media somewhere safe, allow only the time and number of devices you need, and revoke the token straight away if the media is lost.
      </WarnNote>
      {list.error && <ErrorNote>{list.error}</ErrorNote>}

      <section>
        <h3 className="mb-1 font-semibold">Active tokens</h3>
        {active.length === 0 ? (
          <p className="text-neutral-500">None. Tokens are temporary: each one stops working when it expires or runs out of uses. Rebuilds create their own, valid for 48 hours and one device.</p>
        ) : (
          <ul className="divide-y divide-neutral-100">{active.map(row)}</ul>
        )}
        {ended.length > 0 && (
          <div className="mt-2">
            <button className="text-xs text-neutral-500 underline" onClick={() => setShowEnded(!showEnded)}>
              {showEnded ? "Hide" : "Show"} {ended.length} ended
            </button>
            {showEnded && <ul className="divide-y divide-neutral-100">{ended.map(row)}</ul>}
          </div>
        )}
      </section>

      <section className="rounded-lg border border-neutral-200 p-4">
        <h3 className="mb-3 font-semibold">Create a token</h3>
        <div className="space-y-4">
          <Field label="Label" hint="Where this token will be kept, so you know what to revoke if it is lost.">
            <input className={inputClass} value={label} onChange={(e) => setLabel(e.target.value)} placeholder="e.g. Master SD card, workshop shelf" />
          </Field>
          <Field label="Valid for" as="div">
            <div className="flex flex-wrap gap-2">
              {lifetimes.map(([name, h]) => (
                <Button key={h} size="sm" variant={hours === h ? "primary" : "secondary"} onClick={() => setHours(h)}>
                  {name}
                </Button>
              ))}
            </div>
          </Field>
          <Field label="Number of devices" hint="How many devices can join with it (1 to 100). Use 1 unless one card must set up several.">
            <input className={inputClass + " max-w-28"} type="number" min={1} max={100} value={uses} onChange={(e) => setUses(Math.min(100, Math.max(1, Number(e.target.value) || 1)))} />
          </Field>
          {error && <ErrorNote>{error}</ErrorNote>}
          <Button variant="primary" loading={busy} disabled={!label.trim()} onClick={create}>
            Create token
          </Button>
        </div>
      </section>

      {created && (
        <div className="rounded-lg border border-amber-200 bg-amber-50 p-3">
          <p className="mb-2 font-medium">Copy this token now. It will not be shown again.</p>
          <div className="flex items-center gap-2">
            <code className="flex-1 break-all rounded bg-white/70 px-2 py-1.5 font-mono text-xs">{created}</code>
            <Button size="sm" onClick={copy}>{copied ? "Copied" : "Copy"}</Button>
          </div>
        </div>
      )}
    </div>
  );
}
