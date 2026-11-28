import { MockLanguageModelV4 } from "ai/test";
import type { LanguageModelV4StreamPart } from "@ai-sdk/provider";
import { describe, expect, it, vi } from "vitest";
import { executeRun, defaultRunDependencies } from "../src/run.js";
import type { RunnerEvent } from "../src/types.js";
import { definition, FakeMCP, request } from "./helpers.js";

function stream(parts: LanguageModelV4StreamPart[]) {
  return new ReadableStream<LanguageModelV4StreamPart>({
    start(controller) {
      for (const part of parts) controller.enqueue(part);
      controller.close();
    },
  });
}

function usage(input: number, output: number) {
  return {
    inputTokens: { total: input, noCache: input - 2, cacheRead: 2, cacheWrite: 1 },
    outputTokens: { total: output, text: output - 2, reasoning: 2 },
  };
}

describe("real AI SDK v7 multi-step loop", () => {
  it("honors isStepCount (the v7 stepCountIs successor), onStepStart, and aggregate usage", async () => {
    const outcomes = [
      {
        stream: stream([
          { type: "stream-start", warnings: [] },
          { type: "tool-call", toolCallId: "call-1", toolName: "course_get", input: "{}" },
          { type: "finish", usage: usage(10, 4), finishReason: { unified: "tool-calls", raw: "tool_calls" } },
        ] as LanguageModelV4StreamPart[]),
      },
      {
        stream: stream([
          { type: "stream-start", warnings: [] },
          { type: "tool-call", toolCallId: "call-2", toolName: "course_get", input: "{}" },
          { type: "finish", usage: usage(20, 6), finishReason: { unified: "tool-calls", raw: "tool_calls" } },
        ] as LanguageModelV4StreamPart[]),
      },
      {
        stream: stream([
          { type: "stream-start", warnings: [] },
          { type: "text-start", id: "text-1" },
          { type: "text-delta", id: "text-1", delta: "must not be called" },
          { type: "text-end", id: "text-1" },
          { type: "finish", usage: usage(1000, 1000), finishReason: { unified: "stop", raw: "stop" } },
        ] as LanguageModelV4StreamPart[]),
      },
    ];
    let call = 0;
    const mock = new MockLanguageModelV4({
      provider: "mock",
      modelId: "mock-model",
      doStream: async () => outcomes[call++]!,
    });
    const client = new FakeMCP([definition("course_get")]);
    const dependencies = {
      ...defaultRunDependencies,
      createEgress: () => ({ fetch: vi.fn(), close: vi.fn(async () => undefined) }),
      createProvider: () => ({ model: mock, provider: "mock" }),
      connectMCP: async () => client,
    };
    const events: RunnerEvent[] = [];
    await executeRun(request({ limits: { max_steps: 2, max_tool_calls: 5 } }), (event) => events.push(event), undefined, dependencies);
    expect(mock.doStreamCalls).toHaveLength(2);
    expect(events.filter((event) => event.type === "step")).toHaveLength(2);
    expect(events.filter((event) => event.type === "tool_start")).toHaveLength(2);
    expect(events.at(-1)).toMatchObject({
      type: "done",
      text: "",
      steps: 2,
      tool_calls: 2,
      usage: { input: 30, output: 10, reasoning: 4, cache_read: 4, cache_write: 2 },
      finish_reason: "tool-calls",
    });
    expect(JSON.stringify(events)).not.toContain("must not be called");
  });
});
