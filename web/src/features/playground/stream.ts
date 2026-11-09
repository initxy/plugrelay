import { rawFetch } from "@/lib/api/client";
import { ApiError } from "@/lib/api/errors";
import type { ApiErrorBody, PlaygroundRequest, Usage } from "@/lib/api/types";

/** One event of a playground answer (see POST /playground). */
export type PlaygroundEvent =
  | { type: "start"; request_id?: string }
  | { type: "text" | "reasoning"; text: string }
  | {
      type: "done";
      request_id?: string;
      ok?: boolean;
      status?: number;
      ttfb_ms?: number;
      latency_ms?: number;
      usage?: Pick<Usage, "input" | "cache_read" | "cache_write_5m" | "cache_write_1h" | "output" | "reasoning">;
      error?: string;
    };

/** Splits server-sent events out of `buffer`; returns the parsed events and what is left over. */
export function parseEvents(buffer: string): { events: PlaygroundEvent[]; rest: string } {
  const events: PlaygroundEvent[] = [];
  const frames = buffer.split("\n\n");
  const rest = frames.pop() ?? "";
  for (const f of frames) {
    let name = "";
    let data = "";
    for (const line of f.split("\n")) {
      if (line.startsWith("event: ")) name = line.slice(7);
      else if (line.startsWith("data: ")) data += line.slice(6);
    }
    if (!name || !data) continue;
    try {
      events.push({ type: name, ...(JSON.parse(data) as object) } as PlaygroundEvent);
    } catch {
      // A frame that is not JSON is not ours; skip it.
    }
  }
  return { events, rest };
}

/** Sends a playground request and calls `onEvent` for each event as it arrives. */
export async function streamPlayground(req: PlaygroundRequest, onEvent: (e: PlaygroundEvent) => void, signal?: AbortSignal): Promise<void> {
  const res = await rawFetch("/api/v1/playground", {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
    body: JSON.stringify(req),
    signal,
  });
  if (!res.ok) {
    const body = (await res.json().catch(() => undefined)) as ApiErrorBody | undefined;
    throw new ApiError(res.status, body, `${res.status} ${res.statusText || "request failed"}`);
  }
  if (!res.body) {
    for (const e of parseEvents(await res.text()).events) onEvent(e);
    return;
  }
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const { events, rest } = parseEvents(buffer);
    buffer = rest;
    for (const e of events) onEvent(e);
  }
  for (const e of parseEvents(buffer + "\n\n").events) onEvent(e);
}
