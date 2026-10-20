import { useState } from "react";
import { Archive, Cloud, Github, Globe, KeyRound, Lock } from "lucide-react";
import { api } from "../api";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, CardHeader, CodeBlock, ErrorNote, Field, Loading, PageHeader, inputClass } from "../components/ui";
import { usePoll } from "../lib";
import { confirmDialog } from "../components/dialogs";

export default function SettingsPage() {
  useCrumbs([{ label: "Settings" }]);
  const settings = usePoll(api.settings, 0);
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [login, setLogin] = useState<string | null>(null);
  const [base, setBase] = useState<string | null>(null);
  const [baseMsg, setBaseMsg] = useState<string | null>(null);
  const [cfToken, setCfToken] = useState("");
  const [cfMsg, setCfMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [sslMsg, setSslMsg] = useState<string | null>(null);
  const [bkPw, setBkPw] = useState("");
  const [bkMsg, setBkMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const s = settings.data;
  if (!s) return <Loading />;
  const webhook = `${window.location.origin}${s.github.webhook_path}`;

  const save = async (value: string) => {
    setBusy(true);
    setError(null);
    try {
      const r = await api.setGitHubToken(value);
      setLogin(r.login ?? null);
      setToken("");
      settings.reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const cf = async (b: { token?: string; proxied?: boolean }, done: (r: Awaited<ReturnType<typeof api.setCloudflare>>) => string) => {
    setCfMsg(null);
    try {
      const r = await api.setCloudflare(b);
      setCfToken("");
      setCfMsg({ ok: true, text: done(r) });
      settings.reload();
    } catch (e) {
      setCfMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    }
  };

  const setSSL = async (mode: "http" | "dns") => {
    setSslMsg(null);
    try {
      await api.setSSLMode(mode);
      settings.reload();
    } catch (e) {
      setSslMsg(e instanceof Error ? e.message : String(e));
    }
  };

  const saveBackupPassword = async (value: string) => {
    setBkMsg(null);
    try {
      await api.setBackupPassword(value);
      setBkPw("");
      setBkMsg({ ok: true, text: value ? "Saved. New backups are encrypted with this password." : "Removed. New backups are not encrypted." });
      settings.reload();
    } catch (e) {
      setBkMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    }
  };

  const saveBase = async () => {
    setBaseMsg(null);
    try {
      const r = await api.setBaseDomain(base ?? s.base_domain);
      setBase(r.base_domain);
      setBaseMsg(r.base_domain ? "Saved. Workers now get name." + r.base_domain : "Saved. Built-in worker URLs are off.");
      settings.reload();
    } catch (e) {
      setBaseMsg(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div className="max-w-3xl">
      <PageHeader title="Settings" />
      <div className="space-y-6">
        <Card>
          <CardHeader
            title={
              <span className="flex items-center gap-2">
                <Github className="size-4" /> GitHub
              </span>
            }
            subtitle="Deploy from repositories and redeploy on every push."
            action={s.github.connected ? <Badge tone="ok">Connected{login ? ` as ${login}` : ""}</Badge> : <Badge>Not connected</Badge>}
          />
          <div className="space-y-6 p-5">
            <div>
              <p className="mb-1 text-sm font-medium">1. Access token</p>
              <p className="mb-3 text-sm text-neutral-500">
                Create a fine-grained personal access token with read-only <b>Contents</b> and <b>Metadata</b> access to the repositories you want to
                deploy. It is used to list repositories, read commits and clone private repos on your devices. Public repos work without one.
              </p>
              <div className="flex gap-2">
                <input
                  className={inputClass}
                  type="password"
                  placeholder={s.github.connected ? "Paste a new token to replace the current one" : "github_pat_…"}
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                />
                <Button variant="primary" loading={busy} disabled={!token.trim()} onClick={() => save(token)}>
                  Connect
                </Button>
                {s.github.connected && (
                  <Button variant="danger" onClick={() => save("")}>
                    Disconnect
                  </Button>
                )}
              </div>
              {error && (
                <div className="mt-3">
                  <ErrorNote>{error}</ErrorNote>
                </div>
              )}
            </div>
            <div>
              <p className="mb-1 text-sm font-medium">2. Push webhook</p>
              <p className="mb-3 text-sm text-neutral-500">
                In each repository go to <b>Settings → Webhooks → Add webhook</b>, use content type <code>application/json</code>, the secret
                below, and the <b>Just the push event</b> option. The dashboard must be reachable from GitHub for this to work.
              </p>
              <div className="space-y-3">
                <Field label="Payload URL">
                  <CodeBlock code={webhook} />
                </Field>
                <Field label="Secret">
                  <CodeBlock code={s.github.webhook_secret} />
                </Field>
              </div>
            </div>
          </div>
        </Card>

        <Card>
          <CardHeader
            title={
              <span className="flex items-center gap-2">
                <Cloud className="size-4" /> Cloudflare
              </span>
            }
            subtitle="Create and update DNS records for your workers automatically."
            action={s.cloudflare.connected ? <Badge tone="ok">Connected</Badge> : <Badge>Not connected</Badge>}
          />
          <div className="space-y-3 p-5">
            <p className="text-sm text-neutral-500">
              Create an API token at <b>Cloudflare → My Profile → API Tokens</b> with <b>Zone → Zone → Read</b> and <b>Zone → DNS → Edit</b> on the zones you use. The
              token is stored on this server and sent to a device only when SSL uses the DNS challenge.
            </p>
            <div className="flex gap-2">
              <input
                className={inputClass}
                type="password"
                autoComplete="off"
                placeholder={s.cloudflare.connected ? "Paste a new token to replace the current one" : "Cloudflare API token"}
                value={cfToken}
                onChange={(e) => setCfToken(e.target.value)}
              />
              <Button variant="primary" disabled={!cfToken.trim()} onClick={() => cf({ token: cfToken }, (r) => `Token verified. Zones: ${r.zones?.length ? r.zones.join(", ") : "none visible to this token"}.`)}>
                {s.cloudflare.connected ? "Replace" : "Connect"}
              </Button>
              {s.cloudflare.connected && (
                <Button variant="danger" onClick={async () => (await confirmDialog({ title: "Disconnect Cloudflare?", message: "Existing DNS records stay; SSL goes back to the default challenge.", confirmLabel: "Disconnect", danger: true })) && cf({}, () => "Disconnected.")}>
                  Disconnect
                </Button>
              )}
            </div>
            {s.cloudflare.connected && (
              <label className="flex items-center gap-2 text-sm">
                <input type="checkbox" checked={s.cloudflare.proxied} onChange={(e) => cf({ proxied: e.target.checked }, () => "Saved.")} />
                Proxy records through Cloudflare (orange cloud)
              </label>
            )}
            {cfMsg && (cfMsg.ok ? <p className="text-sm text-neutral-600">{cfMsg.text}</p> : <ErrorNote>{cfMsg.text}</ErrorNote>)}
            <ul className="list-disc space-y-1 pl-5 text-sm text-neutral-500">
              <li>Each hostname a device serves gets an A record pointing at that device's first IPv4 address. Existing A records are updated; a name that already has a CNAME is left alone, and records are never deleted.</li>
              <li>Records point at the address the device reports, which is often a private LAN address. That works on your network; for access from outside, port-forward or use a public address.</li>
            </ul>
          </div>
        </Card>
        <Card>
          <CardHeader
            title={
              <span className="flex items-center gap-2">
                <Lock className="size-4" /> SSL
              </span>
            }
            subtitle="How each device's proxy gets HTTPS certificates (Let's Encrypt, via Caddy)."
            action={<Badge tone="ok">{s.ssl_mode === "dns" ? "DNS challenge" : "HTTP challenge"}</Badge>}
          />
          <div className="space-y-3 p-5">
            {(
              [
                ["http", "HTTP challenge (default)", "Needs ports 80 and 443 on the device reachable from the internet and DNS pointing straight at it."],
                ["dns", "Cloudflare DNS challenge", "Proves ownership through a DNS record, so ports can stay closed and it works behind the Cloudflare proxy. Needs Cloudflare connected above."],
              ] as const
            ).map(([mode, label, text]) => (
              <label key={mode} className="flex cursor-pointer items-start gap-2 text-sm">
                <input type="radio" className="mt-1" name="ssl" checked={s.ssl_mode === mode} disabled={mode === "dns" && !s.cloudflare.connected} onChange={() => setSSL(mode)} />
                <span>
                  <span className="font-medium">{label}</span>
                  <br />
                  <span className="text-neutral-500">{text}</span>
                </span>
              </label>
            ))}
            {sslMsg && <ErrorNote>{sslMsg}</ErrorNote>}
            <p className="text-sm text-neutral-500">
              The DNS challenge runs a Caddy build that includes the Cloudflare module (<code>ghcr.io/caddybuilds/caddy-cloudflare</code>). Switching modes recreates the proxy container on each device, keeping its certificates.
            </p>
          </div>
        </Card>
        <Card>
          <CardHeader
            title={
              <span className="flex items-center gap-2">
                <Globe className="size-4" /> Domains
              </span>
            }
            subtitle="Every worker gets name.<base domain>. Point a wildcard DNS record at your device, or add records per worker."
          />
          <div className="p-5">
            <p className="mb-1 text-sm font-medium">Base domain</p>
            <div className="flex gap-2">
              <input className={inputClass} placeholder="example.com" value={base ?? s.base_domain} onChange={(e) => setBase(e.target.value)} />
              <Button variant="primary" onClick={saveBase}>
                Save
              </Button>
            </div>
            {baseMsg && <p className="mt-2 text-sm text-neutral-600">{baseMsg}</p>}
          </div>
        </Card>
        <Card>
          <CardHeader
            title={
              <span className="flex items-center gap-2">
                <Archive className="size-4" /> Backup password
              </span>
            }
            subtitle="Optional. With a password set, new backups are encrypted (AES-256) before they are written to disk."
            action={s.backup_password_set ? <Badge tone="ok">Encrypting</Badge> : <Badge>Off</Badge>}
          />
          <div className="space-y-3 p-5">
            <div className="flex gap-2">
              <input
                className={inputClass}
                type="password"
                autoComplete="new-password"
                placeholder={s.backup_password_set ? "Enter a new password to replace the current one" : "At least 8 characters"}
                value={bkPw}
                onChange={(e) => setBkPw(e.target.value)}
              />
              <Button variant="primary" disabled={bkPw.length === 0} onClick={() => saveBackupPassword(bkPw)}>
                {s.backup_password_set ? "Change" : "Set"}
              </Button>
              {s.backup_password_set && (
                <Button
                  variant="danger"
                  onClick={async () => (await confirmDialog({ title: "Stop encrypting new backups?", message: "Backups already made stay encrypted and need this password to restore.", confirmLabel: "Remove password", danger: true })) && saveBackupPassword("")}
                >
                  Remove
                </Button>
              )}
            </div>
            {bkMsg && (bkMsg.ok ? <p className="text-sm text-neutral-600">{bkMsg.text}</p> : <ErrorNote>{bkMsg.text}</ErrorNote>)}
            <ul className="list-disc space-y-1 pl-5 text-sm text-neutral-500">
              <li>The password is kept on this server so scheduled backups can run and the dashboard can restore and download them. What it protects is the backup files on disk and any copy of them taken elsewhere.</li>
              <li>Changing it does not re-encrypt old backups: restoring one needs the password it was made with, so keep the old password until those backups are gone. There is no way to recover a lost password.</li>
              <li>
                To open an encrypted copy without this server: <code>ORCH_BACKUP_PASSWORD=… orchestrator-server decrypt-backup -in x.tar.gz.enc -out x.tar.gz</code>
              </li>
            </ul>
          </div>
        </Card>
        <Card>
          <CardHeader
            title={
              <span className="flex items-center gap-2">
                <KeyRound className="size-4" /> Join token
              </span>
            }
            subtitle="New devices use this token once to register. Each device then gets its own credential."
            action={
              <Button
                size="sm"
                onClick={async () => {
                  if (!(await confirmDialog({ title: "Rotate the join token?", message: "Install commands you've already copied will stop working.", confirmLabel: "Rotate", danger: true }))) return;
                  await api.rotateJoinToken();
                  settings.reload();
                }}
              >
                Rotate
              </Button>
            }
          />
          <div className="p-5">
            <CodeBlock code={s.join_token} />
          </div>
        </Card>

        <Card>
          <CardHeader title="About" />
          <dl className="space-y-2 p-5 text-sm">
            <div className="flex justify-between">
              <dt className="text-neutral-500">Version</dt>
              <dd>{s.version}</dd>
            </div>
            <div className="flex justify-between">
              <dt className="text-neutral-500">Agent heartbeat</dt>
              <dd>every {s.heartbeat_interval_seconds}s</dd>
            </div>
          </dl>
        </Card>
      </div>
    </div>
  );
}
