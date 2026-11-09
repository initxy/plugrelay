import type { MessageKey } from "@/i18n/en";

type T = (key: MessageKey, params?: Record<string, string | number>) => string;

/** A diagnostic line in plain words; `raw` is set when no wording is known and the code text is shown as is. */
export interface Worded {
  text: string;
  raw?: string;
}

const STEPS: [RegExp, MessageKey][] = [
  [/^patch: model$/, "logs.step.patchModel"],
  [/^(?:patch|params): output limit sent as (\S+)$/, "logs.step.outputLimit"],
  [/^patch: (\d+) private state items of issuer (\S+) removed$/, "logs.step.foreignState"],
  [/^params: reasoning removed$/, "logs.step.reasoningRemoved"],
  [/^params: effort (\S+) mapped to budget (\d+)$/, "logs.step.effortToBudget"],
  [/^params: budget (\d+) mapped to effort (\S+)$/, "logs.step.budgetToEffort"],
  [/^params: (\S+) removed$/, "logs.step.paramRemoved"],
  [/^prompt: sent as one user message$/, "logs.step.oneMessage"],
  [/^tools: freeform tools wrapped as function tools$/, "logs.step.freeformWrapped"],
  [/^tools: namespaced tools flattened$/, "logs.step.namespacesFlattened"],
  [/^tools: offered server tools the target cannot run removed$/, "logs.step.serverToolsRemoved"],
  [/^tools: deferred loading expanded$/, "logs.step.deferExpanded"],
  [/^tools: freeform history rewritten as function calls$/, "logs.step.freeformHistory"],
  [/^tools: server tool history rewritten as text$/, "logs.step.serverToolHistory"],
  [/^tools: namespaced history calls flattened$/, "logs.step.namespacedHistory"],
  [/^tools: flattened calls restored to their namespace$/, "logs.step.namespacesRestored"],
  [/^tools: wrapped calls restored as freeform$/, "logs.step.freeformRestored"],
  [/^content: tool result images moved after the results$/, "logs.step.imagesMoved"],
  [/^opaque: state from other issuers dropped$/, "logs.step.opaqueForeign"],
  [/^opaque: raw items dropped$/, "logs.step.opaqueRaw"],
  [/^cache: 1h breakpoints shortened to (\S+)$/, "logs.step.cacheShortened"],
  [/^cache: placed breakpoints by policy$/, "logs.step.cachePlaced"],
  [/^cache: breakpoints removed for keyed cache$/, "logs.step.cacheKeyed"],
  [/^cache: cache key set from session$/, "logs.step.cacheKeyFromSession"],
  [/^cache: breakpoints removed$/, "logs.step.cacheRemoved"],
  [/^embeddings: (\d+) inputs sent as one batch$/, "logs.step.embedBatch"],
  // A request edit made by a module; its id is not for people.
  [/^patch: \S+$/, "logs.step.patchByRule"],
];

const NOTES: [RegExp, MessageKey][] = [
  [/^client protocol cannot echo upstream state; dropped$/, "logs.note.stateNotEchoed"],
  [/^embeddings: base64 encoded by the relay$/, "logs.note.base64"],
  [/^rerank: object documents may be sent as their text$/, "logs.note.rerankText"],
  [/^rerank: documents and top_n applied by the relay/, "logs.note.rerankTopN"],
  [/^reasoning budget clamped to (\d+) below the output limit$/, "logs.note.budgetClamped"],
  [/^tool_choice (\S+) is in several namespaces; forced the first, (\S+)$/, "logs.note.toolChoice"],
];

const THINGS: Record<string, MessageKey> = {
  opaque_state: "logs.thing.opaqueState",
  tool_result_images: "logs.thing.toolResultImages",
  raw_items: "logs.thing.rawItems",
  reasoning: "logs.thing.reasoning",
  cache_ttl_1h: "logs.thing.cacheTtl",
  cache_breakpoints: "logs.thing.cacheBreakpoints",
  defer_loading: "logs.thing.deferLoading",
  server_tool_history: "logs.thing.serverToolHistory",
  compaction: "logs.thing.compaction",
  context_window: "logs.thing.contextWindow",
  conversation: "logs.thing.conversation",
  documents: "logs.thing.documents",
  fill_in_middle: "logs.thing.fillInMiddle",
  freeform_tools: "logs.thing.freeformTools",
  function_tools: "logs.thing.functionTools",
  images: "logs.thing.images",
  server_state: "logs.thing.serverState",
  embed_tokens: "logs.thing.embedTokens",
  text_generation: "logs.thing.textGeneration",
};

function byPattern(table: [RegExp, MessageKey][], s: string, t: T): Worded | undefined {
  for (const [re, key] of table) {
    const m = re.exec(s);
    if (m) return { text: t(key, { a: m[1] ?? "", b: m[2] ?? "" }) };
  }
  return undefined;
}

/** One conversion step, e.g. "tools: namespaced tools flattened". */
export function wordStep(s: string, t: T): Worded {
  return byPattern(STEPS, s, t) ?? { text: t("logs.diag.other"), raw: s };
}

export function wordNote(s: string, t: T): Worded {
  return byPattern(NOTES, s, t) ?? { text: t("logs.diag.other"), raw: s };
}

/** Something dropped or missing: a known thing, a parameter, a server tool, or a field path. */
export function wordThing(s: string, t: T): Worded {
  const known = THINGS[s];
  if (known) return { text: t(known) };
  const [kind, rest] = s.includes(":") ? [s.slice(0, s.indexOf(":")), s.slice(s.indexOf(":") + 1)] : ["", s];
  if (kind === "param") return { text: t("logs.thing.param"), raw: rest };
  if (kind === "server_tool") return { text: t("logs.thing.serverTool"), raw: rest };
  return { text: t("logs.thing.field"), raw: s };
}

/** Protocol ids as product names. */
const PROTOCOL_NAMES: Record<string, string> = {
  anthropic: "Anthropic Messages",
  "openai-chat": "OpenAI Chat",
  "openai-responses": "OpenAI Responses",
  gemini: "Gemini",
};

export function protocolName(p: string | undefined): string {
  if (!p) return "";
  return PROTOCOL_NAMES[p] ?? p;
}
