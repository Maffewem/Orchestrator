import { useState } from "react";
import { KeyRound, Trash2, UserPlus } from "lucide-react";
import { api, type Role, type User } from "../api";
import { useCrumbs } from "../components/Layout";
import { useMe } from "../components/auth";
import { Badge, Button, Card, CardHeader, ErrorNote, Field, Loading, Modal, PageHeader, Table, Td, inputClass } from "../components/ui";
import { ago, usePoll } from "../lib";

const roleHelp: Record<Role, string> = {
  viewer: "Read-only: sees everything, changes nothing.",
  operator: "Deploys, restarts, backs up, manages domains and projects.",
  admin: "Everything, including users, settings and updates.",
};

export default function UsersPage() {
  useCrumbs([{ label: "Users" }]);
  const me = useMe();
  const users = usePoll(api.users, 0);
  const [error, setError] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const [reset, setReset] = useState<User | null>(null);

  const act = async (fn: () => Promise<unknown>) => {
    setError(null);
    try {
      await fn();
      users.reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };
  if (!users.data) return users.error ? <ErrorNote>{users.error}</ErrorNote> : <Loading />;

  return (
    <div className="max-w-4xl">
      <PageHeader title="Users" description="Who can sign in to this dashboard, and what they can do." actions={<Button variant="primary" icon={<UserPlus className="size-4" />} onClick={() => setAdding(true)}>Add user</Button>} />
      {error && (
        <div className="mb-4">
          <ErrorNote>{error}</ErrorNote>
        </div>
      )}
      <Card>
        <CardHeader title="Accounts" subtitle="The admin token keeps working as a fallback and is not listed here." />
        <Table head={["User", "Role", "Added", ""]}>
          {users.data.map((u) => (
            <tr key={u.id}>
              <Td>
                <span className="font-medium">{u.username}</span> {u.username === me?.username && <Badge tone="info">you</Badge>} {u.disabled && <Badge tone="warn">disabled</Badge>}
              </Td>
              <Td>
                <select
                  className={inputClass + " w-36"}
                  value={u.role}
                  aria-label={`Role of ${u.username}`}
                  title={roleHelp[u.role]}
                  onChange={(e) => act(() => api.updateUser(u.id, { role: e.target.value as Role }))}
                >
                  <option value="viewer">Viewer</option>
                  <option value="operator">Operator</option>
                  <option value="admin">Admin</option>
                </select>
              </Td>
              <Td className="text-neutral-500">{ago(u.created_at)}</Td>
              <Td className="text-right">
                <div className="flex justify-end gap-1">
                  <Button aria-label={`Reset password of ${u.username}`} icon={<KeyRound className="size-4" />} onClick={() => setReset(u)} />
                  <Button onClick={() => act(() => api.updateUser(u.id, { disabled: !u.disabled }))}>{u.disabled ? "Enable" : "Disable"}</Button>
                  <Button
                    variant="danger"
                    aria-label={`Delete ${u.username}`}
                    icon={<Trash2 className="size-4" />}
                    onClick={() => confirm(`Delete ${u.username}?`) && act(() => api.deleteUser(u.id))}
                  />
                </div>
              </Td>
            </tr>
          ))}
        </Table>
        {users.data.length === 0 && <p className="p-5 text-sm text-neutral-500">No accounts yet. Sign out and create the first administrator.</p>}
      </Card>
      <dl className="mt-6 grid gap-3 text-sm sm:grid-cols-3">
        {(Object.keys(roleHelp) as Role[]).map((r) => (
          <div key={r} className="rounded-lg border border-neutral-200 bg-white p-4">
            <dt className="font-medium capitalize">{r}</dt>
            <dd className="mt-1 text-neutral-500">{roleHelp[r]}</dd>
          </div>
        ))}
      </dl>
      {adding && <AddUser onClose={() => setAdding(false)} onDone={() => { setAdding(false); users.reload(); }} />}
      {reset && <ResetPassword user={reset} onClose={() => setReset(null)} />}
    </div>
  );
}

function AddUser({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<Role>("operator");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  return (
    <Modal title="Add user" onClose={onClose}>
      <form
        className="space-y-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError(null);
          try {
            await api.createUser({ username, password, role });
            onDone();
          } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
            setBusy(false);
          }
        }}
      >
        <Field label="Username" hint="Letters, digits, dot, dash or underscore.">
          <input className={inputClass} autoFocus value={username} onChange={(e) => setUsername(e.target.value)} />
        </Field>
        <Field label="Password" hint="At least 10 characters. They can't change it themselves yet, so share it safely.">
          <input className={inputClass} type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} />
        </Field>
        <Field label="Role" hint={roleHelp[role]}>
          <select className={inputClass} value={role} onChange={(e) => setRole(e.target.value as Role)}>
            <option value="viewer">Viewer</option>
            <option value="operator">Operator</option>
            <option value="admin">Admin</option>
          </select>
        </Field>
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex justify-end gap-2">
          <Button type="button" onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!username || !password}>Add user</Button>
        </div>
      </form>
    </Modal>
  );
}

function ResetPassword({ user, onClose }: { user: User; onClose: () => void }) {
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  return (
    <Modal title={`Reset password for ${user.username}`} onClose={onClose}>
      <form
        className="space-y-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError(null);
          try {
            await api.updateUser(user.id, { password });
            onClose();
          } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
            setBusy(false);
          }
        }}
      >
        <Field label="New password" hint="They are signed out everywhere and must use this one.">
          <input className={inputClass} type="password" autoFocus autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} />
        </Field>
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex justify-end gap-2">
          <Button type="button" onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!password}>Reset password</Button>
        </div>
      </form>
    </Modal>
  );
}
