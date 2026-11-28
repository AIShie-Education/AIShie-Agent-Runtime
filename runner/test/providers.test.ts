import http from "node:http";
import net from "node:net";
import dns from "node:dns";
import { afterEach, describe, expect, it, vi } from "vitest";
import { createEgress, isPrivateAddress, parseEndpoint } from "../src/net.js";
import { createProvider, detectProvider } from "../src/providers.js";
import { model } from "./helpers.js";

const fetchMock = async () => new Response("{}");

describe("five provider adapters", () => {
  it("builds Anthropic with thinking and prompt cache options", () => {
    const bundle = createProvider(model({ reasoning_effort: "low" }), fetchMock);
    expect(bundle.model.modelId).toBe("claude-sonnet-4-5");
    expect(bundle.providerOptions).toMatchObject({ anthropic: { thinking: { type: "adaptive" }, effort: "low", cacheControl: { type: "ephemeral" } } });
  });

  it("builds native OpenAI Responses and Azure api-key authentication", () => {
    const native = createProvider(model({ adapter: "openai_responses", provider: "openai", model_id: "gpt-5.4", reasoning_effort: "minimal" }), fetchMock);
    expect(native.model.modelId).toBe("gpt-5.4");
    expect(native.providerOptions).toMatchObject({ openai: { reasoningEffort: "minimal", store: false } });
    const azure = createProvider(model({ adapter: "openai_responses", provider: "azure", base_url: "https://resource.openai.azure.com/openai/v1" }), fetchMock);
    expect(azure.model.modelId).toBe("claude-sonnet-4-5");
  });

  it("uses native chat only for OpenAI and openai-compatible elsewhere", () => {
    const native = createProvider(model({ adapter: "openai_chat", provider: "openai", model_id: "gpt-5.4" }), fetchMock);
    expect(native.model.modelId).toBe("gpt-5.4");
    for (const provider of ["deepseek", "qwen", "moonshot", "glm", "azure", "ollama", "lmstudio", "vllm", "openai_compatible"]) {
      const bundle = createProvider(model({
        adapter: "openai_chat", provider, base_url: ["vllm", "openai_compatible", "azure"].includes(provider) ? "https://llm.example.edu/v1" : null,
      }), fetchMock);
      expect(bundle.model.modelId).toBe("claude-sonnet-4-5");
      expect(bundle.provider).toBe(provider);
    }
  });

  it("builds Gemini thinkingConfig and Bedrock reasoning/SigV4-or-Bearer settings", () => {
    const gemini = createProvider(model({ adapter: "gemini", provider: "gemini", model_id: "gemini-3.1-pro", reasoning_effort: "minimal" }), fetchMock);
    expect(gemini.providerOptions).toMatchObject({ google: { thinkingConfig: { thinkingLevel: "minimal", includeThoughts: true } } });
    const bearer = createProvider(model({ adapter: "bedrock_converse", provider: "bedrock", model_id: "anthropic.claude-v2", region: "us-east-1", reasoning_effort: "high" }), fetchMock);
    expect(bearer.providerOptions).toMatchObject({ bedrock: { reasoningConfig: { type: "adaptive", maxReasoningEffort: "high" } } });
    const sigv4 = createProvider(model({ adapter: "bedrock_converse", provider: "bedrock", model_id: "anthropic.claude-v2", api_key: "", region: "us-east-1" }), fetchMock);
    expect(sigv4.model.modelId).toBe("anthropic.claude-v2");
  });

  it("detects provider hosts without treating lookalike domains as official", () => {
    expect(detectProvider("openai_chat", "https://api.openai.com.evil.example/v1")).toBe("openai_compatible");
    expect(detectProvider("openai_chat", "https://openrouter.ai/api/v1")).toBe("openrouter");
    expect(detectProvider("openai_chat", "http://127.0.0.1:11434/v1")).toBe("ollama");
    expect(detectProvider("bedrock_converse", null)).toBe("bedrock");
  });
});

describe("egress and SSRF protection", () => {
  const servers: http.Server[] = [];
  afterEach(async () => {
    await Promise.all(servers.splice(0).map((server) => new Promise<void>((resolve) => server.close(() => resolve()))));
  });

  it("rejects hosted private, loopback, metadata, credential, and cleartext endpoints", () => {
    for (const url of ["http://127.0.0.1:8080/v1", "https://169.254.169.254/latest", "https://10.0.0.2/v1", "https://localhost/v1", "https://user:pass@example.com/v1"]) {
      expect(() => parseEndpoint(url, true)).toThrow();
    }
    expect(() => parseEndpoint("http://127.0.0.1:8080/v1", false)).not.toThrow();
    expect(() => parseEndpoint("https://api.example.com/v1", true)).not.toThrow();
    expect(isPrivateAddress("::ffff:127.0.0.1")).toBe(true);
    expect(isPrivateAddress("100.64.0.1")).toBe(true);
    expect(isPrivateAddress("8.8.8.8")).toBe(false);
  });

  it("injects OpenRouter routing and normalizes reasoning without changing other bodies", async () => {
    let received = "";
    const server = http.createServer((request, response) => {
      request.setEncoding("utf8");
      request.on("data", (chunk) => { received += chunk; });
      request.on("end", () => { response.writeHead(200, { "content-type": "application/json" }); response.end("{}"); });
    });
    servers.push(server);
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("no address");
    const egress = createEgress({ hosted: false, openrouter: { order: ["anthropic"], allow_fallbacks: false }, proxy: null });
    await egress.fetch(`http://127.0.0.1:${address.port}/chat`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ model: "m", reasoning_effort: "low", stream: true }),
    });
    await egress.close();
    expect(JSON.parse(received)).toMatchObject({ provider: { order: ["anthropic"], allow_fallbacks: false }, reasoning: { effort: "low" }, stream: true });
    expect(JSON.parse(received).reasoning_effort).toBeUndefined();
  });

  it("routes outbound calls through EGRESS_PROXY and its CONNECT tunnel", async () => {
    const target = http.createServer((_request, response) => {
      response.writeHead(200, { "content-type": "application/json" });
      response.end("{}");
    });
    servers.push(target);
    await new Promise<void>((resolve) => target.listen(0, "127.0.0.1", resolve));
    const targetAddress = target.address();
    if (!targetAddress || typeof targetAddress === "string") throw new Error("no target address");
    let requested = "";
    const proxy = http.createServer((request, response) => {
      requested = request.url ?? "";
      response.writeHead(200, { "content-type": "application/json" });
      response.end("{}");
    });
    proxy.on("connect", (request, client, head) => {
      requested = request.url ?? "";
      const upstream = net.connect(targetAddress.port, "127.0.0.1", () => {
        client.write("HTTP/1.1 200 Connection Established\r\n\r\n");
        if (head.length) upstream.write(head);
        upstream.pipe(client);
        client.pipe(upstream);
      });
      upstream.on("error", () => client.destroy());
      client.on("error", () => upstream.destroy());
    });
    servers.push(proxy);
    await new Promise<void>((resolve) => proxy.listen(0, "127.0.0.1", resolve));
    const proxyAddress = proxy.address();
    if (!proxyAddress || typeof proxyAddress === "string") throw new Error("no proxy address");
    const previous = process.env.EGRESS_PROXY;
    process.env.EGRESS_PROXY = `http://127.0.0.1:${proxyAddress.port}`;
    try {
      const egress = createEgress({ hosted: false, baseURL: `http://127.0.0.1:${targetAddress.port}/v1` });
      const response = await egress.fetch(`http://127.0.0.1:${targetAddress.port}/v1/chat`);
      expect(response.status).toBe(200);
      await egress.close();
    } finally {
      if (previous === undefined) delete process.env.EGRESS_PROXY;
      else process.env.EGRESS_PROXY = previous;
    }
    expect(requested).toContain(String(targetAddress.port));
  });

  it("rejects a hostname whose actual DNS answer is loopback before connecting", async () => {
    const lookup = vi.spyOn(dns, "lookup").mockImplementation(((hostname: string, options: unknown, callback: unknown) => {
      void hostname;
      void options;
      (callback as (error: null, addresses: { address: string; family: number }[]) => void)(null, [{ address: "127.0.0.1", family: 4 }]);
    }) as unknown as typeof dns.lookup);
    try {
      const egress = createEgress({ hosted: true, baseURL: "https://rebind.example/v1" });
      await expect(egress.fetch("https://rebind.example/v1/chat")).rejects.toThrow(/blocked endpoint address|fetch failed/);
      expect(lookup).toHaveBeenCalled();
      await egress.close();
    } finally {
      lookup.mockRestore();
    }
  });

  it("refuses redirects instead of forwarding credentials", async () => {
    const server = http.createServer((_request, response) => { response.writeHead(302, { location: "http://127.0.0.1:9/stolen" }); response.end(); });
    servers.push(server);
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("no address");
    const egress = createEgress({ hosted: false, proxy: null });
    await expect(egress.fetch(`http://127.0.0.1:${address.port}/v1`)).rejects.toThrow(/redirected/);
    await egress.close();
  });
});
