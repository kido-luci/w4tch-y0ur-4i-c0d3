import { describe, expect, it } from "vitest";
import { renderInlineMarkdown, renderMarkdown } from "./markdown";

// Newline-stripped render, so structural assertions aren't brittle about the
// join whitespace between blocks.
const r = (md: string): string => renderMarkdown(md).replace(/\n/g, "");

describe("blocks inside list items (the feature)", () => {
  it("renders a fenced code block inside an item as a real <pre>, not merged text", () => {
    const html = r("- run this:\n\n  ```\n  const x = 1;\n  ```");
    expect(html).toMatch(/<li>[\s\S]*<pre><code[\s\S]*const x = 1;[\s\S]*<\/code><\/pre>[\s\S]*<\/li>/);
    expect(html).not.toContain("```");
  });

  it("keeps a code fence's body opaque (list markers inside it stay literal)", () => {
    const html = r("- x:\n\n  ```\n  - not a list item\n  ```");
    expect(html).toContain("- not a list item");
    // the fence body must not have become a nested <ul>
    expect(html).toMatch(/<pre><code[\s\S]*- not a list item[\s\S]*<\/code><\/pre>/);
  });

  it("renders multiple paragraphs in one item (loose)", () => {
    expect(r("- one\n\n  two")).toBe("<ul><li><p>one</p><p>two</p></li></ul>");
  });

  it("renders a nested list inside an item (tight, no <p> around the lead text)", () => {
    expect(r("- a\n  - b")).toBe("<ul><li>a<ul><li>b</li></ul></li></ul>");
  });

  it("renders a blockquote inside an item", () => {
    expect(r("- note:\n\n  > quoted")).toBe(
      "<ul><li><p>note:</p><blockquote><p>quoted</p></blockquote></li></ul>",
    );
  });

  it("renders a code block AND a following paragraph in the same item", () => {
    const html = r("- step:\n\n  ```\n  go\n  ```\n\n  after the code");
    expect(html).toMatch(/<li>[\s\S]*<pre>[\s\S]*<\/pre>[\s\S]*<p>after the code<\/p>[\s\S]*<\/li>/);
  });

  it("renders a GFM table inside a list item, nested in the <li>", () => {
    const html = r("1. Results:\n\n   | name | score |\n   |------|-------|\n   | a | 10 |\n\n2. next");
    expect(html).toMatch(/<li>[\s\S]*<table>[\s\S]*<\/table>[\s\S]*<\/li><li>/);
    expect(html).toContain("<th>name</th>");
    expect(html).toContain("<td>10</td>");
  });
});

describe("tight vs loose lists", () => {
  it("tight list items render inline (no <p>)", () => {
    expect(r("- a\n- b")).toBe("<ul><li>a</li><li>b</li></ul>");
  });

  it("a blank line between items makes the whole list loose (<p> everywhere)", () => {
    expect(r("- a\n\n- b")).toBe("<ul><li><p>a</p></li><li><p>b</p></li></ul>");
  });
});

describe("list regressions (from earlier reviews)", () => {
  it("ordered list keeps the source start number", () => {
    expect(r("3. third\n4. fourth")).toBe('<ol start="3"><li>third</li><li>fourth</li></ol>');
  });

  it("start number 1 emits a bare <ol>", () => {
    expect(r("1. one\n2. two")).toBe("<ol><li>one</li><li>two</li></ol>");
  });

  it("a bullet⇄ordered switch at the same level starts a new list", () => {
    expect(r("1. one\n- two")).toBe("<ol><li>one</li></ol><ul><li>two</li></ul>");
  });

  it("an intermediate-indent dedent keeps same-indent items as siblings", () => {
    // C and D (indent 2) must be siblings, never D-under-C.
    expect(r("- A\n    - B\n  - C\n  - D")).toContain("<li>C</li><li>D</li>");
  });

  it("loose ordered list keeps its numbering across blank lines", () => {
    expect(r("1. one\n\n2. two\n\n3. three")).toBe(
      "<ol><li><p>one</p></li><li><p>two</p></li><li><p>three</p></li></ol>",
    );
  });

  it("an indented no-marker line continues the item (lazy continuation)", () => {
    expect(r("1. first\n   continued\n2. second")).toBe(
      "<ol><li>first<br>continued</li><li>second</li></ol>",
    );
  });

  it("a list ends when a blank line is followed by a non-list paragraph", () => {
    // The trailing blank is after the list, not between items, so the single
    // item stays tight (no <p>).
    expect(r("- a\n\nplain paragraph")).toBe("<ul><li>a</li></ul><p>plain paragraph</p>");
  });
});

describe("fixes from the block-parser review", () => {
  it("a block construct right after a list (no blank) ends the list", () => {
    expect(r("- a\n- b\n---\nNext")).toBe("<ul><li>a</li><li>b</li></ul><hr><p>Next</p>");
    expect(r("- a\n- b\n# Heading")).toBe("<ul><li>a</li><li>b</li></ul><h1>Heading</h1>");
  });

  it("a blank inside a nested list does not loosen the outer list", () => {
    // outer item `a` stays tight (no <p>); only the inner b/c list is loose.
    expect(r("- a\n  - b\n\n  - c")).toBe(
      "<ul><li>a<ul><li><p>b</p></li><li><p>c</p></li></ul></li></ul>",
    );
  });

  it("deeply nested input degrades gracefully instead of overflowing the stack", () => {
    expect(() => renderMarkdown(">".repeat(5000) + " deep quote")).not.toThrow();
    const deepList = Array.from({ length: 2000 }, (_, n) => "  ".repeat(n) + "- x").join("\n");
    expect(() => renderMarkdown(deepList)).not.toThrow();
  });
});

describe("horizontal rules", () => {
  it("renders ---, ***, ___ and spaced - - - as <hr>", () => {
    expect(r("---")).toBe("<hr>");
    expect(r("***")).toBe("<hr>");
    expect(r("___")).toBe("<hr>");
    expect(r("- - -")).toBe("<hr>");
  });

  it("does not treat a two-char -- as a rule, or a real list item as one", () => {
    expect(r("--")).toBe("<p>--</p>");
    expect(r("- item")).toBe("<ul><li>item</li></ul>");
  });
});

describe("other blocks", () => {
  it("renders headings h1–h3", () => {
    expect(r("# One")).toBe("<h1>One</h1>");
    expect(r("### Three")).toBe("<h3>Three</h3>");
  });

  it("renders a fenced code block with a language class", () => {
    expect(r("```js\nconst x = 1;\n```")).toBe(
      '<pre><code class="language-js">const x = 1;</code></pre>',
    );
  });

  it("renders a GFM table", () => {
    expect(r("| a | b |\n|---|---|\n| 1 | 2 |")).toBe(
      '<div class="md-table"><table><thead><tr><th>a</th><th>b</th></tr></thead>' +
        "<tbody><tr><td>1</td><td>2</td></tr></tbody></table></div>",
    );
  });

  it("renders a blockquote with paragraph content", () => {
    expect(r("> hi")).toBe("<blockquote><p>hi</p></blockquote>");
  });

  it("wraps plain text in a paragraph", () => {
    expect(r("hello world")).toBe("<p>hello world</p>");
  });
});

describe("reference-style links", () => {
  it("resolves a full reference link and drops the definition line", () => {
    expect(r("[text][ref]\n\n[ref]: https://example.com")).toBe(
      '<p><a href="https://example.com" target="_blank" rel="noopener noreferrer">text</a></p>',
    );
  });

  it("resolves collapsed [text][] and shortcut [text] forms", () => {
    expect(r("[docs][]\n\n[docs]: https://e.com")).toContain('href="https://e.com"');
    expect(r("see [docs] now\n\n[docs]: https://e.com")).toContain(
      '<a href="https://e.com" target="_blank" rel="noopener noreferrer">docs</a>',
    );
  });

  it("carries an optional title and matches labels case-insensitively", () => {
    const html = r('[Here][Ref]\n\n[ref]: https://e.com "a title"');
    expect(html).toContain('href="https://e.com"');
    expect(html).toContain('title="a title"');
    expect(html).toContain(">Here</a>");
  });

  it("leaves an undefined reference literal", () => {
    expect(r("[text][nope] and [alsonope]")).toBe("<p>[text][nope] and [alsonope]</p>");
  });

  it("neutralizes a javascript: reference url and escapes the title", () => {
    const html = r('[t][x]\n\n[x]: javascript:alert(1) "a<b>c"');
    expect(html).toContain('href="#"');
    expect(html).not.toContain("javascript:");
    expect(html).toContain("&lt;b&gt;");
  });

  it("a lone definition renders nothing", () => {
    expect(r("[ref]: https://e.com")).toBe("");
  });

  // --- fixes from the reference-links review ---

  it("does not extract a definition inside a nested-backtick code fence", () => {
    const html = r("````\n```\n[foo]: /evil\n```\n````\n\nUse [foo] here.");
    expect(html).toContain("[foo]: /evil"); // stayed in the code block
    expect(html).not.toContain('href="/evil"'); // [foo] is undefined → literal
  });

  it("does not resolve a reference sitting inside a code span or a link URL", () => {
    expect(r("`arr[x]`\n\n[x]: https://e.com")).toContain("<code>arr[x]</code>");
    expect(r("`arr[x]`\n\n[x]: https://e.com")).not.toContain("<code>arr<a");
    const h = r("[docs](https://e.com/v2/[1]/spec)\n\n[1]: https://other.com");
    expect(h).toContain('href="https://e.com/v2/[1]/spec"');
    expect(h).not.toContain("https://other.com");
  });

  it("resolves a label containing HTML-special characters (AT&T)", () => {
    const html = r("[AT&T][]\n\n[AT&T]: https://att.com");
    expect(html).toContain('href="https://att.com"');
    expect(html).toContain(">AT&amp;T</a>");
  });

  it("supports reference images ![alt][label]", () => {
    expect(r("![logo][img]\n\n[img]: https://e.com/l.png")).toContain(
      '<img src="https://e.com/l.png" alt="logo">',
    );
  });
});

describe("definition lists", () => {
  it("renders a term with one definition", () => {
    expect(r("Term\n: definition")).toBe("<dl><dt>Term</dt><dd>definition</dd></dl>");
  });

  it("renders multiple definitions for one term", () => {
    expect(r("Term\n: first\n: second")).toBe(
      "<dl><dt>Term</dt><dd>first</dd><dd>second</dd></dl>",
    );
  });

  it("renders multiple terms sharing a definition", () => {
    expect(r("T1\nT2\n: def")).toBe("<dl><dt>T1</dt><dt>T2</dt><dd>def</dd></dl>");
  });

  it("a blank line before the definition makes it loose (<dd> gets a <p>)", () => {
    expect(r("Term\n\n: def")).toBe("<dl><dt>Term</dt><dd><p>def</p></dd></dl>");
  });

  it("groups consecutive term/definition pairs into one list", () => {
    expect(r("A\n: 1\n\nB\n: 2")).toBe("<dl><dt>A</dt><dd>1</dd><dt>B</dt><dd>2</dd></dl>");
  });

  it("applies inline markup in terms and definitions", () => {
    expect(r("**T**\n: *d*")).toBe("<dl><dt><strong>T</strong></dt><dd><em>d</em></dd></dl>");
  });

  it("continues a definition onto an indented following line", () => {
    expect(r("Term\n: long\n  continued")).toBe("<dl><dt>Term</dt><dd>long<br>continued</dd></dl>");
  });

  it("plain text with no definition line stays a paragraph", () => {
    expect(r("just a sentence")).toBe("<p>just a sentence</p>");
    expect(r("line one\nline two")).toBe("<p>line one<br>line two</p>");
  });

  it("ends the list at a following block", () => {
    expect(r("Term\n: def\n\n# Heading")).toBe(
      "<dl><dt>Term</dt><dd>def</dd></dl><h1>Heading</h1>",
    );
  });

  it("nests inside a list item (recursion is free)", () => {
    expect(r("- item:\n\n  Term\n  : def")).toMatch(/<li>[\s\S]*<dl><dt>Term<\/dt><dd>def<\/dd><\/dl>[\s\S]*<\/li>/);
  });
});

describe("inline markup and safety", () => {
  it("applies bold / italic / code inline", () => {
    expect(r("**b** *i* `c`")).toBe("<p><strong>b</strong> <em>i</em> <code>c</code></p>");
  });

  it("escapes HTML in every context", () => {
    expect(r("- <script>alert(1)</script>")).toContain("&lt;script&gt;");
    expect(r("<b>x</b>")).toBe("<p>&lt;b&gt;x&lt;/b&gt;</p>");
    expect(r("```\n<script>\n```")).toContain("&lt;script&gt;");
  });

  it("neutralizes javascript: link and image URLs", () => {
    expect(r("[x](javascript:alert(1))")).toContain('href="#"');
    expect(r("![a](javascript:alert(1))")).toContain('src="#"');
  });

  it("renderInlineMarkdown does inline only, escaped", () => {
    expect(renderInlineMarkdown("**hi** <b>")).toBe("<strong>hi</strong> &lt;b&gt;");
  });
});
