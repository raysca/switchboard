#!/usr/bin/env node
// Seeds a running development server with a ready "Dev Assistant" agent and a two-step
// "Dealership desk" agent for demos, built from the providers the server detected in its
// environment (GET /api/v1/providers): the first provider that can run each stage, on its
// catalog defaults. Idempotent: existing agents of those names are reused. Prints only the
// Dev Assistant id on stdout. With MCPTEST_URL set it also registers the test MCP servers.
//
// Needs GOVOICE_ADMIN_URL and GOVOICE_MANAGEMENT_API_KEY.
const AGENT_NAME = "Dev Assistant";
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
let assistant = find(AGENT_NAME);
if (assistant) {
  log(`using existing agent "${AGENT_NAME}"`);
} else {
  const base = await defaults();
  if (!base) process.exit(0);
  const config = { ...base, llm: { ...base.llm, system_prompt: "You are a concise, friendly voice assistant. Answer in one or two short sentences." } };
  assistant = await api("POST", "/agents", { name: AGENT_NAME, description: "Created by make dev", config });
  log(`created agent "${AGENT_NAME}" (${["stt", "llm", "tts"].map((k) => config[k].provider).join(" / ")})`);
}

// "Dealership desk": a two-step agent for demos. The front desk hands buyers to sales.
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
    // A desk that cannot be created must not cost the examples their Dev Assistant id.
    try {
      await api("POST", "/agents", {
        name: DESK,
        description: "Front desk hands buyers to sales",
        config: {
          start: "front",
          steps: [
            { id: "front", name: "Front desk", config: front, position: { x: 0, y: 80 } },
            { id: "sales", name: "Sales", config: sales, position: { x: 420, y: 80 } },
          ],
          handoffs: [{ id: "front-sales", from: "front", to: "sales", condition: "The caller wants to buy a car or asks about prices." }],
        },
      });
      log(`created agent "${DESK}"`);
    } catch (err) {
      log(`could not create "${DESK}": ${err.message}`);
    }
  }
}
// Test MCP servers (cmd/mcptest, started by make dev): one per state the MCP page shows.
const mcpURL = process.env.MCPTEST_URL;
if (mcpURL) {
  const servers = await api("GET", "/mcp-servers");
  for (const [name, path, authEnv] of [
    ["Test MCP: ok", "/ok", "MCP_TOKEN_TEST"],
    ["Test MCP: failing", "/fail/tools", ""],
    ["Test MCP: slow", "/slow", ""],
  ]) {
    if (servers.some((s) => s.name === name)) continue;
    try {
      const created = await api("POST", "/mcp-servers", { name, endpoint: mcpURL + path, auth_env: authEnv, enabled: true });
      const checked = await api("POST", `/mcp-servers/${created.id}/validate`);
      log(`created MCP server "${name}" (${checked.validation_state})`);
    } catch (err) {
      log(`could not create MCP server "${name}": ${err.message}`);
    }
  }
}
console.log(assistant.id);
