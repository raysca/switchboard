import { describe, expect, it } from "vitest";
import { isTextOnly, TEXT_ONLY_PROVIDER } from "./textOnly";

const step = (id: string, config: Record<string, unknown>) => ({ id, name: id, config });

describe("isTextOnly", () => {
  it("reads a flat flow's speech-to-text", () => {
    expect(isTextOnly({ stt: { provider: TEXT_ONLY_PROVIDER, model: "text" } })).toBe(true);
    expect(isTextOnly({ stt: { provider: "deepgram" }, tts: { provider: "silence" } })).toBe(false); // silent voice, real listening: the mic is needed
    expect(isTextOnly({ stt: { provider: "echo" } })).toBe(false);
  });

  it("reads a routed flow's speech-to-text from its START step, not the first or the last", () => {
    const silence = { stt: { provider: "silence", model: "text" } };
    const voice = { stt: { provider: "deepgram", model: "nova-3" } };
    const handedOff = (start: string) => ({ start, steps: [step("a", voice), step("b", silence), step("c", {})], handoffs: [] });
    expect(isTextOnly(handedOff("b"))).toBe(true);
    expect(isTextOnly(handedOff("a"))).toBe(false);
    expect(isTextOnly(handedOff("c"))).toBe(false); // the start step names no speech-to-text
  });

  it("ignores speech settings on steps that are not the start (only the start step has call-level settings)", () => {
    const cfg = { start: "front", steps: [step("front", { stt: { provider: "deepgram" } }), step("sales", { stt: { provider: "silence" } })], handoffs: [] };
    expect(isTextOnly(cfg)).toBe(false);
  });

  it("is false for anything that is not a flow config", () => {
    for (const v of [undefined, null, 0, "silence", [], {}, { stt: null }, { stt: {} }, { stt: { provider: 1 } }, { start: "x", steps: [] }, { steps: [null] }]) {
      expect(isTextOnly(v)).toBe(false);
    }
  });
});
