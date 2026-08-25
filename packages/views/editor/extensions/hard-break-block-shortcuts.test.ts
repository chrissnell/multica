import { afterEach, describe, expect, it } from "vitest";
import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import CodeBlockLowlight from "@tiptap/extension-code-block-lowlight";
import { Markdown } from "@tiptap/markdown";
import { codeLowlight } from "../syntax-highlight";
import { createHardBreakBlockShortcutsExtension } from "./hard-break-block-shortcuts";

function makeEditor() {
  const element = document.createElement("div");
  document.body.appendChild(element);
  return new Editor({
    element,
    extensions: [
      StarterKit.configure({ link: false, codeBlock: false }),
      CodeBlockLowlight.configure({ lowlight: codeLowlight }),
      createHardBreakBlockShortcutsExtension(),
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
      f(view, from, to, ch, () => editor.view.state.tr),
    );
    if (!handled) view.dispatch(view.state.tr.insertText(ch, from, to));
  }
}

/**
 * Drive the ProseMirror `handleKeyDown` chain for Enter, mirroring the pattern in
 * submit-shortcut.test. `editor.commands.keyboardShortcut("Enter")` simulates the
 * shortcut and swallows a handler's own dispatch, so it can't drive this.
 */
function pressEnter(editor: Editor): boolean {
  const event = new KeyboardEvent("keydown", { key: "Enter", code: "Enter" });
  let handled = false;
  editor.view.someProp("handleKeyDown", (handler) => {
    handled = handler(editor.view, event) || false;
    return handled;
  });
  return handled;
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
    editor.commands.insertContent({ type: "hardBreak" });
    type(editor, "> quoted");

    expect(editor.getHTML()).toContain("<blockquote>");
    // Serializes as a real quote, not an escaped `&gt;` paragraph.
    expect(editor.getMarkdown().trim()).toBe("Intro\n\n> quoted");
    expect(editor.getMarkdown()).not.toContain("&gt;");
  });

  it("tolerates up to three indent spaces after the hard break", () => {
    editor = makeEditor();
    type(editor, "Intro");
    editor.commands.insertContent({ type: "hardBreak" });
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
    editor.commands.insertContent({ type: "hardBreak" });
    type(editor, "a > b ");

    expect(editor.getHTML()).not.toContain("<blockquote>");
  });

  it("does not convert at a four-space indent (CommonMark code, not a quote)", () => {
    editor = makeEditor();
    type(editor, "Intro");
    editor.commands.insertContent({ type: "hardBreak" });
    type(editor, "    > quoted");

    expect(editor.getHTML()).not.toContain("<blockquote>");
  });

  it("nests when `> ` follows a hard break inside an existing blockquote", () => {
    editor = makeEditor();
    type(editor, "> outer");
    editor.commands.insertContent({ type: "hardBreak" });
    type(editor, "> inner");

    // `>` inside a quote deepens it — a nested blockquote, per markdown.
    expect(editor.getMarkdown()).toContain("> > inner");
  });
});

describe("code fence after a hard break", () => {
  let editor: Editor | undefined;

  afterEach(() => {
    editor?.destroy();
    editor = undefined;
    document.body.innerHTML = "";
  });

  it("converts ` ``` ` typed on a hard-broken line into a code block", () => {
    editor = makeEditor();
    type(editor, "Intro");
    editor.commands.insertContent({ type: "hardBreak" });
    type(editor, "``` ");

    expect(editor.getHTML()).toContain("<pre>");
    // Serializes as a real fenced block, not an escaped `\`\`\`` paragraph.
    expect(editor.getMarkdown()).not.toContain("\\`");
  });

  it("keeps the language after ` ```lang `", () => {
    editor = makeEditor();
    type(editor, "Intro");
    editor.commands.insertContent({ type: "hardBreak" });
    type(editor, "```js ");

    expect(editor.getHTML()).toContain('class="language-js"');
    expect(editor.getMarkdown()).toContain("```js");
  });

  it("supports `~~~` tilde fences too", () => {
    editor = makeEditor();
    type(editor, "Intro");
    editor.commands.insertContent({ type: "hardBreak" });
    type(editor, "~~~ ");

    expect(editor.getHTML()).toContain("<pre>");
  });

  it("lands the caret inside the code block so typed code stays in it", () => {
    editor = makeEditor();
    type(editor, "Intro");
    editor.commands.insertContent({ type: "hardBreak" });
    type(editor, "```js ");
    type(editor, "const x = 1");

    expect(editor.getMarkdown()).toContain("```js\nconst x = 1\n```");
  });

  it("still converts ` ``` ` at the start of a block", () => {
    editor = makeEditor();
    type(editor, "``` ");

    expect(editor.getHTML()).toContain("<pre>");
  });

  it("does not convert an inline ` ``` ` mid-line", () => {
    editor = makeEditor();
    type(editor, "a ``` b ");

    expect(editor.getHTML()).not.toContain("<pre>");
  });
});

describe("opening a code fence with Enter", () => {
  let editor: Editor | undefined;

  afterEach(() => {
    editor?.destroy();
    editor = undefined;
    document.body.innerHTML = "";
  });

  it("turns a whole ` ``` ` block into a code block on Enter", () => {
    editor = makeEditor();
    type(editor, "```");
    pressEnter(editor);

    expect(editor.getHTML()).toContain("<pre>");
  });

  it("keeps the language from ` ```lang ` on Enter", () => {
    editor = makeEditor();
    type(editor, "```js");
    pressEnter(editor);

    expect(editor.getHTML()).toContain('class="language-js"');
  });

  it("opens a tilde fence on Enter", () => {
    editor = makeEditor();
    type(editor, "~~~");
    pressEnter(editor);

    expect(editor.getHTML()).toContain("<pre>");
  });

  it("opens a fence typed on a hard-broken line and Enter (the reported case)", () => {
    editor = makeEditor();
    type(editor, "here is an example");
    editor.commands.insertContent({ type: "hardBreak" });
    type(editor, "```");
    pressEnter(editor);
    type(editor, "this appears in code");

    // A real fenced block after the intro, not an escaped `\`\`\`` paragraph.
    expect(editor.getMarkdown()).toContain("```\nthis appears in code\n```");
    expect(editor.getMarkdown()).not.toContain("\\`");
  });

  it("lands the caret inside the code block on Enter", () => {
    editor = makeEditor();
    type(editor, "intro");
    editor.commands.insertContent({ type: "hardBreak" });
    type(editor, "```py");
    pressEnter(editor);
    type(editor, "x = 1");

    expect(editor.getMarkdown()).toContain("```py\nx = 1\n```");
  });

  it("leaves a normal (non-fence) Enter alone", () => {
    editor = makeEditor();
    type(editor, "hello");
    pressEnter(editor);

    expect(editor.getHTML()).not.toContain("<pre>");
    expect(editor.state.doc.childCount).toBe(2);
  });
});
