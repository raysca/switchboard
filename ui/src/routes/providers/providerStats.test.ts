import { describe, expect, it } from "vitest";
import type { AgentDTO, ProviderStatus } from "../../api/client";
import { catalogLines, credential, flowsUsing, hasService, missingEnv, serviceList, serviceState } from "./providerStats";

const google: ProviderStatus = {
  id: "google",
  display_name: "Google",
  capabilities: ["stt", "llm", "tts"],
  detected: true,
  env: ["GOOGLE_API_KEY"],
  services: {
    stt: { available: true, custom: true, models: [{ id: "chirp_2", label: "Chirp 2" }, { id: "long", label: "Latest long" }], languages: [{ id: "en-US", label: "English (US)" }] },
    llm: { available: true, custom: true, models: [{ id: "gemini", label: "Gemini 2.5 Flash", default: true }] },
    tts: { available: false, reason: "not_in_build", custom: true, voices: [{ id: "aoede", label: "Chirp 3 HD Aoede" }] },
  },
};
const xai: ProviderStatus = {
  id: "xai",
  display_name: "xAI",
  capabilities: [],
  detected: false,
  env: ["XAI_API_KEY"],
  services: {
    llm: { available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], custom: true },
    stt: { available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], custom: true },
  },
};

const flow = (id: string, config: Record<string, unknown>, archived = false): AgentDTO => ({
  id,
  name: id,
  description: "",
  enabled: true,
  archived,
  current_version: { id: `${id}-v`, number: 1, schema_version: 1, config, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
});

describe("serviceList and hasService", () => {
  it("lists only the services the catalog has, in flow order", () => {
    expect(serviceList(google)).toBe("STT · LLM · TTS");
    expect(serviceList(xai)).toBe("STT · LLM");
    expect(hasService(xai, "tts")).toBe(false);
    expect(hasService(xai, "llm")).toBe(true);
  });
});

describe("credential", () => {
  it("says where the credential comes from: the server's environment, or nowhere", () => {
    expect(credential(google)).toEqual({ text: "Environment variable", ok: true });
    expect(credential(xai)).toEqual({ text: "Not configured", ok: false });
  });
});

describe("missingEnv", () => {
  it("names each missing variable once, even when two services need it", () => {
    expect(missingEnv(xai)).toEqual(["XAI_API_KEY"]);
  });
  it("falls back to the provider's own variables when nothing is named, and is empty once detected", () => {
    expect(missingEnv({ ...xai, services: {} })).toEqual(["XAI_API_KEY"]);
    expect(missingEnv(google)).toEqual([]);
  });
});

describe("serviceState", () => {
  it("says available, what is needed, or that the build lacks it", () => {
    expect(serviceState(google, "llm")).toEqual({ state: "ok", text: "available" });
    expect(serviceState(google, "tts")).toEqual({ state: "absent", text: "not in this build" });
    expect(serviceState(xai, "llm")).toEqual({ state: "missing", text: "needs XAI_API_KEY" });
    expect(serviceState(xai, "tts")).toBeNull();
  });
});

describe("catalogLines", () => {
  it("lists models (voices for TTS), then languages", () => {
    expect(catalogLines(google)).toEqual([
      { service: "stt", text: "Chirp 2, Latest long · English (US)" },
      { service: "llm", text: "Gemini 2.5 Flash" },
      { service: "tts", text: "Chirp 3 HD Aoede" },
    ]);
  });
  it("says so when the catalog has nothing for a service", () => {
    expect(catalogLines(xai)).toEqual([
      { service: "stt", text: "No catalog entries" },
      { service: "llm", text: "No catalog entries" },
    ]);
  });
});

describe("flowsUsing", () => {
  const flat = flow("flat", { stt: { provider: "google" }, llm: { provider: "google" }, tts: { provider: "google" } });
  const graph = flow("graph", {
    entry: "a",
    nodes: [
      { id: "a", config: { stt: { provider: "google" }, llm: { provider: "google" }, tts: { provider: "google" } } },
      { id: "b", config: { llm: { provider: "xai" }, tts: { provider: "google" } } },
    ],
    edges: [],
  });
  it("counts a flow once, whichever stage or step uses the provider", () => {
    expect(flowsUsing([flat, graph], "google").map((f) => f.id)).toEqual(["flat", "graph"]);
    expect(flowsUsing([flat, graph], "xai").map((f) => f.id)).toEqual(["graph"]);
  });
  it("never counts an archived flow, a flow with no version, or a malformed config", () => {
    const gone = flow("gone", flat.current_version!.config, true);
    const bare = { ...flow("bare", {}), current_version: undefined } as AgentDTO;
    const odd = flow("odd", { nodes: "nope", llm: "google" });
    expect(flowsUsing([gone, bare, odd], "google")).toEqual([]);
  });
});
