import { describe, expect, it, vi } from "vitest";
import { asRecord } from "../src/json.js";
import { BUILTIN_DENY, deniedBy, listOfferedTools, SecureTools, toolKey } from "../src/tools.js";
import type { RunnerEvent } from "../src/types.js";
import { definition, documentResult, execute, FakeMCP, request } from "./helpers.js";

function secure(client: FakeMCP, req = request(), events: RunnerEvent[] = []) {
  return new SecureTools({ request: req, client, signal: new AbortController().signal, emit: (event) => events.push(event), onFatal: vi.fn() }, client.tools);
}

describe("tool catalogue policy", () => {
  it("applies builtin, configured allow/deny, readOnly, and keeps native search/text reads", async () => {
    const req = request({
      allow_writes: false,
      allowed_tools: ["course_materials_search", "document_text", "document_text_update", "conversation_answer", "document_create", "course_get"],
      denied_tools: ["course_*"],
    });
    const client = new FakeMCP([
      definition("course_materials_search"), definition("document_text"), definition("document_text_update", false),
      definition("conversation_answer", false), definition("document_create", false), definition("course_get"),
    ]);
    // course_* also denies course_get and course_materials_search, just like the Go wildcard gate.
    expect((await listOfferedTools(client, req)).map((tool) => tool.name)).toEqual(["document_text"]);
    expect(deniedBy("document_text_update", BUILTIN_DENY)).toBe(true);
    expect(deniedBy("document_text", BUILTIN_DENY)).toBe(false);
  });

  it("hides all runtime-bound arguments from model schemas", async () => {
    const client = new FakeMCP([definition("document_create", false, {
      type: "object",
      properties: { course_id: { type: "string" }, idempotency_key: { type: "string" }, revises: { type: "string" }, title: { type: "string" } },
      required: ["course_id", "idempotency_key", "revises", "title"],
      additionalProperties: false,
    })]);
    const tools = await secure(client).build();
    const schema = (tools.document_create as unknown as { inputSchema: { jsonSchema: Record<string, unknown> } }).inputSchema.jsonSchema;
    expect(Object.keys(asRecord(schema.properties) ?? {})).toEqual(["title"]);
    expect(schema.required).toEqual(["title"]);
  });
});

describe("execution bindings, limits, and result safety", () => {
  it("overrides course/revises/model keys, validates, and reuses stable keys without consuming another write", async () => {
    const client = new FakeMCP([definition("document_create", false)], () => ({ status: "proposed", action_id: "action-1" }));
    const tools = await secure(client).build();
    const args = { course_id: "attacker-course", idempotency_key: "attacker-key", revises: "action-x", title: "Doc" };
    const first = JSON.parse(await execute(tools, "document_create", args, "one"));
    const second = JSON.parse(await execute(tools, "document_create", args, "two"));
    expect(first.note).toMatch(/Not done yet/);
    expect(second.note).toMatch(/Not done yet/);
    expect(client.calls).toHaveLength(2);
    expect(client.calls[0]?.arguments).toMatchObject({ course_id: "course-1", title: "Doc", idempotency_key: "tool:conversation-1:message-1:1:1" });
    expect(client.calls[0]?.arguments.revises).toBeUndefined();
    expect(client.calls[1]?.arguments.idempotency_key).toBe(client.calls[0]?.arguments.idempotency_key);
    await execute(tools, "document_create", { title: "Other" }, "three");
    expect(client.calls[2]?.arguments.idempotency_key).toBe("tool:conversation-1:message-1:1:2");
    expect(toolKey("x".repeat(210), "m", 1, 1)).toMatch(/^tool:[a-f0-9]{64}$/);
  });

  it("validates required bound fields only after overriding forged values with the final runtime values", async () => {
    const client = new FakeMCP([definition("document_create", false, {
      type: "object",
      properties: {
        course_id: { type: "string", const: "course-1" },
        idempotency_key: { type: "string", pattern: "^tool:" },
        revises: { type: "null" },
        title: { type: "string" },
      },
      required: ["course_id", "idempotency_key", "title"],
      additionalProperties: false,
    })], () => ({ status: "executed", result: { document_id: "doc-new" } }));
    const tools = await secure(client).build();
    const result = JSON.parse(await execute(tools, "document_create", {
      course_id: "forged-course", idempotency_key: "forged-key", title: "Valid",
    }));
    expect(result.status).toBe("executed");
    expect(client.calls).toHaveLength(1);
    expect(client.calls[0]?.arguments).toEqual({ course_id: "course-1", idempotency_key: "tool:conversation-1:message-1:1:1", title: "Valid" });
  });

  it("refuses malformed arguments before Core", async () => {
    const def = definition("document_create", false, {
      type: "object", properties: { course_id: { type: "string" }, title: { type: "string" } }, required: ["title"], additionalProperties: false,
    });
    const client = new FakeMCP([def]);
    const tools = await secure(client).build();
    const result = JSON.parse(await execute(tools, "document_create", { title: 42 }));
    expect(result.error.code).toBe("invalid_argument");
    expect(client.calls).toHaveLength(0);
  });

  it("enforces write and total tool budgets", async () => {
    const req = request({ limits: { max_tool_calls: 2, max_writes: 1 } });
    const client = new FakeMCP([definition("document_create", false), definition("course_get")]);
    const tools = await secure(client, req).build();
    await execute(tools, "document_create", { title: "one" }, "one");
    const overWrite = JSON.parse(await execute(tools, "document_create", { title: "two" }, "two"));
    const overCalls = JSON.parse(await execute(tools, "course_get", {}, "three"));
    expect(overWrite.error.code).toBe("failed_precondition");
    expect(overCalls.error.message).toMatch(/tool-call budget/);
    expect(client.calls).toHaveLength(1);
  });

  it("enforces Decides even when a write was otherwise offered", async () => {
    const req = request({ seat: { self_member_id: "self", principal_member_id: "p", opener_member_id: "o", action_decide_level: "autonomous" } });
    const client = new FakeMCP([definition("action_decide", false)]);
    const events: RunnerEvent[] = [];
    const tools = await secure(client, req, events).build();
    const result = JSON.parse(await execute(tools, "action_decide", { action_id: "a", decision: "approve" }));
    expect(result.error.code).toBe("forbidden");
    expect(client.calls).toHaveLength(0);
    expect(events.map((event) => event.type)).toEqual(["tool_start", "tool_end"]);
  });

  it("recursively strips signed URLs and truncates UTF-8 while preserving status/error", async () => {
    const client = new FakeMCP([definition("course_get")], () => ({
      status: "error",
      error: { code: "invalid_argument", message: "safe error" },
      result: { download_url: "https://secret.example/file?signature=secret", nested: [{ downloadUrl: "secret", text: "教材".repeat(2000) }] },
    }));
    const req = request({ limits: { max_result_bytes: 1024 } });
    const tools = await secure(client, req).build();
    const raw = await execute(tools, "course_get", {});
    expect(Buffer.byteLength(raw)).toBeLessThanOrEqual(1024);
    expect(raw).not.toContain("signature");
    expect(raw).not.toContain("download_url");
    const parsed = JSON.parse(raw);
    expect(parsed.status).toBe("error");
    expect(parsed.error.code).toBe("invalid_argument");
    expect(raw).toContain("result_truncated");
  });

  it("counts all write outcomes", async () => {
    const statuses = ["executed", "proposed", "denied", "failed"];
    let index = 0;
    const client = new FakeMCP([definition("document_create", false)], () => ({ status: statuses[index++ % statuses.length] }));
    const tools = await secure(client).build();
    for (const title of statuses) await execute(tools, "document_create", { title });
    expect(client.calls).toHaveLength(4);
  });
});

describe("SeatGuard", () => {
  it("blocks self/principal/opener without any Core lookup", async () => {
    const client = new FakeMCP([definition("member_update_perms", false)]);
    for (const member_id of ["SELF", "principal", "opener"]) {
      const tools = await secure(client).build();
      const result = JSON.parse(await execute(tools, "member_update_perms", { member_id }));
      expect(result.error.code).toBe("forbidden");
    }
    expect(client.calls).toHaveLength(0);
  });

  it("blocks another delegate of a protected person and fails closed when seat ownership is unreadable", async () => {
    const client = new FakeMCP([definition("member_update_perms", false)], (name) => {
      if (name === "member_get") return { status: "executed", result: { role: "assistant", principal_member_id: "PRINCIPAL" } };
      return { status: "executed", result: {} };
    });
    const tools = await secure(client).build();
    expect(JSON.parse(await execute(tools, "member_update_perms", { member_id: "other-agent" })).error.code).toBe("forbidden");
    expect(client.calls.map((call) => call.name)).toEqual(["member_get"]);

    const unreadable = new FakeMCP([definition("member_update_perms", false)], () => ({ status: "denied" }));
    const deniedTools = await secure(unreadable).build();
    expect(JSON.parse(await execute(deniedTools, "member_update_perms", { member_id: "unknown" })).error.message).toMatch(/could not verify/);
    expect(unreadable.calls.some((call) => call.name === "member_update_perms")).toBe(false);
  });

  it("checks role-wide changes against protected people and their other agents", async () => {
    const client = new FakeMCP([definition("member_update_perms_bulk", false)], (name, args) => {
      if (name === "member_get") return { status: "executed", result: { role: args.member_id === "opener" ? "student" : "instructor", principal_member_id: "" } };
      if (name === "member_list") return { status: "executed", result: { members: [{ id: "agent-2", principal_member_id: "principal" }] } };
      return { status: "executed", result: {} };
    });
    const tools = await secure(client).build();
    expect(JSON.parse(await execute(tools, "member_update_perms_bulk", { role: "instructor", perms: {} })).error.code).toBe("forbidden");

    const second = new FakeMCP([definition("member_update_perms_bulk", false)], (name, args) => {
      if (name === "member_get") return { status: "executed", result: { role: args.member_id === "opener" ? "student" : "instructor", principal_member_id: "" } };
      if (name === "member_list") return { status: "executed", result: { members: [{ id: "agent-2", principal_member_id: "principal" }] } };
      return { status: "executed", result: {} };
    });
    const secondTools = await secure(second).build();
    expect(JSON.parse(await execute(secondTools, "member_update_perms_bulk", { role: "assistant", perms: {} })).error.code).toBe("forbidden");
    expect(second.calls.some((call) => call.name === "member_list")).toBe(true);
    expect(second.calls.some((call) => call.name === "member_update_perms_bulk")).toBe(false);
  });
});

describe("sources and safe targets", () => {
  it("marks search-only runs unstated, then records a located document read", async () => {
    const hitRead = { tool: "document_get", arguments: { document_id: "doc-1", version_id: "version-1" } };
    const client = new FakeMCP([definition("course_materials_search"), definition("document_get")], (name) => {
      if (name === "course_materials_search") return {
        status: "executed",
        result: { hits: [{ document_id: "doc-1", title: "Week 3 Slides", where: { kind: "slide", n: 3 }, part: 1, read: hitRead }] },
      };
      return { status: "executed", result: documentResult() };
    });
    const events: RunnerEvent[] = [];
    const tools = await secure(client, request(), events).build();
    await execute(tools, "course_materials_search", { query: "week" }, "search");
    await execute(tools, "document_get", { document_id: "doc-1", version_id: "version-1" }, "read");
    const toolEnd = events.filter((event) => event.type === "tool_end");
    expect(toolEnd[0]).toMatchObject({ target: "Week 3 Slides" });
    expect(toolEnd[1]).toMatchObject({ target: "Week 3 Slides" });
  });

  it("does not expose draft, archived, rubric, or assignment titles as targets", async () => {
    const result = documentResult({ title: "Secret\nRubric", kind: "rubric" });
    const client = new FakeMCP([definition("document_get")], () => ({ status: "executed", result }));
    const events: RunnerEvent[] = [];
    const tools = await secure(client, request(), events).build();
    await execute(tools, "document_get", { document_id: "doc-1" });
    expect(events.at(-1)).not.toHaveProperty("target");
  });
});
