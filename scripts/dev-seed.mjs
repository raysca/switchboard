#!/usr/bin/env node
// Seeds a running development server with a ready "Dev Assistant" agent and a two-agent
// "Dealership desk" graph for demos, built from the providers the server detected in its
// environment (GET /api/v1/providers): the first provider that can run each stage, on its
// catalog defaults. Idempotent: existing agents of those names are reused. Prints only the
// Dev Assistant id on stdout.
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
const agents = (await api("GET", "/agents?limit=100")).filter((a) => !a.archived);
const find = (name) => agents.find((a) => a.name === name);

// The first detected provider for each stage, on its catalog defaults; null (logged) when a stage has none.
async function stageDefaults() {
  const providers = await api("GET", "/providers");
  const dflt = (list) => list?.find((e) => e.default)?.id;
  const config = {};
  for (const kind of ["stt", "llm", "tts"]) {
    const p = providers.find((p) => p.services[kind]?.available);
    if (!p) {
      const options = providers.filter((p) => p.services[kind]?.missing_env?.length).map((p) => `${p.id}: ${p.services[kind].missing_env.join(" + ")}`);
      log(`no ${kind.toUpperCase()} provider detected: skipped; set the variables of one provider (${options.join("; ")}) in examples/.env (see examples/.env.example) and rerun make dev`);
      return null;
    }
    const svc = p.services[kind];
    config[kind] = { provider: p.id };
    if (kind === "tts") config[kind].voice = dflt(svc.voices);
    else config[kind].model = dflt(svc.models);
    if (kind === "stt") config[kind].language = dflt(svc.languages);
  }
  return config;
}

let stages;
const defaults = async () => (stages ??= await stageDefaults());

// "Dev Assistant": one agent, the target of the examples.
let assistant = find(AGENT);
if (assistant) {
  log(`using existing agent "${AGENT}"`);
} else {
  const base = await defaults();
  if (!base) process.exit(0);
  const config = { ...base, llm: { ...base.llm, system_prompt: "You are a concise, friendly voice assistant. Answer in one or two short sentences." } };
  assistant = await api("POST", "/agents", { name: AGENT, description: "Created by make dev", config });
  log(`created agent "${AGENT}" (${["stt", "llm", "tts"].map((k) => config[k].provider).join(" / ")})`);
}

// "Dealership desk": a two-agent graph for demos. The front desk hands buyers to sales.
const DESK = "Dealership desk";
if (find(DESK)) {
  log(`using existing agent "${DESK}"`);
} else {
  const base = await defaults();
  if (base) {
    const front = {
      ...base,
      llm: { ...base.llm, system_prompt: "You are the front desk of a car dealership. Greet the caller and answer in one short sentence. When the caller wants to buy a car or asks about prices, transfer them to Sales and say you are connecting them." },
    };
    const sales = {
      llm: { ...base.llm, system_prompt: "You are the sales agent of a car dealership. We sell a compact hatchback, a family SUV and an electric saloon. Help the caller choose in one or two short sentences." },
      tts: base.tts,
    };
    await api("POST", "/agents", {
      name: DESK,
      description: "Front desk hands buyers to sales",
      config: {
        entry: "front",
        nodes: [
          { id: "front", name: "Front desk", config: front, position: { x: 0, y: 80 } },
          { id: "sales", name: "Sales", config: sales, position: { x: 420, y: 80 } },
        ],
        edges: [{ id: "front-sales", from: "front", to: "sales", condition: "The caller wants to buy a car or asks about prices." }],
      },
    });
    log(`created agent "${DESK}"`);
  }
}
console.log(assistant.id);
