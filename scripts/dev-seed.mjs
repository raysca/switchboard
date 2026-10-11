#!/usr/bin/env node
// Seeds a running development server with a ready "Dev Assistant" agent built from
// the providers the server detected in its environment (GET /api/v1/providers): the
// first provider that can run each stage, on its catalog defaults. Idempotent: an
// existing agent of that name is reused. Prints only the agent id on stdout.
//
// Needs GOVOICE_ADMIN_URL and GOVOICE_MANAGEMENT_API_KEY.
const AGENT = "Dev Assistant";
const admin = process.env.GOVOICE_ADMIN_URL;
const key = process.env.GOVOICE_MANAGEMENT_API_KEY;
const log = (msg) => console.error(`[seed] ${msg}`);

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

// Archived (deleted in the UI) agents are still listed; never reuse one.
const existing = (await api("GET", "/agents?limit=100")).find((a) => !a.archived && a.name === AGENT);
if (existing) {
  log(`using existing agent "${AGENT}"`);
  console.log(existing.id);
  process.exit(0);
}

const providers = await api("GET", "/providers");
const dflt = (list) => list?.find((e) => e.default)?.id;
const config = {};
for (const kind of ["stt", "llm", "tts"]) {
  const p = providers.find((p) => p.services[kind]?.available);
  if (!p) {
    const missing = [...new Set(providers.flatMap((p) => p.services[kind]?.missing_env ?? []))];
    log(`no ${kind.toUpperCase()} provider detected: skipped; set one of ${missing.join(", ")} in examples/.env (see examples/.env.example) and rerun make dev`);
    process.exit(0);
  }
  const svc = p.services[kind];
  config[kind] = { provider: p.id };
  if (kind === "tts") config[kind].voice = dflt(svc.voices);
  else config[kind].model = dflt(svc.models);
  if (kind === "stt") config[kind].language = dflt(svc.languages);
}
config.llm.system_prompt = "You are a concise, friendly voice assistant. Answer in one or two short sentences.";

const agent = await api("POST", "/agents", { name: AGENT, description: "Created by make dev", config });
log(`created agent "${AGENT}" (${["stt", "llm", "tts"].map((k) => config[k].provider).join(" / ")})`);
console.log(agent.id);
