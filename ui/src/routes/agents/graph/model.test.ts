import { describe, expect, it } from "vitest";
import {
  MAX_NODES,
  addNode,
  connect,
  connectError,
  fromAgentConfig,
  removeNode,
  setEntry,
  slug,
  toPayload,
  validate,
} from "./model";

const stage = { provider: "google", credential_id: "c1" };
const flat = {
  stt: { ...stage, model: "chirp" },
  llm: { ...stage, model: "gemini", system_prompt: "You are the front desk." },
  tts: { ...stage, voice: "v" },
  vad_stop_ms: 400,
};

describe("graph model", () => {
  it("reads a flat config as one entry node", () => {
    const g = fromAgentConfig(flat);
    expect(g.nodes).toHaveLength(1);
    expect(g.entry).toBe(g.nodes[0].id);
    expect(g.nodes[0].config).toEqual(flat);
    expect(validate(g)).toEqual([]);
  });

  it("round-trips a one-node draft back to the flat config", () => {
    expect(toPayload(fromAgentConfig(flat))).toEqual(flat);
  });

  it("reads a stored graph", () => {
    const stored = {
      entry: "front",
      nodes: [
        { id: "front", name: "Front", config: flat, position: { x: 1, y: 2 } },
        { id: "sales", name: "Sales", config: { llm: flat.llm, tts: flat.tts }, tools: ["srv/lookup"] },
      ],
      edges: [{ id: "e1", from: "front", to: "sales", condition: "wants to buy" }],
      handoff_cue: { name: "chime", after_ms: 200 },
    };
    const g = fromAgentConfig(stored);
    expect(g.nodes[0].position).toEqual({ x: 1, y: 2 });
    expect(g.nodes[1].tools).toEqual(["srv/lookup"]);
    expect(g.handoff_cue).toEqual({ name: "chime", after_ms: 200 });
    expect(toPayload(g)).toMatchObject({ entry: "front", edges: stored.edges, handoff_cue: stored.handoff_cue });
  });

  it("addNode copies the entry's llm and tts and stops at the maximum", () => {
    let g = fromAgentConfig(flat);
    g = addNode(g, "Sales");
    expect(g.nodes[1].name).toBe("Sales");
    expect((g.nodes[1].config.llm as { provider: string }).provider).toBe("google");
    expect((g.nodes[1].config.llm as { system_prompt?: string }).system_prompt).toBeUndefined();
    expect(g.nodes[1].config.stt).toBeUndefined();
    for (let i = 0; i < MAX_NODES + 2; i++) g = addNode(g, `Node ${i}`);
    expect(g.nodes).toHaveLength(MAX_NODES);
  });

  it("removeNode drops incident edges and refuses the entry", () => {
    let g = addNode(fromAgentConfig(flat), "Sales");
    const sales = g.nodes[1].id;
    g = connect(g, g.entry, sales, "wants to buy");
    expect(g.edges).toHaveLength(1);
    expect(removeNode(g, g.entry)).toBe(g);
    const after = removeNode(g, sales);
    expect(after.nodes).toHaveLength(1);
    expect(after.edges).toHaveLength(0);
  });

  it("connect rejects self edges, duplicates and empty conditions", () => {
    let g = addNode(fromAgentConfig(flat), "Sales");
    const [a, b] = [g.nodes[0].id, g.nodes[1].id];
    expect(connectError(g, a, a, "x")).toMatch(/itself/);
    expect(connectError(g, a, b, "  ")).toMatch(/condition/);
    g = connect(g, a, b, "wants to buy");
    expect(connectError(g, a, b, "again")).toMatch(/already/);
    expect(connect(g, a, b, "again")).toBe(g);
    expect(connectError(g, b, a, "back")).toBeNull();
  });

  it("validate reports duplicate slugs and missing stage fields", () => {
    let g = addNode(addNode(fromAgentConfig(flat), "Sales Team"), "sales team");
    expect(validate(g).join(" ")).toMatch(/unique/);
    g = fromAgentConfig({ ...flat, llm: { provider: "google", credential_id: "c1", model: "" } });
    expect(validate(g).join(" ")).toMatch(/LLM model/);
  });

  it("setEntry moves the call-level settings to the new entry", () => {
    let g = addNode(fromAgentConfig(flat), "Sales");
    const sales = g.nodes[1].id;
    g = setEntry(g, sales);
    expect(g.entry).toBe(sales);
    expect(g.nodes[1].config.stt).toEqual(flat.stt);
    expect(g.nodes[1].config.vad_stop_ms).toBe(400);
    expect(g.nodes[0].config.stt).toBeUndefined();
    expect(g.nodes[0].config.vad_stop_ms).toBeUndefined();
    expect(validate(g)).toEqual([]);
  });

  it("toPayload keeps call-level settings off non-entry nodes", () => {
    let g = addNode(fromAgentConfig(flat), "Sales");
    g = { ...g, nodes: g.nodes.map((n, i) => (i === 1 ? { ...n, config: { ...n.config, stt: flat.stt, vad_stop_ms: 300 } } : n)) };
    const payload = toPayload(g) as { nodes: { config: Record<string, unknown> }[] };
    expect(payload.nodes[1].config.stt).toBeUndefined();
    expect(payload.nodes[1].config.vad_stop_ms).toBeUndefined();
  });

  it("slug matches the server's tool-name rule", () => {
    expect(slug("Sales Team!")).toBe("sales_team");
  });
});
