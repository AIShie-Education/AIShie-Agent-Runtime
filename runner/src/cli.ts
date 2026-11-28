import { randomUUID } from "node:crypto";
import readline from "node:readline";
import { executeRun } from "./run.js";
import { RunRequestSchema, type ModelConfig, type RunMessage } from "./types.js";

interface Options {
  core: string;
  token: string;
  course: string;
  adapter: ModelConfig["adapter"];
  provider: string;
  model: string;
  baseURL: string | null;
  apiKey: string;
  region: string;
  allowWrites: boolean;
  tools: string[];
  timeout: number;
}

function value(args: string[], name: string): string | undefined {
  const index = args.indexOf(name);
  return index >= 0 ? args[index + 1] : undefined;
}

function adapterFor(value: string): { adapter: ModelConfig["adapter"]; provider: string } {
  switch (value) {
    case "anthropic": return { adapter: "anthropic", provider: "anthropic" };
    case "openai_responses": return { adapter: "openai_responses", provider: "openai" };
    case "openai_chat": case "openai": return { adapter: "openai_chat", provider: "openai" };
    case "gemini": return { adapter: "gemini", provider: "gemini" };
    case "bedrock_converse": case "bedrock": return { adapter: "bedrock_converse", provider: "bedrock" };
    default: return { adapter: "openai_chat", provider: value };
  }
}

function parseOptions(args: string[]): Options {
  if (args.includes("--help") || args.includes("-h")) {
    process.stdout.write(`Usage: npm run dev:chat -- --core http://127.0.0.1:8080 --token ais_... --course <uuid> [options]\n\n` +
      `Options: --provider anthropic|openai|openai_responses|gemini|bedrock|openrouter|deepseek|... (default anthropic)\n` +
      `         --model <model-id> --base-url <url> --region <aws-region>\n` +
      `         --tools <comma-separated MCP names> --allow-writes --timeout <ms>\n` +
      `API keys come from the provider's usual environment variable, or --api-key (not recommended).\n`);
    process.exit(0);
  }
  const selected = adapterFor(value(args, "--provider") ?? "anthropic");
  const core = (value(args, "--core") ?? "http://127.0.0.1:8080").replace(/\/+$/, "");
  const token = value(args, "--token") ?? process.env.AISHIE_CORE_TOKEN ?? "";
  const course = value(args, "--course") ?? "";
  if (!token || !course) throw new Error("--token and --course are required");
  const defaults: Record<string, string> = {
    anthropic: "claude-sonnet-4-5",
    openai: "gpt-5.4",
    gemini: "gemini-3.1-pro-preview",
    bedrock: "anthropic.claude-sonnet-4-5-v1:0",
    openrouter: "anthropic/claude-sonnet-4-5",
  };
  return {
    core: `${core.replace(/\/$/, "")}${core.endsWith("/mcp") ? "" : "/mcp"}`,
    token, course,
    adapter: selected.adapter,
    provider: selected.provider,
    model: value(args, "--model") ?? defaults[selected.provider] ?? "default",
    baseURL: value(args, "--base-url") ?? null,
    apiKey: value(args, "--api-key") ?? process.env[`${selected.provider.toUpperCase().replace(/[^A-Z0-9]/g, "_")}_API_KEY`] ??
      (selected.adapter === "anthropic" ? process.env.ANTHROPIC_API_KEY : selected.adapter === "gemini" ? process.env.GOOGLE_GENERATIVE_AI_API_KEY : process.env.OPENAI_API_KEY) ?? "",
    region: value(args, "--region") ?? "",
    allowWrites: args.includes("--allow-writes"),
    tools: (value(args, "--tools") ?? "course_get,course_materials_search,document_list,document_get,document_text").split(",").map((tool) => tool.trim()).filter(Boolean),
    timeout: Number(value(args, "--timeout") ?? "120000"),
  };
}

async function main(): Promise<void> {
  const options = parseOptions(process.argv.slice(2));
  const history: RunMessage[] = [];
  const rl = readline.createInterface({ input: process.stdin, output: process.stdout, prompt: "you> " });
  process.stdout.write("Local AIShie chat. /quit exits; /clear starts a new conversation.\n");
  rl.prompt();
  for await (const line of rl) {
    const text = line.trim();
    if (text === "/quit" || text === "/exit") break;
    if (text === "/clear") { history.length = 0; rl.prompt(); continue; }
    if (!text) { rl.prompt(); continue; }
    history.push({ role: "user", content: text });
    const request = RunRequestSchema.parse({
      core_mcp_url: options.core,
      core_token: options.token,
      course_id: options.course,
      conversation_id: randomUUID(),
      message_id: randomUUID(),
      attempt: 1,
      allow_writes: options.allowWrites,
      allowed_tools: options.tools,
      denied_tools: [],
      seat: { self_member_id: "", principal_member_id: "", opener_member_id: "", action_decide_level: "confirm_required" },
      system_prompt: "Answer the user's course question. Use only the offered tools; never invent course material.",
      messages: history,
      model: {
        adapter: options.adapter,
        provider: options.provider,
        model_id: options.model,
        base_url: options.baseURL,
        api_key: options.apiKey,
        region: options.region,
        hosted: false,
        max_output_tokens: 4000,
      },
      limits: { timeout_ms: options.timeout, max_writes: options.allowWrites ? 5 : 0 },
    });
    let answer = "";
    try {
      await executeRun(request, (event) => {
        switch (event.type) {
          case "text_delta": answer += event.delta; process.stdout.write(event.delta); break;
          case "tool_start": process.stdout.write(`\n[tool ${event.name}]\n`); break;
          case "tool_end": if (event.target) process.stdout.write(`[read ${event.target}]\n`); break;
          case "error": process.stdout.write(`\n[${event.kind}: ${event.message}]\n`); break;
        }
      });
      process.stdout.write("\n");
      if (answer) history.push({ role: "assistant", content: answer });
    } catch {
      process.stdout.write("\n[The run ended before an answer was completed.]\n");
    }
    rl.prompt();
  }
  rl.close();
}

main().catch((error: unknown) => {
  const message = error instanceof Error && !/key|token|ais_/i.test(error.message) ? error.message : "The chat could not start.";
  process.stderr.write(`${message}\n`);
  process.exitCode = 1;
});
