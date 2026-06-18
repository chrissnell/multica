import { describe, expect, it } from "vitest";
import { preprocessLinks } from "@multica/ui/markdown/linkify";

// The bug: linkify-it does not treat CJK full-width punctuation as a URL
// boundary, so the href can swallow trailing punctuation and the Chinese
// characters that follow it (up to the next space). The fix truncates the
// detected URL at the first CJK full-width punctuation character.

describe("preprocessLinks — CJK punctuation boundary", () => {
  it("stops URL at ideographic full stop 。", () => {
    const out = preprocessLinks("见 https://example.com/path。然后继续");
    expect(out).toBe("见 [https://example.com/path](https://example.com/path)。然后继续");
  });

  it("stops URL at fullwidth comma ，", () => {
    const out = preprocessLinks("打开 https://example.com/a，以及其他");
    expect(out).toBe("打开 [https://example.com/a](https://example.com/a)，以及其他");
  });

  it("stops URL at ideographic comma 、", () => {
    const out = preprocessLinks("两个地址 https://a.com/x、https://b.com/y");
    expect(out).toBe(
      "两个地址 [https://a.com/x](https://a.com/x)、[https://b.com/y](https://b.com/y)",
    );
  });

  it("stops URL at fullwidth right paren ）", () => {
    const out = preprocessLinks("（见 https://example.com/x）后文");
    expect(out).toBe("（见 [https://example.com/x](https://example.com/x)）后文");
  });

  it("stops URL at corner bracket 」", () => {
    const out = preprocessLinks("「https://example.com/a」后文");
    expect(out).toBe("「[https://example.com/a](https://example.com/a)」后文");
  });

  it("stops URL at fullwidth exclamation ！", () => {
    const out = preprocessLinks("太好了 https://example.com/x！继续");
    expect(out).toBe("太好了 [https://example.com/x](https://example.com/x)！继续");
  });

  it("handles the original bug report (PR link then 。 then more text)", () => {
    const out = preprocessLinks(
      "已合并 PR #1623：https://github.com/multica-ai/multica/pull/1623。merge commit",
    );
    expect(out).toBe(
      "已合并 PR #1623：[https://github.com/multica-ai/multica/pull/1623](https://github.com/multica-ai/multica/pull/1623)。merge commit",
    );
  });

  it("does not swallow the entire remainder when there is no trailing space", () => {
    const out = preprocessLinks("https://github.com/x/y/issues/1619。我接下来把这个");
    expect(out).toBe(
      "[https://github.com/x/y/issues/1619](https://github.com/x/y/issues/1619)。我接下来把这个",
    );
  });

  it("preserves ASCII trailing period handling (no regression)", () => {
    const out = preprocessLinks("visit https://example.com/path. next.");
    expect(out).toBe("visit [https://example.com/path](https://example.com/path). next.");
  });

  it("preserves plain URL with no trailing punctuation (no regression)", () => {
    const out = preprocessLinks("go https://example.com/path");
    expect(out).toBe("go [https://example.com/path](https://example.com/path)");
  });

  it("preserves CJK letters inside URL path (only trims on punctuation)", () => {
    const out = preprocessLinks("https://zh.wikipedia.org/wiki/中国 参考");
    expect(out).toBe(
      "[https://zh.wikipedia.org/wiki/中国](https://zh.wikipedia.org/wiki/中国) 参考",
    );
  });

  it("does not re-link an already-linked URL that contains 。", () => {
    // If a user or upstream already wrote [text](url。), we leave it alone.
    const input = "见 [link](https://example.com/x。)后文";
    expect(preprocessLinks(input)).toBe(input);
  });

  it("does not linkify fuzzy domains inside existing markdown link labels", () => {
    const input =
      "数据来源：[NBA.com Schedule](https://www.nba.com/schedule)、[NBC Insider](https://www.nbc.com/nbc-insider/every-nba-playoff-game-this-week-on-nbc-peacock-april-25-28)";

    expect(preprocessLinks(input)).toBe(input);
  });

  it("still linkifies fuzzy domains outside existing markdown links", () => {
    const input = "数据来源：[NBA.com Schedule](https://www.nba.com/schedule)，官网 NBA.com";

    expect(preprocessLinks(input)).toBe(
      "数据来源：[NBA.com Schedule](https://www.nba.com/schedule)，官网 [NBA.com](http://NBA.com)",
    );
  });
});

// The bug: linkify-it treats "*" as a valid URL path character, so a URL
// wrapped in markdown bold/italic markers (**url**) swallows the trailing
// asterisks into the href. The link then 404s and the emphasis markers are
// consumed instead of rendering the text bold. The fix trims trailing
// asterisks off the detected URL so the delimiters survive for the parser.
describe("preprocessLinks — markdown emphasis delimiter boundary", () => {
  it("does not swallow trailing ** from a bold-wrapped URL (original report)", () => {
    const out = preprocessLinks("**https://github.com/chrissnell/omnimodem/pull/6**");
    expect(out).toBe(
      "**[https://github.com/chrissnell/omnimodem/pull/6](https://github.com/chrissnell/omnimodem/pull/6)**",
    );
  });

  it("does not swallow a single trailing * from an italic-wrapped URL", () => {
    const out = preprocessLinks("*https://example.com/foo*");
    expect(out).toBe("*[https://example.com/foo](https://example.com/foo)*");
  });

  it("handles a bold URL inside surrounding prose", () => {
    const out = preprocessLinks("See **https://example.com/a/b** for details");
    expect(out).toBe(
      "See **[https://example.com/a/b](https://example.com/a/b)** for details",
    );
  });

  it("trims trailing asterisks from a fuzzy (schemeless) bold URL", () => {
    const out = preprocessLinks("**example.com/path**");
    expect(out).toBe("**[example.com/path](http://example.com/path)**");
  });

  it("preserves a plain URL with no trailing asterisks (no regression)", () => {
    const out = preprocessLinks("go https://example.com/path");
    expect(out).toBe("go [https://example.com/path](https://example.com/path)");
  });

  it("trims asterisks from multiple bold URLs on the same line", () => {
    const out = preprocessLinks("**https://x.com/p** and **https://y.com/q**");
    expect(out).toBe(
      "**[https://x.com/p](https://x.com/p)** and **[https://y.com/q](https://y.com/q)**",
    );
  });

  it("strips asterisks but keeps CJK boundary handling intact", () => {
    const out = preprocessLinks("**https://example.com/x**。后文");
    expect(out).toBe(
      "**[https://example.com/x](https://example.com/x)**。后文",
    );
  });
});
