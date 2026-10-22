import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { Link, NavLink, Outlet, useLocation } from "react-router";
import { Activity, Archive, Boxes, RefreshCw, ChevronRight, LayoutDashboard, KeyRound, LogOut, Menu, Rocket, Server, Settings, Users, X } from "lucide-react";
import { api } from "../api";
import { cx } from "../lib";
import { useMe } from "./auth";
import { Button, ErrorNote, Field, Modal, inputClass } from "./ui";

export type Crumb = { label: string; to?: string };

const CrumbContext = createContext<(c: Crumb[]) => void>(() => {});

/** Sets the top-bar breadcrumb for the current page. */
export function useCrumbs(crumbs: Crumb[]) {
  const set = useContext(CrumbContext);
  const key = JSON.stringify(crumbs);
  useEffect(() => {
    set(crumbs);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, set]);
}

type NavItem = { to: string; label: string; icon: typeof Server; end?: boolean; admin?: boolean };

// Grouped from most-used to least-used: what is running, where it runs, then administration.
const navGroups: { title?: string; items: NavItem[] }[] = [
  { items: [{ to: "/", label: "Overview", icon: LayoutDashboard, end: true }] },
  {
    title: "Deploy",
    items: [
      { to: "/projects", label: "Projects", icon: Rocket },
      { to: "/containers", label: "Workers", icon: Boxes },
    ],
  },
  {
    title: "Infrastructure",
    items: [
      { to: "/devices", label: "Devices", icon: Server },
      { to: "/backups", label: "Backups", icon: Archive },
      { to: "/updates", label: "Updates", icon: RefreshCw, admin: true },
    ],
  },
  { title: "Monitor", items: [{ to: "/activity", label: "Activity", icon: Activity }] },
  {
    title: "Admin",
    items: [
      { to: "/users", label: "Users", icon: Users, admin: true },
      { to: "/settings", label: "Settings", icon: Settings, admin: true },
    ],
  },
];

const tabBar: NavItem[] = [
  { to: "/", label: "Overview", icon: LayoutDashboard, end: true },
  { to: "/projects", label: "Projects", icon: Rocket },
  { to: "/containers", label: "Workers", icon: Boxes },
  { to: "/devices", label: "Devices", icon: Server },
];

export function Logo() {
  return (
    <span className="flex items-center gap-2 font-semibold tracking-tight">
      <svg viewBox="0 0 32 32" className="size-7" aria-hidden>
        <rect width="32" height="32" rx="8" fill="var(--color-brand)" />
        <circle cx="16" cy="16" r="5" fill="white" />
        <circle cx="16" cy="6" r="2.5" fill="white" />
        <circle cx="24.7" cy="21" r="2.5" fill="white" />
        <circle cx="7.3" cy="21" r="2.5" fill="white" />
      </svg>
      Orchestrator
    </span>
  );
}

export default function Layout({ onLogout }: { onLogout: () => void }) {
  const [crumbs, setCrumbs] = useState<Crumb[]>([]);
  const [open, setOpen] = useState(false);
  const [pw, setPw] = useState(false);
  const me = useMe();
  const loc = useLocation();
  useEffect(() => setOpen(false), [loc.pathname]);

  const sidebar = (
    <nav className="flex h-full flex-col gap-0.5 p-3">
      <div className="mb-4 px-2 py-1.5">
        <Link to="/">
          <Logo />
        </Link>
      </div>
      {navGroups.map((g) => {
        const items = g.items.filter((n) => !n.admin || me?.role === "admin");
        if (!items.length) return null;
        return (
          <div key={g.title ?? "top"} className="mb-3">
            {g.title && <p className="px-2.5 pb-1 pt-2 text-xs font-medium uppercase tracking-wide text-neutral-400">{g.title}</p>}
            {items.map(({ to, label, icon: Icon, end }) => (
              <NavLink
                key={to}
                to={to}
                end={end}
                className={({ isActive }) =>
                  cx(
                    "flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm transition-colors",
                    isActive ? "bg-neutral-200/70 font-medium text-neutral-900" : "text-neutral-600 hover:bg-neutral-100 hover:text-neutral-900",
                  )
                }
              >
                <Icon className="size-4" />
                {label}
              </NavLink>
            ))}
          </div>
        );
      })}
      <div className="mt-auto">
        {me && (
          <div className="mb-1 px-2.5 py-2 text-sm">
            <p className="truncate font-medium">{me.username}</p>
            <p className="text-xs capitalize text-neutral-500">{me.role}</p>
          </div>
        )}
        {me && !me.token_login && (
          <button
            onClick={() => setPw(true)}
            className="flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm text-neutral-600 hover:bg-neutral-100 hover:text-neutral-900"
          >
            <KeyRound className="size-4" />
            Change password
          </button>
        )}
        <button
          onClick={onLogout}
          className="flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm text-neutral-600 hover:bg-neutral-100 hover:text-neutral-900"
        >
          <LogOut className="size-4" />
          Sign out
        </button>
      </div>
    </nav>
  );

  return (
    <CrumbContext.Provider value={setCrumbs}>
      <div className="flex min-h-full">
        <aside className="sticky top-0 hidden h-screen w-60 shrink-0 border-r border-neutral-200 bg-neutral-50 lg:block">{sidebar}</aside>
        {open && (
          <div className="fixed inset-0 z-40 bg-black/30 lg:hidden" onClick={() => setOpen(false)}>
            <aside className="h-full w-64 bg-neutral-50" onClick={(e) => e.stopPropagation()}>
              {sidebar}
            </aside>
          </div>
        )}
        <div className="flex min-w-0 flex-1 flex-col">
          <header className="sticky top-0 z-30 flex h-14 items-center gap-2 border-b border-neutral-200 bg-white/90 px-4 backdrop-blur sm:px-6">
            <button className="-ml-1 rounded-md p-1.5 hover:bg-neutral-100 lg:hidden" onClick={() => setOpen(!open)} aria-label="Menu">
              {open ? <X className="size-5" /> : <Menu className="size-5" />}
            </button>
            <Breadcrumbs crumbs={crumbs} />
          </header>
          <main className="mx-auto w-full max-w-7xl flex-1 px-4 py-6 pb-24 sm:px-8 sm:py-8 lg:pb-8">
            <Outlet />
          </main>
        </div>
      </div>
      <nav
        aria-label="Main"
        className="fixed inset-x-0 bottom-0 z-30 grid grid-cols-5 border-t border-neutral-200 bg-white/95 pb-[env(safe-area-inset-bottom)] backdrop-blur lg:hidden"
      >
        {tabBar.map(({ to, label, icon: Icon, end }) => (
          <NavLink
            key={to}
            to={to}
            end={end}
            className={({ isActive }) => cx("flex min-h-14 flex-col items-center justify-center gap-0.5 text-[11px]", isActive ? "font-medium text-primary" : "text-neutral-500")}
          >
            <Icon className="size-5" />
            {label}
          </NavLink>
        ))}
        <button onClick={() => setOpen(true)} className="flex min-h-14 flex-col items-center justify-center gap-0.5 text-[11px] text-neutral-500">
          <Menu className="size-5" />
          More
        </button>
      </nav>
      {pw && <ChangePassword onClose={() => setPw(false)} />}
    </CrumbContext.Provider>
  );
}

function Breadcrumbs({ crumbs }: { crumbs: Crumb[] }) {
  return (
    <ol className="flex min-w-0 items-center gap-1.5 text-sm">
      {crumbs.map((c, i) => (
        <li key={i} className="flex min-w-0 items-center gap-1.5">
          {i > 0 && <ChevronRight className="size-3.5 shrink-0 text-neutral-400" />}
          {c.to && i < crumbs.length - 1 ? (
            <Link to={c.to} className="truncate text-neutral-500 hover:text-neutral-900">
              {c.label}
            </Link>
          ) : (
            <span className={cx("truncate", i === crumbs.length - 1 ? "font-medium" : "text-neutral-500")}>{c.label}</span>
          )}
        </li>
      ))}
    </ol>
  );
}

export function Section({ title, children, action }: { title: string; children: ReactNode; action?: ReactNode }) {
  return (
    <section className="mb-8">
      <div className="mb-3 flex items-center justify-between">
        <h2 className="text-base font-semibold">{title}</h2>
        {action}
      </div>
      {children}
    </section>
  );
}

function ChangePassword({ onClose }: { onClose: () => void }) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  return (
    <Modal title="Change password" onClose={onClose}>
      <form
        className="space-y-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError(null);
          try {
            await api.changePassword(current, next);
            onClose();
          } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
            setBusy(false);
          }
        }}
      >
        <Field label="Current password">
          <input className={inputClass} type="password" autoFocus autoComplete="current-password" value={current} onChange={(e) => setCurrent(e.target.value)} />
        </Field>
        <Field label="New password" hint="At least 10 characters. Other devices are signed out.">
          <input className={inputClass} type="password" autoComplete="new-password" value={next} onChange={(e) => setNext(e.target.value)} />
        </Field>
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex justify-end gap-2">
          <Button type="button" onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!current || !next}>Change password</Button>
        </div>
      </form>
    </Modal>
  );
}
