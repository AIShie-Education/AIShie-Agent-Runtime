import { afterEach, describe, expect, it, vi } from "vitest";
import { createRunnerHTTPServer, type RunnerHTTPServer } from "../src/server.js";
import type { ProbeDependencies } from "../src/probe.js";
import type { RunDependencies } from "../src/run.js";
import type { RunnerEvent } from "../src/types.js";
import { FakeMCP, model, request } from "./helpers.js";

type StreamOptions = Parameters<RunDependencies["streamText"]>[0];
const active: RunnerHTTPServer[] = [];
afterEach(async () => { await Promise.all(active.splice(0).map((runner) => runner.close())); });

function probeDeps(): ProbeDependencies {
  return {
    createEgress: () => ({ fetch: vi.fn(), close: vi.fn(async () => undefined) }),
    createProvider: () => ({ model: { modelId: "mock" } as never, provider: "mock" }),
    generateText: (async () => ({ text: "OK" })) as ProbeDependencies["generateText"],
    now: () => 123,
  };
}

function runDeps(client: FakeMCP, stream: RunDependencies["streamText"]): RunDependencies {
  return {
    createEgress: () => ({ fetch: vi.fn(), close: vi.fn(async () => undefined) }),
    createProvider: () => ({ model: { modelId: "mock" } as never, provider: "mock" }),
    connectMCP: async () => client,
    streamText: stream,
  };
}

function simpleStream(options: StreamOptions): ReturnType<RunDependencies["streamText"]> {
  return {
    fullStream: (async function* () {
      options.onStepStart?.({} as never);
      yield { type: "text-delta", id: "t", text: "hello" };
    })(),
    text: Promise.resolve("hello"),
    usage: Promise.resolve({ inputTokens: 1, outputTokens: 1 }),
    steps: Promise.resolve([{}]),
    finishReason: Promise.resolve("stop"),
  } as unknown as ReturnType<RunDependencies["streamText"]>;
}

function hangingStream(options: StreamOptions): ReturnType<RunDependencies["streamText"]> {
  return {
    fullStream: (async function* () {
      options.onStepStart?.({} as never);
      yield { type: "start" };
      await new Promise<never>((_resolve, reject) => {
        options.abortSignal?.addEventListener("abort", () => reject(new Error("provider aborted")), { once: true });
      });
    })(),
    text: Promise.resolve(""),
    usage: Promise.resolve({}),
    steps: Promise.resolve([]),
    finishReason: Promise.resolve("error"),
  } as unknown as ReturnType<RunDependencies["streamText"]>;
}

async function listen(runner: RunnerHTTPServer): Promise<string> {
  active.push(runner);
  await new Promise<void>((resolve) => runner.server.listen(0, "127.0.0.1", resolve));
  const address = runner.server.address();
  if (!address || typeof address === "string") throw new Error("no address");
  return `http://127.0.0.1:${address.port}`;
}

function runBody(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return JSON.parse(JSON.stringify(request(overrides))) as Record<string, unknown>;
}

describe("loopback HTTP server", () => {
  it("serves healthz and exact NDJSON event framing", async () => {
    const client = new FakeMCP();
    const base = await listen(createRunnerHTTPServer({ run: runDeps(client, simpleStream), probe: probeDeps() }));
    expect(await (await fetch(`${base}/healthz`)).json()).toEqual({ status: "ok" });
    const response = await fetch(`${base}/run`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(runBody()) });
    expect(response.headers.get("content-type")).toContain("application/x-ndjson");
    const events = (await response.text()).trim().split("\n").map((line) => JSON.parse(line) as RunnerEvent);
    expect(events.map((event) => event.type)).toEqual(["step", "text_delta", "done"]);
    expect(events.at(-1)).toMatchObject({ type: "done", text: "hello", sources_stated: true, finish_reason: "stop" });
    expect(client.close).toHaveBeenCalledOnce();
  });

  it("returns safe bad requests and probe results with no upstream strings", async () => {
    const base = await listen(createRunnerHTTPServer({ run: runDeps(new FakeMCP(), simpleStream), probe: probeDeps() }));
    const bad = await fetch(`${base}/run`, { method: "POST", body: "not-json" });
    expect(bad.status).toBe(400);
    expect(await bad.text()).not.toContain("ais_secret_token");
    const probe = await fetch(`${base}/probe`, { method: "POST", body: JSON.stringify({ model: model() }) });
    expect(probe.status).toBe(200);
    expect(await probe.json()).toEqual({ result: "ok", http_status: null, provider_code: null, latency_ms: 0 });
  });

  it("emits timeout as NDJSON when the model exceeds its deadline", async () => {
    const base = await listen(createRunnerHTTPServer({ run: runDeps(new FakeMCP(), hangingStream), probe: probeDeps() }));
    const response = await fetch(`${base}/run`, { method: "POST", body: JSON.stringify(runBody({ limits: { timeout_ms: 20 } })) });
    const events = (await response.text()).trim().split("\n").map((line) => JSON.parse(line) as RunnerEvent);
    expect(events.at(-1)).toMatchObject({ type: "error", kind: "timeout", retryable: true, http_status: null });
    expect(JSON.stringify(events)).not.toContain("provider aborted");
  });

  it("aborts the model and closes MCP when the HTTP client disconnects", async () => {
    const client = new FakeMCP();
    let signal: AbortSignal | undefined;
    const stream: RunDependencies["streamText"] = ((options: StreamOptions) => {
      signal = options.abortSignal ?? undefined;
      return hangingStream(options);
    }) as RunDependencies["streamText"];
    const base = await listen(createRunnerHTTPServer({ run: runDeps(client, stream), probe: probeDeps() }));
    const response = await fetch(`${base}/run`, { method: "POST", body: JSON.stringify(runBody({ limits: { timeout_ms: 5000 } })) });
    const reader = response.body?.getReader();
    expect(reader).toBeDefined();
    const first = await reader!.read();
    expect(new TextDecoder().decode(first.value)).toContain('"type":"step"');
    await reader!.cancel();
    for (let i = 0; i < 50 && !signal?.aborted; i++) await new Promise((resolve) => setTimeout(resolve, 10));
    expect(signal?.aborted).toBe(true);
    for (let i = 0; i < 50 && !client.close.mock.calls.length; i++) await new Promise((resolve) => setTimeout(resolve, 10));
    expect(client.close).toHaveBeenCalledOnce();
  });
});
