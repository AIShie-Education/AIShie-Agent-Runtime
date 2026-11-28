import { createAmazonBedrock } from "@ai-sdk/amazon-bedrock";
import { createAnthropic } from "@ai-sdk/anthropic";
import { createGoogle } from "@ai-sdk/google";
import { createOpenAI } from "@ai-sdk/openai";
import { createOpenAICompatible } from "@ai-sdk/openai-compatible";
import type { LanguageModelV4, SharedV4ProviderOptions } from "@ai-sdk/provider";
import type { ModelConfig } from "./types.js";
import { RunnerError } from "./errors.js";

export const PROVIDERS = [
  "openai", "azure", "anthropic", "gemini", "bedrock", "deepseek", "qwen", "moonshot",
  "glm", "openrouter", "ollama", "lmstudio", "vllm", "openai_compatible",
] as const;

export function detectProvider(adapter: ModelConfig["adapter"], baseURL: string | null): string {
  if (adapter === "gemini") return "gemini";
  if (adapter === "bedrock_converse") return "bedrock";
  if (!baseURL) {
    if (adapter === "anthropic") return "anthropic";
    if (adapter === "openai_responses" || adapter === "openai_chat") return "openai";
  }
  let host = "";
  let port = "";
  try { const url = new URL(baseURL ?? ""); host = url.hostname.toLowerCase(); port = url.port; } catch { return "openai_compatible"; }
  const has = (suffix: string) => host === suffix || host.endsWith(`.${suffix}`);
  if (has("api.openai.com")) return "openai";
  if (has("openai.azure.com") || has("cognitiveservices.azure.com") || has("services.ai.azure.com")) return "azure";
  if (has("api.anthropic.com")) return "anthropic";
  if (has("generativelanguage.googleapis.com")) return "gemini";
  if (has("amazonaws.com") && host.includes("bedrock")) return "bedrock";
  if (has("api.deepseek.com")) return "deepseek";
  if (has("aliyuncs.com")) return "qwen";
  if (has("api.moonshot.ai") || has("api.moonshot.cn")) return "moonshot";
  if (has("api.z.ai") || has("open.bigmodel.cn")) return "glm";
  if (has("openrouter.ai")) return "openrouter";
  if (port === "11434") return "ollama";
  if (port === "1234" && (host === "localhost" || host === "127.0.0.1" || host === "::1")) return "lmstudio";
  if (adapter === "anthropic") return "anthropic";
  if (adapter === "openai_responses") return "openai";
  return "openai_compatible";
}

const compatibleDefaults: Record<string, string> = {
  openrouter: "https://openrouter.ai/api/v1",
  deepseek: "https://api.deepseek.com",
  qwen: "https://dashscope.aliyuncs.com/compatible-mode/v1",
  moonshot: "https://api.moonshot.ai/v1",
  glm: "https://api.z.ai/api/paas/v4",
  ollama: "http://localhost:11434/v1",
  lmstudio: "http://localhost:1234/v1",
};

function safeProviderName(provider: string): string {
  return provider.replace(/[^a-zA-Z0-9_-]/g, "_") || "openai_compatible";
}

function headersOf(config: ModelConfig): Record<string, string> {
  const headers = { ...config.headers };
  for (const [name, value] of Object.entries(headers)) {
    if (/\r|\n/.test(name) || /\r|\n/.test(value)) {
      throw new RunnerError("bad_request", "A model header contains an invalid newline.");
    }
  }
  return headers;
}

export interface ProviderBundle {
  model: LanguageModelV4;
  provider: string;
  providerOptions?: SharedV4ProviderOptions;
}

export function createProvider(config: ModelConfig, fetchImpl: typeof globalThis.fetch): ProviderBundle {
  const provider = config.provider || detectProvider(config.adapter, config.base_url);
  const baseURL = config.base_url || undefined;
  const headers = headersOf(config);
  const effort = config.reasoning_effort;
  switch (config.adapter) {
    case "anthropic": {
      const anthropic = createAnthropic({ apiKey: config.api_key || undefined, baseURL, headers, fetch: fetchImpl });
      const anthropicEffort = effort === "minimal" ? "low" : ["low", "medium", "high", "xhigh", "max"].includes(effort) ? effort : "";
      return {
        model: anthropic(config.model_id), provider,
        providerOptions: {
          anthropic: {
            sendReasoning: true,
            cacheControl: { type: "ephemeral" },
            ...(anthropicEffort ? { thinking: { type: "adaptive" }, effort: anthropicEffort } : {}),
          },
        } as SharedV4ProviderOptions,
      };
    }
    case "openai_responses": {
      const azure = provider === "azure";
      const openai = createOpenAI({
        apiKey: azure ? undefined : config.api_key || undefined,
        baseURL,
        headers: azure ? { ...headers, ...(config.api_key ? { "api-key": config.api_key } : {}) } : headers,
        fetch: fetchImpl,
      });
      return {
        model: openai.responses(config.model_id), provider,
        providerOptions: { openai: { store: false, ...(effort ? { reasoningEffort: effort } : {}) } },
      };
    }
    case "openai_chat": {
      if (provider === "openai") {
        const openai = createOpenAI({ apiKey: config.api_key || undefined, baseURL, headers, fetch: fetchImpl });
        return {
          model: openai.chat(config.model_id), provider,
          providerOptions: { openai: { store: false, ...(effort ? { reasoningEffort: effort } : {}) } },
        };
      }
      const name = safeProviderName(provider);
      const compatibleBase = baseURL ?? compatibleDefaults[provider];
      if (!compatibleBase) throw new RunnerError("bad_request", "This OpenAI-compatible provider requires base_url.");
      const azure = provider === "azure";
      const compatible = createOpenAICompatible({
        name,
        baseURL: compatibleBase,
        apiKey: azure ? undefined : config.api_key || undefined,
        headers: azure ? { ...headers, ...(config.api_key ? { "api-key": config.api_key } : {}) } : headers,
        fetch: fetchImpl,
        includeUsage: true,
      });
      return {
        model: compatible.chatModel(config.model_id), provider,
        providerOptions: { [name]: { ...(effort ? { reasoningEffort: effort } : {}) } },
      };
    }
    case "gemini": {
      const google = createGoogle({ apiKey: config.api_key || undefined, baseURL, headers, fetch: fetchImpl });
      const level = ["minimal", "low", "medium", "high", "xhigh"].includes(effort) ? effort : undefined;
      return {
        model: google(config.model_id), provider,
        providerOptions: effort ? {
          google: { thinkingConfig: { includeThoughts: true, ...(level ? { thinkingLevel: level } : { thinkingBudget: 2048 }) } },
        } as SharedV4ProviderOptions : undefined,
      };
    }
    case "bedrock_converse": {
      const bedrock = createAmazonBedrock({
        region: config.region || process.env.AWS_REGION || process.env.AWS_DEFAULT_REGION,
        apiKey: config.api_key || undefined,
        baseURL,
        headers,
        fetch: fetchImpl,
      });
      const level = ["low", "medium", "high", "xhigh", "max"].includes(effort) ? effort : undefined;
      return {
        model: bedrock(config.model_id), provider,
        providerOptions: effort ? {
          bedrock: { reasoningConfig: { type: "adaptive", ...(level ? { maxReasoningEffort: level } : {}) } },
        } as SharedV4ProviderOptions : undefined,
      };
    }
  }
}
