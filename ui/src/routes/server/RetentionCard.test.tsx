import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RetentionCard } from "./RetentionCard";
import type { ApiClient, RetentionStatus } from "../../api/client";

const status = (over: Partial<RetentionStatus> = {}): RetentionStatus => ({
  enabled: true,
  interval_seconds: 3600,
  content_retention_days: 7,
  metadata_retention_days: 30,
  batch_size: 500,
  recording_retention_days: 3,
  last_run: { started_at: new Date(Date.now() - 5 * 60_000).toISOString(), duration_ms: 40, turns_purged: 12, sessions_deleted: 4, turns_deleted: 9, tool_calls_deleted: 2, spans_deleted: 31, events_deleted: 70 },
  ...over,
});

function setup(getRetention: ApiClient["getRetention"]) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RetentionCard client={{ getRetention } as unknown as ApiClient} />
    </QueryClientProvider>,
  );
  return userEvent.setup();
}

describe("RetentionCard", () => {
  it("shows the lifetimes in force and what the last run did", async () => {
    setup(vi.fn().mockResolvedValue(status()));
    const table = await screen.findByRole("table", { name: "Data retention" });
    expect(table).toHaveTextContent("Transcripts kept7 days");
    expect(table).toHaveTextContent("Call records kept30 days");
    expect(table).toHaveTextContent("Recordings kept3 days");
    expect(table).toHaveTextContent("12 turns blanked, 4 calls deleted");
  });

  it("says purging is off and offers no preview", async () => {
    setup(vi.fn().mockResolvedValue(status({ enabled: false, last_run: null })));
    expect(await screen.findByText(/stay until someone deletes them/)).toBeVisible();
    expect(screen.getByRole("table")).toHaveTextContent("Automatic purgingOff");
    expect(screen.queryByRole("button", { name: /Preview/ })).toBeNull();
  });

  it("names a failed run and a run that has not happened yet", async () => {
    setup(vi.fn().mockResolvedValue(status({ last_run: { started_at: new Date().toISOString(), duration_ms: 1, error: "database is locked", turns_purged: 0, sessions_deleted: 0, turns_deleted: 0, tool_calls_deleted: 0, spans_deleted: 0, events_deleted: 0 } })));
    expect(await screen.findByText(/database is locked/)).toBeVisible();
  });

  it("previews the next run without changing anything", async () => {
    const getRetention = vi.fn().mockImplementation(async (dry?: boolean) => status(dry ? { dry_run: { content_sessions: 3, content_turns: 5, metadata_sessions: 2, metadata_turns: 4, metadata_tool_calls: 1, metadata_spans: 8, metadata_events: 9 } } : {}));
    const user = setup(getRetention);
    await user.click(await screen.findByRole("button", { name: "Preview the next run" }));
    expect(await screen.findByRole("status")).toHaveTextContent("would blank 5 turns and delete 2 calls (4 turns, 1 tool call");
    expect(getRetention).toHaveBeenLastCalledWith(true);
  });

  it("stays quiet but honest when the status cannot be read", async () => {
    setup(vi.fn().mockRejectedValue(new Error("nope")));
    expect(await screen.findByRole("note")).toHaveTextContent("unavailable");
  });
});
