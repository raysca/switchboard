import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import type { ProviderStatus, ServerConfigDTO } from "../../../api/client";
import { FlowSettingsPanel } from "./FlowSettingsPanel";
import { emptyFlow, fromFlowConfig, type FlowDraft } from "./model";

const svc = { available: true, custom: true, models: [{ id: "nova-3", label: "Nova 3", default: true }], languages: [{ id: "en", label: "English", default: true }] };
const providers: ProviderStatus[] = [
  { id: "deepgram", display_name: "Deepgram", capabilities: ["stt"], detected: true, env: [], services: { stt: svc } },
];
const two = fromFlowConfig({
  start: "a",
  steps: [
    { id: "a", name: "Reception", config: { llm: { provider: "g", model: "m" }, tts: { provider: "g" } } },
    { id: "b", name: "Billing", config: { llm: { provider: "g", model: "m" }, tts: { provider: "g" } } },
  ],
  routes: [{ id: "e1", from: "a", to: "b", condition: "invoice" }],
});

function Harness({ initial = two, onChange = vi.fn(), onClose = vi.fn(), serverConfig }: { initial?: FlowDraft; onChange?: (g: FlowDraft) => void; onClose?: () => void; serverConfig?: ServerConfigDTO }) {
  const [g, setG] = useState(initial);
  const [name, setName] = useState("Front desk");
  return (
    <div style={{ position: "relative" }}>
      <FlowSettingsPanel
        graph={g}
        providers={providers}
        serverConfig={serverConfig}
        name={name}
        description=""
        onName={setName}
        onDescription={vi.fn()}
        onClose={onClose}
        onChange={(n) => {
          setG(n);
          onChange(n);
        }}
      />
    </div>
  );
}
const startCfg = (fn: ReturnType<typeof vi.fn>) => {
  const g = fn.mock.calls.at(-1)![0] as FlowDraft;
  return g.steps.find((n) => n.id === g.start)!.config;
};

describe("FlowSettingsPanel", () => {
  it("says speech-to-text is missing, and sets it on the start step", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    expect(screen.getByRole("note")).toHaveTextContent("Choose speech-to-text in Flow settings.");
    await user.selectOptions(screen.getByLabelText("STT provider"), "deepgram");
    expect(startCfg(onChange).stt).toEqual({ provider: "deepgram", model: "nova-3", language: "en" });
    expect(screen.queryByRole("note")).toBeNull();
  });

  it("writes turn-taking and memory, and clears them when emptied", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    await user.type(screen.getByLabelText("Wait after the caller stops (ms)"), "480");
    expect(startCfg(onChange).vad_stop_ms).toBe(480);
    await user.clear(screen.getByLabelText("Wait after the caller stops (ms)"));
    expect(startCfg(onChange)).not.toHaveProperty("vad_stop_ms");
    await user.type(screen.getByLabelText("Max messages"), "40");
    await user.click(screen.getByLabelText("Summarise older turns"));
    expect(startCfg(onChange).context).toEqual({ max_messages: 40, summarization: { enabled: true } });
  });

  it("requires the recording disclosure once recording is on", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    expect(screen.getByLabelText("Callers are told the call is recorded")).toBeDisabled();
    await user.click(screen.getByLabelText("Record calls"));
    expect(screen.getByRole("note")).toHaveTextContent("Confirm that callers are told the call is recorded.");
    await user.click(screen.getByLabelText("Callers are told the call is recorded"));
    expect(startCfg(onChange).recording).toEqual({ enabled: true, disclosure_confirmed: true });
  });

  it("sets text cleaning and the fallback rules on the start step, and opens Advanced when they are set", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    await user.click(screen.getByText("Advanced"));
    const group = screen.getByRole("group", { name: "When to move to a fallback" });
    expect(group).toHaveTextContent("Only matters when a stage has fallbacks.");
    expect(within(group).getByRole("checkbox", { name: "Timeouts" })).toBeChecked();
    expect(within(group).getByRole("checkbox", { name: "Rejected requests (bad request, bad key)" })).not.toBeChecked();
    await user.click(within(group).getByRole("checkbox", { name: "Rejected requests (bad request, bad key)" }));
    expect(startCfg(onChange).failover).toEqual({ on: ["timeout", "server_error", "circuit_open", "client_error"] });
    await user.click(within(group).getByRole("checkbox", { name: "Rejected requests (bad request, bad key)" }));
    expect(startCfg(onChange)).not.toHaveProperty("failover");
    const clean = screen.getByLabelText(/Clean text before speaking/);
    expect(clean).toBeChecked();
    await user.click(clean);
    expect(startCfg(onChange).tts).toMatchObject({ clean_text: false });
    await user.click(clean);
    expect(startCfg(onChange).tts).not.toHaveProperty("clean_text");
    // a stored flow that set them opens Advanced on load
    const stored = fromFlowConfig({ llm: { provider: "g", model: "m" }, tts: { provider: "g", clean_text: false }, failover: { on: ["client_error"] } });
    render(<Harness initial={stored} />);
    expect(screen.getAllByText("Advanced · 2 set")).toHaveLength(1);
  });

  describe("route sound", () => {
    const cueServer: ServerConfigDTO = {
      profile: "development",
      max_sessions: 10,
      max_call_duration_seconds: 1800,
      voice_token_max_lifetime_seconds: 60,
      client_ip: "",
      secure: false,
      audio: { backgrounds: [], cues: ["chime", "ding"], allow_flow_background: true, background_default: "" },
      idle: { user_timeout_ms: 0, max_prompts: 2, prompt_mode: "llm" },
      turn: { manager: true, min_words: 0, mute: [], max_mute_ms: 30000, speech_timeout_ms: 600, end_of_turn: "default", smart_turn_available: false, smart_hold_ms: 3000 },
    };

    it("is a choice among the server's cues, only when the flow has routes", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness onChange={onChange} serverConfig={cueServer} />);
      const select = screen.getByRole("combobox", { name: "Route sound" });
      expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(["None", "chime", "ding"]);
      await user.selectOptions(select, "chime");
      await user.type(screen.getByLabelText("Start after (ms)"), "50");
      expect((onChange.mock.calls.at(-1)![0] as FlowDraft).route_cue).toEqual({ name: "chime", after_ms: 50 });
      await user.selectOptions(select, "");
      expect((onChange.mock.calls.at(-1)![0] as FlowDraft).route_cue).toBeUndefined();
    });

    it("keeps a stored name selected while the server's list has not loaded", () => {
      render(<Harness initial={{ ...two, route_cue: { name: "chime" } }} />);
      expect(screen.getByRole("combobox", { name: "Route sound" })).toHaveValue("chime");
    });

    it("shows a stored name the server lacks as selected and not on this server", () => {
      render(<Harness initial={{ ...two, route_cue: { name: "chimes" } }} serverConfig={cueServer} />);
      expect(screen.getByRole("combobox", { name: "Route sound" })).toHaveValue("chimes");
      expect(screen.getByRole("option", { name: "chimes (not on this server)" })).toBeInTheDocument();
    });
  });

  it("asks for a step first on an empty flow, and renames the flow", async () => {
    const user = userEvent.setup();
    render(<Harness initial={emptyFlow()} />);
    expect(screen.getByText("Add a step first; these settings live on the start step.")).toBeVisible();
    expect(screen.queryByLabelText("Route sound")).toBeNull();
    await user.clear(screen.getByLabelText("Flow name"));
    await user.type(screen.getByLabelText("Flow name"), "After hours");
    expect(screen.getByLabelText("Flow name")).toHaveValue("After hours");
  });
});

describe("FlowSettingsPanel published version", () => {
  it("shows the published version and its config hash, read-only", () => {
    render(
      <div style={{ position: "relative" }}>
        <FlowSettingsPanel
          graph={two}
          providers={providers}
          name="Front desk"
          description=""
          onName={vi.fn()}
          onDescription={vi.fn()}
          onChange={vi.fn()}
          onClose={vi.fn()}
          published={{ number: 3, createdAt: "2026-10-05T12:00:00Z", hash: "abc123" }}
        />
      </div>,
    );
    const section = screen.getByRole("group", { name: "Published version" });
    expect(section).toHaveTextContent("v3");
    expect(section).toHaveTextContent("abc123");
  });
});

describe("FlowSettingsPanel footer", () => {
  it("ends with a footer: the changes are part of the draft, and Done closes the panel", async () => {
    const onClose = vi.fn();
    render(<Harness onClose={onClose} />);
    expect(screen.getByText("Part of the draft until you publish.")).toBeVisible();
    await userEvent.setup().click(screen.getByRole("button", { name: "Done" }));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("explains the obscure settings with a button beside each, without renaming the fields", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    expect(screen.getByLabelText("Wait after the caller stops (ms)")).toBeInTheDocument();
    for (const name of ["Wait after the caller stops", "Memory limits", "Summarise older turns", "Recording"]) {
      expect(screen.getByRole("button", { name: `Explain ${name}`, expanded: false })).toBeInTheDocument();
    }
    await user.click(screen.getByRole("button", { name: "Explain Memory limits" }));
    expect(screen.getByRole("region", { name: "Explain Memory limits" })).toHaveTextContent("20 messages and 4000 tokens");
  });

  it("explains the route sound once a route exists", () => {
    render(<Harness />);
    expect(screen.getByRole("button", { name: "Explain Route sound" })).toBeInTheDocument();
  });

  describe("Basic and Advanced", () => {
    const withCfg = (extra: Record<string, unknown>) =>
      fromFlowConfig({
        start: "a",
        steps: [{ id: "a", name: "Reception", config: { llm: { provider: "g", model: "m" }, tts: { provider: "g" }, stt: { provider: "deepgram" }, ...extra } }],
        routes: [],
      });
    const advanced = () => screen.getByText(/^Advanced/).closest("details")!;

    it("keeps the essentials up front and the rest under a closed Advanced for a default flow", () => {
      render(<Harness />);
      expect(screen.getByLabelText("Flow name")).toBeVisible();
      expect(screen.getByLabelText("Record calls")).toBeVisible();
      expect(screen.getByLabelText("STT provider")).toBeVisible();
      expect(advanced()).not.toHaveAttribute("open");
      expect(advanced()).toContainElement(screen.getByLabelText("Wait after the caller stops (ms)"));
      expect(advanced()).toContainElement(screen.getByLabelText("Max messages"));
      expect(advanced()).toContainElement(screen.getByLabelText("Route sound"));
      expect(advanced()).not.toContainElement(screen.getByLabelText("Record calls"));
      expect(screen.getByText(/^Advanced/)).toHaveTextContent(/^Advanced$/);
    });

    it("says how many advanced settings are set and opens when any is", () => {
      render(<Harness initial={withCfg({ vad_stop_ms: 400, context: { max_tokens: 3000 } })} />);
      expect(screen.getByText(/^Advanced/)).toHaveTextContent("Advanced · 2 set");
      expect(advanced()).toHaveAttribute("open");
    });

    it("opens by itself when the start step has interruption settings, and shows them there", () => {
      render(<Harness initial={withCfg({ turn: { mute: ["always"] } })} />);
      expect(screen.getByText(/^Advanced/)).toHaveTextContent("Advanced · 1 set");
      expect(advanced()).toHaveAttribute("open");
      expect(advanced()).toContainElement(screen.getByRole("group", { name: "Interruptions" }));
      expect(screen.getByRole("radio", { name: "Never while the agent speaks" })).toBeChecked();
    });

    it("shows the end-of-turn choices in Turn-taking, and opens by itself when the start step sets one", async () => {
      render(<Harness initial={withCfg({ turn: { end_of_turn: "eager", speech_timeout_ms: 900 } })} />);
      expect(advanced()).toHaveAttribute("open");
      expect(screen.getByRole("group", { name: "Turn-taking" })).toContainElement(screen.getByRole("combobox", { name: "End-of-turn detection" }));
      expect(screen.getByRole("combobox", { name: "End-of-turn detection" })).toHaveValue("eager");
      expect(screen.getByRole("spinbutton", { name: "Wait for the caller to resume (ms)" })).toHaveValue(900);
    });

    it("opens by itself when an advanced field has a problem", () => {
      render(<Harness initial={withCfg({ vad_stop_ms: 9000 })} />);
      expect(advanced()).toHaveAttribute("open");
      expect(screen.getByRole("note")).toHaveTextContent("Wait after the caller stops must be 200 to 2000 ms.");
    });

    it("toggles when its summary is activated (a native details element, so the keyboard works too)", async () => {
      const user = userEvent.setup();
      render(<Harness />);
      await user.click(screen.getByText(/^Advanced/));
      expect(advanced()).toHaveAttribute("open");
      await user.click(screen.getByText(/^Advanced/));
      expect(advanced()).not.toHaveAttribute("open");
    });
  });

  describe("summarisation details and the late-transcript wait", () => {
    const stored = fromFlowConfig({
      start: "a",
      steps: [
        {
          id: "a",
          name: "Reception",
          config: {
            llm: { provider: "google", model: "m" },
            tts: { provider: "g" },
            stt: { provider: "deepgram" },
            stt_p99_ms: 900,
            context: { summarization: { enabled: true, trigger_tokens: 900, keep_turns: 2, prompt: "Notes." } },
          },
        },
      ],
      routes: [],
    });

    it("shows the three fields only while summarising is on, and writes them", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness onChange={onChange} />);
      await user.click(screen.getByText(/^Advanced/));
      expect(screen.queryByLabelText("Summarise when history reaches (tokens)")).toBeNull();
      await user.click(screen.getByLabelText("Summarise older turns"));
      await user.type(screen.getByLabelText("Summarise when history reaches (tokens)"), "900");
      await user.type(screen.getByLabelText("Keep the latest turns"), "2");
      await user.type(screen.getByLabelText("Summary instructions"), "Notes.");
      expect(startCfg(onChange).context).toEqual({ summarization: { enabled: true, trigger_tokens: 900, keep_turns: 2, prompt: "Notes." } });
      await user.clear(screen.getByLabelText("Keep the latest turns"));
      expect(startCfg(onChange).context).toEqual({ summarization: { enabled: true, trigger_tokens: 900, prompt: "Notes." } });
      await user.clear(screen.getByLabelText("Summary instructions"));
      expect(startCfg(onChange).context).toEqual({ summarization: { enabled: true, trigger_tokens: 900 } });
      await user.click(screen.getByLabelText("Summarise older turns"));
      expect(startCfg(onChange)).not.toHaveProperty("context");
      expect(screen.queryByLabelText("Keep the latest turns")).toBeNull();
    });

    it("opens a stored flow showing its values", () => {
      render(<Harness initial={stored} />);
      expect(screen.getByLabelText("Summarise when history reaches (tokens)")).toHaveValue(900);
      expect(screen.getByLabelText("Keep the latest turns")).toHaveValue(2);
      expect(screen.getByLabelText("Summary instructions")).toHaveValue("Notes.");
      expect(screen.getByLabelText("Wait for a late transcript (ms)")).toHaveValue(900);
    });

    it("sets and clears the late-transcript wait", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness onChange={onChange} />);
      await user.click(screen.getByText(/^Advanced/));
      await user.type(screen.getByLabelText("Wait for a late transcript (ms)"), "800");
      expect(startCfg(onChange).stt_p99_ms).toBe(800);
      await user.clear(screen.getByLabelText("Wait for a late transcript (ms)"));
      expect(startCfg(onChange)).not.toHaveProperty("stt_p99_ms");
    });

    it("explains each new field", () => {
      render(<Harness initial={stored} />);
      for (const name of ["Summarise-at", "Keep the latest turns", "Summary instructions", "Wait for a late transcript"]) {
        expect(screen.getByRole("button", { name: `Explain ${name}` })).toBeInTheDocument();
      }
    });

    it("does not count a route sound the flow has no routes to show", () => {
      const g = fromFlowConfig({ start: "a", steps: [{ id: "a", name: "Reception", config: { stt: { provider: "deepgram" } } }], routes: [] });
      render(<Harness initial={{ ...g, route_cue: { name: "chime" } }} />);
      expect(screen.getByText(/^Advanced/)).toHaveTextContent(/^Advanced$/);
    });
  });

  describe("quiet caller and audio", () => {
    const server: ServerConfigDTO = {
      profile: "development",
      max_sessions: 10,
      max_call_duration_seconds: 1800,
      voice_token_max_lifetime_seconds: 60,
      client_ip: "",
      secure: false,
      audio: { backgrounds: ["cafe", "office"], cues: ["chime"], allow_flow_background: true, background_default: "" },
      idle: { user_timeout_ms: 15000, max_prompts: 2, prompt_mode: "llm" },
      turn: { manager: true, min_words: 0, mute: [], max_mute_ms: 30000, speech_timeout_ms: 600, end_of_turn: "default", smart_turn_available: false, smart_hold_ms: 3000 },
    };
    const withStart = (extra: Record<string, unknown>) =>
      fromFlowConfig({ start: "a", steps: [{ id: "a", name: "Reception", config: { llm: { provider: "g", model: "m" }, tts: { provider: "g" }, stt: { provider: "deepgram" }, ...extra } }], routes: [] });
    const openAdvanced = (user: ReturnType<typeof userEvent.setup>) => user.click(screen.getByText(/^Advanced/));

    it("shows the checkbox and the wait up front, with the server's default as the placeholder", () => {
      render(<Harness serverConfig={server} />);
      expect(screen.getByLabelText("End the call if the caller stays silent")).toBeChecked();
      expect(screen.getByLabelText("Wait this long (seconds)")).toHaveAttribute("placeholder", "15");
      expect(screen.getByText("The call ends one wait after the last question.")).toBeVisible();
      expect(screen.getByRole("button", { name: "Explain Silent caller wait" })).toBeInTheDocument();
    });

    it("falls back to a plain placeholder while the server's answer is missing", () => {
      render(<Harness />);
      expect(screen.getByLabelText("Wait this long (seconds)")).toHaveAttribute("placeholder", "Server default");
    });

    it("writes the wait in milliseconds and removes the whole idle object when it is cleared", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness serverConfig={server} onChange={onChange} />);
      await user.type(screen.getByLabelText("Wait this long (seconds)"), "8");
      expect(startCfg(onChange).idle).toEqual({ user_timeout_ms: 8000 });
      await user.clear(screen.getByLabelText("Wait this long (seconds)"));
      expect(startCfg(onChange)).not.toHaveProperty("idle");
    });

    it("unticking the box turns it off for this flow with an explicit 0, and ticking it again goes back to the server's", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness serverConfig={server} onChange={onChange} />);
      await user.click(screen.getByLabelText("End the call if the caller stays silent"));
      expect(startCfg(onChange).idle).toEqual({ user_timeout_ms: 0 });
      expect(screen.getByLabelText("Wait this long (seconds)")).toBeDisabled();
      await user.click(screen.getByLabelText("End the call if the caller stays silent"));
      expect(startCfg(onChange)).not.toHaveProperty("idle");
    });

    it("when the server has it off, ticking the box asks for the built-in 15 seconds", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness serverConfig={{ ...server, idle: { ...server.idle, user_timeout_ms: 0 } }} onChange={onChange} />);
      expect(screen.getByLabelText("End the call if the caller stays silent")).not.toBeChecked();
      await user.click(screen.getByLabelText("End the call if the caller stays silent"));
      expect(startCfg(onChange).idle).toEqual({ user_timeout_ms: 15000 });
    });

    it("keeps the other questions' members when the box is toggled", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness initial={withStart({ idle: { user_timeout_ms: 5000, prompt: "Hello?" } })} serverConfig={server} onChange={onChange} />);
      await user.click(screen.getByLabelText("End the call if the caller stays silent"));
      expect(startCfg(onChange).idle).toEqual({ user_timeout_ms: 0, prompt: "Hello?" });
    });

    it("sets how many times, what to say, how, and the goodbye in Advanced", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness serverConfig={server} onChange={onChange} />);
      await openAdvanced(user);
      await user.type(screen.getByLabelText("Ask this many times"), "3");
      await user.type(screen.getByLabelText("What to say"), "Hello?");
      await user.click(screen.getByLabelText("Say it exactly"));
      await user.type(screen.getByLabelText("Goodbye before hanging up"), "Bye.");
      expect(startCfg(onChange).idle).toEqual({ max_prompts: 3, prompt: "Hello?", prompt_mode: "say", goodbye: "Bye." });
      await user.click(screen.getByLabelText("Let the model word it"));
      expect(startCfg(onChange).idle).toEqual({ max_prompts: 3, prompt: "Hello?", goodbye: "Bye." });
      await user.clear(screen.getByLabelText("Ask this many times"));
      await user.clear(screen.getByLabelText("What to say"));
      await user.clear(screen.getByLabelText("Goodbye before hanging up"));
      expect(startCfg(onChange)).not.toHaveProperty("idle");
    });

    it("shows the built-in wording as placeholders", async () => {
      const user = userEvent.setup();
      render(<Harness serverConfig={server} />);
      await openAdvanced(user);
      expect(screen.getByLabelText("What to say")).toHaveAttribute("placeholder", "Are you still there?");
      expect(screen.getByLabelText("Goodbye before hanging up")).toHaveAttribute("placeholder", "I'll end the call now. Goodbye.");
      expect(screen.getByLabelText("Ask this many times")).toHaveAttribute("placeholder", "2");
    });

    it("turns noise reduction on with its strength, and off again", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness serverConfig={server} onChange={onChange} />);
      await openAdvanced(user);
      expect(screen.queryByLabelText("Noise reduction strength (%)")).toBeNull();
      await user.click(screen.getByLabelText("Reduce background noise"));
      expect(startCfg(onChange).audio).toEqual({ denoise: {} });
      await user.type(screen.getByLabelText("Noise reduction strength (%)"), "80");
      expect(screen.getByLabelText("Noise reduction strength (%)")).toHaveAttribute("min", "1");
      expect(startCfg(onChange).audio).toEqual({ denoise: { strength: 0.8 } });
      await user.click(screen.getByLabelText("Reduce background noise"));
      expect(startCfg(onChange)).not.toHaveProperty("audio");
    });

    it("writes the voice volume, including an explicit 0, and removes it when blank", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness serverConfig={server} onChange={onChange} />);
      await openAdvanced(user);
      await user.type(screen.getByLabelText("Voice volume (dB)"), "-3");
      expect(startCfg(onChange).audio).toEqual({ output_gain_db: -3 });
      await user.clear(screen.getByLabelText("Voice volume (dB)"));
      await user.type(screen.getByLabelText("Voice volume (dB)"), "0");
      expect(startCfg(onChange).audio).toEqual({ output_gain_db: 0 });
      await user.clear(screen.getByLabelText("Voice volume (dB)"));
      expect(startCfg(onChange)).not.toHaveProperty("audio");
    });

    it("chooses a background from the server's sounds, with its volume and repeat", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness serverConfig={server} onChange={onChange} />);
      await openAdvanced(user);
      const select = screen.getByLabelText("Background sound");
      expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(["None", "cafe", "office"]);
      expect(screen.queryByLabelText("Background volume (dB)")).toBeNull();
      await user.selectOptions(select, "office");
      expect(startCfg(onChange).audio).toEqual({ background: { name: "office" } });
      await user.type(screen.getByLabelText("Background volume (dB)"), "-20");
      await user.click(screen.getByLabelText("Repeat"));
      expect(startCfg(onChange).audio).toEqual({ background: { name: "office", gain_db: -20, loop: false } });
      await user.click(screen.getByLabelText("Repeat"));
      expect(startCfg(onChange).audio).toEqual({ background: { name: "office", gain_db: -20 } });
      await user.selectOptions(select, "");
      expect(startCfg(onChange)).not.toHaveProperty("audio");
    });

    it("with a server default sound, offers it by name and a separate None that writes an empty name", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness serverConfig={{ ...server, audio: { ...server.audio, background_default: "cafe" } }} onChange={onChange} />);
      await openAdvanced(user);
      const select = screen.getByLabelText("Background sound");
      expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(["Server default (cafe)", "None", "cafe", "office"]);
      expect(select).toHaveValue("default");
      await user.selectOptions(select, "");
      expect(startCfg(onChange).audio).toEqual({ background: { name: "" } });
      expect(screen.queryByLabelText("Background volume (dB)")).toBeNull();
      await user.selectOptions(select, "default");
      expect(startCfg(onChange)).not.toHaveProperty("audio");
    });

    it("does not call a stored sound unknown while the server's answer is missing", () => {
      render(<Harness initial={withStart({ audio: { background: { name: "office" } } })} />);
      expect(screen.queryByText(/is not a sound on this server/)).toBeNull();
    });

    it("shows the server's own question count and wording mode until the flow sets its own", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness serverConfig={{ ...server, idle: { user_timeout_ms: 15000, max_prompts: 4, prompt_mode: "say" } }} onChange={onChange} />);
      await openAdvanced(user);
      expect(screen.getByLabelText("Ask this many times")).toHaveAttribute("placeholder", "4");
      expect(screen.getByLabelText("Say it exactly")).toBeChecked();
      await user.click(screen.getByLabelText("Let the model word it"));
      expect(startCfg(onChange).idle).toEqual({ prompt_mode: "llm" });
    });

    it("disables the sound with the reason when the server does not let flows choose one", async () => {
      const user = userEvent.setup();
      render(<Harness serverConfig={{ ...server, audio: { backgrounds: ["office"], cues: [], allow_flow_background: false, background_default: "" } }} />);
      await openAdvanced(user);
      expect(screen.getByLabelText("Background sound")).toBeDisabled();
      expect(screen.getByText("This server does not allow flow background sounds.")).toBeVisible();
    });

    it("names a stored sound the server no longer has", () => {
      render(<Harness initial={withStart({ audio: { background: { name: "gone" } } })} serverConfig={server} />);
      expect(within(screen.getByLabelText("Background sound")).getByRole("option", { name: "gone (not on this server)" })).toBeInTheDocument();
      expect(screen.getByText('"gone" is not a sound on this server.')).toBeVisible();
    });

    it("opens a stored flow showing every value, and counts the two advanced groups", () => {
      const stored = {
        idle: { user_timeout_ms: 8000, max_prompts: 3, prompt: "Hello?", prompt_mode: "say", goodbye: "Bye." },
        audio: { denoise: { strength: 0.8 }, output_gain_db: -3, background: { name: "office", gain_db: -20, loop: false } },
      };
      render(<Harness initial={withStart(stored)} serverConfig={server} />);
      expect(screen.getByText(/^Advanced/)).toHaveTextContent("Advanced · 2 set");
      expect(screen.getByLabelText("Wait this long (seconds)")).toHaveValue(8);
      expect(screen.getByLabelText("Ask this many times")).toHaveValue(3);
      expect(screen.getByLabelText("What to say")).toHaveValue("Hello?");
      expect(screen.getByLabelText("Say it exactly")).toBeChecked();
      expect(screen.getByLabelText("Goodbye before hanging up")).toHaveValue("Bye.");
      expect(screen.getByLabelText("Reduce background noise")).toBeChecked();
      expect(screen.getByLabelText("Noise reduction strength (%)")).toHaveValue(80);
      expect(screen.getByLabelText("Voice volume (dB)")).toHaveValue(-3);
      expect(screen.getByLabelText("Background sound")).toHaveValue("office");
      expect(screen.getByLabelText("Background volume (dB)")).toHaveValue(-20);
      expect(screen.getByLabelText("Repeat")).not.toBeChecked();
    });

    it("explains each new field", async () => {
      const user = userEvent.setup();
      render(<Harness serverConfig={server} initial={withStart({ audio: { denoise: {}, background: { name: "office" } } })} />);
      await openAdvanced(user);
      for (const name of ["Silent caller wait", "Questions before hanging up", "What to say", "How to say it", "Goodbye", "Noise reduction", "Voice volume", "Background sound", "Background volume", "Repeat background"]) {
        expect(screen.getByRole("button", { name: `Explain ${name}` })).toBeInTheDocument();
      }
    });
  });
});
