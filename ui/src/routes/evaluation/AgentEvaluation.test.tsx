import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { AgentEvaluation } from "./AgentEvaluation";
import type { ApiClient } from "../../api/client";

describe("AgentEvaluation", () => {
  it("shows the agent, a tablist with keyboard support and keeps the tab in the address", async () => {
    const client = {
      getAgent: vi.fn().mockResolvedValue({ id: "a1", name: "Support", current_version: { number: 2 } }),
      getEvalHistory: vi.fn().mockResolvedValue([]),
      listEvalCases: vi.fn().mockResolvedValue([]),
      listEvalRuns: vi.fn().mockResolvedValue([]),
      getProviders: vi.fn().mockResolvedValue([]),
      getEvalSuite: vi.fn().mockResolvedValue({ runs: 1, min_pass: 1, pass_threshold: 1, judge: null }),
      getEvalScorers: vi.fn().mockResolvedValue([]),
    } as unknown as ApiClient;
    render(
      <QueryClientProvider client={new QueryClient()}>
        <MemoryRouter initialEntries={["/evaluation/a1"]}>
          <Routes>
            <Route path="/evaluation/:agentId" element={<AgentEvaluation client={client} keyValue="k" />} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(await screen.findByRole("heading", { level: 1, name: "Support" })).toHaveAttribute("tabindex", "-1");
    const suite = screen.getByRole("tab", { name: "Test suite" });
    expect(suite).toHaveAttribute("aria-selected", "true");
    suite.focus();
    await userEvent.setup().keyboard("{ArrowRight}");
    const history = screen.getByRole("tab", { name: "History" });
    expect(history).toHaveAttribute("aria-selected", "true");
    expect(history).toHaveFocus();
    expect(await screen.findByText("No finished suite runs yet")).toBeVisible();
  });
});
