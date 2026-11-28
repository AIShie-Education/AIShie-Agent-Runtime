import { createHash } from "node:crypto";

export function canonicalJSON(value: unknown): string {
  return JSON.stringify(sortJSON(value));
}

function sortJSON(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sortJSON);
  if (value !== null && typeof value === "object") {
    const input = value as Record<string, unknown>;
    const out: Record<string, unknown> = {};
    for (const key of Object.keys(input).sort()) {
      if (input[key] !== undefined) out[key] = sortJSON(input[key]);
    }
    return out;
  }
  return value;
}

export function sha256(value: string): string {
  return createHash("sha256").update(value, "utf8").digest("hex");
}

export function byteLength(value: string): number {
  return Buffer.byteLength(value, "utf8");
}

export function cutUTF8(value: string, maxBytes: number): string {
  if (maxBytes <= 0) return "";
  const input = Buffer.from(value, "utf8");
  if (input.length <= maxBytes) return value;
  let end = maxBytes;
  while (end > 0 && (input[end]! & 0xc0) === 0x80) end--;
  return input.subarray(0, end).toString("utf8");
}

export function truncateText(value: string, maxBytes: number): string {
  if (byteLength(value) <= maxBytes) return value;
  const mark = `…[truncated, ${byteLength(value)} bytes]`;
  return cutUTF8(value, Math.max(0, maxBytes - byteLength(mark))) + mark;
}

export function oneLine(value: string, maxChars = 120): string {
  const clean = value.replace(/[\u0000-\u001f\u007f]+/g, " ").replace(/\s+/g, " ").trim();
  return Array.from(clean).length <= maxChars ? clean : Array.from(clean).slice(0, maxChars - 1).join("") + "…";
}

export function stripDownloadURLs(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(stripDownloadURLs);
  if (value !== null && typeof value === "object") {
    const out: Record<string, unknown> = {};
    for (const [key, item] of Object.entries(value as Record<string, unknown>)) {
      if (key === "download_url" || key === "downloadUrl") continue;
      out[key] = stripDownloadURLs(item);
    }
    return out;
  }
  return value;
}

export function asRecord(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined;
}

export function stringField(value: unknown, key: string): string {
  const field = asRecord(value)?.[key];
  return typeof field === "string" ? field : "";
}

export function numberField(value: unknown, key: string): number | undefined {
  const field = asRecord(value)?.[key];
  return typeof field === "number" && Number.isFinite(field) ? field : undefined;
}

export function boolField(value: unknown, key: string): boolean {
  return asRecord(value)?.[key] === true;
}
