import { isStepCount, streamText, type ModelMessage } from "ai";
import { RunnerError } from "./errors.js";
import { connectMCP } from "./mcp.js";
import { createEgress, type Egress, type EgressOptions } from "./net.js";
import { createProvider, type ProviderBundle } from "./providers.js";
import { listOfferedTools, SecureTools } from "./tools.js";
import type { MCPClientLike, RunMessage, RunRequest, RunnerEvent, Usage } from "./types.js";

export type StreamOptions = Parameters<typeof streamText>[0];
export interface StreamPart { type: string; text?: string; error?: unknown }
export interface StreamUsage {
  inputTokens?: number;
  outputTokens?: number;
  inputTokenDetails?: { cacheReadTokens?: number; cacheWriteTokens?: number };
  outputTokenDetails?: { reasoningTokens?: number };
}
export interface StreamResultLike {
  fullStream: AsyncIterable<StreamPart>;
  text: PromiseLike<string>;
  usage: PromiseLike<StreamUsage>;
  steps: PromiseLike<unknown[]>;
  finishReason: PromiseLike<unknown>;
}

export interface RunDependencies {
  createEgress: (options: EgressOptions) => Egress;
  createProvider: (config: RunRequest["model"], fetchImpl: typeof globalThis.fetch) => ProviderBundle;
  connectMCP: (
    request: Pick<RunRequest, "core_mcp_url" | "core_token">,
    fetchImpl: typeof globalThis.fetch,
    signal?: AbortSignal,
  ) => Promise<MCPClientLike>;
  streamText: (options: StreamOptions) => StreamResultLike;
}

export const defaultRunDependencies: RunDependencies = {
  createEgress,
  createProvider,
  connectMCP,
  streamText: (options) => streamText(options),
};

function modelMessages(messages: RunMessage[]): ModelMessage[] {
  return messages.map((message) => {
    if (message.parts) {
      if (message.role === "assistant" && message.parts.some((part) => part.type === "image")) {
        throw new RunnerError("bad_request", "Assistant history cannot contain an image part.");
      }
      const content = message.parts.map((part) => part.type === "text"
        ? { type: "text" as const, text: part.text }
        : { type: "image" as const, image: part.data_base64, mediaType: part.media_type });
      return { role: message.role, content } as ModelMessage;
    }
    return { role: message.role, content: message.content ?? "" } as ModelMessage;
  });
}

function usageOf(raw: {
  inputTokens?: number;
  outputTokens?: number;
  inputTokenDetails?: { cacheReadTokens?: number; cacheWriteTokens?: number };
  outputTokenDetails?: { reasoningTokens?: number };
}): Usage {
  return {
    input: raw.inputTokens ?? 0,
    output: raw.outputTokens ?? 0,
    reasoning: raw.outputTokenDetails?.reasoningTokens ?? 0,
    cache_read: raw.inputTokenDetails?.cacheReadTokens ?? 0,
    cache_write: raw.inputTokenDetails?.cacheWriteTokens ?? 0,
  };
}

export async function executeRun(
  request: RunRequest,
  emit: (event: RunnerEvent) => void,
  externalSignal?: AbortSignal,
  dependencies: RunDependencies = defaultRunDependencies,
): Promise<void> {
  const controller = new AbortController();
  let timedOut = false;
  let fatal: unknown;
  let client: MCPClientLike | undefined;
  let modelEgress: Egress | undefined;
  let coreEgress: Egress | undefined;
  const abort = () => { if (!controller.signal.aborted) controller.abort(); };
  const onExternalAbort = () => abort();
  if (externalSignal?.aborted) abort();
  externalSignal?.addEventListener("abort", onExternalAbort, { once: true });
  const timer = setTimeout(() => { timedOut = true; abort(); }, request.limits.timeout_ms);
  const onFatal = (error: unknown) => { fatal ??= error; abort(); };
  try {
    modelEgress = dependencies.createEgress({
      hosted: request.model.hosted,
      baseURL: request.model.base_url,
      openrouter: request.model.provider === "openrouter" || detectOpenRouter(request.model) ? request.model.openrouter : null,
    });
    coreEgress = dependencies.createEgress({ hosted: false, baseURL: request.core_mcp_url, proxy: null });
    const provider = dependencies.createProvider(request.model, modelEgress.fetch);
    client = await dependencies.connectMCP(request, coreEgress.fetch, controller.signal);
    const definitions = await listOfferedTools(client, request, controller.signal);
    const secure = new SecureTools({ request, client, signal: controller.signal, emit, onFatal }, definitions);
    const tools = await secure.build();
    const result = dependencies.streamText({
      model: provider.model,
      system: request.system_prompt,
      messages: modelMessages(request.messages),
      tools,
      stopWhen: isStepCount(request.limits.max_steps),
      abortSignal: controller.signal,
      maxRetries: 0,
      toolCallConcurrency: 1,
      providerOptions: provider.providerOptions,
      maxOutputTokens: request.model.max_output_tokens,
      ...(request.model.temperature === null ? {} : { temperature: request.model.temperature }),
      ...(request.model.top_p === null ? {} : { topP: request.model.top_p }),
      onStepStart: () => emit({ type: "step", kind: "thinking" }),
      onWarning: () => { /* Provider warnings can contain request details; never log them. */ },
    });
    for await (const part of result.fullStream) {
      switch (part.type) {
        case "text-delta":
          if (part.text) emit({ type: "text_delta", delta: part.text });
          break;
        case "error":
          onFatal(part.error);
          throw part.error;
        case "abort":
          if (!timedOut && !externalSignal?.aborted && !fatal) onFatal(new RunnerError("aborted", "The run was cancelled."));
          break;
      }
    }
    if (fatal) throw fatal;
    if (timedOut) throw new RunnerError("timeout", "The model call timed out.", { retryable: true });
    if (externalSignal?.aborted) throw new RunnerError("aborted", "The run was cancelled.");
    const [text, usage, steps, finishReason] = await Promise.all([
      result.text,
      result.usage,
      result.steps,
      result.finishReason,
    ]);
    const finish = String(finishReason);
    if (/content.?filter/i.test(finish)) throw new RunnerError("content_filter", "The model refused this content.");
    const sources = secure.sources.snapshot();
    emit({
      type: "done",
      text,
      sources: sources.sources,
      sources_stated: sources.sources_stated,
      usage: usageOf(usage),
      steps: steps.length,
      tool_calls: secure.toolCalls,
      writes: { ...secure.writes },
      finish_reason: finish,
    });
  } catch (error) {
    if (timedOut) throw new RunnerError("timeout", "The model call timed out.", { retryable: true, cause: error });
    if (externalSignal?.aborted) throw new RunnerError("aborted", "The run was cancelled.", { cause: error });
    throw error;
  } finally {
    clearTimeout(timer);
    externalSignal?.removeEventListener("abort", onExternalAbort);
    const close = (closer: (() => Promise<void>) | undefined) => {
      if (!closer) return Promise.resolve();
      try { return closer(); } catch { return Promise.resolve(); }
    };
    const mcpClient = client;
    const modelDispatcher = modelEgress;
    const coreDispatcher = coreEgress;
    await Promise.allSettled([
      close(mcpClient ? () => mcpClient.close() : undefined),
      close(modelDispatcher ? () => modelDispatcher.close() : undefined),
      close(coreDispatcher ? () => coreDispatcher.close() : undefined),
    ]);
  }
}

function detectOpenRouter(model: RunRequest["model"]): boolean {
  if (!model.base_url) return false;
  try { return new URL(model.base_url).hostname.toLowerCase().endsWith("openrouter.ai"); } catch { return false; }
}
