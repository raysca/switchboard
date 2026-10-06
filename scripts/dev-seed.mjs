#!/usr/bin/env node
// Seeds a running development server with provider credentials and a ready
// "Dev Assistant" agent, from examples/.env (or the environment). Idempotent:
// an existing agent of that name is reused. Prints only the agent id on stdout;
// secrets are never printed.
//
// Needs GOVOICE_ADMIN_URL and GOVOICE_MANAGEMENT_API_KEY.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const AGENT = "Dev Assistant";
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const examples = path.join(root, "examples");
const admin = process.env.GOVOICE_ADMIN_URL;
const key = process.env.GOVOICE_MANAGEMENT_API_KEY;
const log = (msg) => console.error(`[seed] ${msg}`);

// examples/.env fills what the environment does not set.
const envFile = path.join(examples, ".env");
if (fs.existsSync(envFile)) {
  for (const line of fs.readFileSync(envFile, "utf8").split("\n")) {
    const m = line.match(/^\s*(GOOGLE_API_KEY|GOOGLE_APPLICATION_CREDENTIALS)\s*=\s*(.*?)\s*$/);
    if (m && !process.env[m[1]]) process.env[m[1]] = m[2].replace(/^["']|["']$/g, "");
  }
}

async function api(method, route, body) {
  const res = await fetch(`${admin}/api/v1${route}`, {
    method,
    headers: { Authorization: `Bearer ${key}`, "Content-Type": "application/json" },
    body: body && JSON.stringify(body),
  });
  const json = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(`${method} ${route}: ${res.status} ${json?.error?.code ?? ""} ${json?.error?.message ?? ""}`);
  return json.data;
}

// Reuses a stored Google API key whose masked suffix matches, else adds it.
// Service-account JSON always ends the same way ('om"}'), so the suffix cannot
// tell two apart: it is always added fresh.
async function credentialFor(secret) {
  const suffix = [...secret.trim()].slice(-4).join("");
  const existing = !secret.trim().startsWith("{") && (await api("GET", "/providers/credentials")).find(
    (c) => c.provider === "google" && c.masked_value.endsWith(suffix),
  );
  return existing?.id ?? (await api("POST", "/providers/credentials", { provider: "google", secret })).id;
}

// Archived (deleted in the UI) agents are still listed; never reuse one.
const existing = (await api("GET", "/agents?limit=100")).find((a) => !a.archived && a.name === AGENT);
if (existing) {
  log(`using existing agent "${AGENT}"`);
  console.log(existing.id);
  process.exit(0);
}

// Streaming Speech-to-Text needs a service account; the LLM prefers the Gemini
// API key (service accounts often lack Vertex AI access).
const sa = process.env.GOOGLE_APPLICATION_CREDENTIALS;
const saPath = sa ? (path.isAbsolute(sa) ? sa : path.join(examples, sa)) : "";
const saSecret = saPath && fs.existsSync(saPath) ? JSON.stringify(JSON.parse(fs.readFileSync(saPath, "utf8"))) : "";
const apiKey = process.env.GOOGLE_API_KEY ?? "";
if (!saSecret && !apiKey) {
  log("no Google secrets in examples/.env (see examples/.env.example): skipped; add credentials and an agent in the admin UI");
  process.exit(0);
}
if (!saSecret) log("no service account: voice input will not work (streaming STT rejects API keys); text turns will");

const speech = await credentialFor(saSecret || apiKey);
const llm = apiKey ? await credentialFor(apiKey) : speech;
const agent = await api("POST", "/agents", {
  name: AGENT,
  description: "Created by make dev",
  config: {
    stt: { provider: "google", model: "chirp_2", language: "en-US", credential_id: speech },
    llm: {
      provider: "google",
      model: "gemini-2.5-flash",
      system_prompt: "You are a concise, friendly voice assistant. Answer in one or two short sentences.",
      credential_id: llm,
    },
    tts: { provider: "google", voice: "en-US-Journey-F", credential_id: speech },
  },
});
log(`created agent "${AGENT}"`);
console.log(agent.id);
