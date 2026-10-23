import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { SidePanel } from "./SidePanel";

function Harness({ autoFocusField = false }: { autoFocusField?: boolean }) {
  const [open, setOpen] = useState(false);
  const [wide, setWide] = useState(false);
  const [opener, setOpener] = useState<HTMLElement | null>(null);
  return (
    <>
      <button
        onClick={(e) => {
          setOpener(e.currentTarget);
          setOpen(true);
        }}
      >
        Open
      </button>
      {open && (
        <SidePanel title="Agent settings" onClose={() => setOpen(false)} wide={wide} onToggleWide={() => setWide((w) => !w)} returnFocusTo={opener} footer={<button>Done</button>}>
          <label>
            Name
            <input autoFocus={autoFocusField} />
          </label>
        </SidePanel>
      )}
    </>
  );
}

describe("SidePanel", () => {
  it("is labelled by its title, focuses it, and returns focus to the opener on Escape", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Open" }));
    const panel = screen.getByRole("complementary", { name: "Agent settings" });
    expect(screen.getByRole("heading", { name: "Agent settings" })).toHaveFocus();
    expect(panel).toContainElement(screen.getByRole("button", { name: "Done" }));
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("complementary")).toBeNull();
    expect(screen.getByRole("button", { name: "Open" })).toHaveFocus();
  });

  it("leaves focus on a field that asked for it", async () => {
    const user = userEvent.setup();
    render(<Harness autoFocusField />);
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(screen.getByLabelText("Name")).toHaveFocus();
  });

  it("closes from its close button and toggles width", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Open" }));
    await user.click(screen.getByRole("button", { name: "Widen" }));
    expect(screen.getByRole("button", { name: "Narrow" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("complementary")).toHaveClass("side-panel--wide");
    await user.click(screen.getByRole("button", { name: "Close Agent settings" }));
    expect(screen.queryByRole("complementary")).toBeNull();
  });
});
