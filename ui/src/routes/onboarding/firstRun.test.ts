import { describe, expect, it } from "vitest";
import type { ProviderStatus } from "../../api/client";
import { DEFAULT_JOB, agentFor, stacksFrom } from "./firstRun";

const svc = (extra: object = {}) => ({ available: true, custom: true, ...extra });
const google: ProviderStatus = {
  id: "google",
  display_name: "Google",
  capabilities: ["stt", "llm", "tts"],
  detected: true,
  env: ["GOOGLE_API_KEY"],
  services: {
    stt: svc({ models: [{ id: "latest_long", label: "Latest long" }, { id: "chirp_2", label: "Chirp 2", default: true }], languages: [{ id: "en-GB", label: "English (UK)", default: true }] }),
    llm: svc({ models: [{ id: "gemini-2.5-flash", label: "Gemini 2.5 Flash", default: true }] }),
    tts: svc({ voices: [{ id: "aoede", label: "Aoede", default: true }, { id: "puck", label: "Puck" }] }),
  },
};
const xai: ProviderStatus = {
  id: "xai",
  display_name: "xAI",
  capabilities: [],
  detected: false,
  env: ["XAI_API_KEY"],
  services: {
    stt: svc({ available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], models: [{ id: "scribe", label: "Scribe" }] }),
    llm: svc({ available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], models: [{ id: "grok", label: "Grok" }] }),
    tts: svc({ available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], voices: [{ id: "ara", label: "Ara" }] }),
  },
};
const deepgram: ProviderStatus = { id: "deepgram", display_name: "Deepgram", capabilities: ["stt"], detected: true, env: ["DEEPGRAM_API_KEY"], services: { stt: svc({ models: [{ id: "nova-3", label: "Nova 3" }] }) } };

describe("stacksFrom", () => {
  it("offers one stack per provider that covers all three stages, ready ones first", () => {
    expect(stacksFrom([xai, deepgram, google]).map((s) => [s.provider.id, s.ready])).toEqual([["google", true], ["xai", false]]);
  });
  it("takes the catalog's defaults, and the first entry when none is marked", () => {
    const [g] = stacksFrom([google]);
    expect([g.stt.model, g.stt.language, g.llm.model, g.tts.voice]).toEqual(["chirp_2", "en-GB", "gemini-2.5-flash", "aoede"]);
    const [x] = stacksFrom([xai]);
    expect([x.stt.model, x.llm.model, x.tts.voice]).toEqual(["scribe", "grok", "ara"]);
  });
  it("names what is still to set for a stack that is not ready", () => {
    expect(stacksFrom([xai])[0].missing).toEqual(["XAI_API_KEY"]);
    expect(stacksFrom([google])[0].missing).toEqual([]);
  });
  it("skips a provider whose service has no models or voices, and copes with no providers", () => {
    const bare = { ...google, id: "bare", services: { ...google.services, llm: svc({ models: [] }) } } as ProviderStatus;
    expect(stacksFrom([bare])).toEqual([]);
    expect(stacksFrom([])).toEqual([]);
  });
  it("lists the voices the user can pick from", () => {
    expect(stacksFrom([google])[0].tts.voices.map((v) => v.id)).toEqual(["aoede", "puck"]);
  });
});

describe("agentFor", () => {
  const [g] = stacksFrom([google]);
  it("builds a flat flow from the stack's catalog defaults, the name and the job", () => {
    expect(agentFor(g, { name: "  Front desk ", job: " Answer calls. ", voice: "puck" })).toEqual({
      name: "Front desk",
      description: "",
      enabled: true,
      config: {
        stt: { provider: "google", model: "chirp_2", language: "en-GB" },
        llm: { provider: "google", model: "gemini-2.5-flash", system_prompt: "Answer calls." },
        tts: { provider: "google", voice: "puck" },
      },
    });
  });
  it("falls back to the default voice when the chosen one is not in the catalog", () => {
    expect((agentFor(g, { name: "x", job: "y", voice: "nope" }).config as { tts: { voice: string } }).tts.voice).toBe("aoede");
  });
  it("leaves the language out when the catalog names none", () => {
    const [x] = stacksFrom([xai]);
    expect((agentFor(x, { name: "x", job: "y", voice: "ara" }).config as { stt: object }).stt).toEqual({ provider: "xai", model: "scribe" });
  });
  it("adds the greeting, trimmed, only when there is one", () => {
    const cfg = (greeting?: string) => agentFor(g, { name: "x", job: "y", voice: "aoede", greeting }).config as { greeting?: string };
    expect(cfg("  Hello, thanks for calling.  ").greeting).toBe("Hello, thanks for calling.");
    expect(cfg("   ")).not.toHaveProperty("greeting");
    expect(cfg()).not.toHaveProperty("greeting");
  });
  it("starts the job with plain, usable text", () => {
    expect(DEFAULT_JOB.length).toBeGreaterThan(40);
  });
});

describe("the simulated provider is never the default", () => {
  const echo: ProviderStatus = { ...google, id: "echo", display_name: "Echo (simulated)", env: [], detected: true };
  it("lists a real ready stack before echo, though echo sorts first by name", () => {
    expect(stacksFrom([echo, google]).map((s) => s.provider.id)).toEqual(["google", "echo"]);
  });
  it("still offers echo alone when it is the only stack ready", () => {
    expect(stacksFrom([echo, xai]).map((s) => [s.provider.id, s.ready])).toEqual([["echo", true], ["xai", false]]);
  });
});
