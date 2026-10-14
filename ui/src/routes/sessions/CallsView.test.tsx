import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import type { FlowDTO, ApiClient, SessionDTO } from "../../api/client";
import { ApiError } from "../../api/client";
import { CallsView } from "./CallsView";

const flow = (over: Partial<FlowDTO> = {}): FlowDTO => ({
  id: "a1",
  name: "Front desk",
  description: "",
  enabled: true,
  archived: false,
  current_version: { id: "v7", number: 7, schema_version: 1, config: {}, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
  ...over,
});
const call = (over: Partial<SessionDTO> = {}): SessionDTO => ({
  draft: false,
  id: "0192aaaa-0000-7000-8000-000000000001",
  flow_id: "a1",
  flow_version_id: "v7",
  config_hash: "h",
  started_at: "2026-10-12T14:00:00Z",
  ended_at: "2026-10-12T14:04:12Z",
  status: "ended",
  turn_count: 6,
  safe_summary: "",
  ...over,
});

function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname + l.search}</p>;
}
// The live count asks for status=active with limit 100; the list asks for 26.
const listOnly = (rows: SessionDTO[] | ((p: { offset?: number; status?: string; flow_id?: string }) => SessionDTO[]), live: SessionDTO[] = []) =>
  vi.fn().mockImplementation(async (p?: { limit?: number; offset?: number; status?: string; flow_id?: string }) =>
    p?.limit === 100 ? live : typeof rows === "function" ? rows(p ?? {}) : rows,
  );

function setup(client: Partial<ApiClient>, path = "/sessions", key: string | null = "k") {
  const full = {
    getSessions: listOnly([call()]),
    getFlows: vi.fn().mockResolvedValue([flow(), flow({ id: "a2", name: "Appointments" })]),
    ...client,
  } as unknown as ApiClient;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route
            path="/sessions"
            element={
              <>
                <CallsView client={full} keyValue={key} />
                <Where />
              </>
            }
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client: full, user: userEvent.setup() };
}

describe("CallsView", () => {
  it("asks for a management key first", () => {
    setup({}, "/sessions", null);
    expect(screen.getByText(/Enter a management key/)).toBeVisible();
  });

  it("lists calls with flow, version, duration, turns and outcome", async () => {
    setup({
      getSessions: listOnly([
        call(),
        call({ id: "0192aaaa-0000-7000-8000-000000000002", status: "active", ended_at: null, flow_version_id: "v6", turn_count: 2 }),
      ]),
    });
    const rows = await screen.findAllByRole("row");
    expect(rows).toHaveLength(3); // header + 2
    const ended = within(rows[1]);
    expect(ended.getByText("Front desk")).toBeVisible();
    expect(ended.getByText("v7")).toBeVisible();
    expect(ended.getByText("04:12")).toBeVisible();
    expect(ended.getByText("6")).toBeVisible();
    expect(ended.getByText("Ended")).toBeVisible();
    const live = within(rows[2]);
    expect(live.getByText("earlier version")).toBeVisible();
    expect(live.getByText("Live")).toBeVisible();
    expect(rows[2].querySelector('.lamp[data-tone="warn"]')).not.toBeNull();
    expect(rows[1].querySelector('.lamp[data-tone="warn"]')).toBeNull();
    expect(within(rows[1]).getByRole("link")).toHaveAttribute("href", "/sessions/0192aaaa-0000-7000-8000-000000000001");
  });

  it("marks a draft test call with a Draft chip, and no other", async () => {
    setup({ getSessions: listOnly([call({ draft: true }), call({ id: "0192aaaa-0000-7000-8000-000000000002" })]) });
    const rows = await screen.findAllByRole("row");
    expect(within(rows[1]).getByText("Draft")).toBeVisible();
    expect(within(rows[2]).queryByText("Draft")).toBeNull();
  });

  it("shows a status the server might add as it is, instead of hiding the call", async () => {
    setup({ getSessions: listOnly([call({ status: "weird" })]) });
    expect(await screen.findByText("weird")).toBeVisible();
  });

  it("names the number of live calls from the live query, and leaves it out when that query fails", async () => {
    setup({ getSessions: listOnly([call()], [call({ status: "active" }), call({ id: "x", status: "active" })]) });
    expect(await screen.findByText("2 live now")).toBeVisible();
    expect(screen.getByRole("button", { name: "Live · 2" })).toBeVisible();
  });

  it("does not break when the live-call count fails", async () => {
    const getSessions = vi.fn().mockImplementation(async (p?: { limit?: number }) => {
      if (p?.limit === 100) throw new ApiError("slow", "internal", 500, "r");
      return [call()];
    });
    setup({ getSessions });
    expect(within(await screen.findByRole("table")).getByText("Front desk")).toBeVisible();
    expect(screen.queryByText(/live now/)).toBeNull();
    expect(screen.getByRole("button", { name: "Live" })).toBeVisible();
  });

  it("filters by flow and keeps the choice in the URL", async () => {
    const { client, user } = setup({});
    await screen.findByRole("table");
    await user.selectOptions(screen.getByLabelText("Flow"), "a2");
    await waitFor(() => expect(client.getSessions).toHaveBeenLastCalledWith(expect.objectContaining({ flow_id: "a2", limit: 26, offset: 0 })));
    expect(screen.getByTestId("where")).toHaveTextContent("/sessions?flow=a2");
  });

  it("filters Live and Ended with pressed buttons", async () => {
    const { client, user } = setup({});
    await screen.findByRole("table");
    expect(screen.getByRole("button", { name: "All" })).toHaveAttribute("aria-pressed", "true");
    await user.click(screen.getByRole("button", { name: /^Live/ }));
    expect(screen.getByRole("button", { name: /^Live/ })).toHaveAttribute("aria-pressed", "true");
    await waitFor(() => expect(client.getSessions).toHaveBeenLastCalledWith(expect.objectContaining({ status: "active", limit: 26 })));
    await user.click(screen.getByRole("button", { name: "Ended" }));
    await waitFor(() => expect(client.getSessions).toHaveBeenLastCalledWith(expect.objectContaining({ status: "ended" })));
    expect(screen.getByTestId("where")).toHaveTextContent("outcome=ended");
  });

  it("starts from the filters in the URL, and ignores a garbage outcome", async () => {
    const { client } = setup({}, "/sessions?flow=a1&outcome=live");
    await waitFor(() => expect(client.getSessions).toHaveBeenCalledWith(expect.objectContaining({ flow_id: "a1", status: "active", limit: 26 })));
    expect(screen.getByRole("button", { name: /^Live/ })).toHaveAttribute("aria-pressed", "true");
  });

  it("treats an unknown outcome as All", async () => {
    const { client } = setup({}, "/sessions?outcome=bogus");
    await screen.findByRole("table");
    expect(client.getSessions).toHaveBeenCalledWith({ limit: 26, offset: 0, status: undefined, flow_id: undefined });
    expect(screen.getByRole("button", { name: "All" })).toHaveAttribute("aria-pressed", "true");
  });

  it("pages: 26 rows offer Older but show 25, and filters go back to the first page", async () => {
    const many = Array.from({ length: 26 }, (_, i) => call({ id: `0192aaaa-0000-7000-8000-0000000001${String(i).padStart(2, "0")}` }));
    const getSessions = listOnly((p) => (p.offset === 25 ? [call({ id: "0192aaaa-0000-7000-8000-000000000999" }), call({ id: "0192aaaa-0000-7000-8000-000000000998" })] : many));
    const { client, user } = setup({ getSessions });
    await screen.findAllByRole("row");
    expect(screen.getAllByRole("row")).toHaveLength(26); // header + 25
    expect(screen.getByRole("button", { name: "Newer" })).toBeDisabled();
    expect(screen.getByText("Calls 1–25")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Older" }));
    await waitFor(() => expect(client.getSessions).toHaveBeenCalledWith(expect.objectContaining({ offset: 25, limit: 26 })));
    expect(await screen.findByText("Calls 26–27")).toBeVisible();
    expect(screen.getByRole("button", { name: "Older" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Ended" }));
    await waitFor(() => expect(client.getSessions).toHaveBeenLastCalledWith(expect.objectContaining({ offset: 0, status: "ended" })));
  });

  it("does not offer Older for exactly one full page", async () => {
    const full = Array.from({ length: 25 }, (_, i) => call({ id: `0192aaaa-0000-7000-8000-0000000002${String(i).padStart(2, "0")}` }));
    setup({ getSessions: listOnly(full) });
    await screen.findAllByRole("row");
    expect(screen.getByRole("button", { name: "Older" })).toBeDisabled();
  });

  it("says there are no calls yet, with the way to make one", async () => {
    setup({ getSessions: listOnly([]) });
    expect(await screen.findByRole("heading", { name: "No calls yet." })).toBeVisible();
    expect(screen.getByRole("link", { name: "Go to Flows" })).toHaveAttribute("href", "/flows");
  });

  it("says nothing matches a filter, and clears it", async () => {
    const { user } = setup({ getSessions: listOnly([]) }, "/sessions?outcome=ended");
    expect(await screen.findByRole("heading", { name: "No calls match these filters." })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(screen.getByTestId("where")).toHaveTextContent(/^\/sessions$/);
  });

  it("shows a load failure with a retry", async () => {
    setup({ getSessions: vi.fn().mockImplementation(async (p?: { limit?: number }) => { if (p?.limit === 100) return []; throw new ApiError("down", "internal", 500, "r"); }) });
    expect(await screen.findByRole("alert")).toHaveTextContent("down");
    expect(screen.getByRole("button", { name: "Try again" })).toBeVisible();
  });

  it("lists calls with an unknown flow when the flow list fails", async () => {
    setup({ getFlows: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    expect(await screen.findByText("Unknown flow")).toBeVisible();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps a flow filter from the URL even when that flow is not in the list", async () => {
    setup({}, "/sessions?flow=gone");
    await screen.findByRole("table");
    expect(screen.getByLabelText("Flow")).toHaveValue("gone");
  });

  it("keeps the page in the URL, so Back and links land on the same page", async () => {
    const many = Array.from({ length: 26 }, (_, i) => call({ id: `0192aaaa-0000-7000-8000-0000000003${String(i).padStart(2, "0")}` }));
    const { client, user } = setup({ getSessions: listOnly(many) }, "/sessions?outcome=ended&page=2");
    await waitFor(() => expect(client.getSessions).toHaveBeenCalledWith(expect.objectContaining({ offset: 25, status: "ended" })));
    expect(await screen.findByText("Calls 26–50")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Newer" }));
    expect(screen.getByTestId("where")).toHaveTextContent("/sessions?outcome=ended");
    expect(screen.getByTestId("where")).not.toHaveTextContent("page=");
  });

  it("ignores a garbage page number", async () => {
    const { client } = setup({}, "/sessions?page=-3");
    await screen.findByRole("table");
    expect(client.getSessions).toHaveBeenCalledWith(expect.objectContaining({ offset: 0 }));
  });

  it("keeps Newer when a later page turns out to be empty, instead of stranding the user", async () => {
    const { user } = setup({ getSessions: listOnly((p) => ((p.offset ?? 0) > 0 ? [] : [call()])) }, "/sessions?page=2");
    expect(await screen.findByText("No calls on this page.")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Newer" }));
    expect(await screen.findByRole("table")).toBeVisible();
  });

  it("keeps the table and the pager while the next page loads, so keyboard focus stays", async () => {
    const many = Array.from({ length: 26 }, (_, i) => call({ id: `0192aaaa-0000-7000-8000-0000000004${String(i).padStart(2, "0")}` }));
    const getSessions = vi.fn().mockImplementation((p?: { limit?: number; offset?: number }) =>
      p?.limit === 100 ? Promise.resolve([]) : (p?.offset ?? 0) > 0 ? new Promise(() => {}) : Promise.resolve(many),
    );
    const { user } = setup({ getSessions });
    await screen.findByRole("table");
    const older = screen.getByRole("button", { name: "Older" });
    older.focus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("table")).toBeVisible();
    expect(screen.getByRole("button", { name: "Older" })).toHaveFocus();
  });

  it("counts live calls of the chosen flow only", async () => {
    setup({ getSessions: listOnly([call()], [call({ status: "active" }), call({ id: "x", status: "active", flow_id: "a2" }), call({ id: "y", status: "active", flow_id: "a2" })]) }, "/sessions?flow=a2");
    expect(await screen.findByText("2 live now")).toBeVisible();
    expect(screen.getByRole("button", { name: "Live · 2" })).toBeVisible();
  });

  it("follows the canvas: the live count sits beside the heading, and the figures have their own right-aligned columns", async () => {
    setup({ getSessions: listOnly([call()], [call({ status: "active" })]) });
    await screen.findByRole("table");
    expect(screen.getByRole("heading", { level: 1, name: "Calls" }).parentElement).toHaveTextContent("1 live now");
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Started", "Flow", "Duration", "Turns", "Outcome"]);
    const row = screen.getAllByRole("row")[1];
    expect(within(row).getByText("04:12").closest("td")).toHaveClass("num");
    expect(within(row).getByText("6").closest("td")).toHaveClass("num");
  });
});
