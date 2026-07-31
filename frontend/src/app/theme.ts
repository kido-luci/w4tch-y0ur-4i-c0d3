// Light/dark theming. The palette lives in style.css keyed off the
// `data-theme` attribute on <html>; this module decides which value to set.
//
// Precedence: an explicit user choice (stored in localStorage) wins; with no
// stored choice we follow the OS `prefers-color-scheme` and keep tracking it
// live until the user picks a theme themselves.

export type Theme = "light" | "dark";

const STORAGE_KEY = "wyac.theme";

function stored(): Theme | null {
  const v = localStorage.getItem(STORAGE_KEY);
  return v === "light" || v === "dark" ? v : null;
}

function systemTheme(): Theme {
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function apply(theme: Theme): void {
  document.documentElement.setAttribute("data-theme", theme);
}

/** Current effective theme (stored choice, else the OS preference). */
export function getTheme(): Theme {
  return stored() ?? systemTheme();
}

/** Apply the effective theme and track OS changes while unset. Call once at boot. */
export function initTheme(): void {
  apply(getTheme());
  window
    .matchMedia("(prefers-color-scheme: dark)")
    .addEventListener("change", (e) => {
      if (stored() === null) apply(e.matches ? "dark" : "light");
    });
}

/** Flip the theme, persist the choice, and apply it. Returns the new theme. */
export function toggleTheme(): Theme {
  const next: Theme = getTheme() === "dark" ? "light" : "dark";
  localStorage.setItem(STORAGE_KEY, next);
  apply(next);
  return next;
}

/** Wire a button to reflect and toggle the theme. The editorial glyph pair:
 *  ◐ while light, ◑ while dark — monochrome, per the mock's chrome. */
export function mountThemeToggle(btn: HTMLElement): void {
  const sync = (): void => {
    const dark = getTheme() === "dark";
    btn.textContent = dark ? "◑" : "◐";
    btn.title = dark ? "switch to light" : "switch to dark";
  };
  sync();
  btn.addEventListener("click", () => {
    toggleTheme();
    sync();
  });
}
