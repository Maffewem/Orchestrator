import { useState } from "react";
import { api, type SSOView } from "../api";
import { Button, ErrorNote, Field, inputClass } from "./ui";

const list = (v: string) => v.split(",").map((x) => x.trim()).filter(Boolean);
const roles = [["pending", "Wait for an admin to approve"], ["viewer", "Viewer"], ["operator", "Operator"]];

const presets: Record<string, { name: string; issuer: string; hint: string }> = {
  custom: { name: "Single sign-on", issuer: "", hint: "Any OpenID Connect provider: Authentik, Keycloak, Entra, Okta." },
  google: { name: "Google", issuer: "https://accounts.google.com", hint: "Create an OAuth client in Google Cloud Console." },
  cloudflare: { name: "Cloudflare Access", issuer: "", hint: "Zero Trust > Access > Applications > Add > SaaS > OIDC. Copy the issuer URL it shows." },
};

export function SSOSettings({ value, onSaved }: { value: SSOView; onSaved: () => void }) {
  const [c, setC] = useState(value);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const redirect = (p: string) => `${window.location.origin}/api/auth/sso/${p}/callback`;
  const gh = c.github, oi = c.oidc;
  const setGH = (p: Partial<typeof gh>) => setC({ ...c, github: { ...gh, ...p } });
  const setOI = (p: Partial<typeof oi>) => setC({ ...c, oidc: { ...oi, ...p } });

  const save = async () => {
    setBusy(true);
    setMsg(null);
    try {
      await api.setSSO({ github: gh, oidc: oi, hide_password: c.hide_password });
      setMsg({ ok: true, text: "Saved. The sign-in page now shows the enabled methods." });
      onSaved();
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-5 p-5">
      <p className="text-sm text-neutral-500">Built-in accounts and the admin token always keep working, so a broken provider never locks you out.</p>

      <div className="space-y-3 rounded-lg border border-neutral-200 p-3">
        <label className="flex items-center gap-2 text-sm font-medium">
          <input type="checkbox" checked={gh.enabled} onChange={(e) => setGH({ enabled: e.target.checked })} /> GitHub
        </label>
        {gh.enabled && (
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="Client ID"><input className={inputClass} value={gh.client_id} onChange={(e) => setGH({ client_id: e.target.value })} /></Field>
            <Field label="Client secret"><input className={inputClass} type="password" value={gh.client_secret ?? ""} placeholder={c.github_secret_set ? "Saved. Leave blank to keep" : ""} onChange={(e) => setGH({ client_secret: e.target.value })} /></Field>
            <Field label="Allowed organisations" hint="Comma separated. Members of these GitHub organisations can sign in."><input className={inputClass} value={gh.orgs.join(", ")} onChange={(e) => setGH({ orgs: list(e.target.value) })} /></Field>
            <Field label="New users start as">
              <select className={inputClass} value={gh.default_role} onChange={(e) => setGH({ default_role: e.target.value })}>
                {roles.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
              </select>
            </Field>
            <p className="text-sm text-neutral-500 sm:col-span-2">Create an OAuth app in GitHub (Settings, then Developer settings). Callback URL: <code className="break-all">{redirect("github")}</code>. Organisations that restrict third-party apps must approve it.</p>
          </div>
        )}
      </div>

      <div className="space-y-3 rounded-lg border border-neutral-200 p-3">
        <label className="flex items-center gap-2 text-sm font-medium">
          <input type="checkbox" checked={oi.enabled} onChange={(e) => setOI({ enabled: e.target.checked })} /> OpenID Connect (Cloudflare Access, Google, Authentik, Entra, Keycloak)
        </label>
        {oi.enabled && (
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="Provider">
              <select className={inputClass} value="" onChange={(e) => e.target.value && setOI({ name: presets[e.target.value].name, issuer: presets[e.target.value].issuer })}>
                <option value="">Fill in from a preset...</option>
                {Object.entries(presets).map(([k, p]) => <option key={k} value={k}>{p.name}</option>)}
              </select>
            </Field>
            <Field label="Button label"><input className={inputClass} value={oi.name} onChange={(e) => setOI({ name: e.target.value })} /></Field>
            <Field label="Issuer URL" hint="Cloudflare: https://<team>.cloudflareaccess.com/cdn-cgi/access/sso/oidc/<client id>"><input className={inputClass} value={oi.issuer} onChange={(e) => setOI({ issuer: e.target.value })} /></Field>
            <Field label="Client ID"><input className={inputClass} value={oi.client_id} onChange={(e) => setOI({ client_id: e.target.value })} /></Field>
            <Field label="Client secret"><input className={inputClass} type="password" value={oi.client_secret ?? ""} placeholder={c.oidc_secret_set ? "Saved. Leave blank to keep" : ""} onChange={(e) => setOI({ client_secret: e.target.value })} /></Field>
            <Field label="Allowed email domains" hint="Comma separated, or * to accept everyone the provider signs in (for example when Cloudflare Access already limits who can reach it)."><input className={inputClass} value={oi.domains.join(", ")} onChange={(e) => setOI({ domains: list(e.target.value) })} /></Field>
            <Field label="New users start as">
              <select className={inputClass} value={oi.default_role} onChange={(e) => setOI({ default_role: e.target.value })}>
                {roles.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
              </select>
            </Field>
            <Field label="Group claim" hint="Optional. Roles follow group membership."><input className={inputClass} value={oi.group_claim} onChange={(e) => setOI({ group_claim: e.target.value })} placeholder="groups" /></Field>
            <Field label="Admin group"><input className={inputClass} value={oi.admin_group} onChange={(e) => setOI({ admin_group: e.target.value })} /></Field>
            <Field label="Operator group"><input className={inputClass} value={oi.operator_group} onChange={(e) => setOI({ operator_group: e.target.value })} /></Field>
            <p className="text-sm text-neutral-500 sm:col-span-2">Redirect URL to register with the provider: <code className="break-all">{redirect("oidc")}</code></p>
          </div>
        )}
      </div>

      <label className="flex cursor-pointer items-start gap-2 text-sm">
        <input type="checkbox" className="mt-1" checked={c.hide_password} onChange={(e) => setC({ ...c, hide_password: e.target.checked })} />
        <span>
          <span className="font-medium">Hide the username and password form</span>
          <br />
          <span className="text-neutral-500">Only once a provider works. "Use the admin token instead" stays on the sign-in page.</span>
        </span>
      </label>

      <div className="flex items-center gap-3">
        <Button variant="primary" loading={busy} onClick={save}>Save sign-in methods</Button>
        {msg && (msg.ok ? <span className="text-sm text-ok">{msg.text}</span> : <ErrorNote>{msg.text}</ErrorNote>)}
      </div>
    </div>
  );
}
