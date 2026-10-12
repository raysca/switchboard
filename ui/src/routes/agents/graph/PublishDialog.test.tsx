import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PublishDialog } from "./PublishDialog";

const props = { flowName: "Front desk", version: 8, firstVersion: false, changes: ["Added step Support"], problems: [] as string[], pending: false, onConfirm: vi.fn(), onCancel: vi.fn() };

describe("PublishDialog", () => {
  it("explains what publishing does and lists the changes", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    render(<PublishDialog {...props} onConfirm={onConfirm} />);
    const dialog = screen.getByRole("dialog", { name: "Publish Front desk v8?" });
    expect(screen.getByRole("heading", { name: "Publish Front desk v8?" })).toHaveFocus();
    expect(dialog).toHaveTextContent("Calls already in progress finish on the version they started with.");
    expect(dialog).toHaveTextContent("Added step Support");
    await user.click(screen.getByRole("button", { name: "Publish v8" }));
    expect(onConfirm).toHaveBeenCalledOnce();
  });

  it("blocks publishing while there are problems", () => {
    render(<PublishDialog {...props} problems={["The route from Reception to Billing needs a condition."]} />);
    expect(screen.getByRole("alert")).toHaveTextContent("needs a condition");
    expect(screen.getByRole("button", { name: "Publish v8" })).toBeDisabled();
  });

  it("cancels with Escape or Keep editing", async () => {
    const user = userEvent.setup();
    const onCancel = vi.fn();
    render(<PublishDialog {...props} onCancel={onCancel} />);
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(onCancel).toHaveBeenCalledTimes(2);
  });

  it("says when this is the first version", () => {
    render(<PublishDialog {...props} version={1} firstVersion changes={[]} />);
    expect(screen.getByText("This is the first version of this flow.")).toBeVisible();
  });
});
