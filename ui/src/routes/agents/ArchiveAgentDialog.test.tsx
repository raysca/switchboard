import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ArchiveAgentDialog } from "./ArchiveAgentDialog";

describe("ArchiveAgentDialog", () => {
  it("says what archiving does and confirms or keeps the agent", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(<ArchiveAgentDialog name="Front desk" pending={false} error={null} onConfirm={onConfirm} onCancel={onCancel} />);
    const dialog = screen.getByRole("dialog", { name: "Archive Front desk?" });
    expect(dialog).toHaveTextContent("stops taking calls");
    expect(dialog).toHaveTextContent("version history and past calls are kept");
    await user.click(screen.getByRole("button", { name: "Keep agent" }));
    await user.click(screen.getByRole("button", { name: "Archive agent" }));
    expect(onCancel).toHaveBeenCalledOnce();
    expect(onConfirm).toHaveBeenCalledOnce();
  });

  it("shows a failure and locks the button while archiving", () => {
    render(<ArchiveAgentDialog name="X" pending error="Archive failed" onConfirm={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Archive failed");
    expect(screen.getByRole("button", { name: "Archiving…" })).toBeDisabled();
  });

  it("says how many calls are in progress and which tools stay pinned", () => {
    render(<ArchiveAgentDialog name="Front desk" pending={false} error={null} liveCalls={2} tools={["lookup_order", "get_order"]} onConfirm={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByRole("note")).toHaveTextContent("2 calls are in progress. They finish normally; no new calls start.");
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveTextContent("It uses these tools: lookup_order, get_order.");
    expect(dialog).toHaveTextContent("Archiving does not release them: the agent's current version keeps using them");
  });

  it("adds nothing for an agent with no live calls and no tools", () => {
    render(<ArchiveAgentDialog name="Front desk" pending={false} error={null} liveCalls={0} tools={[]} onConfirm={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.queryByRole("note")).toBeNull();
    expect(screen.getByRole("dialog")).not.toHaveTextContent("It uses");
  });

  it("uses the singular for one call and one tool", () => {
    render(<ArchiveAgentDialog name="X" pending={false} error={null} liveCalls={1} tools={["get_order"]} onConfirm={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByRole("note")).toHaveTextContent("1 call is in progress. It finishes normally");
    expect(screen.getByRole("dialog")).toHaveTextContent("It uses this tool: get_order.");
  });
});
