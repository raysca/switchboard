import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
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

  it("turns text cleaning off explicitly and back on by omission", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    const box = screen.getByLabelText(/Clean text before speaking/);
    expect(box).toBeChecked();
    await user.click(box);
    expect(last(onChange).tts).toMatchObject({ clean_text: false });
    await user.click(box);
    expect(last(onChange).tts).not.toHaveProperty("clean_text");
  });

  it("explains the obscure settings and leaves the field names alone", async () => {
    const user = userEvent.setup();
    render(<Harness onChange={vi.fn()} />);
    expect(screen.getByLabelText("Tool calls at once")).toBeInTheDocument();
    for (const name of ["Fallbacks", "Tool calls at once", "Text cleaning"]) {
      await user.click(screen.getByRole("button", { name: `Explain ${name}` }));
    }
    expect(screen.getByRole("region", { name: "Explain Tool calls at once" })).toHaveTextContent("1 to 16");
  });

  describe("failover policy", () => {
    const box = (name: RegExp | string) => screen.getByRole("checkbox", { name });

    it("shows the three defaults checked, with the rejected-requests class off", () => {
      render(<Harness onChange={vi.fn()} />);
      const group = screen.getByRole("group", { name: "When to move to a fallback" });
      expect(group).toHaveTextContent("Only matters when a stage has fallbacks.");
      expect(box("Timeouts")).toBeChecked();
      expect(box("Server errors and dropped connections")).toBeChecked();
      expect(box("The provider's circuit breaker is open")).toBeChecked();
      expect(box("Rejected requests (bad request, bad key)")).not.toBeChecked();
    });

    it("writes the policy in a fixed order, and drops the key when it is the default again", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness onChange={onChange} />);
      await user.click(box("Rejected requests (bad request, bad key)"));
      expect(last(onChange).failover).toEqual({ on: ["timeout", "server_error", "circuit_open", "client_error"] });
      await user.click(box("Rejected requests (bad request, bad key)"));
      expect(last(onChange)).not.toHaveProperty("failover");
    });

    it("keeps at least one class checked: an empty list would silently mean the defaults", async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      render(<Harness onChange={onChange} />);
      await user.click(box("Timeouts"));
      await user.click(box("Server errors and dropped connections"));
      expect(last(onChange).failover).toEqual({ on: ["circuit_open"] });
      expect(box("The provider's circuit breaker is open")).toBeDisabled();
    });

    it("opens a stored policy showing it, and leaves it alone until changed", () => {
      const stored = fromFlowConfig({ llm: { provider: "google", model: "gemini" }, tts: { provider: "google" }, stt: { provider: "google" }, failover: { on: ["client_error"] } });
      render(<StepAdvanced graph={stored} step={stored.steps[0]} providers={providers} onChange={vi.fn()} />);
      expect(box("Rejected requests (bad request, bad key)")).toBeChecked();
      expect(box("Timeouts")).not.toBeChecked();
      expect(screen.getByRole("button", { name: "Explain When to move to a fallback" })).toBeInTheDocument();
    });
  });
});
