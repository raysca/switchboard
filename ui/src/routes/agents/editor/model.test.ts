import { describe, expect, it } from "vitest";
import {
  MAX_GREETING_BYTES,
  MAX_STEPS,
  applySelection,
  addDraftHandoff,
  insertStep,
  emptyAgent,
  agentIssues,
  fromAgentConfig,
  isUnreachable,
  removeStep,
  setStart,
  slug,
  stepIssues,
  toPayload,
  uniqueName,
  updateStep,
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
    const g = fromAgentConfig({
      start: "a",
      steps: [
        { id: "a", name: "Reception", config: {} },
        { id: "b", name: "Billing", config: {} },
        { id: "c", name: "Support", config: {} },
        { id: "d", name: "Escalation", config: {} },
      ],
      handoffs: [
        { id: "e1", from: "a", to: "b", condition: "x" },
        { id: "e2", from: "d", to: "c", condition: "y" },
      ],
    });
    expect(unreachableSteps(g)).toEqual(["Support", "Escalation"]);
  });
  it("is empty when every step can be reached", () => {
    const g = fromAgentConfig({ start: "a", steps: [{ id: "a", name: "A", config: {} }, { id: "b", name: "B", config: {} }], handoffs: [{ id: "e", from: "a", to: "b", condition: "x" }] });
    expect(unreachableSteps(g)).toEqual([]);
  });
});

describe("graph model", () => {
  it("reads a flat config as one start step", () => {
    const g = fromAgentConfig(flat);
    expect(g.steps).toHaveLength(1);
    expect(g.start).toBe(g.steps[0].id);
    expect(g.steps[0].config).toEqual(flat);
    expect(validate(g)).toEqual([]);
  });

  it("round-trips a one-step draft back to the flat config", () => {
    expect(toPayload(fromAgentConfig(flat))).toEqual(flat);
  });

  it("reads a stored graph", () => {
    const stored = {
      start: "front",
      steps: [
        { id: "front", name: "Front", config: flat, position: { x: 1, y: 2 } },
        { id: "sales", name: "Sales", config: { llm: flat.llm, tts: flat.tts }, tools: ["srv/lookup"] },
      ],
      handoffs: [{ id: "e1", from: "front", to: "sales", condition: "wants to buy" }],
      handoff_cue: { name: "chime", after_ms: 200 },
    };
    const g = fromAgentConfig(stored);
    expect(g.steps[0].position).toEqual({ x: 1, y: 2 });
    expect(g.steps[1].tools).toEqual(["srv/lookup"]);
    expect(g.handoff_cue).toEqual({ name: "chime", after_ms: 200 });
    expect(toPayload(g)).toMatchObject({ start: "front", handoffs: stored.handoffs, handoff_cue: stored.handoff_cue });
  });

  it("addNode copies the entry's llm and tts and stops at the maximum", () => {
    let g = fromAgentConfig(flat);
    g = insertStep(g, "Sales");
    expect(g.steps[1].name).toBe("Sales");
    expect((g.steps[1].config.llm as { provider: string }).provider).toBe("google");
    expect((g.steps[1].config.llm as { system_prompt?: string }).system_prompt).toBeUndefined();
    expect(g.steps[1].config.stt).toBeUndefined();
    for (let i = 0; i < MAX_STEPS + 2; i++) g = insertStep(g, `Node ${i}`);
    expect(g.steps).toHaveLength(MAX_STEPS);
  });

  it("removeStep drops incident routes and refuses the start step", () => {
    let g = insertStep(fromAgentConfig(flat), "Sales");
    const sales = g.steps[1].id;
    g = addDraftHandoff(g, g.start, sales);
    expect(g.handoffs).toHaveLength(1);
    expect(removeStep(g, g.start)).toBe(g);
    const after = removeStep(g, sales);
    expect(after.steps).toHaveLength(1);
    expect(after.handoffs).toHaveLength(0);
  });

  it("validate reports duplicate slugs and missing stage fields", () => {
    let g = insertStep(insertStep(fromAgentConfig(flat), "Sales Team"), "sales team");
    expect(validate(g).join(" ")).toMatch(/unique/);
    g = fromAgentConfig({ ...flat, llm: { provider: "google", model: "" } });
    expect(validate(g).join(" ")).toMatch(/LLM model/);
  });

  it("setEntry moves the call-level settings to the new entry", () => {
    let g = insertStep(fromAgentConfig(flat), "Sales");
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
    let g = insertStep(fromAgentConfig(flat), "Sales");
    g = { ...g, steps: g.steps.map((n, i) => (i === 1 ? { ...n, config: { ...n.config, stt: flat.stt, vad_stop_ms: 300 } } : n)) };
    const payload = toPayload(g) as { steps: { config: Record<string, unknown> }[] };
    expect(payload.steps[1].config.stt).toBeUndefined();
    expect(payload.steps[1].config.vad_stop_ms).toBeUndefined();
  });

  it("needs only providers and a model, and never emits credential_id", () => {
    const g = insertStep(fromAgentConfig(flat), "Sales");
    expect(validate(g)).toEqual([]);
    expect(JSON.stringify(toPayload(g))).not.toContain("credential");
    expect(validate(fromAgentConfig({ ...flat, stt: {} })).join(" ")).toMatch(/STT provider/);
  });

  it("slug matches the server's tool-name rule", () => {
    expect(slug("Sales Team!")).toBe("sales_team");
  });

  it("seeds a flat flow's start step with the flow's selected MCP tools", () => {
    const g = fromAgentConfig(flat, ["srv/lookup"]);
    expect(g.steps[0].tools).toEqual(["srv/lookup"]);
    // a one-step flow still publishes as the flat config: it exposes every selected tool anyway
    expect(toPayload(g)).toEqual(flat);
    const two = insertStep(g, "Sales");
    const payload = toPayload(two) as { steps: { tools?: string[] }[] };
    expect(payload.steps[0].tools).toEqual(["srv/lookup"]);
    expect(payload.steps[1].tools).toBeUndefined();
  });

  it("validate rejects a route sound name the server would reject", () => {
    const base = insertStep(fromAgentConfig(flat), "Sales");
    expect(validate({ ...base, handoff_cue: { name: "Bad Name" } }).join(" ")).toMatch(/sound name/);
    expect(validate({ ...base, handoff_cue: { name: "chime", after_ms: 20000 } }).join(" ")).toMatch(/after/);
    expect(validate({ ...base, handoff_cue: { name: "chime", after_ms: 300 } })).toEqual([]);
  });
});

describe("agent authoring", () => {
  const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
  const two = () =>
    fromAgentConfig({
      start: "a",
      steps: [
        { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
        { id: "b", name: "Billing", config: ready },
      ],
      handoffs: [],
    });

  it("starts a blank flow with no steps, and the first step added becomes the start step", () => {
    const g = insertStep(emptyAgent(), "New step");
    expect(g.steps).toHaveLength(1);
    expect(g.start).toBe(g.steps[0].id);
    expect(g.steps[0].config).toEqual({});
  });

  it("names new steps uniquely", () => {
    const g = insertStep(insertStep(emptyAgent(), "New step"), "New step 2");
    expect(uniqueName(emptyAgent(), "New step")).toBe("New step");
    expect(uniqueName(g, "New step")).toBe("New step 3");
  });

  it("adds a draft route with an empty condition and refuses self, duplicate and unknown routes", () => {
    const g = addDraftHandoff(two(), "a", "b");
    expect(g.handoffs).toEqual([expect.objectContaining({ from: "a", to: "b", condition: "" })]);
    expect(addDraftHandoff(g, "a", "b")).toBe(g);
    expect(addDraftHandoff(g, "a", "a")).toBe(g);
    expect(addDraftHandoff(g, "a", "zzz")).toBe(g);
  });

  it("reports what a step still needs, by the tab that fixes it", () => {
    const blank = insertStep(emptyAgent(), "New step");
    expect(stepIssues(blank, blank.start)).toEqual([
      { tab: "basics", message: "Choose a model." },
      { tab: "basics", message: "Choose a voice." },
    ]);
    const handedOff = addDraftHandoff(two(), "a", "b");
    expect(stepIssues(handedOff, "a")).toEqual([{ tab: "handoffs", message: "Give the route to Billing a condition." }]);
    expect(stepIssues(two(), "b")).toEqual([]);
  });

  it("flags duplicate and empty step names on the basics tab", () => {
    const g = { ...two(), steps: two().steps.map((n) => ({ ...n, name: n.id === "b" ? "reception" : n.name })) };
    expect(stepIssues(g, "b")).toContainEqual({ tab: "basics", message: "Another step has this name." });
    const unnamed = { ...two(), steps: two().steps.map((n) => ({ ...n, name: n.id === "b" ? "  " : n.name })) };
    expect(stepIssues(unnamed, "b")).toContainEqual({ tab: "basics", message: "Give the step a name." });
  });

  it("reports flow-level problems: speech-to-text and recording disclosure", () => {
    const noStt = insertStep(emptyAgent(), "New step");
    expect(agentIssues(noStt)).toEqual(["Choose speech-to-text in Flow settings."]);
    const g = two();
    const rec = { ...g, steps: g.steps.map((n) => (n.id === "a" ? { ...n, config: { ...n.config, recording: { enabled: true, disclosure_confirmed: false } } } : n)) };
    expect(agentIssues(rec)).toEqual(["Confirm that callers are told the call is recorded."]);
    expect(agentIssues(g)).toEqual([]);
    expect(agentIssues(emptyAgent())).toEqual([]);
  });

  it("knows which steps no route leads to", () => {
    expect(isUnreachable(two(), "b")).toBe(true);
    expect(isUnreachable(two(), "a")).toBe(false);
    expect(isUnreachable(addDraftHandoff(two(), "a", "b"), "b")).toBe(false);
  });

  it("blocks publishing while a route has no condition", () => {
    expect(validate(addDraftHandoff(two(), "a", "b"))).toContain("The route from Reception to Billing needs a condition.");
  });
});

describe("server limits", () => {
  it("counts a route condition in bytes, as the server does", () => {
    const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
    const g = fromAgentConfig({
      start: "a",
      steps: [
        { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
        { id: "b", name: "Billing", config: ready },
      ],
      handoffs: [{ id: "e1", from: "a", to: "b", condition: "é".repeat(300) }],
    });
    expect("é".repeat(300).length).toBeLessThan(512);
    expect(validate(g)).toContain("The route from Reception to Billing has a condition longer than 512 bytes.");
  });
});

describe("what the server will accept", () => {
  const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
  const one = (start: Record<string, unknown> = {}, step: Record<string, unknown> = {}) =>
    fromAgentConfig({
      start: "a",
      steps: [
        { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready, ...start } },
        { id: "b", name: "Billing", config: { ...ready, ...step } },
      ],
      handoffs: [{ id: "e1", from: "a", to: "b", condition: "invoice" }],
    });

  it("keeps a named single step as a graph so its name survives publishing", () => {
    const g = insertStep(emptyAgent(), "Reception");
    const payload = toPayload(g) as { steps: { name: string }[] };
    expect(payload.steps.map((n) => n.name)).toEqual(["Reception"]);
  });

  it("flags turn-taking and memory values outside the server's ranges", () => {
    expect(agentIssues(one({ vad_stop_ms: 100 }))).toContain("Wait after the caller stops must be 200 to 2000 ms.");
    expect(agentIssues(one({ context: { max_messages: 1 } }))).toContain("Max messages must be at least 2.");
    expect(agentIssues(one({ context: { max_tokens: -5 } }))).toContain("Max tokens cannot be negative.");
    expect(validate(one({ vad_stop_ms: 2500 }))).toContain("Wait after the caller stops must be 200 to 2000 ms.");
  });

  it("takes a removed tool off the start step's background list, and drops the key when it empties", () => {
    const t = (n: string) => ({ mcp_server_id: "crm", remote_name: n, schema_hash: "h", required: false });
    const g = one({ async_tools: ["crm/echo", "crm/lookup"] });
    const kept = applySelection(g, [t("lookup")]);
    expect(kept.steps.find((n) => n.id === kept.start)!.config.async_tools).toEqual(["crm/lookup"]);
    const none = applySelection(g, []);
    expect(none.steps.find((n) => n.id === none.start)!.config).not.toHaveProperty("async_tools");
    expect((toPayload(g) as { steps: { id: string; config: object }[] }).steps.find((n) => n.id === "b")!.config).not.toHaveProperty("async_tools");
  });

  it("flags an unknown failover class in a stored step", () => {
    const g = one({}, { failover: { on: ["timeout", "meteor"] } });
    expect(stepIssues(g, g.steps.at(-1)!.id)).toContainEqual({ tab: "advanced", message: "Failover lists an unknown error class." });
    const ok = one({}, { failover: { on: ["timeout"] } });
    expect(stepIssues(ok, ok.steps.at(-1)!.id).filter((i) => i.message.includes("Failover"))).toEqual([]);
  });

  it("flags summaries when a step's model provider cannot summarise", () => {
    const g = one({ context: { summarization: { enabled: true } } }, { llm: { provider: "groq", model: "llama" } });
    expect(agentIssues(g)).toContain("Billing uses groq, which cannot summarise older turns. Turn summaries off or change its model.");
  });

  it("mirrors the summarisation and late-transcript limits", () => {
    const sum = (summarization: Record<string, unknown>, ctx: Record<string, unknown> = {}) => one({ context: { ...ctx, summarization: { enabled: true, ...summarization } } }, { llm: { provider: "google", model: "m" } });
    expect(agentIssues(sum({ trigger_tokens: 4000 }))).toContain("Summarise-at must be below Max tokens (4000).");
    expect(agentIssues(sum({ trigger_tokens: 900 }, { max_tokens: 900 }))).toContain("Summarise-at must be below Max tokens (900).");
    expect(agentIssues(sum({ trigger_tokens: 3999 }))).toEqual([]);
    expect(agentIssues(sum({ keep_turns: -1 }))).toContain("Summarise-at and Keep the latest turns cannot be negative.");
    expect(agentIssues(sum({ prompt: "x".repeat(2049) }))).toContain("Summary instructions are over 2048 bytes.");
    expect(agentIssues(sum({ prompt: "é".repeat(1025) }))).toContain("Summary instructions are over 2048 bytes.");
    expect(agentIssues(sum({ prompt: "x".repeat(2048) }))).toEqual([]);
    expect(agentIssues(one({ context: { max_tokens: 100, summarization: { trigger_tokens: 5000 } } }))).toContain("Summarise-at must be below Max tokens (100).");
    for (const bad of [99, 10001]) expect(agentIssues(one({ stt_p99_ms: bad }))).toContain("Late-transcript wait must be between 100 and 10000 ms.");
    expect(agentIssues(one({ stt_p99_ms: 800 }))).toEqual([]);
    expect(agentIssues(one({ stt_p99_ms: 0 }))).toEqual([]);
  });

  it("mirrors the quiet-caller and audio limits", () => {
    const bad = (cfg: Record<string, unknown>) => agentIssues(one(cfg));
    for (const ms of [999, 300001, -1]) expect(bad({ idle: { user_timeout_ms: ms } })).toContain("Wait for a silent caller must be 1 to 300 seconds, or 0 to never hang up.");
    for (const ms of [0, 1000, 300000]) expect(bad({ idle: { user_timeout_ms: ms } })).toEqual([]);
    for (const n of [0, 11, 2.5]) expect(bad({ idle: { max_prompts: n } })).toContain("Ask this many times must be 1 to 10.");
    expect(bad({ idle: { max_prompts: 10 } })).toEqual([]);
    expect(bad({ idle: { prompt: "x".repeat(201) } })).toContain("What to say is over 200 bytes.");
    expect(bad({ idle: { prompt: "é".repeat(101) } })).toContain("What to say is over 200 bytes.");
    expect(bad({ idle: { prompt: "x".repeat(200), goodbye: "x".repeat(200) } })).toEqual([]);
    expect(bad({ idle: { goodbye: "x".repeat(201) } })).toContain("Goodbye is over 200 bytes.");
    for (const g of [-30.1, 6.1]) expect(bad({ audio: { output_gain_db: g } })).toContain("Voice volume must be -30 to +6 dB.");
    for (const g of [-30, 0, 6]) expect(bad({ audio: { output_gain_db: g } })).toEqual([]);
    for (const s of [-0.1, 0, 1.1]) expect(bad({ audio: { denoise: { strength: s } } })).toContain("Noise reduction strength must be 1 to 100%.");
    expect(bad({ audio: { denoise: { strength: 0.01 } } })).toEqual([]);
    expect(bad({ audio: { denoise: {}, background: { name: "office", gain_db: 0.1 } } })).toContain("Background volume must be -60 to 0 dB.");
    expect(bad({ audio: { background: { name: "office", gain_db: -60 } } })).toEqual([]);
    for (const ms of [-1, 10001]) expect(bad({ audio: { tool_cue: { name: "chime", after_ms: ms } } })).toContain("Hold sound starts after must be 0 to 10000 ms.");
    for (const ms of [0, 10000]) expect(bad({ audio: { tool_cue: { name: "chime", after_ms: ms } } })).toEqual([]);
  });

  it("keeps idle and audio on the start step only", () => {
    const g = one({ idle: { user_timeout_ms: 8000 }, audio: { output_gain_db: -3 } }, { idle: { user_timeout_ms: 5000 }, audio: { output_gain_db: 1 } });
    const steps = (toPayload(g) as { steps: { id: string; config: Record<string, unknown> }[] }).steps;
    expect(steps.find((n) => n.id === "a")!.config).toMatchObject({ idle: { user_timeout_ms: 8000 }, audio: { output_gain_db: -3 } });
    expect(steps.find((n) => n.id === "b")!.config).not.toHaveProperty("idle");
    expect(steps.find((n) => n.id === "b")!.config).not.toHaveProperty("audio");
  });

  it("keeps only audio.background on a step other than the start (K7): toPayload strips the rest, setStart moves the rest", () => {
    const bg = { name: "office" };
    const g = one({ audio: { output_gain_db: -3, background: { name: "cafe" } } }, { audio: { background: bg, denoise: {}, output_gain_db: 1, tool_cue: { name: "chime" } } });
    const steps = (toPayload(g) as { steps: { id: string; config: Record<string, unknown> }[] }).steps;
    expect(steps.find((n) => n.id === "a")!.config.audio).toEqual({ output_gain_db: -3, background: { name: "cafe" } });
    expect(steps.find((n) => n.id === "b")!.config.audio).toEqual({ background: bg });
    const onlyOthers = one({}, { audio: { output_gain_db: 1, tool_cue: { name: "chime" } } });
    expect((toPayload(onlyOthers) as { steps: { id: string; config: Record<string, unknown> }[] }).steps.find((n) => n.id === "b")!.config).not.toHaveProperty("audio");

    const moved = setStart(one({ audio: { output_gain_db: -3, tool_cue: { name: "chime" }, background: { name: "cafe" } } }, { audio: { background: bg } }), "b");
    expect(moved.steps.find((n) => n.id === "b")!.config.audio).toEqual({ background: bg, output_gain_db: -3, tool_cue: { name: "chime" } });
    expect(moved.steps.find((n) => n.id === "a")!.config.audio).toEqual({ background: { name: "cafe" } });
    expect(setStart(one({ audio: { output_gain_db: -3 } }), "b").steps.find((n) => n.id === "a")!.config).not.toHaveProperty("audio");
  });

  it("keeps turn on the start step only: toPayload drops it elsewhere and setStart moves it", () => {
    const g = one({ turn: { min_words: 3 } }, { turn: { min_words: 1 } });
    const steps = (toPayload(g) as { steps: { id: string; config: Record<string, unknown> }[] }).steps;
    expect(steps.find((n) => n.id === "a")!.config).toMatchObject({ turn: { min_words: 3 } });
    expect(steps.find((n) => n.id === "b")!.config).not.toHaveProperty("turn");
    const moved = setStart(one({ turn: { mute: ["always"] } }), "b");
    expect(moved.steps.find((n) => n.id === "b")!.config.turn).toEqual({ mute: ["always"] });
    expect(moved.steps.find((n) => n.id === "a")!.config).not.toHaveProperty("turn");
  });

  it("flags a longest mute above the server's cap when the cap is known", () => {
    const g = one({ turn: { max_mute_ms: 40000 } });
    expect(agentIssues(g)).toEqual([]);
    expect(agentIssues(g, { maxMuteMs: 30000 })).toContain("Longest mute is above this server's limit of 30 seconds.");
    expect(agentIssues(one({ turn: { max_mute_ms: 30000 } }), { maxMuteMs: 30000 })).toEqual([]);
    expect(validate(g, { maxMuteMs: 30000 }).join(" ")).toMatch(/server's limit/);
  });

  it("keeps the call limit on the start step only: toPayload drops it elsewhere and setStart moves it", () => {
    const g = one({ max_call_duration_seconds: 600 }, { max_call_duration_seconds: 300 });
    const steps = (toPayload(g) as { steps: { id: string; config: Record<string, unknown> }[] }).steps;
    expect(steps.find((n) => n.id === "a")!.config).toMatchObject({ max_call_duration_seconds: 600 });
    expect(steps.find((n) => n.id === "b")!.config).not.toHaveProperty("max_call_duration_seconds");
    const moved = setStart(one({ max_call_duration_seconds: 600 }), "b");
    expect(moved.steps.find((n) => n.id === "b")!.config.max_call_duration_seconds).toBe(600);
    expect(moved.steps.find((n) => n.id === "a")!.config).not.toHaveProperty("max_call_duration_seconds");
  });

  it("flags a call limit outside 1 to 1440 minutes, or above the server's when it is known", () => {
    expect(agentIssues(one({ max_call_duration_seconds: 59 }))).toContain("Longest call must be 1 to 1440 minutes.");
    expect(agentIssues(one({ max_call_duration_seconds: 86401 }))).toContain("Longest call must be 1 to 1440 minutes.");
    expect(agentIssues(one({ max_call_duration_seconds: 0 }))).toContain("Longest call must be 1 to 1440 minutes.");
    expect(agentIssues(one({ max_call_duration_seconds: -60 }))).toContain("Longest call must be 1 to 1440 minutes.");
    expect(agentIssues(one({ max_call_duration_seconds: 600 }))).toEqual([]);
    const g = one({ max_call_duration_seconds: 1900 });
    expect(agentIssues(g)).toEqual([]);
    expect(agentIssues(g, { maxCallSeconds: 1800 })).toContain("Longest call is above this server's limit of 30 minutes.");
    expect(agentIssues(one({ max_call_duration_seconds: 1800 }), { maxCallSeconds: 1800 })).toEqual([]);
  });

  it("mirrors the interruption limits", () => {
    const bad = (turn: Record<string, unknown>) => agentIssues(one({ turn }));
    for (const n of [-1, 11, 2.5]) expect(bad({ min_words: n })).toContain("Interrupt after must be 1 to 10 words.");
    for (const n of [0, 1, 10]) expect(bad({ min_words: n })).toEqual([]);
    for (const ms of [999, 60001]) expect(bad({ max_mute_ms: ms })).toContain("Longest mute must be 1 to 60 seconds.");
    for (const ms of [1000, 60000]) expect(bad({ max_mute_ms: ms })).toEqual([]);
  });

  it("mirrors the end-of-turn limits", () => {
    const bad = (turn: Record<string, unknown>) => agentIssues(one({ turn }));
    for (const ms of [199, 3001]) expect(bad({ speech_timeout_ms: ms })).toContain("Wait for the caller to resume must be 200 to 3000 ms.");
    for (const ms of [200, 3000]) expect(bad({ speech_timeout_ms: ms })).toEqual([]);
    for (const ms of [499, 5001]) expect(bad({ end_of_turn: "smart", smart_hold_ms: ms })).toContain("Smart hold must be 500 to 5000 ms.");
    for (const ms of [500, 5000]) expect(bad({ end_of_turn: "smart", smart_hold_ms: ms })).toEqual([]);
  });

  it("checks the summarisation details even while summarising is off", () => {
    const g = one({ context: { max_tokens: 800, summarization: { enabled: false, trigger_tokens: 900 } } });
    expect(agentIssues(g)).toContain("Summarise-at must be below Max tokens (800).");
  });

  it("accepts summaries on the echo provider, which runs the real Google processors", () => {
    const g = one({ context: { summarization: { enabled: true } } }, { llm: { provider: "echo", model: "gemini-2.5-flash" } });
    expect(agentIssues(g).filter((m) => m.includes("cannot summarise"))).toEqual([]);
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
      handoffs: [{ id: "r", from: "a", to: "b", condition: "x" }],
    };
    const g = fromAgentConfig(cfg);
    expect(g.steps[0].end_call).toBe(true);
    expect(g.steps[1].end_call).toBeFalsy();
    const out = toPayload(g) as { steps: { id: string; end_call?: boolean }[] };
    expect(out.steps[0].end_call).toBe(true);
    expect("end_call" in out.steps[1]).toBe(false);
  });

  it("keeps end_call when deleting a step leaves a one-step flow", () => {
    const cfg = {
      start: "a",
      steps: [
        { id: "a", name: "A", end_call: true, config: { llm: {}, tts: {} } },
        { id: "b", name: "B", config: { llm: {}, tts: {} } },
      ],
      handoffs: [{ id: "r", from: "a", to: "b", condition: "x" }],
    };
    const out = toPayload(removeStep(fromAgentConfig(cfg), "b")) as { steps: { end_call?: boolean }[] };
    expect(out.steps).toHaveLength(1);
    expect(out.steps[0].end_call).toBe(true);
  });

  it("saves a one-step main flow with end_call in steps form, and flat without it", () => {
    const flat = { llm: { system_prompt: "p" }, tts: {} };
    const g = fromAgentConfig(flat);
    expect(toPayload(g)).toEqual(flat);
    const out = toPayload(updateStep(g, "main", { end_call: true })) as { start: string; steps: { id: string; end_call?: boolean }[] };
    expect(out.start).toBe("main");
    expect(out.steps[0].end_call).toBe(true);
  });
});

describe("applySelection", () => {
  const sel = (name: string) => ({ mcp_server_id: "crm", remote_name: name, schema_hash: "h", required: false });
  const two = () =>
    fromAgentConfig({
      start: "a",
      steps: [
        { id: "a", name: "A", tools: ["crm/echo", "crm/lookup"], config: {} },
        { id: "b", name: "B", tools: ["crm/lookup"], config: {} },
      ],
      handoffs: [],
    });
  it("takes a dropped tool off every step and leaves the others", () => {
    const g = applySelection(two(), [sel("lookup")]);
    expect(g.steps.map((s) => s.tools)).toEqual([["crm/lookup"], ["crm/lookup"]]);
  });
  it("does not add a newly selected tool to a step that has several to choose from", () => {
    const g = applySelection(two(), [sel("echo"), sel("lookup"), sel("other")]);
    expect(g.steps.map((s) => s.tools)).toEqual([["crm/echo", "crm/lookup"], ["crm/lookup"]]);
  });
  it("leaves a lone named step's own choice alone: only a flat flow can call everything", () => {
    const g = fromAgentConfig({ start: "a", steps: [{ id: "a", name: "Greeter", tools: ["crm/lookup"], config: {} }], handoffs: [] });
    expect(applySelection(g, [sel("echo"), sel("lookup")]).steps[0].tools).toEqual(["crm/lookup"]);
    expect(applySelection(g, [sel("echo")]).steps[0].tools).toEqual([]);
  });
  it("gives a lone step every selected tool, as a one-step flow can call them all", () => {
    const g = applySelection(fromAgentConfig(flat), [sel("echo"), sel("lookup")]);
    expect(g.steps).toHaveLength(1);
    expect(g.steps[0].tools).toEqual(["crm/echo", "crm/lookup"]);
    expect(toPayload(g)).toEqual(flat); // still the flat config: the selection is not part of it
  });
});

describe("greeting", () => {
  const stepsWith = (a: unknown, b: unknown) =>
    fromAgentConfig({
      start: "a",
      steps: [
        { id: "a", name: "Reception", config: { stt: { provider: "google" }, llm: { provider: "google", model: "g" }, tts: { provider: "google", voice: "v" }, greeting: a } },
        { id: "b", name: "Billing", config: { llm: { provider: "google", model: "g" }, tts: { provider: "google", voice: "v" }, greeting: b } },
      ],
      handoffs: [],
    });
  it("stays on the start step and on other steps in the payload (it is not a call-level setting)", () => {
    const p = toPayload(stepsWith("Hello.", "Billing here.")) as { steps: { config: { greeting?: string } }[] };
    expect(p.steps.map((s) => s.config.greeting)).toEqual(["Hello.", "Billing here."]);
  });
  it("leaves a blank greeting out of the payload", () => {
    const p = toPayload(stepsWith("   ", "")) as { steps: { config: Record<string, unknown> }[] };
    expect(p.steps.map((s) => "greeting" in s.config)).toEqual([false, false]);
    expect((toPayload(fromAgentConfig({ ...flat, greeting: "  " })) as Record<string, unknown>)).not.toHaveProperty("greeting");
  });
  it("stays at the top of a flat flow", () => {
    expect((toPayload(fromAgentConfig({ ...flat, greeting: "Hi!" })) as { greeting?: string }).greeting).toBe("Hi!");
  });
  it("is an issue past the server's byte limit, counted in bytes, and not for a blank one", () => {
    const over = (t: string) => stepIssues(stepsWith(t, ""), "a").some((i) => i.message === "Greeting is over 500 bytes.");
    expect(over("a".repeat(MAX_GREETING_BYTES))).toBe(false);
    expect(over("a".repeat(MAX_GREETING_BYTES + 1))).toBe(true);
    expect(over("é".repeat(251))).toBe(true); // 251 characters, 502 bytes
    expect(over("   ")).toBe(false);
  });
});

describe("call-wide tts.clean_text and failover", () => {
  const cfg = (extra: Record<string, unknown>, tts: Record<string, unknown> = {}) => ({ llm: { provider: "g", model: "m" }, tts: { provider: "g", ...tts }, ...extra });
  const g = () =>
    fromAgentConfig({
      start: "a",
      steps: [
        { id: "a", name: "A", config: cfg({ failover: { on: ["client_error"] } }, { clean_text: false }) },
        { id: "b", name: "B", config: cfg({ failover: { on: ["timeout"] } }, { clean_text: false }) },
      ],
      handoffs: [{ id: "e", from: "a", to: "b", condition: "x" }],
    });
  type Out = { steps: { id: string; config: { tts: Record<string, unknown>; failover?: unknown } }[] };

  it("toPayload keeps them on the start step and drops them from the others", () => {
    const [a, b] = (toPayload(g()) as Out).steps;
    expect(a.config.tts.clean_text).toBe(false);
    expect(a.config.failover).toEqual({ on: ["client_error"] });
    expect(b.config.tts).not.toHaveProperty("clean_text");
    expect(b.config).not.toHaveProperty("failover");
    expect(b.config.tts.provider).toBe("g");
  });

  it("setStart moves them to the new start step", () => {
    const [a, b] = (toPayload(setStart(g(), "b")) as Out).steps;
    expect(a.config.tts).not.toHaveProperty("clean_text");
    expect(a.config).not.toHaveProperty("failover");
    expect(b.config.tts.clean_text).toBe(false);
    expect(b.config.failover).toEqual({ on: ["client_error"] });
  });
});
