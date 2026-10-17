import { useState } from "react";
import { api } from "../api";
import { Logo } from "../components/Layout";
import { Button, ErrorNote, Field, inputClass } from "../components/ui";

export default function Login({ onLogin }: { onLogin: (token: string) => void }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  return (
    <div className="flex min-h-full items-center justify-center p-4">
      <form
        className="w-full max-w-sm"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError(null);
          try {
            await api.checkToken(token.trim());
            onLogin(token.trim());
          } catch {
            setError("That token was not accepted.");
          } finally {
            setBusy(false);
          }
        }}
      >
        <div className="mb-8 flex justify-center text-lg">
          <Logo />
        </div>
        <div className="rounded-xl border border-neutral-200 bg-white p-6 shadow-sm">
          <h1 className="mb-1 text-lg font-semibold">Sign in</h1>
          <p className="mb-5 text-sm text-neutral-500">
            Paste the admin token the server printed on first start (or the value of <code>ORCH_ADMIN_TOKEN</code>).
          </p>
          <div className="space-y-4">
            <Field label="Admin token">
              <input className={inputClass} type="password" autoFocus value={token} onChange={(e) => setToken(e.target.value)} />
            </Field>
            {error && <ErrorNote>{error}</ErrorNote>}
            <Button variant="primary" className="w-full" loading={busy} disabled={!token.trim()}>
              Continue
            </Button>
          </div>
        </div>
      </form>
    </div>
  );
}
