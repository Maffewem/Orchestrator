import { useEffect, useState } from "react";
import { api, type LoginResult } from "../api";
import { Logo } from "../components/Layout";
import { Button, ErrorNote, Field, Loading, inputClass } from "../components/ui";

export default function Login({ onLogin }: { onLogin: (token: string) => void }) {
  const [setup, setSetup] = useState<boolean | null>(null);
  const [mode, setMode] = useState<"login" | "token">("login");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [token, setToken] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.authStatus().then((s) => setSetup(s.setup_required), () => setSetup(false));
  }, []);

  if (setup === null) return <Loading />;

  const run = async (fn: () => Promise<string>) => {
    setBusy(true);
    setError(null);
    try {
      onLogin(await fn());
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const done = (r: LoginResult) => r.token;

  let body;
  if (setup) {
    body = (
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (password !== confirm) return setError("The passwords don't match.");
          run(async () => done(await api.setup(token.trim(), username, password)));
        }}
      >
        <p className="text-sm text-neutral-500">
          Create the first administrator. Paste the admin token the server printed on first start (or <code>ORCH_ADMIN_TOKEN</code>) to prove it's you.
        </p>
        <Field label="Admin token">
          <input className={inputClass} type="password" autoFocus value={token} onChange={(e) => setToken(e.target.value)} />
        </Field>
        <Field label="Username">
          <input className={inputClass} autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} />
        </Field>
        <Field label="Password" hint="At least 10 characters.">
          <input className={inputClass} type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} />
        </Field>
        <Field label="Repeat password">
          <input className={inputClass} type="password" autoComplete="new-password" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
        </Field>
        {error && <ErrorNote>{error}</ErrorNote>}
        <Button variant="primary" className="w-full" loading={busy} disabled={!token.trim() || !username || !password}>
          Create account
        </Button>
      </form>
    );
  } else if (mode === "login") {
    body = (
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          run(async () => done(await api.login(username, password)));
        }}
      >
        <Field label="Username">
          <input className={inputClass} autoFocus autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} />
        </Field>
        <Field label="Password">
          <input className={inputClass} type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
        </Field>
        {error && <ErrorNote>{error}</ErrorNote>}
        <Button variant="primary" className="w-full" loading={busy} disabled={!username || !password}>
          Sign in
        </Button>
        <button type="button" className="w-full text-center text-sm text-neutral-500 hover:text-neutral-900" onClick={() => { setMode("token"); setError(null); }}>
          Use the admin token instead
        </button>
      </form>
    );
  } else {
    body = (
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          run(async () => {
            await api.me(token.trim());
            return token.trim();
          });
        }}
      >
        <p className="text-sm text-neutral-500">The admin token always works, even if every account is locked out.</p>
        <Field label="Admin token">
          <input className={inputClass} type="password" autoFocus value={token} onChange={(e) => setToken(e.target.value)} />
        </Field>
        {error && <ErrorNote>{error}</ErrorNote>}
        <Button variant="primary" className="w-full" loading={busy} disabled={!token.trim()}>
          Continue
        </Button>
        <button type="button" className="w-full text-center text-sm text-neutral-500 hover:text-neutral-900" onClick={() => { setMode("login"); setError(null); }}>
          Sign in with a username
        </button>
      </form>
    );
  }

  return (
    <div className="flex min-h-full items-center justify-center p-4">
      <div className="w-full max-w-sm">
        <div className="mb-8 flex justify-center text-lg">
          <Logo />
        </div>
        <div className="rounded-xl border border-neutral-200 bg-white p-6 shadow-sm">
          <h1 className="mb-4 text-lg font-semibold">{setup ? "Set up Orchestrator" : "Sign in"}</h1>
          {body}
        </div>
      </div>
    </div>
  );
}
