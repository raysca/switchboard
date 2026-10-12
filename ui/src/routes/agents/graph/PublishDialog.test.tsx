import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { StrictMode, useState } from "react";
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

  it("is a modal dialog that cancels on the browser's Escape (cancel event) or Keep editing", async () => {
    const user = userEvent.setup();
    const onCancel = vi.fn();
    render(<PublishDialog {...props} onCancel={onCancel} />);
    const dialog = screen.getByRole("dialog");
    expect(dialog.tagName).toBe("DIALOG");
    expect(dialog).toHaveAttribute("open");
    fireEvent(dialog, new Event("cancel", { cancelable: true }));
    await user.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(onCancel).toHaveBeenCalledTimes(2);
  });

  it("gives focus back to what opened it", async () => {
    const user = userEvent.setup();
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button onClick={() => setOpen(true)}>Publish</button>
          {open && <PublishDialog {...props} onCancel={() => setOpen(false)} />}
        </>
      );
    }
    // StrictMode (as in main.tsx) runs effects twice: the opener must still be the Publish button.
    render(
      <StrictMode>
        <Harness />
      </StrictMode>,
    );
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(screen.getByRole("button", { name: "Publish" })).toHaveFocus();
  });

  it("says when this is the first version", () => {
    render(<PublishDialog {...props} version={1} firstVersion changes={[]} />);
    expect(screen.getByText("This is the first version of this flow.")).toBeVisible();
  });
});
