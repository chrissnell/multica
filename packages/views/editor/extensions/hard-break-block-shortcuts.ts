import { Extension, InputRule } from "@tiptap/core";
import { Plugin, PluginKey } from "@tiptap/pm/state";
import type { EditorState } from "@tiptap/pm/state";

/**
 * Block-markdown shortcuts that only fire at a block start, made to work on any
 * line and on the Enter key.
 *
 * StarterKit's blockquote / code-block input rules are anchored with `^`, so
 * they only fire at the START of a text block, and the code-block rule only
 * fires on a trailing space (` ``` `). Two things fall through the cracks:
 *
 * 1. After a hard break (Shift+Enter) the `> ` / ` ``` ` markers sit inside a
 *    paragraph, not at a block start, so they stay literal text and are escaped
 *    on serialize (`&gt;`, `` \` ``) — the quote/fence never renders. In a
 *    composer where Enter sends (issue comments, chat) every line past the first
 *    is a hard break, so the visible symptom is "only the first line works".
 *
 * 2. A code fence is normally opened by typing ` ``` ` and pressing Enter, not by
 *    typing a trailing space. Enter is a keystroke, not text input, so the input
 *    rule never sees it and the fence stays literal.
 *
 * This extension closes both gaps:
 *   - input rules that convert `> ` and ` ``` `/`~~~` (optional language) on a
 *     hard-broken line, mirroring the block-start rules; and
 *   - an Enter handler that turns a line consisting solely of ` ``` `/`~~~` into
 *     a code block, whether it is a whole block or a hard-broken line.
 *
 * A hard break renders as `\n` in the input-rule text buffer (its `toText`), and
 * `\n` cannot otherwise appear inside a single paragraph, so the leading `\n`
 * uniquely identifies the soft-break case. The matched text is only the marker
 * run (never the `\n`), so `range` spans just that run and the hard break sits
 * one position before `range.from`. Up to three leading spaces are tolerated,
 * matching CommonMark's indent tolerance.
 *
 * The extension runs at a raised priority so its Enter handler fires before the
 * submit / default-split keymaps: a bare fence line becomes a code block instead
 * of sending the comment or splitting the paragraph.
 */
const HARD_BREAK_BLOCKQUOTE = /\n([ \t]{0,3}>[ \t])$/;
const HARD_BREAK_CODE_FENCE = /\n([ \t]{0,3}(?:```|~~~)([a-z]+)?[ \t])$/;
const FENCE_LANGUAGE = /(?:```|~~~)([a-z]+)/;
const BARE_FENCE_LINE = /^[ \t]{0,3}(?:```|~~~)([a-z]*)$/;

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
    // Above the submit (100) and default-split keymaps so the Enter handler wins
    // on a bare fence line; it is a no-op on every other line.
    priority: 120,
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
        // ` ``` `/`~~~` (with optional language) followed by a space on a
        // hard-broken line → code block.
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
    addProseMirrorPlugins() {
      const { editor } = this;
      return [
        new Plugin({
          key: new PluginKey("codeFenceEnter"),
          props: {
            // Enter on a line that is only ` ``` `/`~~~` opens a code block — the
            // way fences are actually authored, not the trailing-space input rule.
            handleKeyDown(view, event) {
              if (
                event.key !== "Enter" ||
                event.shiftKey ||
                event.altKey ||
                event.metaKey ||
                event.ctrlKey
              ) {
                return false;
              }

              const { state } = view;
              const { selection } = state;
              if (!selection.empty) return false;

              const { $from } = selection;
              const parent = $from.parent;
              // Skip inside an existing code block — Enter there is a newline.
              if (!parent.isTextblock || parent.type.spec.code) return false;

              const codeBlock = state.schema.nodes.codeBlock;
              if (!codeBlock) return false;

              // Current visual line: text after the last hard break (rendered as
              // `\n`) up to the cursor. Offsets stay 1:1 with document positions.
              const before = parent.textBetween(0, $from.parentOffset, undefined, "\n");
              const lastBreak = before.lastIndexOf("\n");
              const match = BARE_FENCE_LINE.exec(before.slice(lastBreak + 1));
              if (!match) return false;

              const language = match[1] || null;
              const attrs = language ? { language } : {};
              const lineStart = $from.start() + lastBreak + 1;
              // Drop the fence run, plus the preceding hard break when the fence
              // is a continuation line rather than a whole block.
              const from = lastBreak === -1 ? lineStart : lineStart - 1;
              const chain = editor.chain().deleteRange({ from, to: $from.pos });
              (lastBreak === -1 ? chain : chain.splitBlock())
                .setNode(codeBlock, attrs)
                .run();
              return true;
            },
          },
        }),
      ];
    },
  });
}
