import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "../api/client";
import { EmptyState, ErrorState, LoadingState, Status } from "./primitives";

describe("primitives keep their semantics", () => {
  it.each(["neutral", "good", "bad"] as const)("Status %s is a status region with its text", (tone) => {
    render(<Status tone={tone}>Ready</Status>);
    expect(screen.getByRole("status")).toHaveTextContent("Ready");
    expect(screen.getByRole("status")).toHaveClass(`status--${tone}`);
  });

  it("ErrorState is an alert with the message, request id and a retry", async () => {
    const retry = vi.fn();
    render(<ErrorState error={new ApiError("Nope", "bad", 500, "req-1")} onRetry={retry} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Nope");
    expect(screen.getByText(/req-1/)).toBeVisible();
    await userEvent.setup().click(screen.getByRole("button", { name: "Try again" }));
    expect(retry).toHaveBeenCalled();
  });

  it("EmptyState has a heading and a paragraph", () => {
    render(<EmptyState title="No agents yet">Create one.</EmptyState>);
    expect(screen.getByRole("heading", { name: "No agents yet" })).toBeVisible();
    expect(screen.getByText("Create one.")).toBeVisible();
  });
});

describe("ErrorState details", () => {
  it("says live calls are not affected only when asked to", () => {
    const err = new ApiError("storage unavailable", "storage_unavailable", 503, "req_01");
    const { rerender } = render(<ErrorState error={err} />);
    expect(screen.queryByText(/Live calls are not affected/)).toBeNull();
    rerender(<ErrorState error={err} reassure />);
    expect(screen.getByText("Live calls are not affected. This only stops the console from reading.")).toBeVisible();
  });

  it("lists the status, code and request id, and copies them without anything else", async () => {
    const user = userEvent.setup(); // setup() installs its own clipboard stub, so override after it
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    render(<ErrorState error={new ApiError("storage unavailable", "storage_unavailable", 503, "req_01")} onRetry={vi.fn()} />);
    expect(screen.getByText("503 storage_unavailable")).toBeVisible();
    expect(screen.getByText("req_01")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Copy details" }));
    expect(writeText).toHaveBeenCalledWith("status 503 storage_unavailable\nrequest req_01\nstorage unavailable");
    expect(await screen.findByText("Copied")).toBeVisible();
  });

  it("says so when the clipboard refuses", async () => {
    const user = userEvent.setup();
    Object.defineProperty(navigator, "clipboard", { value: { writeText: vi.fn().mockRejectedValue(new Error("denied")) }, configurable: true });
    render(<ErrorState error={new ApiError("x", "internal", 500, "r1")} />);
    await user.click(screen.getByRole("button", { name: "Copy details" }));
    expect(await screen.findByText("Could not copy. Select the details and copy them yourself.")).toBeVisible();
  });

  it("shows no details block for an error that carries none", () => {
    render(<ErrorState error={new ApiError("Something broke", "", 0, "")} />);
    expect(screen.queryByRole("button", { name: "Copy details" })).toBeNull();
  });
});

describe("LoadingState skeleton", () => {
  it("keeps the label for assistive technology and draws placeholder rows", () => {
    const { container } = render(<LoadingState label="Loading calls" skeleton={4} />);
    expect(screen.getByRole("status")).toHaveTextContent("Loading calls…");
    expect(container.querySelectorAll(".skeleton__row")).toHaveLength(4);
    expect(container.querySelector("[aria-busy='true']")).not.toBeNull();
  });
  it("is the plain text line without a skeleton", () => {
    const { container } = render(<LoadingState label="Loading" />);
    expect(container.querySelector(".skeleton__row")).toBeNull();
  });
});
