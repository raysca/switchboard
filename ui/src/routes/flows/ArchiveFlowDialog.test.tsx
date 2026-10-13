import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ArchiveFlowDialog } from "./ArchiveFlowDialog";

describe("ArchiveFlowDialog", () => {
  it("says what archiving does and confirms or keeps the flow", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(<ArchiveFlowDialog name="Front desk" pending={false} error={null} onConfirm={onConfirm} onCancel={onCancel} />);
    const dialog = screen.getByRole("dialog", { name: "Archive Front desk?" });
    expect(dialog).toHaveTextContent("stops taking calls");
    expect(dialog).toHaveTextContent("version history and past calls are kept");
    await user.click(screen.getByRole("button", { name: "Keep flow" }));
    await user.click(screen.getByRole("button", { name: "Archive flow" }));
    expect(onCancel).toHaveBeenCalledOnce();
    expect(onConfirm).toHaveBeenCalledOnce();
  });

  it("shows a failure and locks the button while archiving", () => {
    render(<ArchiveFlowDialog name="X" pending error="Archive failed" onConfirm={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Archive failed");
    expect(screen.getByRole("button", { name: "Archiving…" })).toBeDisabled();
  });
});
