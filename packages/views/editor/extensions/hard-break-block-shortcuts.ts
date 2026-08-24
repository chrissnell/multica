import { Extension, InputRule } from "@tiptap/core";
import type { EditorState } from "@tiptap/pm/state";

/**
 * Block-markdown shortcuts after a hard break.
 *
 * StarterKit's blockquote / code-block input rules only fire at the START of a
 * text block (their regexes are anchored with `^`). A hard break (Shift+Enter)
 * is a soft line break WITHIN a paragraph, not a new block, so `> ` or ` ``` `
 * typed on such a line stays literal text: the quote/fence markers are then
 * escaped on serialize and never render. In a composer where Enter sends (issue
 * comments, chat), every line after the first is a hard break, so the visible
 * symptom is "blockquotes / code blocks only work on the first line".
 *
 * These rules add the missing case: `> ` and ` ``` `/`~~~` (with an optional
 * language) at the start of a hard-broken line split the block and apply the
 * matching node, exactly as they would at a block start. The block-start rules
 * StarterKit already owns are untouched.
 *
 * A hard break renders as `\n` in the input-rule text buffer (its `toText`),
 * and `\n` cannot otherwise appear inside a single paragraph, so the leading
 * `\n` uniquely identifies the soft-break case. The match text is only the
 * marker run (never the `\n`), so `range` spans just that run and the hard break
 * sits one position before `range.from`. Up to three leading spaces are allowed,
 * matching CommonMark's indent tolerance for both constructs.
 */
const HARD_BREAK_BLOCKQUOTE = /\n([ \t]{0,3}>[ \t])$/;
const HARD_BREAK_CODE_FENCE = /\n([ \t]{0,3}(?:```|~~~)([a-z]+)?[ \t])$/;
const FENCE_LANGUAGE = /(?:```|~~~)([a-z]+)/;

/**
 * Resolve the hard break that precedes the matched marker run. Returns the
 * position of the hard break, or null when `range.from - 1` is not actually a
 * hard break — the `\n` anchor guarantees it is, but this keeps the destructive
 * edit safe if a future inline node ever serializes with a newline.
 */
function hardBreakBefore(state: EditorState, from: number): number | null {
  const $from = state.doc.resolve(from);
  if (!$from.parent.isTextblock) return null;
  const before = $from.nodeBefore;
  if (!before || before.type.name !== "hardBreak") return null;
  return from - 1;
}

export function createHardBreakBlockShortcutsExtension() {
  return Extension.create({
    name: "hardBreakBlockShortcuts",
    addInputRules() {
      return [
        // `> ` on a hard-broken line → blockquote.
        new InputRule({
          find: (text) => {
            const marker = HARD_BREAK_BLOCKQUOTE.exec(text)?.[1];
            if (marker === undefined) return null;
            return { index: text.length - marker.length, text: marker };
          },
          handler: ({ state, range, chain }) => {
            const blockquote = state.schema.nodes.blockquote;
            const hardBreakFrom = hardBreakBefore(state, range.from);
            if (!blockquote || hardBreakFrom === null) return;

            chain()
              .deleteRange({ from: hardBreakFrom, to: range.to })
              .splitBlock()
              .wrapIn(blockquote)
              .run();
          },
        }),
        // ` ``` `/`~~~` (with optional language) on a hard-broken line → code block.
        new InputRule({
          find: (text) => {
            const marker = HARD_BREAK_CODE_FENCE.exec(text)?.[1];
            if (marker === undefined) return null;
            return { index: text.length - marker.length, text: marker };
          },
          handler: ({ state, range, chain, match }) => {
            const codeBlock = state.schema.nodes.codeBlock;
            const hardBreakFrom = hardBreakBefore(state, range.from);
            if (!codeBlock || hardBreakFrom === null) return;

            const language = FENCE_LANGUAGE.exec(match[0] ?? "")?.[1];
            chain()
              .deleteRange({ from: hardBreakFrom, to: range.to })
              .splitBlock()
              .setNode(codeBlock, language ? { language } : {})
              .run();
          },
        }),
      ];
    },
  });
}
