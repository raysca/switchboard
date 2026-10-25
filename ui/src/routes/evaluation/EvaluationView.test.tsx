import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { EvaluationView } from "./EvaluationView";
import { ApiError, type ApiClient, type EvalFleetDTO } from "../../api/client";

const fleet: EvalFleetDTO = {
  agents: [
    { agent_id: "a1", name: "Support", threshold: 0.9, latest: { run_id: "r1", version: 7, passed: 4, total: 5, pass_rate: 0.8, ended_at: "2026-10-24T10:00:00Z" }, trend: [0.6, 0.8], below_threshold: true },
    { agent_id: "a2", name: "Sales", threshold: 0.5, latest: { run_id: "r2", version: 2, passed: 3, total: 3, pass_rate: 1, ended_at: "2026-10-24T10:00:00Z" }, trend: [1], below_threshold: false },
    { agent_id: "a3", name: "Idle", threshold: 1, trend: [], below_threshold: false },
  ],
  total: 3,
  below_threshold: 1,
};

function renderView(getEvalFleet: ApiClient["getEvalFleet"], keyValue: string | null = "k") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <EvaluationView client={{ getEvalFleet } as unknown as ApiClient} keyValue={keyValue} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("EvaluationView", () => {
  it("asks for a key", () => {
    renderView(vi.fn(), null);
    expect(screen.getByText("Connect to see evaluation results")).toBeVisible();
  });

  it("summarises agents below threshold and marks the row in text", async () => {
    renderView(vi.fn().mockResolvedValue(fleet));
    expect(await screen.findByText("1 agent below its suite threshold")).toHaveAttribute("role", "status");
    const row = screen.getByRole("row", { name: /Support/ });
    expect(within(row).getByText("4 of 5 cases, v7")).toBeVisible();
    expect(within(row).getByText("80%")).toBeVisible();
    expect(within(row).getByText("90%")).toBeVisible();
    expect(within(row).getByText("Below threshold")).toBeVisible();
    expect(within(row).getByText("trend: 60%, 80%")).toBeInTheDocument();
    expect(within(screen.getByRole("row", { name: /Sales/ })).getByText("OK")).toBeVisible();
    expect(within(screen.getByRole("row", { name: /Idle/ })).getByText("No suite run yet")).toBeVisible();
    expect(screen.getByRole("link", { name: "Support" })).toHaveAttribute("href", "/evaluation/a1");
  });

  it("lists agents that never ran, with a hint and a link to their suite", async () => {
    renderView(vi.fn().mockResolvedValue({ agents: [{ ...fleet.agents[2]! }], total: 1, below_threshold: 0 }));
    expect(await screen.findByText(/Open an agent to add test cases/)).toBeVisible();
    expect(screen.getByRole("link", { name: "Idle" })).toHaveAttribute("href", "/evaluation/a3");
    expect(screen.getByText("No suite run yet")).toBeVisible();
    expect(screen.queryByText(/below/)).toBeNull();
  });

  it("shows the empty state only when there are no agents", async () => {
    renderView(vi.fn().mockResolvedValue({ agents: [], total: 0, below_threshold: 0 }));
    expect(await screen.findByText("No agents yet")).toBeVisible();
  });

  it("shows an error with retry", async () => {
    const get = vi.fn().mockRejectedValueOnce(new ApiError("boom", "internal", 500, "req")).mockResolvedValue(fleet);
    renderView(get);
    expect(await screen.findByText("boom")).toBeVisible();
    await userEvent.setup().click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("link", { name: "Support" })).toBeVisible();
  });

  it("pages with limit and offset", async () => {
    const get = vi.fn().mockResolvedValue({ ...fleet, total: 60 });
    renderView(get);
    await screen.findByRole("link", { name: "Support" });
    expect(get).toHaveBeenLastCalledWith(25, 0);
    await userEvent.setup().click(screen.getByRole("button", { name: "Next" }));
    await screen.findByText("Agents 26–28 of 60");
    expect(get).toHaveBeenLastCalledWith(25, 25);
  });
});
