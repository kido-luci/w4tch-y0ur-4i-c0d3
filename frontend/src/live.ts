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

/** Render a failed read into `host`, announce it, and offer the way out.
 *
 *  Every read-path failure used to render a bare empty-state div: a dead end,
 *  recoverable only by reloading the page or navigating away and back, which
 *  on a local-first viewer is a heavy answer to a request that just lost a
 *  race. The write paths already retry (docs/design autosave); reads did not.
 *
 *  `retry` re-runs whatever loader failed. Omit it only where there is nothing
 *  sensible to re-run. */
export function showError(host: HTMLElement, message: string, retry?: () => void): void {
  const div = document.createElement("div");
  div.className = "empty-state";
  div.textContent = message;
  if (retry) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "retry-btn";
    btn.textContent = "retry";
    btn.addEventListener("click", () => {
      // Say something immediately: the retry may take a moment, and a button
      // that visibly does nothing reads as broken.
      host.textContent = "loading…";
      retry();
    });
    div.append(" ", btn);
  }
  host.replaceChildren(div);
  announce(message);
}
