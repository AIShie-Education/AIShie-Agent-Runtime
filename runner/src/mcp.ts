import { createMCPClient } from "@ai-sdk/mcp";
import { RunnerError } from "./errors.js";
import { asRecord } from "./json.js";
import type { CoreEnvelope, MCPClientLike, RunRequest } from "./types.js";

export async function connectMCP(
  request: Pick<RunRequest, "core_mcp_url" | "core_token">,
  fetchImpl: typeof globalThis.fetch,
  signal?: AbortSignal,
): Promise<MCPClientLike> {
  const client = await createMCPClient({
    transport: {
      type: "http",
      url: request.core_mcp_url,
      headers: { Authorization: `Bearer ${request.core_token}` },
      redirect: "error",
      initialProtocolVersion: "2025-11-25",
      terminateSessionOnClose: false,
      fetch: fetchImpl,
    },
    protocolVersionDiscovery: false,
    initializationOptions: signal ? { signal } : {},
    maxRetries: 0,
    clientName: "aishie-loopback-runner",
    version: "0.1.0",
    onUncaughtError: () => { /* Never log a server message that may echo a credential. */ },
  });
  return client as unknown as MCPClientLike;
}

export function envelopeOf(result: unknown): CoreEnvelope {
  const direct = asRecord(result);
  const structured = asRecord(direct?.structuredContent);
  if (structured && typeof structured.status === "string") return structured as unknown as CoreEnvelope;
  if (direct && typeof direct.status === "string") return direct as unknown as CoreEnvelope;
  const toolResult = asRecord(direct?.toolResult);
  if (toolResult && typeof toolResult.status === "string") return toolResult as unknown as CoreEnvelope;
  const content = direct?.content;
  if (Array.isArray(content)) {
    for (const part of content) {
      const text = asRecord(part)?.text;
      if (typeof text !== "string") continue;
      try {
        const envelope = asRecord(JSON.parse(text));
        if (envelope && typeof envelope.status === "string") return envelope as unknown as CoreEnvelope;
      } catch { /* Try the next MCP content part. */ }
    }
  }
  throw new RunnerError("provider_failed", "Core returned a result without an envelope.", { retryable: true });
}

export async function callCore(
  client: MCPClientLike,
  name: string,
  args: Record<string, unknown>,
  signal?: AbortSignal,
): Promise<CoreEnvelope> {
  const result = await client.callTool({ name, arguments: args, options: signal ? { signal } : {} });
  return envelopeOf(result);
}
