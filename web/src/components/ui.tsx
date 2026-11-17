import { useEffect, useId, useRef, useState, type ButtonHTMLAttributes, type InputHTMLAttributes, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from "react";
import { copyText } from "../clipboard";
import { Link, NavLink } from "react-router";
import { AlertTriangle, Check, CircleDashed, Eye, EyeOff, Loader2, Minus, RefreshCw, X } from "lucide-react";
import type { DeploymentStatus, Health, StepStatus } from "../api";
import { cx } from "../lib";

type Variant = "primary" | "secondary" | "danger" | "ghost";

export function Button({
  variant = "secondary",
  size = "md",
  icon,
  loading,
  className,
  children,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: Variant;
  size?: "sm" | "md";
  icon?: ReactNode;
  loading?: boolean;
}) {
  return (
    <button
      {...props}
      disabled={props.disabled || loading}
      className={cx(
        "inline-flex items-center justify-center gap-2 rounded-lg font-medium whitespace-nowrap transition-colors disabled:cursor-not-allowed disabled:opacity-50",
        size === "md" ? "h-11 px-3.5 text-sm sm:h-9" : "h-11 min-w-11 px-3 text-sm sm:h-7 sm:min-w-0 sm:px-2.5 sm:text-xs",
        variant === "primary" && "bg-primary text-white hover:bg-primary-hover",
        variant === "secondary" && "border border-neutral-300 bg-white text-neutral-800 hover:bg-neutral-100",
        variant === "danger" && "border border-red-200 bg-white text-bad hover:bg-red-50",
        variant === "ghost" && "text-neutral-600 hover:bg-neutral-100 hover:text-neutral-900",
        className,
      )}
    >
      {loading ? <Loader2 className="size-4 animate-spin" /> : icon}
      {children}
    </button>
  );
}

export function Card({ className, children }: { className?: string; children: ReactNode }) {
  return <div className={cx("rounded-xl border border-neutral-200 bg-white", className)}>{children}</div>;
}

export function CardHeader({ title, action, subtitle }: { title: ReactNode; subtitle?: ReactNode; action?: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4 border-b border-neutral-200 px-5 py-3.5">
      <div>
        <h3 className="text-sm font-semibold">{title}</h3>
        {subtitle && <p className="mt-0.5 text-xs text-neutral-500">{subtitle}</p>}
      </div>
      {action}
    </div>
  );
}

export function PageHeader({
  title,
  description,
  actions,
  back,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  back?: { to: string; label: string };
}) {
  return (
    <div className="mb-6">
      {back && (
        <Link to={back.to} className="mb-3 inline-flex items-center gap-1.5 text-sm text-neutral-500 hover:text-neutral-900">
          ← {back.label}
        </Link>
      )}
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
          {description && <p className="mt-1 text-sm text-neutral-500">{description}</p>}
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
    </div>
  );
}

/** Cloudflare-style segmented tab bar. */
export function Tabs({ items }: { items: { to: string; label: string; end?: boolean }[] }) {
  return (
    <nav className="inline-flex max-w-full gap-1 overflow-x-auto rounded-xl border border-neutral-200 bg-neutral-100 p-1">
      {items.map((t) => (
        <NavLink
          key={t.to}
          to={t.to}
          end={t.end}
          className={({ isActive }) =>
            cx(
              "rounded-lg px-3.5 py-2.5 text-sm whitespace-nowrap transition-colors sm:py-1.5",
              isActive
                ? "border border-neutral-200 bg-white font-medium text-neutral-900 shadow-sm"
                : "border border-transparent text-neutral-600 hover:text-neutral-900",
            )
          }
        >
          {t.label}
        </NavLink>
      ))}
    </nav>
  );
}

export function Badge({ tone = "neutral", children }: { tone?: "neutral" | "ok" | "warn" | "bad" | "info"; children: ReactNode }) {
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-xs font-medium",
        tone === "neutral" && "bg-neutral-100 text-neutral-700",
        tone === "ok" && "bg-green-50 text-ok",
        tone === "warn" && "bg-amber-50 text-warn",
        tone === "bad" && "bg-red-50 text-bad",
        tone === "info" && "bg-blue-50 text-primary",
      )}
    >
      {children}
    </span>
  );
}

export function StatusDot({ status }: { status: "online" | "offline" | Health | string }) {
  const color =
    status === "online" || status === "ok" || status === "running"
      ? "bg-ok"
      : status === "warning" || status === "restarting" || status === "paused"
        ? "bg-warn"
        : status === "offline" || status === "critical" || status === "dead"
          ? "bg-bad"
          : "bg-neutral-400";
  return <span className={cx("inline-block size-2 shrink-0 rounded-full", color)} />;
}

/** Round status icon used for deployments and steps, like Cloudflare's build badges. */
export function StatusIcon({ status, size = "md" }: { status: DeploymentStatus | StepStatus; size?: "sm" | "md" | "lg" }) {
  const dim = size === "lg" ? "size-7" : size === "md" ? "size-5" : "size-4";
  const inner = size === "lg" ? "size-4" : "size-3";
  switch (status) {
    case "succeeded":
      return (
        <span className={cx(dim, "inline-flex shrink-0 items-center justify-center rounded-full bg-ok text-white")}>
          <Check className={inner} strokeWidth={3} />
        </span>
      );
    case "failed":
      return (
        <span className={cx(dim, "inline-flex shrink-0 items-center justify-center rounded-full bg-bad text-white")}>
          <X className={inner} strokeWidth={3} />
        </span>
      );
    case "running":
      return <Loader2 className={cx(dim, "shrink-0 animate-spin text-primary")} />;
    case "skipped":
      return (
        <span className={cx(dim, "inline-flex shrink-0 items-center justify-center rounded-full bg-neutral-200 text-neutral-500")}>
          <Minus className={inner} strokeWidth={3} />
        </span>
      );
    default:
      return <CircleDashed className={cx(dim, "shrink-0 text-neutral-400")} />;
  }
}

export function deploymentLabel(s: DeploymentStatus) {
  return { queued: "Queued", running: "In progress", succeeded: "Success", failed: "Failed" }[s];
}

export function HealthBadge({ health, status }: { health: Health; status: string }) {
  if (status === "offline") return <Badge tone="bad">Offline</Badge>;
  if (health === "critical") return <Badge tone="bad">Critical</Badge>;
  if (health === "warning") return <Badge tone="warn">Warning</Badge>;
  return <Badge tone="ok">Healthy</Badge>;
}

export function Meter({ value, label }: { value: number; label?: string }) {
  const tone = value >= 90 ? "bg-bad" : value >= 75 ? "bg-warn" : "bg-primary";
  return (
    <div className="min-w-0">
      {label && (
        <div className="mb-1 flex justify-between text-xs text-neutral-500">
          <span>{label}</span>
          <span className="tabular-nums">{value.toFixed(0)}%</span>
        </div>
      )}
      <div className="h-1.5 overflow-hidden rounded-full bg-neutral-200">
        <div className={cx("h-full rounded-full transition-all", tone)} style={{ width: `${Math.min(100, value)}%` }} />
      </div>
    </div>
  );
}

export function Empty({ icon, title, children }: { icon?: ReactNode; title: string; children?: ReactNode }) {
  return (
    <div className="flex flex-col items-center justify-center px-6 py-14 text-center">
      {icon && <div className="mb-3 text-neutral-400">{icon}</div>}
      <p className="font-medium">{title}</p>
      {children && <div className="mt-1 max-w-md text-sm text-neutral-500">{children}</div>}
    </div>
  );
}

export function ErrorNote({ children }: { children: ReactNode }) {
  return (
    <div className="flex items-start gap-2 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-bad">
      <AlertTriangle className="mt-0.5 size-4 shrink-0" />
      <div>{children}</div>
    </div>
  );
}

/** Placeholder block shown while data loads, so the page does not jump when it arrives. */
export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden className={cx("animate-pulse rounded-md bg-neutral-200/70 motion-reduce:animate-none", className)} />;
}

/** An icon-only refresh button that spins while it fetches, like the one on Cloudflare's metrics page. */
export function RefreshButton({ onClick, busy, label = "Refresh" }: { onClick: () => void; busy: boolean; label?: string }) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={busy}
      aria-label={label}
      title={label}
      className="inline-flex size-9 items-center justify-center rounded-lg border border-neutral-300 bg-white text-neutral-600 hover:bg-neutral-50 focus-visible:outline-2 focus-visible:outline-primary disabled:opacity-70"
    >
      <RefreshCw className={cx("size-4", busy && "animate-spin motion-reduce:animate-none")} />
    </button>
  );
}

export function Loading() {
  return (
    <div role="status" aria-live="polite" className="space-y-4 py-2">
      <span className="sr-only">Loading</span>
      <Skeleton className="h-8 w-48" />
      <div className="rounded-xl border border-neutral-200 bg-white">
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="flex items-center gap-3 border-b border-neutral-100 px-5 py-4 last:border-0">
            <Skeleton className="size-8 rounded-full" />
            <div className="flex-1 space-y-2">
              <Skeleton className="h-3.5 w-1/3" />
              <Skeleton className="h-3 w-1/2" />
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

/** Makes a clickable table row reachable and operable from the keyboard (Enter or Space). */
export function rowProps(onActivate: () => void) {
  return {
    tabIndex: 0,
    onClick: onActivate,
    onKeyDown: (e: ReactKeyboardEvent) => {
      if (e.target !== e.currentTarget) return;
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        onActivate();
      }
    },
  };
}

export function Modal({ title, onClose, children, wide }: { title: string; onClose: () => void; children: ReactNode; wide?: boolean }) {
  const panel = useRef<HTMLDivElement>(null);
  const titleId = useId();
  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    const focusable = () =>
      Array.from(panel.current?.querySelectorAll<HTMLElement>('a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])') ?? []);
    // Move focus into the dialog unless something inside already took it (an autofocus field).
    if (!panel.current?.contains(document.activeElement)) (focusable()[1] ?? focusable()[0] ?? panel.current)?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") return closeRef.current();
      if (e.key !== "Tab") return;
      const items = focusable();
      if (!items.length) return;
      const first = items[0];
      const last = items[items.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      previous?.focus?.();
    };
  }, []);
  return (
    <div className="fixed inset-0 z-50 flex items-end justify-center overflow-y-auto bg-black/30 p-0 sm:items-start sm:p-10" onMouseDown={onClose}>
      <div
        ref={panel}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className={cx("w-full rounded-t-xl bg-white shadow-xl outline-none sm:rounded-xl", wide ? "max-w-3xl" : "max-w-lg")}
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between border-b border-neutral-200 px-5 py-3.5">
          <h2 id={titleId} className="font-semibold">{title}</h2>
          <button onClick={onClose} className="rounded-md p-1 text-neutral-500 hover:bg-neutral-100" aria-label="Close">
            <X className="size-4" />
          </button>
        </div>
        <div className="p-5">{children}</div>
      </div>
    </div>
  );
}

/** `as="div"` for fields holding buttons: inside a <label>, a click ending on the label (e.g. after resizing a textarea) would press the first button. */
export function Field({ label, hint, children, as = "label" }: { label: string; hint?: ReactNode; children: ReactNode; as?: "label" | "div" }) {
  const Tag = as;
  return (
    <Tag className="block">
      <span className="mb-1.5 block text-sm font-medium">{label}</span>
      {children}
      {hint && <span className="mt-1 block text-xs text-neutral-500">{hint}</span>}
    </Tag>
  );
}

export const inputClass =
  "block w-full rounded-lg border border-neutral-300 bg-white px-3 py-2 text-sm placeholder:text-neutral-400 focus:border-primary focus:ring-2 focus:ring-primary/20 focus:outline-none";

/** A secret input that is hidden by default, with an eye button to reveal what was typed. */
export function SecretInput({ className = inputClass, ...props }: Omit<InputHTMLAttributes<HTMLInputElement>, "type">) {
  const [shown, setShown] = useState(false);
  return (
    <div className="relative min-w-0 w-full flex-1">
      <input {...props} type={shown ? "text" : "password"} className={cx(className, "pr-10")} spellCheck={false} autoCapitalize="off" />
      <button
        type="button"
        onClick={() => setShown((v) => !v)}
        aria-label={shown ? "Hide value" : "Show value"}
        aria-pressed={shown}
        title={shown ? "Hide" : "Show"}
        className="absolute inset-y-0 right-0 flex w-10 items-center justify-center rounded-r-lg text-neutral-500 hover:text-neutral-800 focus-visible:outline-2 focus-visible:outline-primary"
      >
        {shown ? <EyeOff className="size-4" aria-hidden /> : <Eye className="size-4" aria-hidden />}
      </button>
    </div>
  );
}

export function CopyButton({ text, label = "Copy" }: { text: string; label?: string }) {
  const [done, setDone] = useState(false);
  return (
    <Button
      size="sm"
      onClick={async () => {
        if (await copyText(text)) {
          setDone(true);
          setTimeout(() => setDone(false), 1500);
        }
      }}
      icon={done ? <Check className="size-3.5" /> : undefined}
    >
      {done ? "Copied" : label}
    </Button>
  );
}

export function CodeBlock({ code }: { code: string }) {
  return (
    <div className="relative">
      <pre className="code-surface rounded-lg p-4 pr-20 font-mono text-xs leading-relaxed break-all whitespace-pre-wrap">{code}</pre>
      <div className="absolute top-2 right-2">
        <CopyButton text={code} />
      </div>
    </div>
  );
}

/** Small dependency-free line chart for metric history. */
export function LineChart({
  series,
  height = 120,
  max = 100,
  unit = "%",
}: {
  series: { label: string; color: string; values: number[] }[];
  height?: number;
  max?: number;
  unit?: string;
}) {
  const w = 600;
  const n = Math.max(2, ...series.map((s) => s.values.length));
  const x = (i: number) => (i / (n - 1)) * w;
  const y = (v: number) => height - (Math.min(v, max) / max) * (height - 4) - 2;
  return (
    <div>
      <svg viewBox={`0 0 ${w} ${height}`} preserveAspectRatio="none" className="h-28 w-full" role="img">
        {[0.25, 0.5, 0.75].map((f) => (
          <line key={f} x1={0} x2={w} y1={height * f} y2={height * f} stroke="#e5e5e5" strokeDasharray="4 4" vectorEffect="non-scaling-stroke" />
        ))}
        {series.map((s) => {
          if (s.values.length < 2) return null;
          const offset = n - s.values.length;
          const pts = s.values.map((v, i) => `${x(i + offset)},${y(v)}`).join(" ");
          return (
            <g key={s.label}>
              <polygon points={`${x(offset)},${height} ${pts} ${x(n - 1)},${height}`} fill={s.color} opacity={0.08} />
              <polyline points={pts} fill="none" stroke={s.color} strokeWidth={2} vectorEffect="non-scaling-stroke" strokeLinejoin="round" />
            </g>
          );
        })}
      </svg>
      <div className="mt-2 flex flex-wrap gap-4 text-xs text-neutral-600">
        {series.map((s) => (
          <span key={s.label} className="inline-flex items-center gap-1.5">
            <span className="size-2 rounded-full" style={{ background: s.color }} />
            {s.label}
            <span className="font-medium text-neutral-900 tabular-nums">
              {s.values.length ? `${s.values[s.values.length - 1].toFixed(unit === "%" ? 0 : 2)}${unit}` : "—"}
            </span>
          </span>
        ))}
      </div>
    </div>
  );
}

export function Table({ head, children, compact }: { head: ReactNode[]; children: ReactNode; compact?: boolean }) {
  return (
    <div className="overflow-x-auto">
      <table className={cx("w-full text-left text-sm", compact && "[&_td]:!py-1.5")}>
        <thead>
          <tr className="border-b border-neutral-200 text-xs text-neutral-500">
            {head.map((h, i) => (
              <th key={i} className="px-3 py-2.5 sm:px-5 font-medium whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-neutral-100">{children}</tbody>
      </table>
    </div>
  );
}

export function Td({ children, className }: { children?: ReactNode; className?: string }) {
  return <td className={cx("px-3 py-3 align-middle sm:px-5", className)}>{children}</td>;
}

/** Cloudflare-style on/off switch. */
export function Toggle({ checked, onChange, label }: { checked: boolean; onChange: (v: boolean) => void; label: string }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      onClick={() => onChange(!checked)}
      className={cx("relative h-6 w-11 shrink-0 rounded-full transition-colors", checked ? "bg-primary" : "bg-neutral-300")}
    >
      <span className={cx("absolute top-0.5 size-5 rounded-full bg-white shadow transition-all", checked ? "left-[22px]" : "left-0.5")} />
    </button>
  );
}

/** Glob list: type a path and press Enter or comma; click a chip to remove it. */
export function ChipInput({ value, onChange, placeholder }: { value: string[]; onChange: (v: string[]) => void; placeholder?: string }) {
  const [text, setText] = useState("");
  const add = () => {
    const t = text.trim();
    if (t && !value.includes(t)) onChange([...value, t]);
    setText("");
  };
  return (
    <div className={cx(inputClass, "flex flex-wrap items-center gap-1.5 !py-1.5")}>
      {value.map((v) => (
        <button
          type="button"
          key={v}
          title="Remove"
          onClick={() => onChange(value.filter((x) => x !== v))}
          className="flex items-center gap-1.5 rounded bg-neutral-100 px-2 py-0.5 font-mono text-xs hover:bg-neutral-200"
        >
          {v}
          <span aria-hidden>×</span>
        </button>
      ))}
      <input
        className="min-w-24 flex-1 bg-transparent py-0.5 text-sm outline-none"
        placeholder={value.length ? "" : placeholder}
        value={text}
        onChange={(e) => setText(e.target.value)}
        onBlur={add}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === ",") {
            e.preventDefault();
            add();
          } else if (e.key === "Backspace" && !text && value.length) {
            onChange(value.slice(0, -1));
          }
        }}
      />
    </div>
  );
}
