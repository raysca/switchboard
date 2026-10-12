import { describe, expect, it } from "vitest";
import { describeChanges } from "./diff";
import { addDraftRoute, addNode, fromAgentConfig, setEntry, updateEdge, updateNode, type GraphDraft } from "./model";

const ready = { llm: { provider: "google", model: "gemini", system_prompt: "Hi." }, tts: { provider: "google", voice: "aoede" } };
const base = fromAgentConfig({
  entry: "a",
  nodes: [
    { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
    { id: "b", name: "Billing", config: ready },
  ],
  edges: [{ id: "e1", from: "a", to: "b", condition: "invoice" }],
});

describe("describeChanges", () => {
  it("says nothing for a first version or an unchanged draft", () => {
    expect(describeChanges(null, base)).toEqual([]);
    expect(describeChanges(base, base)).toEqual([]);
  });

  it("names step, model, voice, instruction and route changes", () => {
    let g = updateNode(base, "b", { name: "Accounts", config: { ...ready, llm: { ...ready.llm, model: "flash", system_prompt: "Bills." }, tts: { provider: "google", voice: "puck" } } });
    g = updateEdge(g, "e1", "asks about a bill");
    g = addNode(g, "Support");
    g = addDraftRoute(g, "a", g.nodes[2].id);
    g = updateEdge(g, g.edges[1].id, "has a fault");
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
    let g: GraphDraft = { ...base, edges: [] };
    g = updateNode(g, "a", { config: { ...g.nodes[0].config, vad_stop_ms: 480 } });
    g = setEntry(g, "b");
    expect(describeChanges(base, g)).toEqual(["Removed route Reception → Billing", "Start step is now Billing", "Flow settings: turn-taking changed"]);
  });
});
