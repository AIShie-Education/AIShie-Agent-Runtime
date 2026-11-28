import { describe, expect, it, vi } from "vitest";
import { executeRun, type RunDependencies } from "../src/run.js";
import type { RunnerEvent } from "../src/types.js";
import { definition, documentResult, execute, FakeMCP, model, request } from "./helpers.js";

type StreamOptions = Parameters<RunDependencies["streamText"]>[0];

function dependencies(client: FakeMCP, stream: RunDependencies["streamText"]): RunDependencies & { closed: ReturnType<typeof vi.fn> } {
  const closed = vi.fn(async () => undefined);
  return {
    closed,
    createEgress: () => ({ fetch: vi.fn(), close: closed }),
    createProvider: () => ({ model: { modelId: "mock" } as never, provider: "mock" }),
    connectMCP: async () => client,
    streamText: stream,
  };
}

function textStream(options: StreamOptions): ReturnType<RunDependencies["streamText"]> {
  return {
    fullStream: (async function* () {
      options.onStepStart?.({} as never);
      const tools = options.tools ?? {};
      if (tools.document_get) await execute(tools, "document_get", { document_id: "doc-1", version_id: "version-1" }, "call-1");
      yield { type: "text-delta", id: "text-1", text: "answer" };
      yield { type: "finish", finishReason: "stop", rawFinishReason: "stop", totalUsage: {} };
    })(),
    text: Promise.resolve("answer"),
    usage: Promise.resolve({
      inputTokens: 100,
      outputTokens: 20,
      inputTokenDetails: { cacheReadTokens: 7, cacheWriteTokens: 3 },
      outputTokenDetails: { reasoningTokens: 5 },
    }),
    steps: Promise.resolve([{}, {}]),
    finishReason: Promise.resolve("stop"),
  } as unknown as ReturnType<RunDependencies["streamText"]>;
}

function hangingStream(options: StreamOptions): ReturnType<RunDependencies["streamText"]> {
  return {
    fullStream: (async function* () {
      options.onStepStart?.({} as never);
      yield { type: "start" };
      await new Promise<never>((_resolve, reject) => {
        const rejectAbort = () => reject(new Error("aborted upstream"));
        if (options.abortSignal?.aborted) rejectAbort();
        else options.abortSignal?.addEventListener("abort", rejectAbort, { once: true });
      });
    })(),
    text: Promise.resolve(""),
    usage: Promise.resolve({}),
    steps: Promise.resolve([]),
    finishReason: Promise.resolve("error"),
  } as unknown as ReturnType<RunDependencies["streamText"]>;
}

describe("executeRun event contract", () => {
  it("streams steps, safe tool events, text, usage, sources and write counts, then closes every client", async () => {
    const client = new FakeMCP([definition("document_get")], () => ({ status: "executed", result: documentResult() }));
    const deps = dependencies(client, (options) => textStream(options));
    const events: RunnerEvent[] = [];
    await executeRun(request(), (event) => events.push(event), undefined, deps);
    expect(events.map((event) => event.type)).toEqual(["step", "tool_start", "tool_end", "text_delta", "done"]);
    expect(events[1]).toMatchObject({ type: "tool_start", id: "call-1", name: "document_get" });
    expect(events[2]).toMatchObject({ type: "tool_end", target: "Week 3 Slides" });
    expect(events.at(-1)).toMatchObject({
      type: "done",
      text: "answer",
      sources: [{ document_id: "doc-1", version_id: "version-1" }],
      sources_stated: true,
      usage: { input: 100, output: 20, reasoning: 5, cache_read: 7, cache_write: 3 },
      steps: 2,
      tool_calls: 1,
      writes: { executed: 0, proposed: 0, denied: 0, failed: 0, refused: 0, guarded: 0 },
      finish_reason: "stop",
    });
    expect(client.close).toHaveBeenCalledOnce();
    expect(deps.closed).toHaveBeenCalledTimes(2);
  });

  it("turns the internal deadline into timeout and still closes MCP", async () => {
    const client = new FakeMCP([definition("document_get")]);
    const deps = dependencies(client, hangingStream);
    await expect(executeRun(request({ limits: { timeout_ms: 20 } }), () => undefined, undefined, deps)).rejects.toMatchObject({ kind: "timeout", retryable: true });
    expect(client.close).toHaveBeenCalledOnce();
    expect(deps.closed).toHaveBeenCalledTimes(2);
  });

  it("aborts a live provider stream on external disconnect and classifies it aborted", async () => {
    const client = new FakeMCP([definition("document_get")]);
    const deps = dependencies(client, hangingStream);
    const controller = new AbortController();
    const running = executeRun(request(), () => undefined, controller.signal, deps);
    await new Promise((resolve) => setTimeout(resolve, 20));
    controller.abort();
    await expect(running).rejects.toMatchObject({ kind: "aborted" });
    expect(client.close).toHaveBeenCalledOnce();
  });

  it("accepts lightweight image parts", async () => {
    const client = new FakeMCP([definition("document_get")]);
    let seenParts = false;
    const stream: RunDependencies["streamText"] = ((options: StreamOptions) => {
      seenParts = JSON.stringify(options.messages).includes("image/png") && JSON.stringify(options.messages).includes("aGVsbG8=");
      return textStream(options);
    }) as RunDependencies["streamText"];
    const deps = dependencies(client, stream);
    await executeRun(request({
      messages: [{ role: "user", parts: [{ type: "image", media_type: "image/png", data_base64: "aGVsbG8=" }] }],
      model: model({ hosted: false }),
    }), () => undefined, undefined, deps);
    expect(seenParts).toBe(true);
  });
});
