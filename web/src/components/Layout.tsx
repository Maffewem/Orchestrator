import { Fragment, createContext, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router";
import { Activity, Archive, Boxes, RefreshCw, ChevronRight, LayoutDashboard, LogOut, Menu, Monitor, Moon, PanelLeft, Sun, UserRound, Rocket, Search, Server, Settings, Users, WifiOff, X } from "lucide-react";
import { api, type Device, type Me, type FleetContainer, type Project } from "../api";
import { cx, usePoll, useStaleData } from "../lib";
import { useMe } from "./auth";
import { getTheme, setTheme, type ThemeChoice } from "../theme";

export type Crumb = { label: string; to?: string };

const CrumbContext = createContext<(c: Crumb[]) => void>(() => {});

/** Sets the top-bar breadcrumb for the current page. */
export function useCrumbs(crumbs: Crumb[]) {
  const set = useContext(CrumbContext);
  const key = JSON.stringify(crumbs);
  useEffect(() => {
    set(crumbs);
    // The browser tab names the page, most specific part first: "my-app · Projects · Orchestrator".
    document.title = [...crumbs.map((c) => c.label)].reverse().concat("Orchestrator").join(" · ");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, set]);
}

type NavItem = { to: string; label: string; icon: typeof Server; end?: boolean; admin?: boolean };

// Grouped from most-used to least-used: what is running, where it runs, then administration.
export const settingsSections = [
  ["appearance", "Appearance"],
  ["security", "Sign-in security"],
  ["https", "Dashboard HTTPS"],
  ["sso", "Single sign-on"],
  ["github", "GitHub"],
  ["cloudflare", "Cloudflare"],
  ["ssl", "SSL"],
  ["domains", "Domains"],
  ["backups", "Backup password"],
  ["serverbackup", "Server backup"],
  ["mainserver", "Move the server"],
  ["join", "Join token"],
  ["agents", "Server connectivity"],
] as const;

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

export function Logo({ iconOnly }: { iconOnly?: boolean }) {
  return (
    <span className="flex items-center gap-2 font-semibold tracking-tight">
      <svg viewBox="0 0 32 32" className="size-7" aria-hidden>
        <rect width="32" height="32" rx="8" fill="var(--color-brand)" />
        <circle cx="16" cy="16" r="5" fill="white" />
        <circle cx="16" cy="6" r="2.5" fill="white" />
        <circle cx="24.7" cy="21" r="2.5" fill="white" />
        <circle cx="7.3" cy="21" r="2.5" fill="white" />
      </svg>
      {!iconOnly && "Orchestrator"}
    </span>
  );
}

/** Tells an admin when this server has just taken over, or has handed over, so it is never a surprise. */
function MoveBanner() {
  const { data } = usePoll(() => api.serverMove(), 15000);
  const loc = useLocation();
  if (!data || loc.pathname === "/settings") return null;
  const msg = data.standby_to
    ? `This server has handed over to ${data.standby_to}. Dashboard changes are paused here.`
    : data.moved_from
      ? `This server just took over from ${data.moved_from.old_url}. Check that everything works, then confirm the move.`
      : data.move && ["preparing", "installing", "verifying", "ready", "switched"].includes(data.move.phase)
        ? "A server move is in progress. Dashboard changes are paused until it is finished or cancelled."
        : "";
  if (!msg) return null;
  return (
    <div className="mb-6 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 text-sm">
      <span>{msg}</span>
      <Link to="/settings#mainserver" className="font-medium underline">Open Move the server</Link>
    </div>
  );
}

export default function Layout({ onLogout }: { onLogout: () => void }) {
  const [crumbs, setCrumbs] = useState<Crumb[]>([]);
  const [open, setOpen] = useState(false);
  const [palette, setPalette] = useState(false);
  const [rail, setRail] = useState(() => {
    try {
      return localStorage.getItem("orchestrator.sidebarRail") === "1";
    } catch {
      return false;
    }
  });
  const toggleRail = () =>
    setRail((r) => {
      try {
        localStorage.setItem("orchestrator.sidebarRail", r ? "0" : "1");
      } catch {
        /* keep the choice for this visit only */
      }
      return !r;
    });
  useEffect(() => {
    const key = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((p) => !p);
      }
    };
    window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  }, []);
  const me = useMe();
  const loc = useLocation();
  useEffect(() => setOpen(false), [loc.pathname, loc.hash]);

  const sidebar = (compact: boolean) => (
    <nav className="flex h-full flex-col gap-0.5 overflow-x-hidden p-3">
      <div className="mb-4 px-2 py-1.5">
        <Link to="/" title="Orchestrator">
          {compact ? <Logo iconOnly /> : <Logo />}
        </Link>
      </div>
      {navGroups.map((g) => {
        const items = g.items.filter((n) => !n.admin || me?.role === "admin");
        if (!items.length) return null;
        return (
          <div key={g.title ?? "top"} className="mb-3">
            {g.title && !compact && <p className="px-2.5 pb-1 pt-2 text-xs font-medium uppercase tracking-wide text-neutral-400">{g.title}</p>}
            {items.map(({ to, label, icon: Icon, end }) => (
              <Fragment key={to}>
              <NavLink
                to={to}
                end={end}
                title={compact ? label : undefined}
                className={({ isActive }) =>
                  cx(
                    "flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm transition-colors",
                    compact && "justify-center",
                    isActive ? "bg-neutral-200/70 font-medium text-neutral-900" : "text-neutral-600 hover:bg-neutral-100 hover:text-neutral-900",
                  )
                }
              >
                <Icon className="size-4 shrink-0" />
                {!compact && label}
              </NavLink>
              {!compact && to === "/settings" && loc.pathname === "/settings" && (
                <div key="settings-sections" className="mb-1 ml-5 mt-0.5 space-y-px border-l border-neutral-200 pl-2">
                  {settingsSections.map(([id, name]) => (
                    <Link
                      key={id}
                      to={`/settings#${id}`}
                      className={cx(
                        "block rounded-md px-2.5 py-1.5 text-[13px]",
                        loc.hash === `#${id}` ? "bg-neutral-200/70 font-medium text-neutral-900" : "text-neutral-500 hover:bg-neutral-100 hover:text-neutral-900",
                      )}
                    >
                      {name}
                    </Link>
                  ))}
                </div>
              )}
              </Fragment>
            ))}
          </div>
        );
      })}
    </nav>
  );

  return (
    <CrumbContext.Provider value={setCrumbs}>
      <div className="flex min-h-full w-full max-w-full overflow-x-clip">
        <aside className={cx("sticky top-0 hidden h-screen shrink-0 border-r border-neutral-200 bg-neutral-50 transition-[width] lg:block", rail ? "w-16" : "w-60")}>{sidebar(rail)}</aside>
        {open && (
          <div className="fixed inset-0 z-40 bg-black/30 lg:hidden" onClick={() => setOpen(false)}>
            <aside className="relative h-full w-64 overflow-y-auto bg-neutral-50" onClick={(e) => e.stopPropagation()}>
              <button className="absolute right-2 top-2 rounded-md p-2 text-neutral-500 hover:bg-neutral-100" onClick={() => setOpen(false)} aria-label="Close menu">
                <X className="size-5" />
              </button>
              {sidebar(false)}
            </aside>
          </div>
        )}
        <div className="flex min-w-0 flex-1 flex-col">
          <header className="sticky top-0 z-30 flex h-14 items-center gap-2 border-b border-neutral-200 bg-white/90 px-4 backdrop-blur sm:px-6">
            <button
              className="-ml-1 hidden rounded-md p-1.5 text-neutral-600 hover:bg-neutral-100 lg:block"
              onClick={toggleRail}
              aria-label={rail ? "Expand sidebar" : "Collapse sidebar"}
              title={rail ? "Expand sidebar" : "Collapse sidebar"}
            >
              <PanelLeft className="size-5" />
            </button>
            <Breadcrumbs crumbs={crumbs} />
            <button
              onClick={() => setPalette(true)}
              className="ml-auto inline-flex h-9 items-center gap-2 rounded-lg border border-neutral-200 bg-neutral-50 px-3 text-sm text-neutral-500 hover:bg-neutral-100 sm:w-64 sm:justify-between"
              aria-label="Search"
            >
              <span className="inline-flex items-center gap-2">
                <Search className="size-4" />
                <span className="hidden sm:inline">Search</span>
              </span>
              <kbd className="hidden rounded border border-neutral-300 px-1.5 text-xs sm:inline">Ctrl K</kbd>
            </button>
            {me && <UserMenu me={me} onLogout={onLogout} />}
          </header>
          <main className="mx-auto w-full min-w-0 max-w-7xl flex-1 px-4 py-6 pb-24 sm:px-8 sm:py-8 lg:pb-8">
            <StaleBanner />
            {me?.role === "admin" && <MoveBanner />}
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
      {palette && <CommandPalette onClose={() => setPalette(false)} admin={me?.role === "admin"} />}
    </CrumbContext.Provider>
  );
}

/** One avatar button in the top bar: who is signed in, a theme switch, the account page and Sign out. */
function UserMenu({ me, onLogout }: { me: Me; onLogout: () => void }) {
  const [open, setOpen] = useState(false);
  const [theme, setThemeState] = useState<ThemeChoice>(getTheme());
  const wrap = useRef<HTMLDivElement>(null);
  const btn = useRef<HTMLButtonElement>(null);
  const nav = useNavigate();
  const itemSelector = '[role="menuitem"], [role="menuitemradio"]';
  useEffect(() => {
    if (!open) return;
    wrap.current?.querySelector<HTMLElement>('[role="menuitemradio"][aria-checked="true"]')?.focus();
    const away = (e: MouseEvent) => {
      if (!wrap.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", away);
    return () => document.removeEventListener("mousedown", away);
  }, [open]);
  const onKey = (e: React.KeyboardEvent) => {
    const els = Array.from(wrap.current?.querySelectorAll<HTMLElement>(itemSelector) ?? []);
    const i = els.indexOf(document.activeElement as HTMLElement);
    if (e.key === "Escape") {
      setOpen(false);
      btn.current?.focus();
    } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      if (!els.length) return;
      const n = e.key === "ArrowDown" ? (i + 1) % els.length : (i - 1 + els.length) % els.length;
      els[n]?.focus();
    } else if (e.key === "Home" || e.key === "End") {
      e.preventDefault();
      els[e.key === "Home" ? 0 : els.length - 1]?.focus();
    } else if (e.key === "Tab") setOpen(false);
  };
  const row = "flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-left text-sm text-neutral-700 hover:bg-neutral-100 focus:bg-neutral-100 focus:outline-none";
  return (
    <div ref={wrap} className="relative" onKeyDown={onKey}>
      <button
        ref={btn}
        onClick={() => setOpen((o) => !o)}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`Account menu for ${me.username}`}
        title={me.username}
        className="inline-flex size-9 items-center justify-center rounded-full bg-primary text-sm font-semibold uppercase text-white hover:opacity-90 focus-visible:outline-2 focus-visible:outline-offset-2"
      >
        {me.username.charAt(0) || "?"}
      </button>
      {open && (
        <div role="menu" aria-label="Account" className="absolute right-0 top-11 z-50 w-60 overflow-hidden rounded-xl border border-neutral-200 bg-white p-1.5 shadow-xl">
          <div className="border-b border-neutral-100 px-2.5 pb-2 pt-1.5">
            <p className="truncate text-sm font-medium">{me.username}</p>
            <p className="text-xs capitalize text-neutral-500">{me.role}</p>
          </div>
          <div className="border-b border-neutral-100 px-1 py-2">
            <p className="px-1.5 pb-1 text-xs font-medium uppercase tracking-wide text-neutral-400">Theme</p>
            <div role="group" aria-label="Theme" className="flex gap-0.5 rounded-lg bg-neutral-100 p-0.5">
              {(
                [
                  ["light", "Light", Sun],
                  ["auto", "Auto", Monitor],
                  ["dark", "Dark", Moon],
                ] as const
              ).map(([v, label, Icon]) => (
                <button
                  key={v}
                  role="menuitemradio"
                  aria-checked={theme === v}
                  tabIndex={-1}
                  onClick={() => {
                    setThemeState(v);
                    setTheme(v);
                  }}
                  className={cx(
                    "flex flex-1 items-center justify-center gap-1.5 rounded-md px-2 py-1.5 text-xs focus:outline-none focus-visible:ring-2 focus-visible:ring-primary",
                    theme === v ? "bg-white font-medium text-neutral-900 shadow-sm" : "text-neutral-500 hover:text-neutral-900",
                  )}
                >
                  <Icon className="size-3.5" />
                  {label}
                </button>
              ))}
            </div>
          </div>
          <div className="pt-1">
            <button
              role="menuitem"
              tabIndex={-1}
              className={row}
              onClick={() => {
                setOpen(false);
                nav("/account");
              }}
            >
              <UserRound className="size-4 shrink-0" />
              My account
            </button>
            <button
              role="menuitem"
              tabIndex={-1}
              className={row}
              onClick={() => {
                setOpen(false);
                onLogout();
              }}
            >
              <LogOut className="size-4 shrink-0" />
              Sign out
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

/** Shown when a page that has loaded once can no longer refresh, so stale data is not mistaken for current data. */
function StaleBanner() {
  const stale = useStaleData();
  if (!stale) return null;
  return (
    <div role="status" className="mb-4 flex items-start gap-2 rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-900">
      <WifiOff className="mt-0.5 size-4 shrink-0" />
      <span className="min-w-0">Can't reach the server. What you see may be out of date; retrying automatically.</span>
    </div>
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

type Hit = { label: string; hint: string; to: string };

/** Ctrl+K: jump to any page, worker, device or project. */
function CommandPalette({ onClose, admin }: { onClose: () => void; admin: boolean }) {
  const nav = useNavigate();
  const [q, setQ] = useState("");
  const [i, setI] = useState(0);
  const [data, setData] = useState<{ containers: FleetContainer[]; devices: Device[]; projects: Project[] }>({ containers: [], devices: [], projects: [] });
  useEffect(() => {
    Promise.all([api.containers().catch(() => []), api.devices().catch(() => []), api.projects().catch(() => [])]).then(([containers, devices, projects]) => setData({ containers, devices, projects }));
  }, []);
  const all: Hit[] = [
    ...navGroups.flatMap((g) => g.items).filter((n) => !n.admin || admin).map((n) => ({ label: n.label, hint: "Page", to: n.to })),
    { label: "My account", hint: "Page", to: "/account" },
    ...data.projects.map((p) => ({ label: p.name, hint: "Project", to: `/projects/${p.id}` })),
    ...data.containers.map((c) => ({ label: c.name, hint: `Worker on ${c.device_name}`, to: `/workers/${c.device_id}/${encodeURIComponent(c.name)}` })),
    ...data.devices.map((d) => ({ label: d.name, hint: "Device", to: `/devices/${d.id}` })),
  ];
  const needle = q.trim().toLowerCase();
  const hits = all.filter((h) => !needle || `${h.label} ${h.hint}`.toLowerCase().includes(needle)).slice(0, 10);
  const go = (h?: Hit) => {
    if (!h) return;
    onClose();
    nav(h.to);
  };
  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-black/30 p-4 pt-[12vh]" onMouseDown={onClose}>
      <div role="dialog" aria-label="Search" className="w-full max-w-lg overflow-hidden rounded-xl border border-neutral-200 bg-white shadow-xl" onMouseDown={(e) => e.stopPropagation()}>
        <input
          autoFocus
          value={q}
          onChange={(e) => {
            setQ(e.target.value);
            setI(0);
          }}
          onKeyDown={(e) => {
            if (e.key === "Escape") onClose();
            else if (e.key === "ArrowDown") {
              e.preventDefault();
              setI(Math.min(i + 1, hits.length - 1));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              setI(Math.max(i - 1, 0));
            } else if (e.key === "Enter") go(hits[i]);
          }}
          placeholder="Search pages, projects, workers and devices"
          aria-label="Search"
          className="w-full border-b border-neutral-200 px-4 py-3.5 text-base outline-none"
        />
        <ul role="listbox" className="max-h-80 overflow-y-auto p-1.5">
          {hits.length === 0 && <li className="px-3 py-3 text-sm text-neutral-500">Nothing found.</li>}
          {hits.map((h, k) => (
            <li
              key={h.to + h.label}
              role="option"
              aria-selected={k === i}
              onMouseEnter={() => setI(k)}
              onClick={() => go(h)}
              className={cx("flex cursor-pointer items-center justify-between gap-3 rounded-lg px-3 py-2.5 text-sm", k === i && "bg-blue-50")}
            >
              <span className="truncate font-medium">{h.label}</span>
              <span className="shrink-0 text-xs text-neutral-500">{h.hint}</span>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}
