import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NewFlowDialog, type NewFlowDialogProps } from "./NewFlowDialog";

const flows = [
  { id: "a1", name: "Front desk" },
  { id: "a2", name: "Appointments" },
];
function setup(over: Partial<NewFlowDialogProps> = {}) {
  const props: NewFlowDialogProps = { flows, pending: false, error: null, onBlank: vi.fn(), onCopy: vi.fn(), onCancel: vi.fn(), ...over };
  render(<NewFlowDialog {...props} />);
  return { props, user: userEvent.setup() };
}

describe("NewFlowDialog", () => {
  it("creates a blank flow with the typed name", async () => {
    const { props, user } = setup();
    const create = screen.getByRole("button", { name: "Create and open editor" });
    expect(create).toBeDisabled();
    await user.type(screen.getByLabelText("Name"), "After hours");
    await user.click(create);
    expect(props.onBlank).toHaveBeenCalledWith("After hours");
    expect(props.onCopy).not.toHaveBeenCalled();
  });

  it("copies the chosen flow under a new name", async () => {
    const { props, user } = setup();
    await user.click(screen.getByRole("radio", { name: /Copy an existing flow/ }));
    await user.selectOptions(screen.getByLabelText("Flow to copy"), "a2");
    await user.type(screen.getByLabelText("Name"), "Night line");
    await user.click(screen.getByRole("button", { name: "Copy and open editor" }));
    expect(props.onCopy).toHaveBeenCalledWith("a2", "Night line");
    expect(props.onBlank).not.toHaveBeenCalled();
  });

  it("submits with Enter, and not while the name is blank or a request is running", async () => {
    const { props, user } = setup();
    await user.type(screen.getByLabelText("Name"), "   {Enter}");
    expect(props.onBlank).not.toHaveBeenCalled();
    await user.clear(screen.getByLabelText("Name"));
    await user.type(screen.getByLabelText("Name"), "X{Enter}");
    expect(props.onBlank).toHaveBeenCalledWith("X");
  });

  it("cannot copy when there is nothing to copy", () => {
    setup({ flows: [] });
    expect(screen.getByRole("radio", { name: /Copy an existing flow/ })).toBeDisabled();
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
});
