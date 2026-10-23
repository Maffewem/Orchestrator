import { useEffect, useState, type ReactNode } from "react";
import { useLocation } from "react-router";
import { Archive, Cloud, Database, ShieldCheck, Github, Globe, KeyRound, Lock, Moon } from "lucide-react";
import { api, type Settings } from "../api";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, CardHeader, CodeBlock, ErrorNote, Field, Loading, PageHeader, inputClass } from "../components/ui";
import { usePoll } from "../lib";
import { copyText } from "../clipboard";
import { getTheme, setTheme, type ThemeChoice } from "../theme";
import { HTTPSSettings } from "../components/HTTPSSettings";
import { SSOSettings } from "../components/SSOSettings";
import { ServerBackupsPanel } from "../components/ServerBackups";
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
  const [find, setFind] = useState("");

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

  const keywords: Record<string, string> = {
    appearance: "appearance theme dark light",
    security: "security session lifetime password length https redirect",
    https: "https tls certificate lets encrypt dashboard hsts",
    sso: "sso single sign-on oidc cloudflare access google github login",
    github: "github token webhook repositories",
    cloudflare: "cloudflare dns token zone",
    ssl: "ssl https certificates caddy",
    domains: "domains base domain wildcard",
    backups: "backup password encrypt",
    serverbackup: "server backup restore database schedule download",
    join: "join token devices register",
  };
  const vis = (id: string) => !find.trim() || keywords[id].includes(find.trim().toLowerCase());

  return (
    <div className="max-w-3xl">
      <PageHeader title="Settings" />
      <input className={`${inputClass} mb-4 max-w-xs`} type="search" placeholder="Search settings" aria-label="Search settings" value={find} onChange={(e) => setFind(e.target.value)} />
      {Object.keys(keywords).every((k) => !vis(k)) && <p className="mb-4 text-sm text-neutral-500">No settings match. Try a word such as token, domain or theme.</p>}
      <div className="space-y-6">
        {vis("appearance") && (
          <Collapsible
            id="appearance"
            defaultOpen={false}
            title={
              <span className="flex items-center gap-2">
                <Moon className="size-4" /> Appearance
              </span>
            }
            subtitle="Light, dark, or follow this device."
          >
            <AppearanceChoice />
          </Collapsible>
        )}
        {vis("security") && (
          <Collapsible
            id="security"
            defaultOpen={false}
            title={
              <span className="flex items-center gap-2">
                <ShieldCheck className="size-4" /> Sign-in security
              </span>
            }
            subtitle="Session lifetime, password length and the HTTPS redirect."
            action={<Badge tone="ok">{s.security.session_hours >= 24 ? `${Math.round(s.security.session_hours / 24)} day sessions` : `${s.security.session_hours} hour sessions`}</Badge>}
          >
            <SecuritySettings value={s.security} onSaved={() => settings.reload()} />
          </Collapsible>
        )}
        {vis("https") && (
          <Collapsible
            id="https"
            defaultOpen={false}
            title={
              <span className="flex items-center gap-2">
                <Lock className="size-4" /> Dashboard HTTPS
              </span>
            }
            subtitle="A certificate for this dashboard, uploaded or issued automatically."
            action={s.https.listening ? <Badge tone="ok">{s.https.mode === "auto" ? "Automatic" : "Uploaded cert"}</Badge> : <Badge>Plain HTTP</Badge>}
          >
            <HTTPSSettings value={s.https} onSaved={() => settings.reload()} />
          </Collapsible>
        )}
        {vis("sso") && (
          <Collapsible
            id="sso"
            defaultOpen={false}
            title={
              <span className="flex items-center gap-2">
                <KeyRound className="size-4" /> Single sign-on
              </span>
            }
            subtitle="Let people sign in with GitHub, Cloudflare Access or another OpenID Connect provider."
            action={s.sso.github.enabled || s.sso.oidc.enabled ? <Badge tone="ok">On</Badge> : <Badge>Off</Badge>}
          >
            <SSOSettings value={s.sso} onSaved={() => settings.reload()} />
          </Collapsible>
        )}
        {vis("github") && (
        <Collapsible
          id="github"
          defaultOpen={!s.github.connected}
          openLabel="Set up"
            title={
              <span className="flex items-center gap-2">
                <Github className="size-4" /> GitHub
              </span>
            }
            subtitle="Deploy from repositories and redeploy on every push."
            action={s.github.connected ? <Badge tone="ok">Connected{login ? ` as ${login}` : ""}</Badge> : <Badge>Not connected</Badge>}
        >
          <div className="space-y-6 p-5">
            <div>
              <p className="mb-2 text-sm font-medium">1. Access token</p>
              <div className="mb-3">
                <Help title="How to create the token">
                  <p>
                    Create a fine-grained personal access token with read-only <b>Contents</b> and <b>Metadata</b> access to the repositories you want to
                    deploy. It is used to list repositories, read commits and clone private repos on your devices. Public repos work without one.
                  </p>
                </Help>
              </div>
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
              <p className="mb-2 text-sm font-medium">2. Push webhook</p>
              <div className="mb-3">
                <Help title="How to add the webhook">
                  <p>
                    In each repository go to <b>Settings → Webhooks → Add webhook</b>, use content type <code>application/json</code>, the secret
                    below, and the <b>Just the push event</b> option. The dashboard must be reachable from GitHub for this to work.
                  </p>
                </Help>
              </div>
              <div className="space-y-3">
                <Field label="Payload URL">
                  <CodeBlock code={webhook} />
                </Field>
                <Field label="Secret">
                  <SecretBlock value={s.github.webhook_secret} />
                </Field>
              </div>
            </div>
          </div>
        </Collapsible>
        )}

        {vis("cloudflare") && (
        <Collapsible
          id="cloudflare"
          defaultOpen={!s.cloudflare.connected}
          openLabel="Set up"
            title={
              <span className="flex items-center gap-2">
                <Cloud className="size-4" /> Cloudflare
              </span>
            }
            subtitle="Create and update DNS records for your workers automatically."
            action={s.cloudflare.connected ? <Badge tone="ok">Connected</Badge> : <Badge>Not connected</Badge>}
        >
          <div className="space-y-3 p-5">
            <Help title="How to create the token">
              <p>
                Create an API token at <b>Cloudflare → My Profile → API Tokens</b> with <b>Zone → Zone → Read</b> and <b>Zone → DNS → Edit</b> on the zones you use. The
                token is stored on this server and sent to a device only when SSL uses the DNS challenge.
              </p>
            </Help>
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
            <Help title="What gets created">
            <ul className="list-disc space-y-1 pl-5">
              <li>Each hostname a device serves gets an A record pointing at that device's first IPv4 address. Existing A records are updated; a name that already has a CNAME is left alone, and records are never deleted.</li>
              <li>Records point at the address the device reports, which is often a private LAN address. That works on your network; for access from outside, port-forward or use a public address.</li>
            </ul>
            </Help>
          </div>
        </Collapsible>
        )}
        {vis("ssl") && (
        <Collapsible
          id="ssl"
          defaultOpen={false}
            title={
              <span className="flex items-center gap-2">
                <Lock className="size-4" /> SSL
              </span>
            }
            subtitle="How each device's proxy gets HTTPS certificates (Let's Encrypt, via Caddy)."
            action={<Badge tone="ok">{s.ssl_mode === "dns" ? "DNS challenge" : "HTTP challenge"}</Badge>}
        >
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
        </Collapsible>
        )}
        {vis("domains") && (
        <Collapsible
          id="domains"
          defaultOpen={!s.base_domain}
            title={
              <span className="flex items-center gap-2">
                <Globe className="size-4" /> Domains
              </span>
            }
            subtitle="Every worker gets name.<base domain>. Point a wildcard DNS record at your device, or add records per worker."
        >
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
        </Collapsible>
        )}
        {vis("backups") && (
        <Collapsible
          id="backups"
          defaultOpen={false}
            title={
              <span className="flex items-center gap-2">
                <Archive className="size-4" /> Backup password
              </span>
            }
            subtitle="Optional. With a password set, new backups are encrypted (AES-256) before they are written to disk."
            action={s.backup_password_set ? <Badge tone="ok">Encrypting</Badge> : <Badge>Off</Badge>}
        >
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
            <Help title="What this protects">
            <ul className="list-disc space-y-1 pl-5">
              <li>The password is kept on this server so scheduled backups can run and the dashboard can restore and download them. What it protects is the backup files on disk and any copy of them taken elsewhere.</li>
              <li>Changing it does not re-encrypt old backups: restoring one needs the password it was made with, so keep the old password until those backups are gone. There is no way to recover a lost password.</li>
              <li>
                To open an encrypted copy without this server: <code>ORCH_BACKUP_PASSWORD=… orchestrator-server decrypt-backup -in x.tar.gz.enc -out x.tar.gz</code>
              </li>
            </ul>
            </Help>
          </div>
        </Collapsible>
        )}
        {vis("serverbackup") && (
          <Collapsible
            id="serverbackup"
            defaultOpen={false}
            title={
              <span className="flex items-center gap-2">
                <Database className="size-4" /> Server backup
              </span>
            }
            subtitle="Back up Orchestrator itself: users, projects, settings and history."
          >
            <ServerBackupsPanel />
          </Collapsible>
        )}
        {vis("join") && (
        <Collapsible
          id="join"
          defaultOpen={false}
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
        >
          <div className="p-5">
            <SecretBlock value={s.join_token} />
          </div>
        </Collapsible>
        )}

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

/** A settings card that opens and closes, and opens itself when the sidebar links to it (/settings#id). */
function Collapsible({
  id,
  title,
  subtitle,
  action,
  defaultOpen,
  openLabel = "Manage",
  children,
}: {
  id: string;
  title: ReactNode;
  subtitle?: ReactNode;
  action?: ReactNode;
  defaultOpen: boolean;
  openLabel?: string;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(defaultOpen);
  const { hash } = useLocation();
  useEffect(() => {
    if (hash !== `#${id}`) return;
    setOpen(true);
    requestAnimationFrame(() => document.getElementById(`sec-${id}`)?.scrollIntoView({ block: "start", behavior: "smooth" }));
  }, [hash, id]);
  return (
    <section id={`sec-${id}`} className="scroll-mt-20">
      <Card>
        <CardHeader
          title={title}
          subtitle={subtitle}
          action={
            <span className="flex items-center gap-2">
              {action}
              <Button size="sm" aria-expanded={open} onClick={() => setOpen(!open)}>
                {open ? "Close" : openLabel}
              </Button>
            </span>
          }
        />
        {open && children}
      </Card>
    </section>
  );
}

/** Setup steps and background reading, tucked away once the section is working. */
function Help({ title, children }: { title: string; children: ReactNode }) {
  return (
    <details className="rounded-lg border border-neutral-200 bg-neutral-50">
      <summary className="cursor-pointer px-3 py-2.5 text-sm font-medium">{title}</summary>
      <div className="space-y-2 px-3 pb-3 text-sm text-neutral-500">{children}</div>
    </details>
  );
}

/** A secret shown as dots until asked for, so it cannot be read off a shared screen by accident. */
function SecretBlock({ value }: { value: string }) {
  const [shown, setShown] = useState(false);
  const [copied, setCopied] = useState(false);
  return (
    <div className="flex items-center gap-2 code-surface rounded-lg py-2 pl-3 pr-2 font-mono text-xs">
      <span className="min-w-0 flex-1 break-all">{shown ? value : "•".repeat(28)}</span>
      <button className="shrink-0 rounded-md bg-white/10 px-2.5 py-1.5 hover:bg-white/20" onClick={() => setShown(!shown)}>
        {shown ? "Hide" : "Reveal"}
      </button>
      <button
        className="shrink-0 rounded-md bg-white/10 px-2.5 py-1.5 hover:bg-white/20"
        onClick={async () => {
          if (await copyText(value)) {
            setCopied(true);
            setTimeout(() => setCopied(false), 1500);
          }
        }}
      >
        {copied ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

function AppearanceChoice() {
  const [choice, setChoice] = useState<ThemeChoice>(getTheme());
  return (
    <div className="space-y-3 p-5">
      {(
        [
          ["light", "Light", "Always light."],
          ["dark", "Dark", "Always dark."],
          ["auto", "Auto", "Follows this device's setting."],
        ] as const
      ).map(([v, label, text]) => (
        <label key={v} className="flex cursor-pointer items-start gap-2 text-sm">
          <input
            type="radio"
            className="mt-1"
            name="theme"
            checked={choice === v}
            onChange={() => {
              setChoice(v);
              setTheme(v);
            }}
          />
          <span>
            <span className="font-medium">{label}</span>
            <br />
            <span className="text-neutral-500">{text}</span>
          </span>
        </label>
      ))}
      <p className="text-sm text-neutral-500">Saved in this browser, so each device can use its own.</p>
    </div>
  );
}

const lifetimes: [number, string][] = [[1, "1 hour"], [8, "8 hours"], [24, "1 day"], [168, "7 days"], [336, "14 days"], [720, "30 days"], [2160, "90 days"]];

function SecuritySettings({ value, onSaved }: { value: Settings["security"]; onSaved: () => void }) {
  const [hours, setHours] = useState(value.session_hours);
  const [minLen, setMinLen] = useState(value.min_password);
  const [force, setForce] = useState(value.force_https);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const save = async () => {
    setMsg(null);
    try {
      await api.setSecurity({ session_hours: hours, min_password: minLen, force_https: force });
      setMsg({ ok: true, text: "Saved. New sign-ins use these settings." });
      onSaved();
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    }
  };
  const options = lifetimes.some(([h]) => h === hours) ? lifetimes : [...lifetimes, [hours, `${hours} hours`] as [number, string]];
  return (
    <div className="space-y-4 p-5">
      <Field label="Session lifetime" hint="People must sign in again after this long. Existing sessions keep their old expiry.">
        <select className={`${inputClass} max-w-xs`} value={hours} onChange={(e) => setHours(Number(e.target.value))}>
          {options.map(([h, l]) => (
            <option key={h} value={h}>{l}</option>
          ))}
        </select>
      </Field>
      <Field label="Minimum password length" hint="Applies to new and changed passwords.">
        <input className={`${inputClass} max-w-[8rem]`} type="number" min={8} max={64} value={minLen} onChange={(e) => setMinLen(Number(e.target.value))} />
      </Field>
      <label className="flex cursor-pointer items-start gap-2 text-sm">
        <input type="checkbox" className="mt-1" checked={force} onChange={(e) => setForce(e.target.checked)} />
        <span>
          <span className="font-medium">Redirect HTTP to HTTPS</span>
          <br />
          <span className="text-neutral-500">Browsers on plain HTTP are sent to the HTTPS address. Agents, webhooks and the installer are not redirected. Turn it on while you are viewing the dashboard over HTTPS, for example behind a reverse proxy or tunnel.</span>
        </span>
      </label>
      <div className="flex items-center gap-3">
        <Button onClick={save}>Save</Button>
        {msg && (msg.ok ? <span className="text-sm text-ok">{msg.text}</span> : <ErrorNote>{msg.text}</ErrorNote>)}
      </div>
    </div>
  );
}
