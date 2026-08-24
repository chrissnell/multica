import { Extension, InputRule } from "@tiptap/core";

/**
 * Blockquote-after-hard-break input rule.
 *
 * StarterKit's Blockquote only converts `> ` into a blockquote when it is typed
 * at the START of a text block (its `wrappingInputRule` is anchored with `^`).
 * A hard break (Shift+Enter) is a soft line break WITHIN a paragraph, not a new
 * block, so `> ` typed on such a line stays literal `>` text. On serialize that
 * `>` is escaped to `&gt;` to keep it from being reinterpreted as a quote, and
 * the read-only renderer then shows a literal `>` — the quote never renders.
 *
 * The visible symptom is "blockquotes only work as the very first thing in the
 * body": the first line of an empty editor IS a block start (so the rule fires),
 * but a quote started on any later soft-broken line is not. This extension adds
 * the missing case — `> ` at the start of a hard-broken line splits the block
 * and wraps the tail in a blockquote, exactly as a block-start `> ` would.
 *
 * A hard break renders as `\n` in the input-rule text buffer (its `toText`),
 * and `\n` cannot otherwise appear inside a single paragraph's text, so the
 * `\n` anchor uniquely identifies the soft-break case without disturbing the
 * block-start rule StarterKit already owns. Up to three leading spaces are
 * allowed to match CommonMark's blockquote indent tolerance.
 */
const HARD_BREAK_BLOCKQUOTE = /\n([ \t]{0,3}>[ \t])$/;

export function createBlockquoteHardBreakExtension() {
  return Extension.create({
    name: "blockquoteHardBreak",
    addInputRules() {
      return [
        new InputRule({
          find: (text) => {
            const match = HARD_BREAK_BLOCKQUOTE.exec(text);
            if (!match) return null;
            // `text` is only the `> ` run (plus indent); `range` then spans just
            // that run, leaving the hard break one position before `range.from`.
            return { index: text.length - match[1].length, text: match[1] };
          },
          handler: ({ state, range, chain }) => {
            const blockquote = state.schema.nodes.blockquote;
            if (!blockquote) return null;

            const $from = state.doc.resolve(range.from);
            if (!$from.parent.isTextblock) return null;

            // Confirm the char before the `> ` run really is a hard break before
            // deleting it — the `\n` anchor guarantees this, but guard in case a
            // future inline node ever serializes with a newline.
            const hardBreakFrom = range.from - 1;
            const before = $from.nodeBefore;
            if (!before || before.type.name !== "hardBreak") return null;

            chain()
              .deleteRange({ from: hardBreakFrom, to: range.to })
              .splitBlock()
              .wrapIn(blockquote)
              .run();
          },
        }),
      ];
    },
  });
}
