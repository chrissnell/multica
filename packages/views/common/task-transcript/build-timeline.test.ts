import { describe, expect, it } from "vitest";
import type { TaskMessagePayload } from "@multica/core/types/events";
import { appendTimelineItem, buildTimeline, coalesceTimelineItems, type TimelineItem } from "./build-timeline";

function message(seq: number, type: TaskMessagePayload["type"], content?: string): TaskMessagePayload {
  return {
    task_id: "task-1",
    issue_id: "issue-1",
    seq,
    type,
    content,
  };
}

describe("task transcript timeline", () => {
  it("merges adjacent text and thinking fragments split by streaming flushes", () => {
    const items = buildTimeline([
      message(2, "text", "world"),
      message(1, "text", "hello "),
      message(3, "thinking", "step "),
      message(4, "thinking", "one"),
    ]);

    expect(items).toEqual([
      expect.objectContaining({ seq: 1, type: "text", content: "hello world" }),
      expect.objectContaining({ seq: 3, type: "thinking", content: "step one" }),
    ]);
  });

  it("does not merge across tool or error boundaries", () => {
    const items = coalesceTimelineItems([
      { seq: 1, type: "text", content: "before" },
      { seq: 2, type: "tool_use", tool: "bash" },
      { seq: 3, type: "text", content: "after" },
      { seq: 4, type: "error", content: "failed" },
      { seq: 5, type: "text", content: "done" },
    ]);

    expect(items.map((item) => item.content ?? item.tool)).toEqual([
      "before",
      "bash",
      "after",
      "failed",
      "done",
    ]);
  });

  it("coalesces newly appended live text with the previous text item", () => {
    const existing: TimelineItem[] = [{ seq: 1, type: "text", content: "hello" }];
    const items = appendTimelineItem(existing, { seq: 2, type: "text", content: " world" });

    expect(items).toEqual([
      expect.objectContaining({ seq: 1, type: "text", content: "hello world" }),
    ]);
  });

  it("coalesces out-of-order raw text by sequence", () => {
    const existing: TimelineItem[] = [
      { seq: 1, type: "text", content: "A" },
      { seq: 3, type: "text", content: "C" },
    ];
    const items = appendTimelineItem(existing, { seq: 2, type: "text", content: "B" });

    expect(items).toEqual([
      expect.objectContaining({ seq: 1, type: "text", content: "ABC" }),
    ]);
  });

  it("carries created_at onto each item as start/end timestamps", () => {
    const items = buildTimeline([
      { task_id: "t", issue_id: "i", seq: 1, type: "text", content: "hi", created_at: "2026-06-12T18:00:00Z" },
    ]);

    const ts = Date.parse("2026-06-12T18:00:00Z");
    expect(items[0]?.startTs).toBe(ts);
    expect(items[0]?.endTs).toBe(ts);
  });

  it("spans coalesced text from the first to the last fragment timestamp", () => {
    const items = buildTimeline([
      { task_id: "t", issue_id: "i", seq: 1, type: "text", content: "a", created_at: "2026-06-12T18:00:00Z" },
      { task_id: "t", issue_id: "i", seq: 2, type: "text", content: "b", created_at: "2026-06-12T18:00:03Z" },
    ]);

    expect(items).toHaveLength(1);
    expect(items[0]?.startTs).toBe(Date.parse("2026-06-12T18:00:00Z"));
    expect(items[0]?.endTs).toBe(Date.parse("2026-06-12T18:00:03Z"));
  });

  it("extends a tool_use duration to its matching tool_result", () => {
    const items = buildTimeline([
      { task_id: "t", issue_id: "i", seq: 1, type: "tool_use", tool: "bash", created_at: "2026-06-12T18:00:00Z" },
      { task_id: "t", issue_id: "i", seq: 2, type: "tool_result", tool: "bash", output: "ok", created_at: "2026-06-12T18:00:05Z" },
    ]);

    const use = items.find((i) => i.type === "tool_use");
    expect(use?.startTs).toBe(Date.parse("2026-06-12T18:00:00Z"));
    expect(use?.endTs).toBe(Date.parse("2026-06-12T18:00:05Z"));
  });

  it("leaves timestamps undefined when the backend omits created_at", () => {
    const items = buildTimeline([message(1, "tool_use", undefined)]);
    expect(items[0]?.startTs).toBeUndefined();
    expect(items[0]?.endTs).toBeUndefined();
  });

  it("redacts secrets after adjacent chunks are coalesced", () => {
    const items = buildTimeline([
      message(1, "text", "Authorization: Bearer abc123xyz."),
      message(2, "text", "def456"),
    ]);

    expect(items[0]?.content).toBe("Authorization: Bearer [REDACTED]");
    expect(items[0]?.content).not.toContain("abc123xyz");
    expect(items[0]?.content).not.toContain("def456");
  });
});
