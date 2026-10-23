import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NewAgentDialog, type NewAgentDialogProps } from "./NewAgentDialog";

const agents = [
  { id: "a1", name: "Front desk" },
  { id: "a2", name: "Appointments" },
];
function setup(over: Partial<NewAgentDialogProps> = {}) {
  const props: NewAgentDialogProps = { agents, pending: false, error: null, onBlank: vi.fn(), onCopy: vi.fn(), onCancel: vi.fn(), ...over };
  render(<NewAgentDialog {...props} />);
  return { props, user: userEvent.setup() };
}

describe("NewAgentDialog", () => {
  it("creates a blank agent with the typed name", async () => {
    const { props, user } = setup();
    const create = screen.getByRole("button", { name: "Create and open editor" });
    expect(create).toBeDisabled();
    await user.type(screen.getByLabelText("Name your agent"), "After hours");
    await user.click(create);
    expect(props.onBlank).toHaveBeenCalledWith("After hours");
    expect(props.onCopy).not.toHaveBeenCalled();
  });

  it("copies the chosen agent under a new name", async () => {
    const { props, user } = setup();
    await user.click(screen.getByRole("radio", { name: /Copy an existing agent/ }));
    await user.selectOptions(screen.getByLabelText("Agent to copy"), "a2");
    await user.type(screen.getByLabelText("Name your agent"), "Night line");
    await user.click(screen.getByRole("button", { name: "Copy and open editor" }));
    expect(props.onCopy).toHaveBeenCalledWith("a2", "Night line");
    expect(props.onBlank).not.toHaveBeenCalled();
  });

  it("submits with Enter, and not while the name is blank or a request is running", async () => {
    const { props, user } = setup();
    await user.type(screen.getByLabelText("Name your agent"), "   {Enter}");
    expect(props.onBlank).not.toHaveBeenCalled();
    await user.clear(screen.getByLabelText("Name your agent"));
    await user.type(screen.getByLabelText("Name your agent"), "X{Enter}");
    expect(props.onBlank).toHaveBeenCalledWith("X");
  });

  it("cannot copy when there is nothing to copy", () => {
    setup({ agents: [] });
    expect(screen.getByRole("radio", { name: /Copy an existing agent/ })).toBeDisabled();
  });

  it("shows a failed copy in the dialog and locks the button while it works", () => {
    setup({ error: "Copy failed" });
    expect(screen.getByRole("alert")).toHaveTextContent("Copy failed");
    setup({ pending: true });
    expect(screen.getAllByRole("button", { name: "Working…" })[0]).toBeDisabled();
  });

  it("cancels from the button and from Escape", async () => {
    const { props, user } = setup();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent(screen.getByRole("dialog"), new Event("cancel", { cancelable: true }));
    expect(props.onCancel).toHaveBeenCalledTimes(2);
  });

  it("shows Blank and Copy as option cards that are real radios, with the chosen one marked", async () => {
    const { user } = setup();
    const blank = screen.getByRole("radio", { name: /Blank/ });
    expect(blank.closest(".option-card")).toHaveAttribute("data-selected", "true");
    await user.click(screen.getByRole("radio", { name: /Copy an existing agent/ }));
    expect(screen.getByRole("radio", { name: /Copy an existing agent/ }).closest(".option-card")).toHaveAttribute("data-selected", "true");
    expect(blank.closest(".option-card")).not.toHaveAttribute("data-selected");
  });
});
