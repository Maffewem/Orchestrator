import { useState } from "react";
import { api, type Settings } from "../api";
import { Button, ErrorNote, Field, inputClass } from "./ui";
import { confirmDialog } from "./dialogs";

/** Account ID and token for deploying to Cloudflare Pages and Workers. */
export function CloudflareDeploy({ s, reload }: { s: Settings; reload: () => void }) {
  const d = s.cloudflare.deploy;
  const [account, setAccount] = useState(d.account_id);
  const [token, setToken] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState(false);

  const save = async (acct: string, tok?: string) => {
    setBusy(true);
    setMsg(null);
    try {
      const r = await api.setCloudflareDeploy(acct, tok);
      setToken("");
      reload();
      if (!acct) {
        setAccount("");
        setMsg({ ok: true, text: "Disconnected. Projects already on Cloudflare keep running." });
      } else {
        const missing = [r.pages === false && "Pages", r.workers === false && "Workers"].filter(Boolean);
        setMsg({ ok: true, text: missing.length ? `Connected, but the token can't reach ${missing.join(" or ")}. Deploys to those will fail until it can.` : "Connected. The token can reach Pages and Workers." });
      }
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-3 p-5">
      <p className="text-sm text-neutral-500">
        Choose <b>Cloudflare</b> as the source when you create a project. This server clones the repository, runs the build, and uploads the result to Pages or as a Worker. Pull requests become preview deployments.
      </p>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Account ID" hint="Cloudflare dashboard → Workers &amp; Pages → Account ID.">
          <input className={inputClass} placeholder="32-character account ID" value={account} onChange={(e) => setAccount(e.target.value)} />
        </Field>
        <Field label="API token" hint={d.own_token ? "A token for deployments is saved. Paste another to replace it." : d.configured ? "Empty: use the token from the Cloudflare section above." : "Needs the permissions listed below."}>
          <input className={inputClass} type="password" autoComplete="off" placeholder="Token for Pages and Workers" value={token} onChange={(e) => setToken(e.target.value)} />
        </Field>
      </div>
      <div className="flex gap-2">
        <Button variant="primary" loading={busy} disabled={!account.trim() || (!d.configured && !token.trim() && !s.cloudflare.connected)} onClick={() => save(account.trim(), token.trim())}>
          {d.configured ? "Save" : "Connect"}
        </Button>
        {d.configured && (
          <Button
            variant="danger"
            onClick={async () => (await confirmDialog({ title: "Disconnect Cloudflare Pages & Workers?", message: "Projects keep what they deployed, but new deployments and monitoring stop until you connect again.", confirmLabel: "Disconnect", danger: true })) && save("")}
          >
            Disconnect
          </Button>
        )}
      </div>
      {msg && (msg.ok ? <p className="text-sm text-neutral-600">{msg.text}</p> : <ErrorNote>{msg.text}</ErrorNote>)}
      <details className="rounded-lg border border-neutral-200 bg-neutral-50">
        <summary className="cursor-pointer px-3 py-2.5 text-sm font-medium">Token permissions and limits</summary>
        <div className="space-y-2 px-3 pb-3 text-sm text-neutral-500">
          <p>
            Create the token at <b>My Profile → API Tokens</b> with <b>Account → Cloudflare Pages → Edit</b>, <b>Account → Workers Scripts → Edit</b> and <b>Account → Account Analytics → Read</b> (request counts for Workers).
          </p>
          <ul className="list-disc space-y-1 pl-5">
            <li>Builds run on this server with Node.js and npm. They can read what the server can, so only deploy repositories you trust.</li>
            <li>A Worker must build to one bundled ES module file. Request and error counts are shown for Workers only; Cloudflare keeps none for static Pages sites.</li>
            <li>Rolling a Pages site back promotes the earlier deployment on Cloudflare. A Worker is built and uploaded again from that commit.</li>
          </ul>
        </div>
      </details>
    </div>
  );
}
