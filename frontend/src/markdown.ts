// Dependency-free markdown → HTML renderer, shared by docs-wiki pages and board
// card notes. A small recursive-descent block parser: renderBlocks splits text
// into block-level constructs (fenced code, headings, rules, blockquotes, GFM
// tables, lists, paragraphs) and dispatches each. A list item's indented content
// is dedented to its own column and fed back through renderBlocks, so an item
// can hold paragraphs, a code block, a nested list or a quote. All text passes
// through escapeHtml and URLs are scheme-checked, so the result is safe for
// innerHTML.

import { escapeHtml } from "./format";

const UNSAFE_URL = /^(javascript|data|vbscript|file):/i;
const SAFE_LANGUAGE = /[^A-Za-z0-9_+-]/g;

function safeImgSrc(url: string): string {
  return UNSAFE_URL.test(url) ? "#" : url;
}

function safeLinkHref(url: string): string {
  if (UNSAFE_URL.test(url)) return "#";
  return url.startsWith("http") || url.startsWith("/") || url.startsWith("#") ? url : "#";
}

// Link reference definitions (`[label]: url "title"`) collected per render, so
// `[text][label]` / `[text][]` / `[text]` resolve against them. Module-level
// because renderMarkdown is synchronous: set on entry, cleared on exit.
interface RefDef {
  url: string;
  title: string;
}
let refs = new Map<string, RefDef>();

/** Case- and whitespace-insensitive reference label key. */
function normalizeLabel(label: string): string {
  return label.trim().replace(/\s+/g, " ").toLowerCase();
}

/** An <a> for a reference link (url/title come from a definition, so both are
 *  escaped here); null when the label is undefined — the caller leaves the
 *  bracket text literal. `text` is already HTML-escaped by applyInline. */
function refLink(text: string, label: string): string | null {
  const def = refs.get(normalizeLabel(label));
  if (!def) return null;
  const title = def.title ? ` title="${escapeHtml(def.title)}"` : "";
  return `<a href="${escapeHtml(safeLinkHref(def.url))}"${title} target="_blank" rel="noopener noreferrer">${text}</a>`;
}

/** An <img> for a reference image (`![alt][label]`), or null when undefined. */
function refImg(alt: string, label: string): string | null {
  const def = refs.get(normalizeLabel(label));
  if (!def) return null;
  const title = def.title ? ` title="${escapeHtml(def.title)}"` : "";
  return `<img src="${escapeHtml(safeImgSrc(def.url))}" alt="${alt}"${title}>`;
}

/** Inline markup (bold / italic / code / images / links, inline and reference)
 *  over already-escaped text. Code spans, images and links (and resolved
 *  references) are rendered to held placeholders BEFORE the reference passes run,
 *  so a `[label]` sitting inside generated markup (a URL, an alt, a code span)
 *  is never re-scanned and turned into a nested anchor. */
function applyInline(text: string): string {
  const held: string[] = [];
  const hold = (html: string): string => `${held.push(html) - 1}`;
  return text
    .replace(/\*\*\*(.*?)\*\*\*/gim, "<strong><em>$1</em></strong>")
    .replace(/\*\*(.*?)\*\*/gim, "<strong>$1</strong>")
    .replace(/\*(.*?)\*/gim, "<em>$1</em>")
    .replace(/`(.*?)`/gim, (_, c: string) => hold(`<code>${c}</code>`))
    .replace(/!\[(.*?)\]\((.*?)\)/gim, (_, alt: string, url: string) =>
      hold(`<img src="${safeImgSrc(url)}" alt="${alt}">`),
    )
    // Reference images: ![alt][label], ![alt][], ![alt]. Before the link forms.
    .replace(/!\[([^\]]*)\]\[([^\]]*)\]/gim, (whole, alt: string, l: string) => {
      const h = refImg(alt, l.trim() || alt);
      return h ? hold(h) : whole;
    })
    .replace(/!\[([^\]]+)\]/gim, (whole, alt: string) => {
      const h = refImg(alt, alt);
      return h ? hold(h) : whole;
    })
    .replace(/\[(.*?)\]\((.*?)\)/gim, (_, label: string, url: string) =>
      hold(`<a href="${safeLinkHref(url)}" target="_blank" rel="noopener noreferrer">${label}</a>`),
    )
    // Reference links: full [text][label], collapsed [text][], shortcut [text].
    // An undefined label stays literal.
    .replace(/\[([^\]]*)\]\[([^\]]*)\]/gim, (whole, t: string, l: string) => {
      const h = refLink(t, l.trim() || t);
      return h ? hold(h) : whole;
    })
    .replace(/\[([^\]]+)\]/gim, (whole, t: string) => {
      const h = refLink(t, t);
      return h ? hold(h) : whole;
    })
    .replace(/(\d+)/g, (_, i: string) => held[+i]!);
}

/** One-line markdown for card titles: inline markup only, no block constructs.
 *  A title has no reference context, so `[text]` never becomes a link here. */
export function renderInlineMarkdown(text: string): string {
  refs = new Map();
  return applyInline(escapeHtml(text));
}

// --- block grammar --------------------------------------------------------

const FENCE = /^([ \t]*)(`{3,})(.*)$/;
const HEADING = /^(#{1,3})[ \t]+(.*)$/;
const HR = /^[ \t]*([-*_])[ \t]*(?:\1[ \t]*){2,}$/;
const QUOTE = /^[ \t]*>/;
// 1=indent, 2=bullet char, 3=ordered digits, 4=ordered delimiter, 5=spaces, 6=text
const LIST = /^([ \t]*)(?:([-*+])|(\d{1,9})([.)]))([ \t]+)(.*)$/;
const ROW = /^[ \t]*\|.*\|[ \t]*$/;
const TABLE_SEP = /^[ \t]*\|(?:[ \t]*:?-+:?[ \t]*\|)+[ \t]*$/;
// A definition line in a definition list: `: text` (group 1 = the text).
const DEF_MARKER = /^[ \t]{0,3}:[ \t]+(.*)$/;

/** Leading-whitespace width in columns (a tab counts as four). */
function leadWidth(line: string): number {
  return /^[ \t]*/.exec(line)![0].replace(/\t/g, "    ").length;
}

/** Removes up to `n` columns of leading whitespace (never past non-whitespace). */
function stripIndent(line: string, n: number): string {
  let i = 0;
  let col = 0;
  while (i < line.length && col < n) {
    if (line[i] === " ") col += 1;
    else if (line[i] === "\t") col += 4;
    else break;
    i++;
  }
  return line.slice(i);
}

/** Whether lines[i] (header) + lines[i+1] (delimiter) begin a GFM table. */
function isTableStart(lines: string[], i: number): boolean {
  return lines[i + 1] !== undefined && ROW.test(lines[i]!) && TABLE_SEP.test(lines[i + 1]!);
}

/** Whether lines[i] begins a new block (used to terminate a paragraph). */
function isBlockStart(lines: string[], i: number): boolean {
  const line = lines[i]!;
  return (
    FENCE.test(line) ||
    HEADING.test(line) ||
    HR.test(line) ||
    QUOTE.test(line) ||
    LIST.test(line) ||
    isTableStart(lines, i)
  );
}

/** Whether lines[i] starts a definition list: a run of term lines (a term is any
 *  non-blank line that isn't itself a definition or another block) that ends at
 *  a `: …` definition line, allowing one blank line before it. */
function termLeadsToDef(lines: string[], i: number): boolean {
  const first = lines[i];
  if (first === undefined || first.trim() === "" || DEF_MARKER.test(first) || isBlockStart(lines, i)) {
    return false;
  }
  let j = i;
  while (
    j < lines.length &&
    lines[j]!.trim() !== "" &&
    !DEF_MARKER.test(lines[j]!) &&
    !isBlockStart(lines, j)
  ) {
    j++;
  }
  if (lines[j] !== undefined && lines[j]!.trim() === "") j++; // one optional blank
  return lines[j] !== undefined && DEF_MARKER.test(lines[j]!);
}

function renderTable(rows: string[]): string {
  const cells = (line: string): string[] =>
    line
      .trim()
      .replace(/^\|/, "")
      .replace(/\|[ \t]*$/, "")
      .split("|")
      .map((c) => c.trim());
  const aligns = cells(rows[1]!).map((sep) => {
    const l = sep.startsWith(":");
    const r = sep.endsWith(":");
    return l && r ? "center" : r ? "right" : l ? "left" : "";
  });
  const attr = (n: number): string => (aligns[n] ? ` style="text-align:${aligns[n]}"` : "");
  const cell = (c: string): string => applyInline(escapeHtml(c));
  const thead =
    "<thead><tr>" + cells(rows[0]!).map((c, n) => `<th${attr(n)}>${cell(c)}</th>`).join("") + "</tr></thead>";
  const tbody =
    "<tbody>" +
    rows
      .slice(2)
      .map((r) => "<tr>" + cells(r).map((c, n) => `<td${attr(n)}>${cell(c)}</td>`).join("") + "</tr>")
      .join("") +
    "</tbody>";
  return `<div class="md-table"><table>${thead}${tbody}</table></div>`;
}

/** Parses one definition list starting at lines[start]; returns [html, end].
 *  Terms are single lines; `: …` lines are definitions (a term may have several,
 *  and one blank line before a definition marks the list loose → <dd> gets a
 *  <p>). A definition's text continues onto following indented lines. */
function renderDefList(lines: string[], start: number): [string, number] {
  const items: { tag: "dt" | "dd"; content: string }[] = [];
  let loose = false;
  let sawBlank = false;
  let i = start;

  while (i < lines.length) {
    const line = lines[i]!;
    if (line.trim() === "") {
      // The list continues only if a definition or a new term-with-def follows.
      let j = i + 1;
      while (j < lines.length && lines[j]!.trim() === "") j++;
      if (j < lines.length && (DEF_MARKER.test(lines[j]!) || termLeadsToDef(lines, j))) {
        sawBlank = true;
        i = j;
        continue;
      }
      break; // end of the definition list
    }
    const dm = DEF_MARKER.exec(line);
    if (dm) {
      if (sawBlank) loose = true;
      sawBlank = false;
      const content = [dm[1]!];
      i++;
      // Indented following lines continue the definition's text.
      while (i < lines.length && lines[i]!.trim() !== "" && leadWidth(lines[i]!) >= 2 && !DEF_MARKER.test(lines[i]!)) {
        content.push(lines[i]!.trim());
        i++;
      }
      items.push({ tag: "dd", content: content.join("\n") });
      continue;
    }
    if (isBlockStart(lines, i)) break;
    // Gather a whole run of term lines in one pass, then keep them only if a
    // definition follows — re-scanning per term would make this O(n²).
    const runStart = i;
    while (i < lines.length && lines[i]!.trim() !== "" && !DEF_MARKER.test(lines[i]!) && !isBlockStart(lines, i)) {
      i++;
    }
    let j = i;
    if (lines[j] !== undefined && lines[j]!.trim() === "") j++;
    if (lines[j] === undefined || !DEF_MARKER.test(lines[j]!)) {
      i = runStart;
      break; // trailing text, not terms → the list ends here
    }
    for (let k = runStart; k < i; k++) items.push({ tag: "dt", content: lines[k]!.trim() });
    sawBlank = false;
  }

  const html = items
    .map((it) => {
      const inner = applyInline(escapeHtml(it.content)).replace(/\n/g, "<br>");
      return it.tag === "dt" ? `<dt>${inner}</dt>` : `<dd>${loose ? `<p>${inner}</p>` : inner}</dd>`;
    })
    .join("");
  return [`<dl>${html}</dl>`, i];
}

/** Parses one list starting at lines[start]; returns [html, indexAfterList]. */
function renderListBlock(lines: string[], start: number, depth: number): [string, number] {
  const first = LIST.exec(lines[start]!)!;
  const baseIndent = leadWidth(lines[start]!);
  const ordered = first[2] === undefined;
  const startNum = first[3] ? parseInt(first[3], 10) : 1;
  const tag: "ul" | "ol" = ordered ? "ol" : "ul";

  const items: string[][] = []; // each item = its dedented content lines
  let loose = false;
  let pendingBlank = false;
  let i = start;

  while (i < lines.length) {
    const line = lines[i]!;
    const m = LIST.exec(line);

    if (m && leadWidth(line) === baseIndent) {
      // A marker line of this list. A bullet⇄ordered switch ends it.
      if ((m[2] === undefined) !== ordered) break;
      if (pendingBlank && items.length) loose = true;
      pendingBlank = false;

      const markerText = m[2] ?? m[3]! + m[4]!;
      const contentIndent = baseIndent + markerText.length + m[5]!.length;
      const itemLines: string[] = [m[6]!];
      i++;

      let sawBlank = false;
      while (i < lines.length) {
        const l = lines[i]!;
        if (l.trim() === "") {
          sawBlank = true;
          itemLines.push("");
          i++;
          continue;
        }
        const w = leadWidth(l);
        if (w >= contentIndent) {
          // A blank followed by the item's own next block loosens the list — but
          // a blank inside a nested sublist (the next line is itself a marker)
          // belongs to that sublist, so it mustn't loosen this one.
          if (sawBlank && !LIST.test(stripIndent(l, contentIndent))) loose = true;
          itemLines.push(stripIndent(l, contentIndent));
          sawBlank = false;
          i++;
          continue;
        }
        // Less indented than this item's content.
        if (LIST.test(l) && w <= baseIndent) break; // sibling / shallower item
        if (sawBlank) break; // blank then non-indented non-item → list ends
        // A new block construct on the next line ends the list (only paragraph
        // text lazily continues an item).
        if (FENCE.test(l) || HEADING.test(l) || HR.test(l) || QUOTE.test(l) || isTableStart(lines, i)) break;
        itemLines.push(l.trim()); // lazy paragraph continuation
        i++;
      }
      if (sawBlank) pendingBlank = true;
      items.push(itemLines);
    } else if (line.trim() === "") {
      pendingBlank = true;
      i++;
    } else {
      break;
    }
  }

  const open = ordered && startNum !== 1 ? `<ol start="${startNum}">` : `<${tag}>`;
  const body = items
    .map((itemLines) => `<li>${renderBlocks(itemLines.join("\n"), !loose, depth + 1)}</li>`)
    .join("");
  return [`${open}${body}</${tag}>`, i];
}

// Guards against a stack overflow on pathologically deep nesting (a long run of
// `>` or a very deeply-indented list): past this depth, content renders as
// inline text rather than recursing further.
const MAX_DEPTH = 32;

/** Renders markdown block content. `tight` suppresses <p> around bare paragraphs
 *  (used for tight list items, where a single line should stay inline). */
function renderBlocks(md: string, tight = false, depth = 0): string {
  if (depth > MAX_DEPTH) return applyInline(escapeHtml(md.trim()));
  const lines = md.replace(/\r\n?/g, "\n").split("\n");
  const out: string[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i]!;

    if (line.trim() === "") {
      i++;
      continue;
    }

    // Fenced code — opaque; its body is never scanned for other constructs.
    const fence = FENCE.exec(line);
    if (fence) {
      const pad = fence[1]!.length;
      const close = new RegExp("^[ \\t]*" + fence[2] + "\\s*$");
      const lang = escapeHtml(fence[3]!.trim().replace(SAFE_LANGUAGE, ""));
      const body: string[] = [];
      i++;
      while (i < lines.length && !close.test(lines[i]!)) {
        body.push(stripIndent(lines[i]!, pad));
        i++;
      }
      i++; // consume the closing fence (if present)
      out.push(`<pre><code class="language-${lang}">${escapeHtml(body.join("\n"))}</code></pre>`);
      continue;
    }

    // ATX heading (h1–h3).
    const heading = HEADING.exec(line);
    if (heading) {
      const level = heading[1]!.length;
      out.push(`<h${level}>${applyInline(escapeHtml(heading[2]!))}</h${level}>`);
      i++;
      continue;
    }

    // Horizontal rule.
    if (HR.test(line)) {
      out.push("<hr>");
      i++;
      continue;
    }

    // Blockquote — consecutive `>` lines; content parsed recursively.
    if (QUOTE.test(line)) {
      const body: string[] = [];
      while (i < lines.length && QUOTE.test(lines[i]!)) {
        body.push(lines[i]!.replace(/^[ \t]*>[ \t]?/, ""));
        i++;
      }
      out.push(`<blockquote>${renderBlocks(body.join("\n"), false, depth + 1)}</blockquote>`);
      continue;
    }

    // GFM table.
    if (isTableStart(lines, i)) {
      const rows: string[] = [];
      while (i < lines.length && ROW.test(lines[i]!)) {
        rows.push(lines[i]!);
        i++;
      }
      out.push(renderTable(rows));
      continue;
    }

    // List.
    if (LIST.test(line)) {
      const [html, next] = renderListBlock(lines, i, depth);
      out.push(html);
      i = next;
      continue;
    }

    // Definition list — a term line (or several) ending at a `: …` definition.
    if (termLeadsToDef(lines, i)) {
      const [html, next] = renderDefList(lines, i);
      out.push(html);
      i = next;
      continue;
    }

    // Paragraph — consecutive non-blank lines up to the next block start.
    const para: string[] = [];
    while (i < lines.length && lines[i]!.trim() !== "" && !isBlockStart(lines, i)) {
      para.push(lines[i]!.trim());
      i++;
    }
    const inner = applyInline(escapeHtml(para.join("\n"))).replace(/\n/g, "<br>");
    out.push(tight ? inner : `<p>${inner}</p>`);
  }

  return out.join("\n");
}

// `[label]: url "title"` — url may be bare or <bracketed>; optional title in
// ", ' or (). Up to three leading spaces, like CommonMark.
const REF_DEF =
  /^[ \t]{0,3}\[([^\]]+)\]:[ \t]*(?:<([^>]*)>|(\S+))(?:[ \t]+(?:"([^"]*)"|'([^']*)'|\(([^)]*)\)))?[ \t]*$/;

/** Extracts link reference definitions into `refs` and drops their lines (first
 *  definition of a label wins). Lines inside a fenced code block are left alone. */
function collectRefDefs(text: string): string {
  const out: string[] = [];
  let fence: string | null = null; // the opening backtick run while inside a fence
  for (const line of text.replace(/\r\n?/g, "\n").split("\n")) {
    if (fence !== null) {
      // Close only on a line of exactly the opening backticks — same pairing as
      // renderBlocks, so the two never disagree about what is code.
      if (new RegExp("^[ \\t]*" + fence + "\\s*$").test(line)) fence = null;
      out.push(line);
      continue;
    }
    const open = /^[ \t]*(`{3,})/.exec(line);
    if (open) {
      fence = open[1]!;
      out.push(line);
      continue;
    }
    const m = REF_DEF.exec(line);
    if (m) {
      // Key on the escaped label: applyInline resolves references over escaped
      // text, so a label with & / < / > must be stored the same way to match.
      const label = normalizeLabel(escapeHtml(m[1]!));
      if (label && !refs.has(label)) {
        refs.set(label, { url: m[2] ?? m[3] ?? "", title: m[4] ?? m[5] ?? m[6] ?? "" });
      }
      continue; // the definition itself renders nothing
    }
    out.push(line);
  }
  return out.join("\n");
}

/** Full markdown for card notes / doc pages; wrap the result in a `.md` element. */
export function renderMarkdown(text: string): string {
  refs = new Map();
  const html = renderBlocks(collectRefDefs(text));
  refs = new Map();
  return html;
}
