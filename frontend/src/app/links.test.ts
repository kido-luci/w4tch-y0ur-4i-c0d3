import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { parseLocation } from "../scope/location";

// Every internal link must name a tab that still exists.
//
// This is the test the codebase was missing. When `git` and `codegraph` became
// `code`, one link was left behind — `<a href="/project/git">← all repos</a>` in
// the repo detail. tsc cannot see it (it is a string inside a template literal)
// and no other test rendered that view, so the whole suite stayed green and the
// dead link was found only by opening a browser and reading the accessibility
// tree. A route rename is exactly when links rot, and it is exactly when nobody
// re-clicks every one of them.
//
// The check goes through parseLocation rather than a copy of the tab sets, so it
// cannot drift from the grammar it is policing: a path whose tab segment is not
// a known tab parses with an EMPTY tab (the segment is read as a scope name
// instead), which is precisely the "this route no longer exists" condition.

// process.cwd() is frontend/ under vitest; import.meta.url is not a file: URL
// once the jsdom environment is in play.
const SRC = join(process.cwd(), "src");

function sourceFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((entry) => {
    const p = join(dir, entry);
    if (statSync(p).isDirectory()) return sourceFiles(p);
    if (!/\.tsx?$/.test(entry) || /\.test\.tsx?$/.test(entry)) return [];
    return [p];
  });
}

/** Static internal hrefs, with any interpolated tail cut off: a link written as
 *  `href="/project/code/${encodeURIComponent(x)}"` still asserts about the part
 *  that is fixed, which is where the tab lives. */
function hrefsIn(text: string): string[] {
  const out: string[] = [];
  for (const m of text.matchAll(/href="(\/(?:project|claude)[^"]*)"/g)) {
    const raw = m[1]!;
    const cut = raw.indexOf("${");
    out.push(cut === -1 ? raw : raw.slice(0, cut).replace(/\/$/, ""));
  }
  return out;
}

describe("internal links", () => {
  const files = sourceFiles(SRC);

  it("finds the links it is meant to be policing", () => {
    // A regex that matched nothing would make every assertion below vacuous —
    // the failure mode where a test file exists and guarantees nothing.
    const all = files.flatMap((f) => hrefsIn(readFileSync(f, "utf8")));
    expect(files.length).toBeGreaterThan(20);
    expect(all.length).toBeGreaterThan(5);
  });

  it("every /project and /claude link names a tab that exists", () => {
    const dead: string[] = [];
    for (const file of files) {
      for (const href of hrefsIn(readFileSync(file, "utf8"))) {
        // Internal links are deliberately scope-LESS (syncScopeToURL splices the
        // scope in at render), so the segment after the family IS the tab.
        if (!parseLocation(href).tab) {
          dead.push(`${file.slice(SRC.length)} → ${href}`);
        }
      }
    }
    expect(dead).toEqual([]);
  });
});
