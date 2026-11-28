import dns, { type LookupOptions } from "node:dns";
import { BlockList, isIP } from "node:net";
import { Agent, ProxyAgent, fetch as undiciFetch, interceptors, type Dispatcher } from "undici";
import { RunnerError } from "./errors.js";

const blocked = new BlockList();
for (const [subnet, prefix] of [
  ["0.0.0.0", 8], ["10.0.0.0", 8], ["100.64.0.0", 10], ["127.0.0.0", 8],
  ["169.254.0.0", 16], ["172.16.0.0", 12], ["192.0.0.0", 24], ["192.0.2.0", 24],
  ["192.168.0.0", 16], ["198.18.0.0", 15], ["198.51.100.0", 24], ["203.0.113.0", 24],
  ["224.0.0.0", 4], ["240.0.0.0", 4],
] as const) blocked.addSubnet(subnet, prefix, "ipv4");
for (const [subnet, prefix] of [
  ["::", 128], ["::1", 128], ["fc00::", 7], ["fe80::", 10], ["ff00::", 8], ["2001:db8::", 32],
] as const) blocked.addSubnet(subnet, prefix, "ipv6");

export function isPrivateAddress(address: string): boolean {
  let ip = address.toLowerCase().replace(/^\[|\]$/g, "").split("%")[0]!;
  if (ip.startsWith("::ffff:") && isIP(ip.slice(7)) === 4) ip = ip.slice(7);
  const family = isIP(ip);
  return family === 4 ? blocked.check(ip, "ipv4") : family === 6 ? blocked.check(ip, "ipv6") : false;
}

export function isPrivateHostname(hostname: string): boolean {
  const host = hostname.toLowerCase().replace(/\.$/, "");
  return host === "localhost" || host.endsWith(".localhost") || host.endsWith(".local") ||
    host.endsWith(".internal") || isPrivateAddress(host);
}

export function parseEndpoint(raw: string, hosted: boolean): URL {
  let url: URL;
  try { url = new URL(raw); } catch { throw new RunnerError("bad_request", "The endpoint is not a valid URL."); }
  if (url.protocol !== "https:" && !(url.protocol === "http:" && !hosted)) {
    throw new RunnerError("bad_request", "A hosted model endpoint must use HTTPS.");
  }
  if (url.username || url.password || url.hash) {
    throw new RunnerError("bad_request", "The endpoint must not contain credentials or a fragment.");
  }
  if (hosted && isPrivateHostname(url.hostname)) {
    throw new RunnerError("bad_request", "A hosted model endpoint must be on a public address.");
  }
  return url;
}

export interface EgressOptions {
  hosted: boolean;
  baseURL?: string | null;
  openrouter?: Record<string, unknown> | null;
  proxy?: string | null;
}

export interface Egress {
  fetch: typeof globalThis.fetch;
  close(): Promise<void>;
}

export function createEgress(options: EgressOptions): Egress {
  if (options.baseURL) parseEndpoint(options.baseURL, options.hosted);
  let dispatcher: Dispatcher;
  const guardDNS = interceptors.dns({
    lookup(origin: URL, dnsOptions: LookupOptions, callback: (err: NodeJS.ErrnoException | null, addresses: { address: string; ttl: number; family: 4 | 6 }[]) => void) {
      const hostname = origin.hostname.replace(/^\[|\]$/g, "");
      if (options.hosted && isPrivateHostname(hostname)) {
        callback(new Error("blocked endpoint"), []);
        return;
      }
      if (isIP(hostname)) {
        callback(null, [{ address: hostname, ttl: 60, family: isIP(hostname) as 4 | 6 }]);
        return;
      }
      dns.lookup(hostname, { ...dnsOptions, all: true, verbatim: true }, (error, addresses) => {
        if (error) { callback(error, []); return; }
        const records = Array.isArray(addresses) ? addresses : [addresses];
        if (options.hosted && records.some(({ address }) => isPrivateAddress(address))) {
          callback(new Error("blocked endpoint address"), []);
          return;
        }
        callback(null, records.map(({ address, family }) => ({ address, family: family as 4 | 6, ttl: 60 })));
      });
    },
  });
  const agentOptions = { interceptors: { Client: [guardDNS] } };
  const proxy = options.proxy === undefined ? (process.env.EGRESS_PROXY ?? "") : (options.proxy ?? "");
  if (proxy) {
    const proxyURL = parseEndpoint(proxy, false);
    if (proxyURL.protocol !== "http:" && proxyURL.protocol !== "https:") {
      throw new RunnerError("bad_request", "EGRESS_PROXY must use HTTP or HTTPS.");
    }
    dispatcher = new ProxyAgent({ uri: proxyURL.toString(), ...agentOptions });
  } else {
    dispatcher = new Agent(agentOptions);
  }

  const fetchImpl = (async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const requestURL = input instanceof Request ? input.url : String(input);
    const url = parseEndpoint(requestURL, options.hosted);
    let nextInit = init;
    if (options.openrouter && Object.keys(options.openrouter).length > 0) {
      let body: string | undefined;
      if (typeof init?.body === "string") body = init.body;
      else if (input instanceof Request) body = await input.clone().text();
      if (body !== undefined) {
        try {
          const parsed: unknown = JSON.parse(body);
          if (parsed !== null && typeof parsed === "object" && !Array.isArray(parsed)) {
            const routed = { ...parsed, provider: options.openrouter } as Record<string, unknown>;
            if (typeof routed.reasoning_effort === "string") {
              const reasoning = routed.reasoning !== null && typeof routed.reasoning === "object" && !Array.isArray(routed.reasoning)
                ? routed.reasoning as Record<string, unknown> : {};
              routed.reasoning = { ...reasoning, effort: routed.reasoning_effort };
              delete routed.reasoning_effort;
            }
            nextInit = { ...init, body: JSON.stringify(routed) };
          }
        } catch { /* Non-JSON requests are left byte-for-byte unchanged. */ }
      }
    }
    const response = await undiciFetch(input as string, { ...nextInit, redirect: "manual", dispatcher } as Parameters<typeof undiciFetch>[1]);
    if (response.status >= 300 && response.status <= 399) {
      await response.body?.cancel();
      throw new RunnerError("provider_failed", "The provider redirected the request; redirects are not followed.", {
        retryable: false,
        httpStatus: response.status,
      });
    }
    // Parsing above pins the URL policy; dispatcher DNS checks pin the connected address.
    void url;
    return response as unknown as Response;
  }) as typeof globalThis.fetch;

  return {
    fetch: fetchImpl,
    async close() { await dispatcher.close(); },
  };
}
