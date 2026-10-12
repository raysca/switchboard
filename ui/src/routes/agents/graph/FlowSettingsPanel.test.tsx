import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
import { FlowSettingsPanel } from "./FlowSettingsPanel";
import { emptyFlow, fromAgentConfig, type GraphDraft } from "./model";

const svc = { available: true, custom: true, models: [{ id: "nova-3", label: "Nova 3", default: true }], languages: [{ id: "en", label: "English", default: true }] };
const providers: ProviderStatus[] = [
  { id: "deepgram", display_name: "Deepgram", capabilities: ["stt"], detected: true, env: [], services: { stt: svc } },
];
const two = fromAgentConfig({
  entry: "a",
  nodes: [
    { id: "a", name: "Reception", config: { llm: { provider: "g", model: "m" }, tts: { provider: "g" } } },
    { id: "b", name: "Billing", config: { llm: { provider: "g", model: "m" }, tts: { provider: "g" } } },
  ],
  edges: [{ id: "e1", from: "a", to: "b", condition: "invoice" }],
});

function Harness({ initial = two, onChange = vi.fn() }: { initial?: GraphDraft; onChange?: (g: GraphDraft) => void }) {
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
        onClose={vi.fn()}
        onChange={(n) => {
          setG(n);
          onChange(n);
        }}
      />
    </div>
  );
}
const startCfg = (fn: ReturnType<typeof vi.fn>) => {
  const g = fn.mock.calls.at(-1)![0] as GraphDraft;
  return g.nodes.find((n) => n.id === g.entry)!.config;
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
    expect((onChange.mock.calls.at(-1)![0] as GraphDraft).handoff_cue).toEqual({ name: "chime", after_ms: 50 });
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
