import { asRecord, stringField } from "./json.js";
import type { RunnerEvent } from "./types.js";

export type ErrorKind = Extract<RunnerEvent, { type: "error" }>["kind"];

export class RunnerError extends Error {
  constructor(
    readonly kind: ErrorKind,
    message: string,
    readonly options: { retryable?: boolean; httpStatus?: number | null; providerCode?: string | null; cause?: unknown } = {},
  ) {
    super(message, options.cause === undefined ? undefined : { cause: options.cause });
    this.name = "RunnerError";
  }

  get retryable(): boolean { return this.options.retryable ?? false; }
  get httpStatus(): number | null { return this.options.httpStatus ?? null; }
  get providerCode(): string | null { return this.options.providerCode ?? null; }
}

export function statusOf(error: unknown): number | null {
  if (error instanceof RunnerError) return error.httpStatus;
  const seen = new Set<unknown>();
  let current: unknown = error;
  for (let depth = 0; depth < 5 && current && !seen.has(current); depth++) {
    seen.add(current);
    const record = asRecord(current);
    for (const key of ["statusCode", "status"]) {
      const n = record?.[key];
      if (typeof n === "number" && n >= 100 && n <= 599) return n;
    }
    const response = asRecord(record?.response);
    if (typeof response?.status === "number") return response.status;
    current = record?.cause ?? record?.lastError;
  }
  return null;
}

function safeCodeOf(error: unknown): string | null {
  if (error instanceof RunnerError) return error.providerCode;
  const record = asRecord(error);
  const candidates: unknown[] = [
    record?.code,
    asRecord(record?.data)?.code,
    asRecord(record?.data)?.type,
    asRecord(asRecord(record?.data)?.error)?.code,
    asRecord(asRecord(record?.data)?.error)?.type,
  ];
  const body = record?.responseBody;
  if (typeof body === "string" && body.length <= 1_000_000) {
    try {
      const parsed = asRecord(JSON.parse(body));
      candidates.push(parsed?.code, asRecord(parsed?.error)?.code, asRecord(parsed?.error)?.type);
    } catch { /* Provider bodies are deliberately not reflected. */ }
  }
  for (const value of candidates) {
    if (typeof value === "string" && /^[A-Za-z0-9_.:-]{1,64}$/.test(value)) return value;
    if (typeof value === "number" && Number.isSafeInteger(value)) return String(value);
  }
  return null;
}

function internalText(error: unknown): string {
  if (error instanceof Error) return `${error.name} ${error.message}`;
  return String(error);
}

export function classifyError(error: unknown, state: { aborted?: boolean; timedOut?: boolean } = {}): RunnerError {
  if (error instanceof RunnerError) return error;
  if (state.timedOut) return new RunnerError("timeout", "The model call timed out.", { retryable: true });
  if (state.aborted) return new RunnerError("aborted", "The run was cancelled.", { retryable: false });
  const status = statusOf(error);
  const code = safeCodeOf(error);
  const text = internalText(error).toLowerCase();
  const common = { httpStatus: status, providerCode: code, cause: error };
  if (status === 401 || status === 403) {
    return new RunnerError("unauthenticated", "The provider or Core credential was refused.", { ...common, retryable: false });
  }
  if (/context.{0,20}(overflow|length|window)|too many tokens|maximum context/.test(`${code ?? ""} ${text}`)) {
    return new RunnerError("context_overflow", "The conversation is too large for this model.", { ...common, retryable: false });
  }
  if (/content.?filter|content.?policy|safety|guardrail|blocked.?content/.test(`${code ?? ""} ${text}`)) {
    return new RunnerError("content_filter", "The model refused this content.", { ...common, retryable: false });
  }
  if (status === 400 || status === 404 || status === 409 || status === 422) {
    return new RunnerError("bad_request", "The model request was refused.", { ...common, retryable: false });
  }
  const retryable = status === null || status === 408 || status === 409 || status === 425 || status === 429 || status >= 500;
  return new RunnerError("provider_failed", "The model provider could not complete this run.", { ...common, retryable });
}

export function errorEvent(error: unknown, state: { aborted?: boolean; timedOut?: boolean } = {}): Extract<RunnerEvent, { type: "error" }> {
  const classified = classifyError(error, state);
  return {
    type: "error",
    kind: classified.kind,
    retryable: classified.retryable,
    http_status: classified.httpStatus,
    message: classified.message,
  };
}

export function providerCode(error: unknown): string | null {
  return safeCodeOf(error);
}

export function coreErrorCode(error: unknown): string {
  const record = asRecord(error);
  return stringField(asRecord(record?.data), "code") || stringField(record, "code");
}
