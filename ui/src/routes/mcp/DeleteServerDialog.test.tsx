import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import type { FlowDTO } from "../../api/client";
import { DeleteServerDialog } from "./DeleteServerDialog";

const flow = (id: string, name: string): FlowDTO => ({
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

function setup(over: Partial<{ inUse: boolean; flows: FlowDTO[] | undefined; pending: boolean; error: string | null }> = {}) {
  const fns = { onDelete: vi.fn(), onDetach: vi.fn(), onCancel: vi.fn() };
  render(
    <MemoryRouter>
      <DeleteServerDialog name="billing" flows={[]} inUse={false} pending={false} error={null} {...fns} {...over} />
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

  it("when the server is still in use, names the flows and needs a second, explicit confirmation", async () => {
    const { onDetach, onDelete, user } = setup({ inUse: true, flows: [flow("f1", "Front desk"), flow("f2", "Support triage")] });
    expect(screen.getByRole("dialog", { name: "billing is still in use" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Front desk" })).toHaveAttribute("href", "/flows/f1");
    expect(screen.getByRole("link", { name: "Support triage" })).toBeVisible();
    expect(screen.getByText(/need review before they take calls again/)).toBeVisible();
    expect(screen.queryByRole("button", { name: "Delete server" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Detach and delete" }));
    expect(onDetach).toHaveBeenCalledOnce();
    expect(onDelete).not.toHaveBeenCalled();
  });

  it("still explains a conflict caused by calls in progress when no flow is known to use it", () => {
    setup({ inUse: true, flows: undefined });
    expect(screen.getByText(/calls in progress/)).toBeVisible();
    expect(screen.getByText("Which flows use it could not be loaded.")).toBeVisible();
  });

  it("shows an error inside the dialog, and disables the button while working", () => {
    setup({ error: "MCP server not found", pending: true });
    expect(screen.getByRole("alert")).toHaveTextContent("MCP server not found");
    expect(screen.getByRole("button", { name: "Deleting…" })).toBeDisabled();
  });
});
