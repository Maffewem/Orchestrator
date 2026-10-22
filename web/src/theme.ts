// Light, dark or follow the device. The choice is kept per browser; a new install stays light.
export type ThemeChoice = "light" | "dark" | "auto";

const KEY = "orchestrator.theme";
const query = () => window.matchMedia("(prefers-color-scheme: dark)");

export function getTheme(): ThemeChoice {
  try {
    const v = localStorage.getItem(KEY);
    if (v === "light" || v === "dark" || v === "auto") return v;
  } catch {
    /* storage blocked: fall through to the default */
  }
  return "light";
}

function apply(choice: ThemeChoice) {
  const dark = choice === "dark" || (choice === "auto" && query().matches);
  document.documentElement.dataset.theme = dark ? "dark" : "light";
}

export function setTheme(choice: ThemeChoice) {
  try {
    localStorage.setItem(KEY, choice);
  } catch {
    /* keep the choice for this visit only */
  }
  apply(choice);
}

/** Call once before the first render so the page does not flash the wrong theme. */
export function initTheme() {
  apply(getTheme());
  query().addEventListener("change", () => {
    if (getTheme() === "auto") apply("auto");
  });
}
