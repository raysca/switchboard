import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { FlowSettingsPanel } from "./FlowSettingsPanel";
import { fromFlowConfig } from "./model";

const graph = fromFlowConfig({
  start: "a",
  steps: [{ id: "a", name: "Reception", config: { llm: { provider: "g", model: "m" }, tts: { provider: "g" } } }],
  handoffs: [],
});

const panel = (onRestore?: () => Promise<void>, number = 3) =>
  render(
    <FlowSettingsPanel
      graph={graph}
      providers={[]}
      name="Front desk"
      description=""
      onName={vi.fn()}
      onDescription={vi.fn()}
      onChange={vi.fn()}
      onClose={vi.fn()}
      published={{ number, createdAt: "2026-10-01T10:00:00Z", hash: "abc123" }}
      onRestore={onRestore}
    />,
  );

describe("FlowSettingsPanel restore", () => {
  it("asks before publishing the previous version again, then does it", async () => {
    const onRestore = vi.fn().mockResolvedValue(undefined);
    panel(onRestore);
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Restore v2…" }));
    expect(screen.getByRole("group", { name: "Confirm restore" })).toHaveTextContent("Publish v2's settings as v4");
    expect(onRestore).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Restore v2" }));
    expect(onRestore).toHaveBeenCalledTimes(1);
  });

  it("cancel backs out without restoring", async () => {
    const onRestore = vi.fn();
    panel(onRestore);
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Restore v2…" }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onRestore).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Restore v2…" })).toBeInTheDocument();
  });

  it("shows why a restore failed", async () => {
    panel(vi.fn().mockRejectedValue(new Error("That version no longer exists.")));
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Restore v2…" }));
    await user.click(screen.getByRole("button", { name: "Restore v2" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("That version no longer exists.");
  });

  it("offers nothing for the first version", () => {
    panel(vi.fn(), 1);
    expect(screen.queryByRole("button", { name: /Restore v/ })).toBeNull();
  });
});
