import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import type { ProviderStatus, ServerConfigDTO } from "../../../api/client";
import { StepAdvanced } from "./StepAdvanced";
import { fromFlowConfig, type FlowDraft } from "./model";

const svc = { available: true, custom: true, models: [{ id: "gemini", label: "Gemini", default: true }], voices: [{ id: "aoede", label: "Aoede", default: true }] };
const providers: ProviderStatus[] = [
  { id: "google", display_name: "Google", capabilities: ["llm", "tts"], detected: true, env: [], services: { llm: svc, tts: svc } },
];
const g0 = fromFlowConfig({ llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "aoede" }, stt: { provider: "google" } });

function Harness({ onChange }: { onChange: (g: FlowDraft) => void }) {
  const [g, setG] = useState(g0);
  return (
    <StepAdvanced
      graph={g}
      step={g.steps[0]}
      providers={providers}
      onChange={(n) => {
        setG(n);
        onChange(n);
      }}
    />
  );
}
const last = (fn: ReturnType<typeof vi.fn>) => (fn.mock.calls.at(-1)![0] as FlowDraft).steps[0].config;

describe("StepAdvanced", () => {
  it("adds and removes a model fallback", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    const model = screen.getByRole("group", { name: "Model fallbacks" });
    await user.selectOptions(model.querySelector("select")!, "google");
    await user.click(screen.getByRole("button", { name: "Add model fallback" }));
    expect(last(onChange).llm).toMatchObject({ fallbacks: [{ provider: "google", model: "gemini" }] });
    await user.click(screen.getByRole("button", { name: "Remove model fallback 1" }));
    expect(last(onChange).llm).not.toHaveProperty("fallbacks");
  });

  it("never offers silence as a fallback: it takes none and cannot be one", () => {
    const silence: ProviderStatus = { id: "silence", display_name: "Silence (text only)", capabilities: ["tts"], detected: true, env: [], services: { tts: svc } };
    render(<StepAdvanced graph={g0} step={g0.steps[0]} providers={[...providers, silence]} onChange={vi.fn()} />);
    const voice = screen.getByRole("group", { name: "Voice fallbacks" });
    const offered = [...voice.querySelectorAll("option")].map((o) => (o as HTMLOptionElement).value);
    expect(offered).toContain("google");
    expect(offered).not.toContain("silence");
  });

  it("stops at 2 fallbacks, as the server allows", async () => {
    const user = userEvent.setup();
    render(<Harness onChange={vi.fn()} />);
    const model = screen.getByRole("group", { name: "Model fallbacks" });
    for (let i = 0; i < 2; i++) {
      await user.selectOptions(model.querySelector("select")!, "google");
      await user.click(screen.getByRole("button", { name: "Add model fallback" }));
    }
    // A third provider choice must not be addable.
    await user.selectOptions(model.querySelector("select")!, "google");
    expect(screen.getByRole("button", { name: "Add model fallback" })).toBeDisabled();
  });

  it("sets tool calls at once within 1 to 16, omitting the default", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    const input = screen.getByLabelText("Tool calls at once");
    await user.clear(input);
    await user.type(input, "40");
    expect(last(onChange).llm).toMatchObject({ max_parallel: 16 });
    // Emptying the field falls back to 1, the server default, which is stored by omission.
    await user.clear(input);
    expect(last(onChange).llm).not.toHaveProperty("max_parallel");
    expect(input).toHaveValue(1);
  });

  it("sets sampling for any LLM provider: numbers, clamped to range, removed when emptied", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    for (const name of ["Temperature", "Top P", "Max output tokens"]) expect(screen.getByLabelText(name)).toHaveValue(null);
    await user.type(screen.getByLabelText("Temperature"), "0");
    expect(last(onChange).llm).toMatchObject({ temperature: 0 }); // zero is a value, not "unset"
    await user.clear(screen.getByLabelText("Temperature"));
    await user.type(screen.getByLabelText("Temperature"), "5");
    await user.type(screen.getByLabelText("Top P"), "0.9");
    await user.type(screen.getByLabelText("Max output tokens"), "99999");
    await user.tab(); // blur shows the stored (clamped) value
    expect(screen.getByLabelText("Temperature")).toHaveValue(2);
    expect(screen.getByLabelText("Max output tokens")).toHaveValue(8192);
    expect(last(onChange).llm).toEqual({ provider: "google", model: "gemini", temperature: 2, top_p: 0.9, max_output_tokens: 8192 });
    await user.clear(screen.getByLabelText("Max output tokens"));
    await user.type(screen.getByLabelText("Max output tokens"), "12.6");
    expect(last(onChange).llm).toMatchObject({ max_output_tokens: 13 }); // a whole number
    await user.clear(screen.getByLabelText("Temperature"));
    await user.clear(screen.getByLabelText("Top P"));
    await user.clear(screen.getByLabelText("Max output tokens"));
    expect(last(onChange).llm).toEqual({ provider: "google", model: "gemini" });
  });

  it("shows the four voice settings only for ElevenLabs and stores them by omission", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { unmount } = render(<Harness onChange={onChange} />);
    expect(screen.queryByLabelText("Stability")).not.toBeInTheDocument();
    unmount();
    const eleven = fromFlowConfig({
      llm: { provider: "google", model: "gemini" },
      tts: { provider: "elevenlabs", voice: "v", voice_settings: { speed: 1.1 } },
      stt: { provider: "google" },
    });
    function Eleven() {
      const [g, setG] = useState(eleven);
      return (
        <StepAdvanced
          graph={g}
          step={g.steps[0]}
          providers={providers}
          onChange={(n) => {
            setG(n);
            onChange(n);
          }}
        />
      );
    }
    render(<Eleven />);
    expect(screen.getByLabelText("Speed")).toHaveValue(1.1);
    await user.type(screen.getByLabelText("Stability"), "0.4");
    await user.type(screen.getByLabelText("Similarity boost"), "5");
    await user.type(screen.getByLabelText("Style"), "0.2");
    await user.tab(); // blur shows the stored (clamped) value, not the typed one
    expect(screen.getByLabelText("Similarity boost")).toHaveValue(1);
    expect(last(onChange).tts).toMatchObject({ voice_settings: { stability: 0.4, similarity_boost: 1, style: 0.2, speed: 1.1 } });
    await user.clear(screen.getByLabelText("Stability"));
    await user.clear(screen.getByLabelText("Similarity boost"));
    await user.clear(screen.getByLabelText("Style"));
    await user.clear(screen.getByLabelText("Speed"));
    expect(last(onChange).tts).not.toHaveProperty("voice_settings");
  });

  it("explains the obscure settings and leaves the field names alone", async () => {
    const user = userEvent.setup();
    render(<Harness onChange={vi.fn()} />);
    expect(screen.getByLabelText("Tool calls at once")).toBeInTheDocument();
    for (const name of ["Fallbacks", "Tool calls at once"]) {
      await user.click(screen.getByRole("button", { name: `Explain ${name}` }));
    }
    expect(screen.getByRole("region", { name: "Explain Tool calls at once" })).toHaveTextContent("1 to 16");
  });

  describe("background sound on a step other than the start", () => {
    const server = {
      audio: { backgrounds: ["cafe", "office"], cues: [], allow_agent_background: true, background_default: "cafe" },
    } as unknown as ServerConfigDTO;
    const two = fromFlowConfig({
      start: "a",
      steps: [
        { id: "a", name: "A", config: { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "aoede" }, stt: { provider: "google" } } },
        { id: "b", name: "B", config: { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "aoede" } } },
      ],
      handoffs: [{ id: "e1", from: "a", to: "b", condition: "x" }],
    });
    function Two({ onChange, serverConfig = server, initial = two }: { onChange: (g: FlowDraft) => void; serverConfig?: ServerConfigDTO; initial?: FlowDraft }) {
      const [g, setG] = useState(initial);
      return <StepAdvanced graph={g} step={g.steps[1]} providers={providers} serverConfig={serverConfig} onChange={(n) => (setG(n), onChange(n))} />;
    }
    const audioOf = (fn: ReturnType<typeof vi.fn>) => (fn.mock.calls.at(-1)![0] as FlowDraft).steps[1].config.audio;

    it("keeps the current sound by default, and writes none or a named sound", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Two onChange={onChange} />);
      const select = screen.getByLabelText("Background sound");
      expect(select).toHaveValue("");
      expect(screen.getByRole("option", { name: "Keep the current sound" })).toBeInTheDocument();
      await user.selectOptions(select, "office");
      expect(audioOf(onChange)).toEqual({ background: { name: "office" } });
      await user.selectOptions(select, "__none__");
      expect(audioOf(onChange)).toEqual({ background: { name: "" } });
      await user.selectOptions(select, "");
      expect(audioOf(onChange)).toBeUndefined();
    });

    it("tells a catalogue sound named none or default from the choices of the same name", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      const odd = { audio: { ...server.audio, backgrounds: ["default", "none"] } } as unknown as ServerConfigDTO;
      render(<Two onChange={onChange} serverConfig={odd} />);
      const select = screen.getByLabelText("Background sound");
      await user.selectOptions(select, screen.getByRole("option", { name: "none" }));
      expect(audioOf(onChange)).toEqual({ background: { name: "none" } });
      await user.selectOptions(select, screen.getByRole("option", { name: "None" }));
      expect(audioOf(onChange)).toEqual({ background: { name: "" } });
      await user.selectOptions(select, screen.getByRole("option", { name: "default" }));
      expect(audioOf(onChange)).toEqual({ background: { name: "default" } });
    });

    it("is not offered on the start step, and is disabled with the reason when flows may not choose", () => {
      const { unmount } = render(<StepAdvanced graph={two} step={two.steps[0]} providers={providers} serverConfig={server} onChange={vi.fn()} />);
      expect(screen.queryByLabelText("Background sound")).toBeNull();
      unmount();
      render(<Two onChange={vi.fn()} serverConfig={{ ...server, audio: { ...server.audio, allow_agent_background: false } } as ServerConfigDTO} />);
      expect(screen.getByLabelText("Background sound")).toBeDisabled();
      expect(screen.getByText("This server does not allow flow background sounds.")).toBeVisible();
    });

    it("shows a stored sound the server no longer has", () => {
      const stale = { ...two, steps: [two.steps[0], { ...two.steps[1], config: { ...two.steps[1].config, audio: { background: { name: "gone" } } } }] };
      render(<Two onChange={vi.fn()} initial={stale} />);
      expect(screen.getByLabelText("Background sound")).toHaveValue("gone");
      expect(screen.getByRole("option", { name: "gone (not on this server)" })).toBeInTheDocument();
    });
  });
});
