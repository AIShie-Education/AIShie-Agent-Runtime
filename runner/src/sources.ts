import { asRecord, boolField, canonicalJSON, numberField, oneLine, stringField } from "./json.js";
import type { CoreEnvelope, Source } from "./types.js";

interface Hit {
  key: string;
  kind?: "page" | "slide";
  n?: number;
  part?: number;
  title?: string;
}

interface LocatedHit extends Hit { several: boolean }

function locator(where: unknown): { kind?: "page" | "slide"; n?: number } {
  if (typeof where === "string") {
    const match = /^(page|slide)(?:\s*[:#]?\s*|\s+)(\d+)$/i.exec(where.trim());
    if (match) return { kind: match[1]!.toLowerCase() as "page" | "slide", n: Number(match[2]) };
  }
  const record = asRecord(where);
  if (!record) return {};
  const kind = (stringField(record, "kind") || stringField(record, "section_kind")).toLowerCase();
  const n = numberField(record, "n") ?? numberField(record, "number") ?? numberField(record, "section_no");
  return (kind === "page" || kind === "slide") && n && n > 0 ? { kind, n } : {};
}

function bodyLocator(body: string): { page?: number; slide?: number } {
  const pageMatches = [...body.matchAll(/^##\s+Page\s+(\d+)\s*$|^##\s+第\s*(\d+)\s*[頁页]\s*$/gim)];
  const slideMatches = [...body.matchAll(/^##\s+(?:Slide|投影片)\s+(\d+)\s*$/gim)];
  if (pageMatches.length === 1 && slideMatches.length === 0) return { page: Number(pageMatches[0]![1] ?? pageMatches[0]![2]) };
  if (slideMatches.length === 1 && pageMatches.length === 0) return { slide: Number(slideMatches[0]![1]) };
  return {};
}

function sameSource(a: Source, b: Source): boolean {
  return a.document_id === b.document_id && a.version_id === b.version_id && a.file_id === b.file_id &&
    a.page === b.page && a.slide === b.slide && a.part === b.part;
}

export class SourceCollector {
  private readonly read: Source[] = [];
  private readonly hits = new Map<string, LocatedHit>();
  private glimpsed = false;

  observe(tool: string, args: Record<string, unknown>, envelope: CoreEnvelope): void {
    if (envelope.status !== "executed") return;
    if (tool === "course_materials_search") { this.observeSearch(envelope.result); return; }
    if (tool === "document_get") this.observeDocumentGet(args, envelope.result);
    if (tool === "document_text") this.observeDocumentText(args, envelope.result);
  }

  private observeSearch(result: unknown): void {
    const hits = asRecord(result)?.hits;
    if (!Array.isArray(hits) || hits.length === 0) return;
    this.glimpsed = true;
    for (const raw of hits) {
      const hit = asRecord(raw);
      const read = asRecord(hit?.read);
      const tool = stringField(read, "tool");
      const args = asRecord(read?.arguments);
      if (!hit || !read || !args || (tool !== "document_get" && tool !== "document_text")) continue;
      const key = this.key(tool, args);
      const where = locator(hit.where);
      const next: Hit = {
        key,
        ...where,
        part: numberField(hit, "part"),
        title: oneLine(stringField(hit, "title")),
      };
      const previous = this.hits.get(key);
      if (!previous) this.hits.set(key, { ...next, several: false });
      else if (previous.kind !== next.kind || previous.n !== next.n || previous.part !== next.part || previous.title !== next.title) {
        this.hits.set(key, { ...previous, kind: undefined, n: undefined, part: undefined, title: undefined, several: true });
      }
    }
  }

  private observeDocumentGet(args: Record<string, unknown>, result: unknown): void {
    const doc = asRecord(result);
    const version = asRecord(doc?.version);
    if (!doc || !version) return;
    const kind = stringField(doc, "kind");
    if (!["material", "instructions", "rubric"].includes(kind) || asRecord(doc.purged) || asRecord(version.purged)) return;
    const documentID = stringField(doc, "id");
    const versionID = stringField(version, "id");
    const body = stringField(version, "body_md").trim();
    if (!documentID || !versionID) { if (body) this.glimpsed = true; return; }
    if (body) this.add(args, "document_get", { document_id: documentID, version_id: versionID });
    const files = version.files;
    if (!Array.isArray(files)) return;
    for (const raw of files) {
      const file = asRecord(raw);
      const text = asRecord(file?.text);
      const fileBody = stringField(text, "body");
      const fileID = stringField(file, "id");
      if (!fileBody.trim()) continue;
      if (!fileID) { this.glimpsed = true; continue; }
      const part = numberField(file, "part") ?? 1;
      this.add(args, "document_get", {
        document_id: documentID,
        version_id: versionID,
        file_id: fileID,
        part,
        ...bodyLocator(fileBody),
      });
    }
  }

  private observeDocumentText(args: Record<string, unknown>, result: unknown): void {
    const out = asRecord(result);
    const body = stringField(asRecord(out?.text), "body");
    if (!out || !body.trim()) return;
    const documentID = stringField(out, "document_id");
    const versionID = stringField(out, "version_id");
    const fileID = stringField(out, "file_id");
    if (!documentID || !versionID || !fileID) { this.glimpsed = true; return; }
    this.add(args, "document_text", {
      document_id: documentID,
      version_id: versionID,
      file_id: fileID,
      part: numberField(out, "part") ?? 1,
      ...bodyLocator(body),
    });
  }

  private add(args: Record<string, unknown>, tool: string, source: Source): void {
    const hit = this.hits.get(this.key(tool, args));
    if (hit && !hit.several && source.file_id) {
      if (source.page === undefined && source.slide === undefined && hit.kind && hit.n) source[hit.kind] = hit.n;
      if (tool === "document_text" && args.part === undefined && hit.part) source.part = hit.part;
    }
    if (this.read.length < 20 && !this.read.some((existing) => sameSource(existing, source))) this.read.push(source);
  }

  private key(tool: string, args: Record<string, unknown>): string {
    const copy = { ...args };
    delete copy.course_id;
    return `${tool}\0${canonicalJSON(copy)}`;
  }

  target(tool: string, args: Record<string, unknown>, envelope: CoreEnvelope): string | undefined {
    if (envelope.status !== "executed") return undefined;
    if (tool === "course_materials_search") {
      const hits = asRecord(envelope.result)?.hits;
      if (!Array.isArray(hits)) return undefined;
      const titles = new Set(hits.map((hit) => oneLine(stringField(hit, "title"))).filter(Boolean));
      return titles.size === 1 ? [...titles][0] : undefined;
    }
    if (tool === "document_get") {
      const doc = asRecord(envelope.result);
      const version = asRecord(doc?.version);
      if (stringField(doc, "kind") !== "material" || stringField(doc, "status") !== "active" ||
        !boolField(version, "published") || doc?.purged_at || asRecord(doc?.purged) || asRecord(version?.purged)) return undefined;
      return oneLine(stringField(doc, "title")) || undefined;
    }
    if (tool === "document_text") {
      const title = this.hits.get(this.key(tool, args))?.title;
      return title || undefined;
    }
    return undefined;
  }

  snapshot(): { sources: Source[]; sources_stated: boolean; glimpsed: boolean } {
    return {
      sources: this.read.map((source) => ({ ...source })),
      sources_stated: this.read.length > 0 || !this.glimpsed,
      glimpsed: this.glimpsed,
    };
  }
}
