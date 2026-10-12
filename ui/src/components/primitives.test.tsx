import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "../api/client";
import { EmptyState, ErrorState, Status } from "./primitives";

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
