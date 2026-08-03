import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// The stylesheet was one 6446-line file for a long time, and the documented
// reason was real: splitting it reorders the cascade, and nothing in this repo
// renders a page to notice. The split went ahead only because it could be made
// provably inert — the parts are @imported in the exact order they appeared,
// Vite inlines them in that order, and the built CSS came out byte-identical
// (same content hash, same 115392 bytes).
//
// That property is what this test guards. It cannot see colours; it does not
// need to. Every way the split can break the cascade is a change to WHICH parts
// are imported or in WHAT ORDER, and both are visible right here in the text.

const CSS = join(process.cwd(), "src", "css");
const ENTRY = join(process.cwd(), "src", "style.css");

function importedFiles(): string[] {
  const out: string[] = [];
  for (const m of readFileSync(ENTRY, "utf8").matchAll(/@import\s+"\.\/css\/([^"]+)"/g)) {
    out.push(m[1]!);
  }
  return out;
}

describe("stylesheet parts", () => {
  it("imports every part exactly once, and imports nothing that is missing", () => {
    const imported = importedFiles();
    const onDisk = readdirSync(CSS).filter((f) => f.endsWith(".css"));

    // A part on disk that nobody imports is dead CSS that looks live; an import
    // naming a file that is gone fails the build, but says nothing about which
    // rules vanished from the cascade.
    expect([...imported].sort()).toEqual([...onDisk].sort());
    expect(new Set(imported).size).toBe(imported.length);
  });

  it("keeps the parts in cascade order, which the numeric prefix encodes", () => {
    // The prefixes exist so this is checkable at all: later rules win, so the
    // import order IS the cascade, and a reordered list is a silent restyle.
    const imported = importedFiles();
    expect(imported).toEqual([...imported].sort());
    for (const name of imported) {
      expect(name).toMatch(/^\d\d-/);
    }
  });

  it("keeps style.css an index, not a place to put rules", () => {
    // One rule added here rather than in a part would sit after every import and
    // quietly win against all of them — the exact failure the split invites.
    const body = readFileSync(ENTRY, "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
    const nonImport = body
      .split("\n")
      .map((l) => l.trim())
      .filter((l) => l && !l.startsWith("@import"));
    expect(nonImport).toEqual([]);
  });
});
