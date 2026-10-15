import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ReplyWaterfall } from "./ReplyWaterfall";
import { parseStages } from "./callStats";

const spoken = { stt_final_ms: 410, llm_start_ms: 520, llm_ttft_ms: 380, tts_start_ms: 905, tts_ttfb_ms: 260, voice_to_voice_ms: 1165 };

describe("parseStages", () => {
  it("reads a spoken turn as Heard, Model and Voice with their windows", () => {
    expect(parseStages(spoken)).toEqual({
      stages: [
        { label: "Heard", start: 0, end: 410 },
        { label: "Model", start: 520, end: 900 },
        { label: "Voice", start: 905, end: 1165 },
      ],
      total: 1165,
    });
  });

  it("reads a typed turn, which has no Heard", () => {
    expect(parseStages({ llm_start_ms: 0, llm_ttft_ms: 300, tts_start_ms: 500, tts_ttfb_ms: 200 })).toEqual({
      stages: [
        { label: "Model", start: 0, end: 300 },
        { label: "Voice", start: 500, end: 700 },
      ],
      total: 700,
    });
  });

  it("gives nothing for an older row that has no offsets, or for garbage, so the pills stay", () => {
    expect(parseStages({ stt_final_ms: 410, llm_ttft_ms: 380, tts_ttfb_ms: 260, voice_to_voice_ms: 1165 })).toBeNull();
    expect(parseStages(null)).toBeNull();
    expect(parseStages("x")).toBeNull();
    expect(parseStages({})).toBeNull();
    expect(parseStages({ llm_start_ms: "a", llm_ttft_ms: 3, tts_start_ms: 5, tts_ttfb_ms: 3 })).toBeNull();
  });

  it("never returns a negative or reversed window", () => {
    expect(parseStages({ llm_start_ms: -5, llm_ttft_ms: 300, tts_start_ms: 500, tts_ttfb_ms: 200 })).toBeNull();
    expect(parseStages({ stt_final_ms: -1, llm_start_ms: 1, llm_ttft_ms: 2, tts_start_ms: 5, tts_ttfb_ms: 2 })?.stages.map((s) => s.label)).toEqual(["Model", "Voice"]);
  });

  it("draws nothing when every stage took no time", () => {
    expect(parseStages({ llm_start_ms: 0, llm_ttft_ms: 0, tts_start_ms: 0, tts_ttfb_ms: 0 })).toBeNull();
  });

  it("lets the total be the voice-to-voice time when it is later than the last stage", () => {
    expect(parseStages({ ...spoken, voice_to_voice_ms: 1300 })?.total).toBe(1300);
  });
});

describe("ReplyWaterfall", () => {
  it("draws one bar per stage on a shared axis and says the same in words", () => {
    render(<ReplyWaterfall latencies={spoken} />);
    const chart = screen.getByRole("img", { name: "Heard 0 to 410 ms, Model 520 to 900 ms, Voice 905 to 1165 ms" });
    const bars = chart.querySelectorAll<HTMLElement>("[data-stage]");
    expect([...bars].map((b) => b.dataset.stage)).toEqual(["Heard", "Model", "Voice"]);
    const pct = (v: string) => parseFloat(v);
    expect(pct(bars[0].style.marginLeft)).toBeCloseTo(0, 1);
    expect(pct(bars[0].style.width)).toBeCloseTo((410 / 1165) * 100, 1);
    expect(pct(bars[1].style.marginLeft)).toBeCloseTo((520 / 1165) * 100, 1);
    expect(pct(bars[2].style.marginLeft) + pct(bars[2].style.width)).toBeCloseTo(100, 1);
    expect(chart).toHaveTextContent("Heard 410 ms");
    expect(chart).toHaveTextContent("Model 380 ms");
    expect(chart).toHaveTextContent("Voice 260 ms");
  });

  it("renders nothing when there is nothing to draw", () => {
    const { container } = render(<ReplyWaterfall latencies={{ voice_to_voice_ms: 900 }} />);
    expect(container).toBeEmptyDOMElement();
  });
});
