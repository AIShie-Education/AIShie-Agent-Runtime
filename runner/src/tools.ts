import { jsonSchema, tool, type ToolSet } from "ai";
import { Ajv, type ValidateFunction } from "ajv";
import { createRequire } from "node:module";
import { RunnerError } from "./errors.js";
import { asRecord, byteLength, canonicalJSON, cutUTF8, sha256, stripDownloadURLs, stringField, truncateText } from "./json.js";
import { callCore } from "./mcp.js";
import { SourceCollector } from "./sources.js";
import type {
  CoreEnvelope,
  MCPClientLike,
  MCPToolDefinition,
  RunRequest,
  RunnerEvent,
  Seat,
  WriteCounts,
} from "./types.js";

export const BUILTIN_DENY = [
  "conversation_*", "memory_*", "event_list", "action_list_mine", "action_withdraw",
  "actor_*", "agent_*", "credential_*", "me_*", "service_*", "agent_runtime_*", "sso_*",
  "preset_*", "term_*", "department_*", "document_upload_url", "document_file", "document_purge",
  "document_rendition_retry", "document_text_*",
  "course_create", "course_update", "course_activate", "course_archive", "course_move",
  "course_seat_instructor", "course_list", "course_join_link_create",
  "member_add_delegate", "member_delegate_defaults", "member_reset_password", "peer_review_submit",
] as const;

const require = createRequire(import.meta.url);
const addFormats = require("ajv-formats") as (instance: Ajv) => unknown;

const BOUND = ["course_id", "idempotency_key", "revises"] as const;
const PROPOSED_NOTE = "Not done yet: a person decides it, under the action_id above. This is the normal outcome here, not an error: do not make it again. You do not learn their decision in this answer; if they send it back for changes, you may be asked again.";

export function deniedBy(name: string, patterns: readonly string[]): boolean {
  return patterns.some((pattern) => pattern.endsWith("*") ? name.startsWith(pattern.slice(0, -1)) : name === pattern);
}

export function toolKey(conversationID: string, messageID: string, attempt: number, n: number): string {
  const key = `tool:${conversationID}:${messageID}:${attempt}:${n}`;
  return Array.from(key).length <= 200 ? key : `tool:${sha256(key)}`;
}

export interface ToolState {
  readonly sources: SourceCollector;
  readonly writes: WriteCounts;
  readonly toolCalls: number;
}

interface ToolContext {
  request: RunRequest;
  client: MCPClientLike;
  signal: AbortSignal;
  emit: (event: RunnerEvent) => void;
  onFatal: (error: unknown) => void;
}

interface Prepared {
  args?: Record<string, unknown>;
  write: boolean;
  refusal?: CoreEnvelope;
  key?: string;
  guarded?: boolean;
}

interface WriteClaim { n: number; key: string }

function emptyCounts(): WriteCounts {
  return { executed: 0, proposed: 0, denied: 0, failed: 0, refused: 0, guarded: 0 };
}

function refusal(code: string, message: string): CoreEnvelope {
  return { status: "error", error: { code, message } };
}

function isWrite(definition: MCPToolDefinition): boolean {
  return definition.annotations?.readOnlyHint !== true;
}

function decides(name: string): boolean {
  return name === "action_decide" || name === "action_review";
}

function sameID(a: string, b: string): boolean {
  return a.trim().toLowerCase() === b.trim().toLowerCase();
}

function protectedPeople(seat: Seat): string[] {
  const people: string[] = [];
  for (const id of [seat.principal_member_id, seat.opener_member_id]) {
    if (id && !sameID(id, seat.self_member_id) && !people.some((other) => sameID(other, id))) people.push(id);
  }
  return people;
}

function shownSchema(input: Record<string, unknown>): Record<string, unknown> {
  const schema = structuredClone(input);
  const properties = asRecord(schema.properties);
  if (properties) {
    for (const bound of BOUND) delete properties[bound];
    schema.properties = properties;
  }
  if (Array.isArray(schema.required)) schema.required = schema.required.filter((key) => !BOUND.includes(key as typeof BOUND[number]));
  return schema;
}

function boundArguments(input: unknown, request: RunRequest): Record<string, unknown> | undefined {
  const args = asRecord(input) ?? (input === undefined ? {} : undefined);
  if (!args) return undefined;
  const effective: Record<string, unknown> = { ...args, course_id: request.course_id };
  delete effective.revises;
  delete effective.idempotency_key;
  return effective;
}

function schemaValidator(definition: MCPToolDefinition): ValidateFunction {
  const ajv = new Ajv({ allErrors: false, strict: false, validateSchema: false });
  addFormats(ajv);
  return ajv.compile(definition.inputSchema);
}

function resultContent(envelope: CoreEnvelope, maxBytes: number): string {
  const cleaned = stripDownloadURLs(envelope) as CoreEnvelope;
  if (cleaned.status === "proposed") cleaned.note = PROPOSED_NOTE;
  const whole = JSON.stringify(cleaned);
  if (byteLength(whole) <= maxBytes) return whole;
  const result = cleaned.result === undefined ? "" : JSON.stringify(cleaned.result) ?? "";
  const base: CoreEnvelope = {
    status: cleaned.status,
    ...(cleaned.action_id ? { action_id: cleaned.action_id } : {}),
    ...(cleaned.review_state ? { review_state: cleaned.review_state } : {}),
    ...(cleaned.replayed ? { replayed: true } : {}),
    ...(cleaned.note ? { note: cleaned.note } : {}),
    ...(cleaned.error ? { error: cleaned.error } : {}),
  };
  const truncated = base as CoreEnvelope & { result_truncated?: string };
  for (let attempt = 0; attempt < 5; attempt++) {
    const fitted = fitTruncatedResult(truncated, result, maxBytes);
    if (fitted !== undefined) return fitted;
    delete truncated.result_truncated;
    if (truncated.error?.details) delete truncated.error.details;
    else if (truncated.error?.message && byteLength(truncated.error.message) > 200) {
      truncated.error.message = truncateText(truncated.error.message, 200);
    } else if (truncated.note) delete truncated.note;
    else break;
  }
  const minimal = { status: cleaned.status, ...(cleaned.error ? { error: { code: cleaned.error.code } } : {}) };
  return JSON.stringify(minimal);
}

function fitTruncatedResult(base: CoreEnvelope & { result_truncated?: string }, result: string, maxBytes: number): string | undefined {
  const originalBytes = byteLength(result);
  const mark = `…[truncated, ${originalBytes} bytes]`;
  let low = 0;
  let high = originalBytes;
  let best: string | undefined;
  while (low <= high) {
    const middle = Math.floor((low + high) / 2);
    const prefix = cutUTF8(result, middle);
    const candidate = prefix + (byteLength(prefix) < originalBytes ? mark : "");
    const encoded = JSON.stringify({ ...base, result_truncated: candidate });
    if (byteLength(encoded) <= maxBytes) {
      best = encoded;
      low = middle + 1;
    } else {
      high = middle - 1;
    }
  }
  return best;
}

export class SecureTools {
  readonly sources = new SourceCollector();
  readonly writes = emptyCounts();
  toolCalls = 0;
  private sent = 0;
  private readonly claims = new Map<string, WriteClaim>();

  constructor(private readonly context: ToolContext, private readonly definitions: MCPToolDefinition[]) {}

  async build(): Promise<ToolSet> {
    const tools: ToolSet = {};
    for (const definition of [...this.definitions].sort((a, b) => a.name.localeCompare(b.name))) {
      const name = definition.name;
      const validate = schemaValidator(definition);
      tools[name] = tool({
        description: definition.description ?? definition.title ?? name,
        inputSchema: jsonSchema(shownSchema(definition.inputSchema)),
        execute: async (input, options) => this.execute(definition, input, options.toolCallId, validate),
      });
    }
    return tools;
  }

  private async execute(
    definition: MCPToolDefinition,
    input: unknown,
    callID: string,
    validate: ValidateFunction,
  ): Promise<string> {
    const { request, emit } = this.context;
    const name = definition.name;
    const write = isWrite(definition);
    this.toolCalls++;
    emit({ type: "tool_start", id: callID, name });
    let envelope: CoreEnvelope | undefined;
    let target: string | undefined;
    try {
      if (this.toolCalls > request.limits.max_tool_calls) {
        if (write) this.writes.refused++;
        envelope = refusal("failed_precondition", `The tool-call budget is spent (${request.limits.max_tool_calls}); ${name} was not called.`);
        return resultContent(envelope, request.limits.max_result_bytes);
      }
      if (deniedBy(name, BUILTIN_DENY) || deniedBy(name, request.denied_tools) ||
        !request.allowed_tools.includes(name) || (write && !request.allow_writes)) {
        if (write) this.writes.refused++;
        envelope = refusal("forbidden", `${name} is not offered to the model.`);
        return resultContent(envelope, request.limits.max_result_bytes);
      }
      const prepared = await this.prepare(definition, input, validate);
      if (prepared.refusal) {
        envelope = prepared.refusal;
        return resultContent(envelope, request.limits.max_result_bytes);
      }
      try {
        envelope = await callCore(this.context.client, name, prepared.args!, this.context.signal);
      } catch (error) {
        if (write) this.writes.failed++;
        this.context.onFatal(error);
        throw error;
      }
      this.recordWrite(write, envelope);
      this.sources.observe(name, prepared.args!, envelope);
      target = this.sources.target(name, prepared.args!, envelope);
      return resultContent(envelope, request.limits.max_result_bytes);
    } finally {
      emit({ type: "tool_end", id: callID, name, ...(target ? { target } : {}) });
    }
  }

  private async prepare(definition: MCPToolDefinition, input: unknown, validate: ValidateFunction): Promise<Prepared> {
    const { request } = this.context;
    const name = definition.name;
    const write = isWrite(definition);
    const args = boundArguments(input, request);
    if (!args) return { write, refusal: refusal("invalid_argument", `The arguments to ${name} are not a JSON object.`) };
    let identity = "";
    let claim: WriteClaim | undefined;
    if (write) {
      identity = `${name}\0${canonicalJSON(args)}`;
      claim = this.claims.get(identity) ?? {
        n: this.sent + 1,
        key: toolKey(request.conversation_id, request.message_id, request.attempt, this.sent + 1),
      };
      // Core validates its complete object, including the required fields the
      // model never sees. The candidate key is not consumed until every check
      // and guard has passed.
      args.idempotency_key = claim.key;
    }
    if (!validate(args)) {
      const detail = validate.errors?.[0];
      const suffix = detail ? ` ${detail.instancePath || "/"} ${detail.message ?? "is invalid"}` : "";
      return { write, refusal: refusal("invalid_argument", `The arguments to ${name} do not match its schema.${suffix}`) };
    }
    if (write && decides(name) && request.seat.action_decide_level !== "confirm_required") {
      this.writes.guarded++;
      return { write, guarded: true, refusal: refusal("forbidden", "A model's decision or review must itself wait for a person to confirm it; decide it yourself.") };
    }
    if (write && name.startsWith("member_")) {
      const why = await this.seatGuard(name, args);
      if (why) {
        this.writes.guarded++;
        return { write, guarded: true, refusal: refusal("forbidden", why) };
      }
    }
    if (!write || !claim) return { write, args };
    if (!this.claims.has(identity)) {
      if (this.sent >= request.limits.max_writes) {
        this.writes.refused++;
        return { write, refusal: refusal("failed_precondition", `The changes this answer may make are spent (${request.limits.max_writes}); ${name} was not made.`) };
      }
      this.sent = claim.n;
      this.claims.set(identity, claim);
    }
    return { write, args, key: claim.key };
  }

  private recordWrite(write: boolean, envelope: CoreEnvelope): void {
    if (!write) return;
    switch (envelope.status) {
      case "executed": this.writes.executed++; break;
      case "proposed": this.writes.proposed++; break;
      case "denied": this.writes.denied++; break;
      default: this.writes.failed++; break;
    }
  }

  private async seatGuard(toolName: string, args: Record<string, unknown>): Promise<string> {
    const { request } = this.context;
    const seat = request.seat;
    if (!seat.self_member_id) return `${toolName} is not offered here: the runtime does not know which seats it must leave alone.`;
    const named = [args.member_id, ...(Array.isArray(args.member_ids) ? args.member_ids : [])]
      .filter((id): id is string => typeof id === "string" && id.length > 0);
    for (const id of named) {
      if (sameID(id, seat.self_member_id)) return `${toolName} would change your own seat; nothing was changed.`;
      if (sameID(id, seat.principal_member_id) || sameID(id, seat.opener_member_id)) {
        return `${toolName} would change the seat of the person you act for; nothing was changed.`;
      }
    }
    const people = protectedPeople(seat);
    for (const id of named) {
      if (people.length === 0) break;
      const member = await this.readSeat(id);
      if (member === null) return `${toolName} could not verify whose seat ${id} is; nothing was changed.`;
      if (member && people.some((person) => sameID(member.principal, person))) {
        return `${toolName} would change another agent of the person you act for; nothing was changed.`;
      }
    }
    if (toolName !== "member_update_perms_bulk") return "";
    const role = stringField(args, "role");
    if (!role) return "";
    for (const person of people) {
      const member = await this.readSeat(person);
      if (member === null || member === undefined) {
        return `${toolName} changes every seat with role ${role}, and the protected seats could not be verified; nothing was changed.`;
      }
      if (member.role.toLowerCase() === role.toLowerCase()) {
        return `${toolName} would change the seat of the person you act for; nothing was changed.`;
      }
    }
    if (people.length > 0) {
      const delegated = await this.agentOfRole(role, people);
      if (delegated === null) return `${toolName} could not verify every affected agent seat; nothing was changed.`;
      if (delegated) return `${toolName} would change another agent of the person you act for; nothing was changed.`;
    }
    return "";
  }

  private async readSeat(id: string): Promise<{ role: string; principal: string } | null | undefined> {
    try {
      const envelope = await callCore(this.context.client, "member_get", {
        course_id: this.context.request.course_id,
        member_id: id,
      }, this.context.signal);
      if (envelope.status === "error" && envelope.error?.code === "not_found") return undefined;
      if (envelope.status !== "executed") return null;
      const result = asRecord(envelope.result);
      const role = stringField(result, "role");
      if (!role) return null;
      return { role, principal: stringField(result, "principal_member_id") };
    } catch (error) {
      this.context.onFatal(error);
      throw error;
    }
  }

  private async agentOfRole(role: string, people: string[]): Promise<string | null> {
    let after = "";
    for (let page = 0; page < 10; page++) {
      const envelope = await callCore(this.context.client, "member_list", {
        course_id: this.context.request.course_id,
        role,
        limit: 200,
        ...(after ? { after } : {}),
      }, this.context.signal);
      if (envelope.status !== "executed") return null;
      const result = asRecord(envelope.result);
      const members = result?.members;
      if (!Array.isArray(members)) return null;
      for (const raw of members) {
        const id = stringField(raw, "id");
        const principal = stringField(raw, "principal_member_id");
        if (id && !sameID(id, this.context.request.seat.self_member_id) && principal &&
          people.some((person) => sameID(person, principal))) return id;
      }
      const next = stringField(result, "next");
      if (!next) return "";
      after = next;
    }
    return null;
  }
}

export async function listOfferedTools(
  client: MCPClientLike,
  request: RunRequest,
  signal?: AbortSignal,
): Promise<MCPToolDefinition[]> {
  const definitions: MCPToolDefinition[] = [];
  let cursor = "";
  for (let page = 0; page < 100; page++) {
    const result = await client.listTools({
      params: cursor ? { cursor } : {},
      options: signal ? { signal } : {},
    });
    for (const definition of result.tools) {
      const name = definition.name;
      if (!request.allowed_tools.includes(name) || deniedBy(name, BUILTIN_DENY) || deniedBy(name, request.denied_tools)) continue;
      if (isWrite(definition) && !request.allow_writes) continue;
      definitions.push(definition);
    }
    if (!result.nextCursor || result.nextCursor === cursor) break;
    cursor = result.nextCursor;
    if (page === 99) throw new RunnerError("provider_failed", "Core returned too many tool catalogue pages.", { retryable: true });
  }
  return definitions;
}
