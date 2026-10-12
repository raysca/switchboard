import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { PublishedBanner } from "./PublishedBanner";

const show = (olderCalls: number, onDismiss = vi.fn()) =>
  render(
    <MemoryRouter>
      <PublishedBanner flowId="f1" flowName="Front desk" version={8} olderCalls={olderCalls} onDismiss={onDismiss} />
    </MemoryRouter>,
  );

describe("PublishedBanner", () => {
  it("says the version is live and that new calls get it", () => {
    show(0);
    const banner = screen.getByRole("status", { name: "Publish confirmation" });
    expect(banner).toHaveTextContent("Published v8. Front desk now answers new calls with it.");
    expect(banner).not.toHaveTextContent("finishing");
    expect(screen.getByRole("link", { name: "Watch calls" })).toHaveAttribute("href", "/sessions?flow=f1&outcome=live");
  });

  it("counts the calls still finishing on an earlier version", () => {
    show(1);
    expect(screen.getByRole("status", { name: "Publish confirmation" })).toHaveTextContent("1 call is finishing on an earlier version");
    show(3);
    expect(screen.getAllByRole("status", { name: "Publish confirmation" })[1]).toHaveTextContent("3 calls are finishing on an earlier version");
  });

  it("dismisses", async () => {
    const onDismiss = vi.fn();
    show(0, onDismiss);
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(onDismiss).toHaveBeenCalledOnce();
  });
});
