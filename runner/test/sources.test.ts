import { describe, expect, it } from "vitest";
import { SourceCollector } from "../src/sources.js";
import { documentResult } from "./helpers.js";

describe("SourceCollector", () => {
  it("distinguishes an empty stated source list from search excerpts alone", () => {
    const sources = new SourceCollector();
    expect(sources.snapshot()).toMatchObject({ sources: [], sources_stated: true });
    sources.observe("course_materials_search", {}, {
      status: "executed",
      result: { hits: [{ title: "Week 3", read: { tool: "document_get", arguments: { document_id: "d" } } }] },
    });
    expect(sources.snapshot()).toMatchObject({ sources: [], sources_stated: false, glimpsed: true });
  });

  it("adds ordered deduplicated sources with hit location and document_text part", () => {
    const sources = new SourceCollector();
    const getArgs = { course_id: "c", document_id: "doc-1", version_id: "version-1" };
    const textArgs = { course_id: "c", document_id: "doc-1", version_id: "version-1", file_id: "file-1", part: 2 };
    sources.observe("course_materials_search", {}, {
      status: "executed",
      result: { hits: [
        { title: "Week 3 Slides", where: { kind: "slide", n: 3 }, part: 1, read: { tool: "document_get", arguments: { document_id: "doc-1", version_id: "version-1" } } },
        { title: "Week 3 Slides", where: "page: 7", part: 2, read: { tool: "document_text", arguments: { document_id: "doc-1", version_id: "version-1", file_id: "file-1", part: 2 } } },
      ] },
    });
    sources.observe("document_get", getArgs, { status: "executed", result: documentResult() });
    sources.observe("document_get", getArgs, { status: "executed", result: documentResult() });
    sources.observe("document_text", textArgs, {
      status: "executed",
      result: {
        document_id: "doc-1", version_id: "version-1", file_id: "file-1", part: 2,
        text: { status: "done", body: "## Page 7\n\nOnly this page." },
      },
    });
    expect(sources.snapshot()).toEqual({
      sources: [
        { document_id: "doc-1", version_id: "version-1" },
        { document_id: "doc-1", version_id: "version-1", file_id: "file-1", part: 2, page: 7 },
      ],
      sources_stated: true,
      glimpsed: true,
    });
  });

  it("collects file text and never sources refusals or purged versions", () => {
    const sources = new SourceCollector();
    const doc = documentResult({ version: { id: "version-1", published: true, files: [{ id: "file-1", text: { status: "done", body: "## Slide 2\nText" } }] } });
    sources.observe("document_get", { document_id: "doc-1" }, { status: "executed", result: doc });
    sources.observe("document_get", { document_id: "doc-1" }, { status: "denied", result: doc });
    sources.observe("document_get", { document_id: "doc-1" }, { status: "executed", result: documentResult({ purged: { reason: "mistake" } }) });
    expect(sources.snapshot().sources).toEqual([{ document_id: "doc-1", version_id: "version-1", file_id: "file-1", part: 1, slide: 2 }]);
  });

  it("caps sources at twenty", () => {
    const sources = new SourceCollector();
    for (let i = 0; i < 25; i++) {
      sources.observe("document_get", { document_id: `doc-${i}` }, {
        status: "executed",
        result: documentResult({ id: `doc-${i}`, version: { id: `version-${i}`, body_md: "body", published: true, files: [] } }),
      });
    }
    expect(sources.snapshot().sources).toHaveLength(20);
  });
});
