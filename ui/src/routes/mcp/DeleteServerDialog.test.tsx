import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import type { AgentDTO } from "../../api/client";
import { DeleteServerDialog } from "./DeleteServerDialog";

const agent = (id: string, name: string): AgentDTO => ({
  id,
  name,
  description: "",
  enabled: true,
  archived: false,
  current_version: null,
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
});

function setup(over: Partial<{ inUse: boolean; agents: AgentDTO[] | undefined; pending: boolean; error: string | null }> = {}) {
  const fns = { onDelete: vi.fn(), onDetach: vi.fn(), onCancel: vi.fn() };
  render(
    <MemoryRouter>
      <DeleteServerDialog name="billing" agents={[]} inUse={false} pending={false} error={null} {...fns} {...over} />
    </MemoryRouter>,
  );
  return { ...fns, user: userEvent.setup() };
}

describe("DeleteServerDialog", () => {
  it("asks first, and deletes only on the button", async () => {
    const { onDelete, onCancel, user } = setup();
    expect(screen.getByRole("dialog", { name: "Delete billing?" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Keep server" }));
    expect(onCancel).toHaveBeenCalledOnce();
    expect(onDelete).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Delete server" }));
    expect(onDelete).toHaveBeenCalledOnce();
  });

  it("when the server is still in use, names the agents and needs a second, explicit confirmation", async () => {
    const { onDetach, onDelete, user } = setup({ inUse: true, agents: [agent("f1", "Front desk"), agent("f2", "Support triage")] });
    expect(screen.getByRole("dialog", { name: "billing is still in use" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Front desk" })).toHaveAttribute("href", "/agents/f1");
    expect(screen.getByRole("link", { name: "Support triage" })).toBeVisible();
    expect(screen.getByText(/need review before they take calls again/)).toBeVisible();
    expect(screen.queryByRole("button", { name: "Delete server" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Detach and delete" }));
    expect(onDetach).toHaveBeenCalledOnce();
    expect(onDelete).not.toHaveBeenCalled();
  });

  it("still explains a conflict caused by calls in progress when no agent is known to use it", () => {
    setup({ inUse: true, agents: undefined });
    expect(screen.getByText(/calls in progress/)).toBeVisible();
    expect(screen.getByText("Which agents use it could not be loaded.")).toBeVisible();
  });

  it("shows an error inside the dialog, and disables the button while working", () => {
    setup({ error: "MCP server not found", pending: true });
    expect(screen.getByRole("alert")).toHaveTextContent("MCP server not found");
    expect(screen.getByRole("button", { name: "Deleting…" })).toBeDisabled();
  });

  it("names the agents that use the server before the first delete, and says delete will be refused", () => {
    setup({ inUse: false, agents: [agent("a1", "Front desk"), agent("a2", "Billing")] });
    expect(screen.getByText(/These agents use its tools, so deleting is refused until you detach the server/)).toBeVisible();
    expect(screen.getByRole("link", { name: "Front desk" })).toHaveAttribute("href", "/agents/a1");
    expect(screen.getByRole("link", { name: "Billing" })).toBeVisible();
  });

  it("says nothing extra when no agent uses the server", () => {
    setup({ inUse: false, agents: [] });
    expect(screen.queryByText(/deleting is refused/)).toBeNull();
  });
});
