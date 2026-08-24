import { afterEach, describe, expect, it } from "vitest";
import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import { Markdown } from "@tiptap/markdown";
import { createBlockquoteHardBreakExtension } from "./blockquote-hard-break";

function makeEditor() {
  const element = document.createElement("div");
  document.body.appendChild(element);
  return new Editor({
    element,
    extensions: [
      StarterKit.configure({ link: false, codeBlock: false }),
      createBlockquoteHardBreakExtension(),
      Markdown.configure({ indentation: { style: "space", size: 3 } }),
    ],
    content: "",
  });
}

/**
 * Type `text` char by char, routing each character through `handleTextInput`
 * so input rules fire — the same path a real keystroke takes. jsdom can't
 * dispatch faithful key events, so this is how input-rule behaviour is driven.
 */
function type(editor: Editor, text: string) {
  const { view } = editor;
  for (const ch of text) {
    const { from, to } = view.state.selection;
    const handled = view.someProp("handleTextInput", (f) =>
      f(view, from, to, ch),
    );
    if (!handled) view.dispatch(view.state.tr.insertText(ch, from, to));
  }
}

describe("blockquote after a hard break", () => {
  let editor: Editor | undefined;

  afterEach(() => {
    editor?.destroy();
    editor = undefined;
    document.body.innerHTML = "";
  });

  it("converts `> ` typed on a hard-broken line into a blockquote", () => {
    editor = makeEditor();
    type(editor, "Intro");
    editor.commands.setHardBreak();
    type(editor, "> quoted");

    expect(editor.getHTML()).toContain("<blockquote>");
    // Serializes as a real quote, not an escaped `&gt;` paragraph.
    expect(editor.getMarkdown().trim()).toBe("Intro\n\n> quoted");
    expect(editor.getMarkdown()).not.toContain("&gt;");
  });

  it("tolerates up to three indent spaces after the hard break", () => {
    editor = makeEditor();
    type(editor, "Intro");
    editor.commands.setHardBreak();
    type(editor, "  > quoted");

    expect(editor.getHTML()).toContain("<blockquote>");
    expect(editor.getMarkdown().trim()).toBe("Intro\n\n> quoted");
  });

  it("still converts `> ` at the start of a block (StarterKit's own rule)", () => {
    editor = makeEditor();
    type(editor, "> quoted");

    expect(editor.getHTML()).toContain("<blockquote>");
  });

  it("still converts `> ` on a new paragraph created with Enter", () => {
    editor = makeEditor();
    type(editor, "para one");
    editor.chain().splitBlock().run();
    type(editor, "> quoted");

    expect(editor.getHTML()).toContain("<blockquote>");
    expect(editor.getMarkdown().trim()).toBe("para one\n\n> quoted");
  });

  it("does not turn an inline `>` into a blockquote", () => {
    editor = makeEditor();
    type(editor, "a > b ");

    expect(editor.getHTML()).not.toContain("<blockquote>");
  });

  it("does not convert an inline `>` on a hard-broken line", () => {
    editor = makeEditor();
    type(editor, "Intro");
    editor.commands.setHardBreak();
    type(editor, "a > b ");

    expect(editor.getHTML()).not.toContain("<blockquote>");
  });

  it("does not convert at a four-space indent (CommonMark code, not a quote)", () => {
    editor = makeEditor();
    type(editor, "Intro");
    editor.commands.setHardBreak();
    type(editor, "    > quoted");

    expect(editor.getHTML()).not.toContain("<blockquote>");
  });

  it("nests when `> ` follows a hard break inside an existing blockquote", () => {
    editor = makeEditor();
    type(editor, "> outer");
    editor.commands.setHardBreak();
    type(editor, "> inner");

    // `>` inside a quote deepens it — a nested blockquote, per markdown.
    expect(editor.getMarkdown()).toContain("> > inner");
  });

  it("converts inside a list item", () => {
    editor = makeEditor();
    type(editor, "- item ");
    editor.commands.setHardBreak();
    type(editor, "> quoted");

    expect(editor.getHTML()).toContain("<blockquote>");
  });
});
