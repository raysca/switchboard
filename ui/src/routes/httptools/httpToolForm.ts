import type { HTTPToolDefinition } from "../../api/client";

/** The editor's fields as text, so a half-typed value is never lost to parsing. */
export type HTTPToolForm = {
  name: string;
  description: string;
  method: string;
  url: string;
  headers: string; // one "Name: value" per line
  authSecret: string;
  authHeader: string;
  schema: string; // JSON
  timeoutMs: string;
  maxResponseBytes: string;
  pick: string; // one JSON pointer per line
};

export const METHODS = ["GET", "POST", "PUT", "PATCH", "DELETE"] as const;

export const EMPTY_SCHEMA = '{\n  "type": "object",\n  "properties": {},\n  "required": []\n}';

export const emptyForm = (): HTTPToolForm => ({ name: "", description: "", method: "GET", url: "https://", headers: "", authSecret: "", authHeader: "", schema: EMPTY_SCHEMA, timeoutMs: "", maxResponseBytes: "", pick: "" });

export function toForm(d: HTTPToolDefinition): HTTPToolForm {
  return {
    name: d.name,
    description: d.description,
    method: d.method,
    url: d.url,
    headers: Object.entries(d.headers ?? {})
      .map(([k, v]) => `${k}: ${v}`)
      .join("\n"),
    authSecret: d.auth?.secret ?? "",
    authHeader: d.auth?.header && d.auth.header !== "Authorization" ? d.auth.header : "",
    schema: JSON.stringify(d.input_schema ?? {}, null, 2),
    timeoutMs: d.timeout_ms ? String(d.timeout_ms) : "",
    maxResponseBytes: d.max_response_bytes ? String(d.max_response_bytes) : "",
    pick: (d.response_pick ?? []).join("\n"),
  };
}

export type FormResult = { definition: HTTPToolDefinition } | { error: string };

/** Text fields to a definition, or the first problem found. The server validates everything again. */
export function fromForm(f: HTTPToolForm): FormResult {
  let schema: unknown;
  try {
    schema = JSON.parse(f.schema);
  } catch {
    return { error: "Input schema is not valid JSON." };
  }
  const headers: Record<string, string> = {};
  for (const line of f.headers.split("\n")) {
    if (!line.trim()) continue;
    const at = line.indexOf(":");
    if (at <= 0) return { error: `Header "${line.trim()}" needs the form Name: value.` };
    headers[line.slice(0, at).trim()] = line.slice(at + 1).trim();
  }
  const number = (label: string, text: string): number | undefined | string => {
    if (!text.trim()) return undefined;
    const n = Number(text);
    return Number.isInteger(n) && n > 0 ? n : `${label} must be a whole number.`;
  };
  const timeout = number("Timeout", f.timeoutMs);
  const size = number("Response limit", f.maxResponseBytes);
  if (typeof timeout === "string") return { error: timeout };
  if (typeof size === "string") return { error: size };
  const secret = f.authSecret.trim();
  const pick = f.pick
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);
  return {
    definition: {
      name: f.name.trim(),
      description: f.description.trim(),
      method: f.method,
      url: f.url.trim(),
      headers: Object.keys(headers).length ? headers : undefined,
      auth: secret ? { secret, header: f.authHeader.trim() || undefined } : undefined,
      input_schema: schema,
      timeout_ms: timeout,
      max_response_bytes: size,
      response_pick: pick.length ? pick : undefined,
    },
  };
}
