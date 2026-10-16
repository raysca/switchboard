import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { PublishDialog } from "./PublishDialog";

const dialog = (tested?: boolean) =>
  render(
    <PublishDialog flowName="Front desk" version={3} currentVersion={2} firstVersion={false} changes={["Renamed a step"]} problems={[]} pending={false} tested={tested} onConfirm={vi.fn()} onCancel={vi.fn()} />,
  );

describe("PublishDialog test-call check", () => {
  it("warns, without blocking, when the draft was not tested", () => {
    dialog(false);
    expect(screen.getByText(/Not tested with a call since your last change/)).toBeVisible();
    expect(screen.getByRole("button", { name: "Publish v3" })).toBeEnabled();
  });

  it("ticks the check once a call ran on this draft", () => {
    dialog(true);
    expect(screen.getByText("Tested with a call on this draft")).toBeVisible();
  });

  it("says nothing when the flow cannot be tested yet", () => {
    dialog(undefined);
    expect(screen.queryByText(/tested/i)).toBeNull();
  });
});
