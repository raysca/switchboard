import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
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

function Harness({ initial = two, onChange = vi.fn(), onClose = vi.fn() }: { initial?: FlowDraft; onChange?: (g: FlowDraft) => void; onClose?: () => void }) {
  const [g, setG] = useState(initial);
  const [name, setName] = useState("Front desk");
  return (
    <div style={{ position: "relative" }}>
      <FlowSettingsPanel
        graph={g}
        providers={providers}
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

  it("edits the route sound only when the flow has routes", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    await user.type(screen.getByLabelText("Sound name"), "chime");
    await user.type(screen.getByLabelText("Start after (ms)"), "50");
    expect((onChange.mock.calls.at(-1)![0] as FlowDraft).route_cue).toEqual({ name: "chime", after_ms: 50 });
  });

  it("asks for a step first on an empty flow, and renames the flow", async () => {
    const user = userEvent.setup();
    render(<Harness initial={emptyFlow()} />);
    expect(screen.getByText("Add a step first; these settings live on the start step.")).toBeVisible();
    expect(screen.queryByLabelText("Sound name")).toBeNull();
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
      expect(advanced()).toContainElement(screen.getByLabelText("Sound name"));
      expect(advanced()).not.toContainElement(screen.getByLabelText("Record calls"));
      expect(screen.getByText(/^Advanced/)).toHaveTextContent(/^Advanced$/);
    });

    it("says how many advanced settings are set and opens when any is", () => {
      render(<Harness initial={withCfg({ vad_stop_ms: 400, context: { max_tokens: 3000 } })} />);
      expect(screen.getByText(/^Advanced/)).toHaveTextContent("Advanced · 2 set");
      expect(advanced()).toHaveAttribute("open");
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
});
