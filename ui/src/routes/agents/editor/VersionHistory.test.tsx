import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AgentVersionSummary } from "../../../api/client";
import { VersionHistory } from "./VersionHistory";

const v = (number: number, current = false, tool_count = 0): AgentVersionSummary => ({ number, config_hash: `h${number}`, created_at: "2026-10-01T10:00:00Z", tool_count, current });
const three = [v(3, true, 2), v(2, false, 1), v(1)];

function setup(load = vi.fn().mockResolvedValue(three), restore = vi.fn().mockResolvedValue(undefined)) {
  render(<VersionHistory load={load} restore={restore} />);
  return { load, restore, user: userEvent.setup() };
}

describe("VersionHistory", () => {
  it("loads nothing until it is opened, then lists the versions newest first with their tools", async () => {
    const { load, user } = setup();
    expect(load).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Version history" }));
    const items = await screen.findAllByRole("listitem");
    expect(items).toHaveLength(3);
    expect(items[0]).toHaveTextContent("v3 · current");
    expect(items[0]).toHaveTextContent("2 tools");
    expect(items[1]).toHaveTextContent("1 tool");
    expect(within(items[0]).queryByRole("button")).toBeNull(); // the current version has nothing to restore
    expect(screen.getByRole("button", { name: "Hide version history" })).toHaveAttribute("aria-expanded", "true");
  });

  it("asks before publishing an old version again, naming the version it will become", async () => {
    const { restore, user } = setup();
    await user.click(screen.getByRole("button", { name: "Version history" }));
    await user.click(await screen.findByRole("button", { name: "Restore v1…" }));
    expect(screen.getByRole("group", { name: "Confirm restore" })).toHaveTextContent("Publish v1's settings as v4?");
    expect(restore).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Restore v1" }));
    expect(restore).toHaveBeenCalledWith(1);
  });

  it("backs out without restoring", async () => {
    const { restore, user } = setup();
    await user.click(screen.getByRole("button", { name: "Version history" }));
    await user.click(await screen.findByRole("button", { name: "Restore v2…" }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(restore).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Restore v2…" })).toBeVisible();
  });

  it("shows why a restore was refused", async () => {
    const { user } = setup(undefined, vi.fn().mockRejectedValue(new Error("invalid tool selection: tool lookup schema changed")));
    await user.click(screen.getByRole("button", { name: "Version history" }));
    await user.click(await screen.findByRole("button", { name: "Restore v2…" }));
    await user.click(screen.getByRole("button", { name: "Restore v2" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("tool lookup schema changed");
    expect(screen.getByRole("button", { name: "Restore v2…" })).toBeVisible();
  });

  it("says when the history could not be loaded", async () => {
    const { user } = setup(vi.fn().mockRejectedValue(new Error("Network down")));
    await user.click(screen.getByRole("button", { name: "Version history" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Network down");
  });
});
