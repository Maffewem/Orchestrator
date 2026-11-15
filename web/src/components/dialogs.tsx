import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { AlertTriangle, CircleAlert, CircleCheck, X } from "lucide-react";
import { Button } from "./ui";

type ConfirmOptions = {
  title: string;
  message?: string;
  confirmLabel?: string;
  cancelLabel?: string;
  danger?: boolean;
  /** A checkbox under the message; its state is returned by confirmWithOption. */
  option?: { label: string; checked: boolean };
};
type Pending = ConfirmOptions & { resolve: (ok: boolean, checked: boolean) => void };
type Toast = { id: number; message: string; tone: "error" | "info" | "success" };

// Module-level store so call sites can `await confirmDialog(...)` from any handler, no hook needed.
let pending: Pending[] = [];
let toasts: Toast[] = [];
let nextId = 1;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((l) => l());
const subscribe = (l: () => void) => (listeners.add(l), () => void listeners.delete(l));

/** In-page replacement for window.confirm; resolves true on Confirm, false on Cancel/Esc/backdrop. */
export function confirmDialog(opts: ConfirmOptions): Promise<boolean> {
  return new Promise((resolve) => {
    pending = [...pending, { ...opts, resolve: (ok) => resolve(ok) }];
    emit();
  });
}

/** confirmDialog with a checkbox; resolves null on Cancel, otherwise the checkbox state. */
export function confirmWithOption(opts: ConfirmOptions): Promise<boolean | null> {
  return new Promise((resolve) => {
    pending = [...pending, { ...opts, resolve: (ok, checked) => resolve(ok ? checked : null) }];
    emit();
  });
}

/** In-page replacement for window.alert. */
export function notify(message: string, tone: Toast["tone"] = "error") {
  const id = nextId++;
  toasts = [...toasts, { id, message, tone }];
  emit();
  setTimeout(() => dismiss(id), tone === "error" ? 8000 : 5000);
}

function dismiss(id: number) {
  toasts = toasts.filter((t) => t.id !== id);
  emit();
}

let optionChecked = false;
function settle(ok: boolean) {
  const [first, ...rest] = pending;
  pending = rest;
  first?.resolve(ok, optionChecked);
  emit();
}

function ConfirmModal({ p }: { p: Pending }) {
  const footerRef = useRef<HTMLDivElement>(null);
  const [checked, setChecked] = useState(p.option?.checked ?? false);
  optionChecked = checked;
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    // Destructive actions focus Cancel so a stray Enter doesn't delete anything.
    const [cancel, confirm] = Array.from(footerRef.current?.querySelectorAll("button") ?? []);
    (p.danger ? cancel : confirm)?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") return settle(false);
      if (e.key === "Tab") {
        // Keep focus inside the two buttons.
        e.preventDefault();
        const next = document.activeElement === cancel ? confirm : cancel;
        next?.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      previous?.focus?.();
    };
  }, [p]);
  return (
    <div className="fixed inset-0 z-[60] flex items-center justify-center bg-black/30 p-4" onMouseDown={() => settle(false)}>
      <div
        role="alertdialog"
        aria-modal="true"
        aria-labelledby="confirm-title"
        className="w-full max-w-md rounded-xl bg-white shadow-xl"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="flex gap-3 p-5">
          {p.danger && (
            <div className="flex size-9 shrink-0 items-center justify-center rounded-full bg-red-50 text-bad">
              <AlertTriangle className="size-5" />
            </div>
          )}
          <div className="min-w-0">
            <h2 id="confirm-title" className="font-semibold">{p.title}</h2>
            {p.message && <p className="mt-1 whitespace-pre-line [overflow-wrap:anywhere] text-sm text-neutral-600">{p.message}</p>}
            {p.option && (
              <label className="mt-3 flex items-start gap-2 text-sm [overflow-wrap:anywhere]">
                <input type="checkbox" className="mt-0.5 size-4 shrink-0 accent-primary" checked={checked} onChange={(e) => setChecked(e.target.checked)} />
                {p.option.label}
              </label>
            )}
          </div>
        </div>
        <div ref={footerRef} className="flex justify-end gap-2 border-t border-neutral-200 px-5 py-3">
          <Button onClick={() => settle(false)}>{p.cancelLabel ?? "Cancel"}</Button>
          <Button
            variant={p.danger ? "danger" : "primary"}
            className={p.danger ? "!bg-bad !text-white hover:!bg-red-700" : undefined}
            onClick={() => settle(true)}
          >
            {p.confirmLabel ?? "Confirm"}
          </Button>
        </div>
      </div>
    </div>
  );
}

/** Mount once at the app root. */
export function DialogHost() {
  const snapshot = useSyncExternalStore(subscribe, () => pending);
  const toastSnapshot = useSyncExternalStore(subscribe, () => toasts);
  return (
    <>
      {snapshot[0] && <ConfirmModal key={snapshot.length + snapshot[0].title} p={snapshot[0]} />}
      {/* Sits above the mobile tab bar; full width minus the gutter on phones. */}
      <div className="fixed inset-x-4 bottom-[calc(4.5rem+env(safe-area-inset-bottom))] z-[70] flex flex-col gap-2 sm:inset-x-auto sm:right-4 sm:w-80 lg:bottom-4">
        {toastSnapshot.map((t) => (
          <div
            key={t.id}
            role={t.tone === "error" ? "alert" : "status"}
            className="flex items-start gap-2 rounded-lg border border-neutral-200 bg-white p-3 text-sm shadow-lg"
          >
            {t.tone === "success" ? (
              <CircleCheck className="mt-0.5 size-4 shrink-0 text-ok" />
            ) : (
              <CircleAlert className={t.tone === "error" ? "mt-0.5 size-4 shrink-0 text-bad" : "mt-0.5 size-4 shrink-0 text-primary"} />
            )}
            <span className="min-w-0 flex-1 break-words">{t.message}</span>
            <button onClick={() => dismiss(t.id)} className="-m-1.5 rounded-md p-1.5 text-neutral-500 hover:text-neutral-800" aria-label="Dismiss">
              <X className="size-4" />
            </button>
          </div>
        ))}
      </div>
    </>
  );
}
