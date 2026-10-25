import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { HistoryTab } from "./HistoryTab";
import type { ApiClient } from "../../api/client";

const sc = (mean: number) => [{ scorer: "expectations", mean, pass: 3, fail: 1, skipped: 0, errors: 0 }];
const rows = [
  { run_id: "r2", version: 2, ended_at: "2026-10-24T11:00:00Z", passed: 4, total: 5, pass_rate: 0.8, scorers: sc(0.8) },
  { run_id: "r1", version: 1, ended_at: "2026-10-24T10:00:00Z", passed: 3, total: 5, pass_rate: 0.6, scorers: sc(0.6) },
];
const view = (h: unknown) =>
  render(
    <QueryClientProvider client={new QueryClient()}>
      <HistoryTab client={{ getEvalHistory: vi.fn().mockResolvedValue(h) } as unknown as ApiClient} agentId="a1" />
    </QueryClientProvider>,
  );

describe("HistoryTab", () => {
  it("shows the empty state", async () => {
    view([]);
    expect(await screen.findByText("No finished suite runs yet")).toBeVisible();
  });

  it("lists runs with per-scorer totals and compares two", async () => {
    view(rows);
    const row = (await screen.findByRole("table", { name: "Finished suite runs" })).querySelectorAll("tbody tr")[0] as HTMLElement;
    expect(within(row).getByText("80% (4 of 5 cases)")).toBeVisible();
    expect(within(row).getByText("mean 0.80: 3 pass, 1 fail, 0 skipped, 0 errors")).toBeVisible();
    // default compare: previous (v1) to newest (v2)
    expect(within(screen.getByRole("table", { name: "Change in mean score per scorer" })).getByText("+0.20")).toBeVisible();
    await userEvent.setup().selectOptions(screen.getByLabelText("From"), "r2");
    expect(within(screen.getByRole("table", { name: "Change in mean score per scorer" })).getByText("+0.00")).toBeVisible();
  });
});
