import { z } from "zod";

export const AdapterSchema = z.enum([
  "anthropic",
  "openai_responses",
  "openai_chat",
  "gemini",
  "bedrock_converse",
]);

export const ModelConfigSchema = z.object({
  adapter: AdapterSchema,
  provider: z.string().trim().min(1).max(64).default(""),
  model_id: z.string().trim().min(1).max(512),
  base_url: z.string().trim().max(2048).nullable().default(null),
  api_key: z.string().max(16384).default(""),
  region: z.string().trim().max(128).default(""),
  headers: z.record(z.string(), z.string()).default({}),
  openrouter: z.record(z.string(), z.unknown()).nullable().default(null),
  max_output_tokens: z.number().int().min(1).max(1_000_000).default(4000),
  temperature: z.number().min(0).max(2).nullable().default(null),
  top_p: z.number().min(0).max(1).nullable().default(null),
  reasoning_effort: z.string().trim().max(32).default(""),
  hosted: z.boolean().default(true),
}).strict();
export type ModelConfig = z.infer<typeof ModelConfigSchema>;

const TextPartSchema = z.object({ type: z.literal("text"), text: z.string().max(2_000_000) }).strict();
const ImagePartSchema = z.object({
  type: z.literal("image"),
  media_type: z.string().regex(/^image\/[a-zA-Z0-9.+-]+$/),
  data_base64: z.string().max(8_000_000),
}).strict();
export const MessageSchema = z.object({
  role: z.enum(["user", "assistant"]),
  content: z.string().max(2_000_000).optional(),
  parts: z.array(z.discriminatedUnion("type", [TextPartSchema, ImagePartSchema])).max(64).optional(),
}).strict().refine((m) => m.content !== undefined || m.parts !== undefined, "content or parts is required");
export type RunMessage = z.infer<typeof MessageSchema>;

export const SeatSchema = z.object({
  self_member_id: z.string().max(128).default(""),
  principal_member_id: z.string().max(128).default(""),
  opener_member_id: z.string().max(128).default(""),
  action_decide_level: z.string().max(32).default(""),
}).strict();
export type Seat = z.infer<typeof SeatSchema>;

export const LimitsSchema = z.object({
  max_steps: z.number().int().min(1).max(100).default(8),
  max_tool_calls: z.number().int().min(0).max(1000).default(12),
  max_writes: z.number().int().min(0).max(100).default(5),
  timeout_ms: z.number().int().min(1).max(3_600_000).default(120_000),
  max_result_bytes: z.number().int().min(1024).max(4_194_304).default(32_768),
}).strict();
export type Limits = z.infer<typeof LimitsSchema>;

export const RunRequestSchema = z.object({
  core_mcp_url: z.string().url().max(2048),
  core_token: z.string().min(1).max(16384),
  course_id: z.string().min(1).max(128),
  conversation_id: z.string().min(1).max(128),
  message_id: z.string().min(1).max(128),
  attempt: z.number().int().min(1).max(1_000_000),
  allow_writes: z.boolean().default(false),
  allowed_tools: z.array(z.string().min(1).max(128)).max(300).default([]),
  denied_tools: z.array(z.string().min(1).max(128)).max(300).default([]),
  seat: SeatSchema.prefault({}),
  system_prompt: z.string().max(2_000_000).default(""),
  messages: z.array(MessageSchema).min(1).max(200),
  model: ModelConfigSchema,
  limits: LimitsSchema.prefault({}),
}).strict();
export type RunRequest = z.infer<typeof RunRequestSchema>;

export const ProbeRequestSchema = z.union([
  z.object({ model: ModelConfigSchema, timeout_ms: z.number().int().min(1).max(60_000).default(25_000) }).strict(),
  ModelConfigSchema.transform((model) => ({ model, timeout_ms: 25_000 })),
]);
export type ProbeRequest = z.infer<typeof ProbeRequestSchema>;

export interface Source {
  document_id: string;
  version_id: string;
  file_id?: string;
  page?: number;
  slide?: number;
  part?: number;
}

export interface Usage {
  input: number;
  output: number;
  reasoning: number;
  cache_read: number;
  cache_write: number;
}

export interface WriteCounts {
  executed: number;
  proposed: number;
  denied: number;
  failed: number;
  refused: number;
  guarded: number;
}

export type RunnerEvent =
  | { type: "step"; kind: "thinking" | "writing" }
  | { type: "tool_start"; id: string; name: string }
  | { type: "tool_end"; id: string; name: string; target?: string }
  | { type: "text_delta"; delta: string }
  | {
      type: "done";
      text: string;
      sources: Source[];
      sources_stated: boolean;
      usage: Usage;
      steps: number;
      tool_calls: number;
      writes: WriteCounts;
      finish_reason: string;
    }
  | {
      type: "error";
      kind: "unauthenticated" | "content_filter" | "context_overflow" | "provider_failed" | "timeout" | "aborted" | "bad_request";
      retryable: boolean;
      http_status: number | null;
      message: string;
    };

export interface CoreError {
  code: string;
  message: string;
  details?: Record<string, unknown>;
}

export interface CoreEnvelope {
  status: string;
  action_id?: string;
  review_state?: string;
  replayed?: boolean;
  result?: unknown;
  note?: string;
  error?: CoreError;
}

export interface MCPToolDefinition {
  name: string;
  title?: string;
  description?: string;
  inputSchema: Record<string, unknown>;
  annotations?: { readOnlyHint?: boolean; [key: string]: unknown };
}

export interface MCPClientLike {
  listTools(options?: { params?: { cursor?: string }; options?: { signal?: AbortSignal } }): Promise<{
    tools: MCPToolDefinition[];
    nextCursor?: string;
  }>;
  callTool(args: {
    name: string;
    arguments?: Record<string, unknown>;
    options?: { signal?: AbortSignal };
  }): Promise<unknown>;
  close(): Promise<void>;
}

export interface ProbeResult {
  result: "ok" | "key_refused" | "model_not_found" | "key_accepted" | "unreachable";
  http_status: number | null;
  provider_code: string | null;
  latency_ms: number;
}
