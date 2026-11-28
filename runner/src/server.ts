import http, { type IncomingMessage, type ServerResponse } from "node:http";
import { isIP } from "node:net";
import { errorEvent } from "./errors.js";
import { probeModel, defaultProbeDependencies, type ProbeDependencies } from "./probe.js";
import { defaultRunDependencies, executeRun, type RunDependencies } from "./run.js";
import { ProbeRequestSchema, RunRequestSchema, type RunnerEvent } from "./types.js";

const MAX_BODY_BYTES = 40 * 1024 * 1024;

export interface RunnerHTTPDependencies {
  run: RunDependencies;
  probe: ProbeDependencies;
}

export interface RunnerHTTPServer {
  server: http.Server;
  close(): Promise<void>;
}

function json(response: ServerResponse, status: number, value: unknown): void {
  const body = JSON.stringify(value);
  response.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "content-length": Buffer.byteLength(body),
    "cache-control": "no-store",
  });
  response.end(body);
}

function loopback(request: IncomingMessage): boolean {
  const address = request.socket.remoteAddress?.replace(/^::ffff:/, "") ?? "";
  return address === "127.0.0.1" || address === "::1" || (isIP(address) === 4 && address.startsWith("127."));
}

async function body(request: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  let bytes = 0;
  for await (const chunk of request) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    bytes += buffer.length;
    if (bytes > MAX_BODY_BYTES) throw new Error("body too large");
    chunks.push(buffer);
  }
  if (chunks.length === 0) throw new Error("empty body");
  return JSON.parse(Buffer.concat(chunks).toString("utf8"));
}

function ndjson(response: ServerResponse, event: RunnerEvent): void {
  if (!response.destroyed && !response.writableEnded) response.write(`${JSON.stringify(event)}\n`);
}

export function createRunnerHTTPServer(dependencies: RunnerHTTPDependencies = {
  run: defaultRunDependencies,
  probe: defaultProbeDependencies,
}): RunnerHTTPServer {
  const active = new Set<AbortController>();
  let closing = false;
  const server = http.createServer((request, response) => {
    void handle(request, response).catch(() => {
      if (!response.headersSent) json(response, 500, { error: { code: "internal", message: "The runner could not handle this request." } });
      else if (!response.writableEnded) response.end();
    });
  });

  async function handle(request: IncomingMessage, response: ServerResponse): Promise<void> {
    if (!loopback(request)) { json(response, 403, { error: { code: "forbidden", message: "Loopback only." } }); return; }
    const path = new URL(request.url ?? "/", "http://127.0.0.1").pathname;
    if (path === "/healthz") {
      if (request.method !== "GET") { json(response, 405, { error: { code: "method_not_allowed", message: "Method not allowed." } }); return; }
      json(response, 200, { status: "ok" });
      return;
    }
    if (path !== "/run" && path !== "/probe") { json(response, 404, { error: { code: "not_found", message: "Not found." } }); return; }
    if (request.method !== "POST") { json(response, 405, { error: { code: "method_not_allowed", message: "Method not allowed." } }); return; }
    if (closing) { json(response, 503, { error: { code: "unavailable", message: "The runner is stopping." } }); return; }
    let input: unknown;
    try { input = await body(request); } catch { json(response, 400, { error: { code: "bad_request", message: "The request body is not valid JSON." } }); return; }
    if (path === "/probe") {
      const parsed = ProbeRequestSchema.safeParse(input);
      if (!parsed.success) { json(response, 400, { error: { code: "bad_request", message: "The probe request is invalid." } }); return; }
      const result = await probeModel(parsed.data, dependencies.probe);
      json(response, 200, result);
      return;
    }
    const parsed = RunRequestSchema.safeParse(input);
    if (!parsed.success) { json(response, 400, { error: { code: "bad_request", message: "The run request is invalid." } }); return; }
    const controller = new AbortController();
    active.add(controller);
    const abort = () => { if (!response.writableEnded) controller.abort(); };
    request.once("aborted", abort);
    response.once("close", abort);
    response.writeHead(200, {
      "content-type": "application/x-ndjson; charset=utf-8",
      "cache-control": "no-store",
      "x-accel-buffering": "no",
      "transfer-encoding": "chunked",
    });
    response.flushHeaders();
    try {
      await executeRun(parsed.data, (event) => ndjson(response, event), controller.signal, dependencies.run);
    } catch (error) {
      if (!controller.signal.aborted || !response.destroyed) {
        ndjson(response, errorEvent(error, { aborted: controller.signal.aborted, timedOut: false }));
      }
    } finally {
      active.delete(controller);
      request.removeListener("aborted", abort);
      response.removeListener("close", abort);
      if (!response.writableEnded) response.end();
    }
  }

  return {
    server,
    async close() {
      closing = true;
      for (const controller of active) controller.abort();
      await new Promise<void>((resolve, reject) => {
        server.close((error) => error ? reject(error) : resolve());
        server.closeIdleConnections();
      });
    },
  };
}

export function startServer(): RunnerHTTPServer {
  const port = Number(process.env.RUNNER_PORT ?? "3001");
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error("RUNNER_PORT must be a valid TCP port");
  const runner = createRunnerHTTPServer();
  runner.server.listen(port, "127.0.0.1", () => {
    process.stdout.write(`AIShie loopback runner listening on http://127.0.0.1:${port}\n`);
  });
  const shutdown = () => { void runner.close().finally(() => { process.exitCode = 0; }); };
  process.once("SIGTERM", shutdown);
  process.once("SIGINT", shutdown);
  return runner;
}
