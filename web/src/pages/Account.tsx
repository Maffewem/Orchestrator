import { useState } from "react";
import { api } from "../api";
import { useCrumbs } from "../components/Layout";
import { useMe } from "../components/auth";
import { Badge, Button, Card, CardHeader, ErrorNote, Field, PageHeader, SecretInput, inputClass } from "../components/ui";

/** The signed-in person's own settings, kept apart from the admin-only Settings page. */
export default function AccountPage() {
  useCrumbs([{ label: "My account" }]);
  const me = useMe();
  if (!me) return null;
  return (
    <div className="max-w-2xl">
      <PageHeader title="My account" description="Your sign-in on this server." />
      <Card className="mb-6">
        <CardHeader title="Profile" />
        <dl className="grid grid-cols-[8rem_1fr] gap-y-2 p-5 text-sm">
          <dt className="text-neutral-500">Username</dt>
          <dd className="font-medium">{me.username}</dd>
          <dt className="text-neutral-500">Role</dt>
          <dd>
            <Badge tone="info">{me.role}</Badge>
          </dd>
        </dl>
      </Card>
      {me.token_login ? (
        <Card>
          <div className="p-5 text-sm text-neutral-600">
            You are signed in with the admin token, which has no password or separate sessions. Sign in with an account to manage them here.
          </div>
        </Card>
      ) : (
        <>
          <PasswordCard />
          <SessionsCard />
        </>
      )}
    </div>
  );
}

function PasswordCard() {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);
  return (
    <Card className="mb-6">
      <CardHeader title="Change password" subtitle="Other devices are signed out when you change it." />
      <form
        className="space-y-4 p-5"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError(null);
          setDone(false);
          try {
            await api.changePassword(current, next);
            setCurrent("");
            setNext("");
            setDone(true);
          } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <Field label="Current password">
          <SecretInput className={inputClass} autoComplete="current-password" value={current} onChange={(e) => setCurrent(e.target.value)} />
        </Field>
        <Field label="New password" hint="Must meet the minimum length set by an administrator.">
          <SecretInput className={inputClass} autoComplete="new-password" value={next} onChange={(e) => setNext(e.target.value)} />
        </Field>
        {error && <ErrorNote>{error}</ErrorNote>}
        {done && <p role="status" className="text-sm text-green-700">Password changed.</p>}
        <Button variant="primary" loading={busy} disabled={!current || !next}>
          Change password
        </Button>
      </form>
    </Card>
  );
}

function SessionsCard() {
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  return (
    <Card>
      <CardHeader title="Sessions" subtitle="Left a browser signed in somewhere else? End every session except this one." />
      <div className="space-y-3 p-5">
        <Button
          loading={busy}
          onClick={async () => {
            setBusy(true);
            setMsg(null);
            try {
              await api.revokeOtherSessions();
              setMsg({ ok: true, text: "Signed out of all other devices." });
            } catch (err) {
              setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
            } finally {
              setBusy(false);
            }
          }}
        >
          Sign out of other devices
        </Button>
        {msg && (msg.ok ? <p role="status" className="text-sm text-green-700">{msg.text}</p> : <ErrorNote>{msg.text}</ErrorNote>)}
      </div>
    </Card>
  );
}
