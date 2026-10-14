import { describe, expect, it } from "vitest";
import { describeChanges, describeToolChanges } from "./diff";
import { addDraftRoute, insertStep, fromFlowConfig, setStart, updateRoute, updateStep, type FlowDraft } from "./model";

const ready = { llm: { provider: "google", model: "gemini", system_prompt: "Hi." }, tts: { provider: "google", voice: "aoede" } };
const base = fromFlowConfig({
  start: "a",
  steps: [
    { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
    { id: "b", name: "Billing", config: ready },
  ],
  routes: [{ id: "e1", from: "a", to: "b", condition: "invoice" }],
});

describe("describeChanges", () => {
  it("says nothing for a first version or an unchanged draft", () => {
    expect(describeChanges(null, base)).toEqual([]);
    expect(describeChanges(base, base)).toEqual([]);
  });

  it("names step, model, voice, instruction and route changes", () => {
    let g = updateStep(base, "b", { name: "Accounts", config: { ...ready, llm: { ...ready.llm, model: "flash", system_prompt: "Bills." }, tts: { provider: "google", voice: "puck" } } });
    g = updateRoute(g, "e1", "asks about a bill");
    g = insertStep(g, "Support");
    g = addDraftRoute(g, "a", g.steps[2].id);
    g = updateRoute(g, g.routes[1].id, "has a fault");
    expect(describeChanges(base, g)).toEqual([
      "Added step Support",
      "Renamed Billing to Accounts",
      "Accounts: model gemini → flash",
      "Accounts: voice aoede → puck",
      "Accounts: instructions changed",
      "Route Reception → Accounts: condition changed",
      "New route Reception → Support",
    ]);
  });

  it("reports removed steps and routes, a new start step and flow settings", () => {
    let g: FlowDraft = { ...base, routes: [] };
    g = updateStep(g, "a", { config: { ...g.steps[0].config, vad_stop_ms: 480 } });
    g = setStart(g, "b");
    expect(describeChanges(base, g)).toEqual(["Removed route Reception → Billing", "Start step is now Billing", "Flow settings: turn-taking changed"]);
  });

  it("reports the end-call toggle", () => {
    expect(describeChanges(base, updateStep(base, "b", { end_call: true }))).toEqual(["Billing: end call enabled"]);
  });
});

describe("describeToolChanges", () => {
  const sel = (name: string, required = false, hash = "h") => ({ mcp_server_id: "crm", remote_name: name, schema_hash: hash, required });
  it("names tools added and removed, and a tool that became required or optional", () => {
    expect(describeToolChanges([], [])).toEqual([]);
    expect(describeToolChanges([sel("a")], [sel("a")])).toEqual([]);
    expect(describeToolChanges([sel("a")], [sel("a"), sel("b")])).toEqual(["Added tool crm/b"]);
    expect(describeToolChanges([sel("a"), sel("b")], [sel("a")])).toEqual(["Removed tool crm/b"]);
    expect(describeToolChanges([sel("a")], [sel("a", true)])).toEqual(["Tool crm/a is now required"]);
    expect(describeToolChanges([sel("a", true)], [sel("a")])).toEqual(["Tool crm/a is now optional"]);
  });
});
