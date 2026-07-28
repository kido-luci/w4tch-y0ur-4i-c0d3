// The app's one polite live region.
//
// This is a single-page app: a "navigation" swaps innerHTML, and the browser
// announces nothing for that. A screen-reader user who activates a tab, or
// whose fetch fails into an empty-state div, gets silence — the screen changed
// and nothing said so. A live region is the standard substitute.
//
// It lives in its own module rather than main.ts because every view needs to
// call it and main.ts imports every view: importing back the other way would
// make that cycle real.

let region: HTMLElement | null = null;
let last = "";

/** The region is created on first use and reused. Kept out of the app's
    innerHTML so a re-render can't destroy it mid-announcement. */
function el(): HTMLElement {
  if (region?.isConnected) return region;
  region = document.getElementById("live-region");
  if (!region) {
    region = document.createElement("div");
    region.id = "live-region";
    region.className = "sr-only";
    region.setAttribute("role", "status");
    region.setAttribute("aria-live", "polite");
    document.body.append(region);
  }
  return region;
}

/** Speak `message` politely — after the current utterance, never interrupting.
    Use for view changes and load failures; anything a sighted user learns from
    the screen simply changing. */
export function announce(message: string): void {
  if (!message) return;
  const node = el();
  // Writing the same string twice is a no-op for most screen readers, so a
  // repeat (two failed loads in a row) would go unspoken. Clearing first makes
  // the second write a change again.
  if (message === last) node.textContent = "";
  last = message;
  node.textContent = message;
}
