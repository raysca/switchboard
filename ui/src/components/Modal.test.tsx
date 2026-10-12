import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { StrictMode, useState } from "react";
import { Modal } from "./Modal";

describe("Modal", () => {
  it("is a modal dialog named by its title, with focus on the title", () => {
    render(
      <Modal title="Archive Front desk?" onCancel={vi.fn()}>
        <button>Keep</button>
      </Modal>,
    );
    const dialog = screen.getByRole("dialog", { name: "Archive Front desk?" });
    expect(dialog.tagName).toBe("DIALOG");
    expect(dialog).toHaveAttribute("open");
    expect(screen.getByRole("heading", { name: "Archive Front desk?" })).toHaveFocus();
    expect(dialog).toContainElement(screen.getByRole("button", { name: "Keep" }));
  });

  it("cancels on the browser's Escape (the cancel event)", () => {
    const onCancel = vi.fn();
    render(
      <Modal title="T" onCancel={onCancel}>
        <p>body</p>
      </Modal>,
    );
    fireEvent(screen.getByRole("dialog"), new Event("cancel", { cancelable: true }));
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it("gives focus back to what opened it, also under StrictMode", async () => {
    const user = userEvent.setup();
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button onClick={() => setOpen(true)}>Open</button>
          {open && (
            <Modal title="T" onCancel={() => setOpen(false)}>
              <button onClick={() => setOpen(false)}>Close</button>
            </Modal>
          )}
        </>
      );
    }
    render(
      <StrictMode>
        <Harness />
      </StrictMode>,
    );
    await user.click(screen.getByRole("button", { name: "Open" }));
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.getByRole("button", { name: "Open" })).toHaveFocus();
  });

  it("puts focus in the first field when there is one, so typing goes straight in", async () => {
    render(
      <Modal title="T" onCancel={vi.fn()}>
        <input aria-label="Name" />
      </Modal>,
    );
    expect(screen.getByLabelText("Name")).toHaveFocus();
    await userEvent.keyboard("abc");
    expect(screen.getByLabelText("Name")).toHaveValue("abc");
  });

  it("has a close button that cancels", async () => {
    const onCancel = vi.fn();
    render(
      <Modal title="T" onCancel={onCancel}>
        <p>body</p>
      </Modal>,
    );
    await userEvent.click(screen.getByRole("button", { name: "Close dialog" }));
    expect(onCancel).toHaveBeenCalledOnce();
  });
});
