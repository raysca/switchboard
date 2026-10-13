import { describe, expect, it } from "vitest";
import {
  MAX_STEPS,
  addDraftRoute,
  insertStep,
  emptyFlow,
  flowIssues,
  fromFlowConfig,
  isUnreachable,
  removeStep,
  setStart,
  slug,
  stepIssues,
  toPayload,
  uniqueName,
  unreachableSteps,
  validate,
} from "./model";

const stage = { provider: "google" };
const flat = {
  stt: { ...stage, model: "chirp" },
  llm: { ...stage, model: "gemini", system_prompt: "You are the front desk." },
  tts: { ...stage, voice: "v" },
  vad_stop_ms: 400,
};

describe("unreachableSteps", () => {
  it("finds the steps no path from the start step leads to, including steps reached only from those", () => {
    const g = fromFlowConfig({
      start: "a",
      steps: [
        { id: "a", name: "Reception", config: {} },
        { id: "b", name: "Billing", config: {} },
        { id: "c", name: "Support", config: {} },
        { id: "d", name: "Escalation", config: {} },
      ],
      routes: [
        { id: "e1", from: "a", to: "b", condition: "x" },
        { id: "e2", from: "d", to: "c", condition: "y" },
      ],
    });
    expect(unreachableSteps(g)).toEqual(["Support", "Escalation"]);
  });
  it("is empty when every step can be reached", () => {
    const g = fromFlowConfig({ start: "a", steps: [{ id: "a", name: "A", config: {} }, { id: "b", name: "B", config: {} }], routes: [{ id: "e", from: "a", to: "b", condition: "x" }] });
    expect(unreachableSteps(g)).toEqual([]);
  });
});

describe("graph model", () => {
  it("reads a flat config as one start step", () => {
    const g = fromFlowConfig(flat);
    expect(g.steps).toHaveLength(1);
    expect(g.start).toBe(g.steps[0].id);
    expect(g.steps[0].config).toEqual(flat);
    expect(validate(g)).toEqual([]);
  });

  it("round-trips a one-step draft back to the flat config", () => {
    expect(toPayload(fromFlowConfig(flat))).toEqual(flat);
  });

  it("reads a stored graph", () => {
    const stored = {
      start: "front",
      steps: [
        { id: "front", name: "Front", config: flat, position: { x: 1, y: 2 } },
        { id: "sales", name: "Sales", config: { llm: flat.llm, tts: flat.tts }, tools: ["srv/lookup"] },
      ],
      routes: [{ id: "e1", from: "front", to: "sales", condition: "wants to buy" }],
      route_cue: { name: "chime", after_ms: 200 },
    };
    const g = fromFlowConfig(stored);
    expect(g.steps[0].position).toEqual({ x: 1, y: 2 });
    expect(g.steps[1].tools).toEqual(["srv/lookup"]);
    expect(g.route_cue).toEqual({ name: "chime", after_ms: 200 });
    expect(toPayload(g)).toMatchObject({ start: "front", routes: stored.routes, route_cue: stored.route_cue });
  });

  it("addNode copies the entry's llm and tts and stops at the maximum", () => {
    let g = fromFlowConfig(flat);
    g = insertStep(g, "Sales");
    expect(g.steps[1].name).toBe("Sales");
    expect((g.steps[1].config.llm as { provider: string }).provider).toBe("google");
    expect((g.steps[1].config.llm as { system_prompt?: string }).system_prompt).toBeUndefined();
    expect(g.steps[1].config.stt).toBeUndefined();
    for (let i = 0; i < MAX_STEPS + 2; i++) g = insertStep(g, `Node ${i}`);
    expect(g.steps).toHaveLength(MAX_STEPS);
  });

  it("removeStep drops incident routes and refuses the start step", () => {
    let g = insertStep(fromFlowConfig(flat), "Sales");
    const sales = g.steps[1].id;
    g = addDraftRoute(g, g.start, sales);
    expect(g.routes).toHaveLength(1);
    expect(removeStep(g, g.start)).toBe(g);
    const after = removeStep(g, sales);
    expect(after.steps).toHaveLength(1);
    expect(after.routes).toHaveLength(0);
  });

  it("validate reports duplicate slugs and missing stage fields", () => {
    let g = insertStep(insertStep(fromFlowConfig(flat), "Sales Team"), "sales team");
    expect(validate(g).join(" ")).toMatch(/unique/);
    g = fromFlowConfig({ ...flat, llm: { provider: "google", model: "" } });
    expect(validate(g).join(" ")).toMatch(/LLM model/);
  });

  it("setEntry moves the call-level settings to the new entry", () => {
    let g = insertStep(fromFlowConfig(flat), "Sales");
    const sales = g.steps[1].id;
    g = setStart(g, sales);
    expect(g.start).toBe(sales);
    expect(g.steps[1].config.stt).toEqual(flat.stt);
    expect(g.steps[1].config.vad_stop_ms).toBe(400);
    expect(g.steps[0].config.stt).toBeUndefined();
    expect(g.steps[0].config.vad_stop_ms).toBeUndefined();
    expect(validate(g)).toEqual([]);
  });

  it("toPayload keeps call-level settings off non-start steps", () => {
    let g = insertStep(fromFlowConfig(flat), "Sales");
    g = { ...g, steps: g.steps.map((n, i) => (i === 1 ? { ...n, config: { ...n.config, stt: flat.stt, vad_stop_ms: 300 } } : n)) };
    const payload = toPayload(g) as { steps: { config: Record<string, unknown> }[] };
    expect(payload.steps[1].config.stt).toBeUndefined();
    expect(payload.steps[1].config.vad_stop_ms).toBeUndefined();
  });

  it("needs only providers and a model, and never emits credential_id", () => {
    const g = insertStep(fromFlowConfig(flat), "Sales");
    expect(validate(g)).toEqual([]);
    expect(JSON.stringify(toPayload(g))).not.toContain("credential");
    expect(validate(fromFlowConfig({ ...flat, stt: {} })).join(" ")).toMatch(/STT provider/);
  });

  it("slug matches the server's tool-name rule", () => {
    expect(slug("Sales Team!")).toBe("sales_team");
  });

  it("seeds a flat flow's start step with the flow's selected MCP tools", () => {
    const g = fromFlowConfig(flat, ["srv/lookup"]);
    expect(g.steps[0].tools).toEqual(["srv/lookup"]);
    // a one-step flow still publishes as the flat config: it exposes every selected tool anyway
    expect(toPayload(g)).toEqual(flat);
    const two = insertStep(g, "Sales");
    const payload = toPayload(two) as { steps: { tools?: string[] }[] };
    expect(payload.steps[0].tools).toEqual(["srv/lookup"]);
    expect(payload.steps[1].tools).toBeUndefined();
  });

  it("validate rejects a route sound name the server would reject", () => {
    const base = insertStep(fromFlowConfig(flat), "Sales");
    expect(validate({ ...base, route_cue: { name: "Bad Name" } }).join(" ")).toMatch(/sound name/);
    expect(validate({ ...base, route_cue: { name: "chime", after_ms: 20000 } }).join(" ")).toMatch(/after/);
    expect(validate({ ...base, route_cue: { name: "chime", after_ms: 300 } })).toEqual([]);
  });
});

describe("flow authoring", () => {
  const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
  const two = () =>
    fromFlowConfig({
      start: "a",
      steps: [
        { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
        { id: "b", name: "Billing", config: ready },
      ],
      routes: [],
    });

  it("starts a blank flow with no steps, and the first step added becomes the start step", () => {
    const g = insertStep(emptyFlow(), "New step");
    expect(g.steps).toHaveLength(1);
    expect(g.start).toBe(g.steps[0].id);
    expect(g.steps[0].config).toEqual({});
  });

  it("names new steps uniquely", () => {
    const g = insertStep(insertStep(emptyFlow(), "New step"), "New step 2");
    expect(uniqueName(emptyFlow(), "New step")).toBe("New step");
    expect(uniqueName(g, "New step")).toBe("New step 3");
  });

  it("adds a draft route with an empty condition and refuses self, duplicate and unknown routes", () => {
    const g = addDraftRoute(two(), "a", "b");
    expect(g.routes).toEqual([expect.objectContaining({ from: "a", to: "b", condition: "" })]);
    expect(addDraftRoute(g, "a", "b")).toBe(g);
    expect(addDraftRoute(g, "a", "a")).toBe(g);
    expect(addDraftRoute(g, "a", "zzz")).toBe(g);
  });

  it("reports what a step still needs, by the tab that fixes it", () => {
    const blank = insertStep(emptyFlow(), "New step");
    expect(stepIssues(blank, blank.start)).toEqual([
      { tab: "basics", message: "Choose a model." },
      { tab: "basics", message: "Choose a voice." },
    ]);
    const routed = addDraftRoute(two(), "a", "b");
    expect(stepIssues(routed, "a")).toEqual([{ tab: "routes", message: "Give the route to Billing a condition." }]);
    expect(stepIssues(two(), "b")).toEqual([]);
  });

  it("flags duplicate and empty step names on the basics tab", () => {
    const g = { ...two(), steps: two().steps.map((n) => ({ ...n, name: n.id === "b" ? "reception" : n.name })) };
    expect(stepIssues(g, "b")).toContainEqual({ tab: "basics", message: "Another step has this name." });
    const unnamed = { ...two(), steps: two().steps.map((n) => ({ ...n, name: n.id === "b" ? "  " : n.name })) };
    expect(stepIssues(unnamed, "b")).toContainEqual({ tab: "basics", message: "Give the step a name." });
  });

  it("reports flow-level problems: speech-to-text and recording disclosure", () => {
    const noStt = insertStep(emptyFlow(), "New step");
    expect(flowIssues(noStt)).toEqual(["Choose speech-to-text in Flow settings."]);
    const g = two();
    const rec = { ...g, steps: g.steps.map((n) => (n.id === "a" ? { ...n, config: { ...n.config, recording: { enabled: true, disclosure_confirmed: false } } } : n)) };
    expect(flowIssues(rec)).toEqual(["Confirm that callers are told the call is recorded."]);
    expect(flowIssues(g)).toEqual([]);
    expect(flowIssues(emptyFlow())).toEqual([]);
  });

  it("knows which steps no route leads to", () => {
    expect(isUnreachable(two(), "b")).toBe(true);
    expect(isUnreachable(two(), "a")).toBe(false);
    expect(isUnreachable(addDraftRoute(two(), "a", "b"), "b")).toBe(false);
  });

  it("blocks publishing while a route has no condition", () => {
    expect(validate(addDraftRoute(two(), "a", "b"))).toContain("The route from Reception to Billing needs a condition.");
  });
});

describe("server limits", () => {
  it("counts a route condition in bytes, as the server does", () => {
    const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
    const g = fromFlowConfig({
      start: "a",
      steps: [
        { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
        { id: "b", name: "Billing", config: ready },
      ],
      routes: [{ id: "e1", from: "a", to: "b", condition: "é".repeat(300) }],
    });
    expect("é".repeat(300).length).toBeLessThan(512);
    expect(validate(g)).toContain("The route from Reception to Billing has a condition longer than 512 bytes.");
  });
});

describe("what the server will accept", () => {
  const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
  const one = (start: Record<string, unknown> = {}, step: Record<string, unknown> = {}) =>
    fromFlowConfig({
      start: "a",
      steps: [
        { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready, ...start } },
        { id: "b", name: "Billing", config: { ...ready, ...step } },
      ],
      routes: [{ id: "e1", from: "a", to: "b", condition: "invoice" }],
    });

  it("keeps a named single step as a graph so its name survives publishing", () => {
    const g = insertStep(emptyFlow(), "Reception");
    const payload = toPayload(g) as { steps: { name: string }[] };
    expect(payload.steps.map((n) => n.name)).toEqual(["Reception"]);
  });

  it("flags turn-taking and memory values outside the server's ranges", () => {
    expect(flowIssues(one({ vad_stop_ms: 100 }))).toContain("Wait after the caller stops must be 200 to 2000 ms.");
    expect(flowIssues(one({ context: { max_messages: 1 } }))).toContain("Max messages must be at least 2.");
    expect(flowIssues(one({ context: { max_tokens: -5 } }))).toContain("Max tokens cannot be negative.");
    expect(validate(one({ vad_stop_ms: 2500 }))).toContain("Wait after the caller stops must be 200 to 2000 ms.");
  });

  it("flags summaries when a step's model provider cannot summarise", () => {
    const g = one({ context: { summarization: { enabled: true } } }, { llm: { provider: "groq", model: "llama" } });
    expect(flowIssues(g)).toContain("Billing uses groq, which cannot summarise older turns. Turn summaries off or change its model.");
  });

  it("flags too many fallbacks and a fallback that repeats the model", () => {
    const three = one({}, { llm: { ...ready.llm, fallbacks: [{ provider: "openai", model: "a" }, { provider: "openai", model: "b" }, { provider: "xai", model: "c" }] } });
    expect(stepIssues(three, "b")).toContainEqual({ tab: "advanced", message: "Use at most 2 model fallbacks." });
    const dup = one({}, { tts: { ...ready.tts, fallbacks: [{ provider: "google", voice: "v" }] } });
    expect(stepIssues(dup, "b")).toContainEqual({ tab: "advanced", message: "A voice fallback repeats the voice or another fallback." });
    expect(validate(dup)).toContain("Billing: A voice fallback repeats the voice or another fallback.");
  });

  it("round-trips a step's end_call flag", () => {
    const cfg = {
      start: "a",
      steps: [
        { id: "a", name: "A", end_call: true, config: { llm: {}, tts: {} } },
        { id: "b", name: "B", config: { llm: {}, tts: {} } },
      ],
      routes: [{ id: "r", from: "a", to: "b", condition: "x" }],
    };
    const g = fromFlowConfig(cfg);
    expect(g.steps[0].end_call).toBe(true);
    expect(g.steps[1].end_call).toBeFalsy();
    const out = toPayload(g) as { steps: { id: string; end_call?: boolean }[] };
    expect(out.steps[0].end_call).toBe(true);
    expect("end_call" in out.steps[1]).toBe(false);
  });
});
