import { generateText } from "ai";
import { classifyError, providerCode, statusOf } from "./errors.js";
import { createEgress, type Egress, type EgressOptions } from "./net.js";
import { createProvider, type ProviderBundle } from "./providers.js";
import type { ModelConfig, ProbeRequest, ProbeResult } from "./types.js";

export type ProbeCallOptions = Parameters<typeof generateText>[0];
export interface ProbeDependencies {
  createEgress: (options: EgressOptions) => Egress;
  createProvider: (config: ModelConfig, fetchImpl: typeof globalThis.fetch) => ProviderBundle;
  generateText: (options: ProbeCallOptions) => Promise<unknown>;
  now: () => number;
}

export const defaultProbeDependencies: ProbeDependencies = {
  createEgress,
  createProvider,
  generateText: (options) => generateText(options),
  now: () => performance.now(),
};

export async function probeModel(
  request: ProbeRequest,
  dependencies: ProbeDependencies = defaultProbeDependencies,
): Promise<ProbeResult> {
  const started = dependencies.now();
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), request.timeout_ms);
  let egress: Egress | undefined;
  try {
    egress = dependencies.createEgress({
      hosted: request.model.hosted,
      baseURL: request.model.base_url,
      openrouter: null, // Upstream routing is never part of a credential trial.
    });
    const provider = dependencies.createProvider(request.model, egress.fetch);
    await dependencies.generateText({
      model: provider.model,
      prompt: "Reply with the word OK.",
      maxOutputTokens: 1,
      maxRetries: 0,
      abortSignal: controller.signal,
      providerOptions: provider.providerOptions,
    });
    return { result: "ok", http_status: null, provider_code: null, latency_ms: elapsed(dependencies, started) };
  } catch (error) {
    const status = statusOf(error);
    const code = providerCode(error);
    const classified = classifyError(error, { timedOut: controller.signal.aborted });
    let result: ProbeResult["result"];
    if (status === 401 || status === 403 || classified.kind === "unauthenticated") result = "key_refused";
    else if (status === 404 || (code !== null && /model_not_found|model_not_exist|NotFound/.test(code))) result = "model_not_found";
    else if (classified.kind === "timeout" || status === null || (status >= 300 && status <= 399)) result = "unreachable";
    else result = "key_accepted";
    return { result, http_status: status, provider_code: code, latency_ms: elapsed(dependencies, started) };
  } finally {
    clearTimeout(timer);
    if (egress) await egress.close().catch(() => undefined);
  }
}

function elapsed(dependencies: ProbeDependencies, started: number): number {
  return Math.max(0, Math.round(dependencies.now() - started));
}
