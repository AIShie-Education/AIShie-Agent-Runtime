import { describe, expect, it, vi } from "vitest";
import { probeModel, type ProbeDependencies } from "../src/probe.js";
import { model } from "./helpers.js";

function dependencies(outcome: () => unknown): ProbeDependencies {
  let now = 10;
  return {
    createEgress: () => ({ fetch: vi.fn(), close: vi.fn(async () => undefined) }),
    createProvider: (config) => ({ model: { modelId: config.model_id } as never, provider: config.provider }),
    generateText: (async () => outcome()) as ProbeDependencies["generateText"],
    now: () => { now += 123; return now; },
  };
}

describe("probeModel", () => {
  it("makes one one-token call without OpenRouter routing and returns no provider text", async () => {
    const deps = dependencies(() => ({ text: "OK" }));
    const generate = vi.spyOn(deps, "generateText");
    const result = await probeModel({ model: model({ provider: "openrouter", openrouter: { order: ["secret-upstream"] } }), timeout_ms: 1000 }, deps);
    expect(result).toEqual({ result: "ok", http_status: null, provider_code: null, latency_ms: 123 });
    expect(generate.mock.calls[0]?.[0]).toMatchObject({ maxOutputTokens: 1, maxRetries: 0 });
    expect(JSON.stringify(result)).not.toContain("secret-upstream");
  });

  it("classifies refused keys and model absence using safe codes only", async () => {
    const auth = await probeModel({ model: model(), timeout_ms: 1000 }, dependencies(() => {
      throw Object.assign(new Error("sk-ant-secret is invalid"), { statusCode: 401, responseBody: JSON.stringify({ error: { code: "invalid_api_key", message: "sk-ant-secret" } }) });
    }));
    expect(auth).toEqual({ result: "key_refused", http_status: 401, provider_code: "invalid_api_key", latency_ms: 123 });
    expect(JSON.stringify(auth)).not.toContain("sk-ant-secret");

    const missing = await probeModel({ model: model(), timeout_ms: 1000 }, dependencies(() => {
      throw Object.assign(new Error("raw upstream text"), { statusCode: 404, data: { error: { type: "model_not_found" } } });
    }));
    expect(missing.result).toBe("model_not_found");
    expect(missing.provider_code).toBe("model_not_found");
  });

  it("classifies other provider answers as key accepted and network/timeout as unreachable", async () => {
    const limited = await probeModel({ model: model(), timeout_ms: 1000 }, dependencies(() => {
      throw Object.assign(new Error("upstream"), { statusCode: 429, code: "rate_limited" });
    }));
    expect(limited.result).toBe("key_accepted");
    expect(limited.http_status).toBe(429);
    const network = await probeModel({ model: model(), timeout_ms: 1000 }, dependencies(() => { throw new Error("secret DNS detail"); }));
    expect(network).toEqual({ result: "unreachable", http_status: null, provider_code: null, latency_ms: 123 });
    expect(JSON.stringify(network)).not.toContain("DNS");
  });

  it("reads authentication status through an SDK wrapper cause", async () => {
    const result = await probeModel({ model: model(), timeout_ms: 1000 }, dependencies(() => {
      throw new Error("MCP wrapper", { cause: Object.assign(new Error("HTTP failure"), { statusCode: 401 }) });
    }));
    expect(result.result).toBe("key_refused");
    expect(result.http_status).toBe(401);
  });

  it("drops provider codes that could contain a secret", async () => {
    const result = await probeModel({ model: model(), timeout_ms: 1000 }, dependencies(() => {
      throw Object.assign(new Error("upstream"), { statusCode: 400, code: "bad code with secret" });
    }));
    expect(result.provider_code).toBeNull();
    expect(result.result).toBe("key_accepted");
  });
});
