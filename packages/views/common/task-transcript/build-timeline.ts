import type { TaskMessagePayload } from "@multica/core/types/events";
import { redactSecrets } from "./redact";

/** A unified timeline entry: tool calls, thinking, text, and errors in chronological order. */
export interface TimelineItem {
  seq: number;
  type: "tool_use" | "tool_result" | "thinking" | "text" | "error";
  tool?: string;
  content?: string;
  input?: Record<string, unknown>;
  output?: string;
  /** Epoch ms when this action started (first underlying message). Undefined on older backends. */
  startTs?: number;
  /** Epoch ms when this action completed (last underlying message, or its tool_result). */
  endTs?: number;
}

function parseTs(value?: string): number | undefined {
  if (!value) return undefined;
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? undefined : ms;
}

function minTs(a?: number, b?: number): number | undefined {
  if (a === undefined) return b;
  if (b === undefined) return a;
  return Math.min(a, b);
}

function maxTs(a?: number, b?: number): number | undefined {
  if (a === undefined) return b;
  if (b === undefined) return a;
  return Math.max(a, b);
}

function canMergeStreamingText(prev: TimelineItem, next: TimelineItem): boolean {
  return (prev.type === "thinking" || prev.type === "text") && prev.type === next.type;
}

/** Merge adjacent text/thinking fragments that were split only by daemon flush timing. */
export function coalesceTimelineItems(items: TimelineItem[]): TimelineItem[] {
  const sorted = [...items].sort((a, b) => a.seq - b.seq);
  const out: TimelineItem[] = [];

  for (const item of sorted) {
    const prev = out[out.length - 1];
    if (prev && canMergeStreamingText(prev, item)) {
      out[out.length - 1] = {
        ...prev,
        content: `${prev.content ?? ""}${item.content ?? ""}`,
        startTs: minTs(prev.startTs, item.startTs),
        endTs: maxTs(prev.endTs, item.endTs),
      };
      continue;
    }
    out.push(item);
  }

  return out;
}

/**
 * Extend each tool_use's endTs to its matching tool_result so the action's
 * duration reflects how long the tool actually ran. Results are paired to the
 * nearest later tool_use of the same tool, each consumed once — a heuristic
 * that holds for the common sequential case and degrades gracefully when calls
 * interleave (a mispair only shifts a duration, never crashes).
 */
function pairToolDurations(items: TimelineItem[]): TimelineItem[] {
  const out = items.map((item) => ({ ...item }));
  for (let i = 0; i < out.length; i++) {
    const use = out[i]!;
    if (use.type !== "tool_use") continue;
    for (let j = i + 1; j < out.length; j++) {
      const res = out[j]!;
      if (res.type === "tool_result" && res.tool === use.tool && res.endTs !== undefined) {
        use.endTs = maxTs(use.endTs, res.endTs);
        break;
      }
    }
  }
  return out;
}

export function appendTimelineItem(items: TimelineItem[], item: TimelineItem): TimelineItem[] {
  return pairToolDurations(coalesceTimelineItems([...items, item]));
}

function redactTimelineItems(items: TimelineItem[]): TimelineItem[] {
  return items.map((item) => ({
    ...item,
    content: item.content ? redactSecrets(item.content) : item.content,
    output: item.output ? redactSecrets(item.output) : item.output,
  }));
}

/** Build a chronologically ordered timeline from raw task messages. */
export function buildTimeline(msgs: TaskMessagePayload[]): TimelineItem[] {
  const items: TimelineItem[] = [];
  for (const msg of msgs) {
    const ts = parseTs(msg.created_at);
    items.push({
      seq: msg.seq,
      type: msg.type,
      tool: msg.tool,
      content: msg.content,
      input: msg.input,
      output: msg.output,
      startTs: ts,
      endTs: ts,
    });
  }
  return redactTimelineItems(pairToolDurations(coalesceTimelineItems(items)));
}
