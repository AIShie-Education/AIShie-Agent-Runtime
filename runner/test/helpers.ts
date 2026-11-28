import { vi } from "vitest";
import type { ToolSet } from "ai";
import { RunRequestSchema, type CoreEnvelope, type MCPClientLike, type MCPToolDefinition, type ModelConfig, type RunRequest } from "../src/types.js";

export function model(overrides: Partial<ModelConfig> = {}): ModelConfig {
  return {
    adapter: "anthropic",
    provider: "anthropic",
    model_id: "claude-sonnet-4-5",
    base_url: null,
    api_key: "secret-provider-key",
    region: "",
    headers: {},
    openrouter: null,
    max_output_tokens: 4000,
    temperature: 0.3,
    top_p: null,
    reasoning_effort: "",
    hosted: true,
    ...overrides,
  };
}

export function request(overrides: Record<string, unknown> = {}): RunRequest {
  return RunRequestSchema.parse({
    core_mcp_url: "http://127.0.0.1:8080/mcp",
    core_token: "ais_secret_token",
    course_id: "course-1",
    conversation_id: "conversation-1",
    message_id: "message-1",
    attempt: 1,
    allow_writes: true,
    allowed_tools: ["course_get", "course_materials_search", "document_get", "document_text", "document_create", "member_update_perms", "member_update_perms_bulk", "action_decide"],
    denied_tools: [],
    seat: {
      self_member_id: "self",
      principal_member_id: "principal",
      opener_member_id: "opener",
      action_decide_level: "confirm_required",
    },
    system_prompt: "system",
    messages: [{ role: "user", content: "question" }],
    model: model(),
    limits: { max_steps: 8, max_tool_calls: 12, max_writes: 5, timeout_ms: 1000, max_result_bytes: 32768 },
    ...overrides,
  });
}

export function definition(name: string, readOnly = true, schema?: Record<string, unknown>): MCPToolDefinition {
  return {
    name,
    description: `${name} description`,
    annotations: { readOnlyHint: readOnly },
    inputSchema: schema ?? {
      type: "object",
      properties: {
        course_id: { type: "string" },
        idempotency_key: { type: "string" },
        revises: { type: "string" },
        member_id: { type: "string" },
        role: { type: "string" },
        title: { type: "string" },
        document_id: { type: "string" },
        version_id: { type: "string" },
        file_id: { type: "string" },
        part: { type: "integer" },
        query: { type: "string" },
      },
      required: [],
      additionalProperties: true,
    },
  };
}

export class FakeMCP implements MCPClientLike {
  readonly calls: { name: string; arguments: Record<string, unknown> }[] = [];
  readonly close = vi.fn(async () => undefined);
  constructor(
    public tools: MCPToolDefinition[] = [],
    public handler: (name: string, args: Record<string, unknown>) => unknown | Promise<unknown> = () => ({ status: "executed", result: {} }),
  ) {}
  async listTools(options?: { params?: { cursor?: string } }) {
    void options;
    return { tools: this.tools };
  }
  async callTool(args: { name: string; arguments?: Record<string, unknown> }) {
    const input = args.arguments ?? {};
    this.calls.push({ name: args.name, arguments: input });
    const result = await this.handler(args.name, input);
    if (result instanceof Error) throw result;
    return { structuredContent: result, content: [{ type: "text", text: JSON.stringify(result) }] };
  }
}

export function envelope(status: string, result?: unknown): CoreEnvelope {
  return { status, ...(result === undefined ? {} : { result }) };
}

export async function execute(tools: ToolSet, name: string, input: unknown, id = "call-1"): Promise<string> {
  const selected = tools[name] as unknown as {
    execute: (input: unknown, options: { toolCallId: string }) => Promise<string>;
  };
  return selected.execute(input, { toolCallId: id });
}

export function documentResult(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "doc-1",
    kind: "material",
    title: "Week 3 Slides",
    status: "active",
    version: {
      id: "version-1",
      body_md: "Week 3 body",
      published: true,
      files: [],
    },
    ...overrides,
  };
}
