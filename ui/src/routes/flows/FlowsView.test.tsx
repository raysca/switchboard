import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import type { FlowDTO, ApiClient, SessionDTO } from "../../api/client";
import { ApiError } from "../../api/client";
import { FlowsView } from "./FlowsView";

const flow = (over: Partial<FlowDTO> = {}, config: Record<string, unknown> = {}): FlowDTO => ({
  id: "a1",
  name: "Front desk",
  description: "Answers the main line.",
  enabled: true,
  archived: false,
  current_version: { id: "v2", number: 2, schema_version: 1, config, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
  ...over,
});
const graph = { start: "a", steps: [{ id: "a" }, { id: "b" }, { id: "c" }, { id: "d" }], routes: [{ id: "e1" }, { id: "e2" }, { id: "e3" }] };
const session = (over: Partial<SessionDTO>): SessionDTO => ({
  draft: false,
  caller: "",
  end_reason: "",
  id: "s",
  flow_id: "a1",
  flow_version_id: "v2",
  config_hash: "h",
  started_at: "",
  status: "active",
  turn_count: 0,
  safe_summary: "",
  ...over,
});

function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname + l.search}</p>;
}
function setup(client: Partial<ApiClient>, key: string | null = "k") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const full = { getFlows: vi.fn().mockResolvedValue([flow({}, graph)]), getSessions: vi.fn().mockResolvedValue([]), getProviders: vi.fn().mockResolvedValue([]), ...client } as unknown as ApiClient;
  render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/flows"]}>
        <Routes>
          <Route path="/flows" element={<FlowsView client={full} keyValue={key} />} />
          <Route path="*" element={<Where />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client: full, queryClient, user: userEvent.setup() };
}

describe("FlowsView", () => {
  it("asks for a management key first", () => {
    setup({}, null);
    expect(screen.getByText(/Enter a management key/)).toBeVisible();
  });

  it("lists flows with status, version, figures and live calls", async () => {
    setup({
      getSessions: vi.fn().mockResolvedValue([session({ id: "1" }), session({ id: "2", flow_version_id: "v1" })]),
      getFlows: vi.fn().mockResolvedValue([
        flow({}, graph),
        flow({ id: "a2", name: "Appointments", enabled: false, readiness: { state: "not_ready", reasons: ["no_provider"] } }),
      ]),
    });
    const row = (await screen.findByRole("link", { name: "Front desk" })).closest("li") as HTMLElement;
    expect(within(row).getByText("v2")).toBeVisible();
    expect(within(row).getByText("Ready")).toBeVisible();
    expect(within(row).getByText("4 steps · 3 routes · 0 tools")).toBeVisible();
    await waitFor(() => expect(within(row).getByText("2 calls live, 1 on an earlier version")).toBeVisible());
    expect(within(row).getByRole("link", { name: "2 calls live, 1 on an earlier version" })).toHaveAttribute("href", "/sessions?flow=a1&outcome=live");
    const second = screen.getByRole("link", { name: "Appointments" }).closest("li") as HTMLElement;
    expect(within(second).getByText("Disabled")).toBeVisible();
    expect(within(second).getByText("1 step · 0 routes · 0 tools")).toBeVisible();
    expect(screen.getByRole("heading", { name: "Flows", level: 1 })).toBeVisible();
    // A disabled flow is not "not ready": it is switched off.
    expect(screen.getByText("1 enabled · 0 not ready · 2 calls in progress")).toBeVisible();
  });

  it("keeps listing flows when the live-calls query fails", async () => {
    setup({ getSessions: vi.fn().mockRejectedValue(new ApiError("boom", "internal", 500, "r")) });
    const row = (await screen.findByRole("link", { name: "Front desk" })).closest("li") as HTMLElement;
    expect(within(row).getByText("Ready")).toBeVisible();
    expect(within(row).queryByText(/calls? live/)).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("shows why a flow is not ready, and offers Test only to a ready one", async () => {
    setup({
      getFlows: vi.fn().mockResolvedValue([
        flow(),
        flow({ id: "a2", name: "Broken", readiness: { state: "not_ready", reasons: ["missing_configuration"] } }),
      ]),
    });
    const ready = (await screen.findByRole("link", { name: "Front desk" })).closest("li") as HTMLElement;
    expect(within(ready).getByRole("link", { name: "Test Front desk" })).toHaveAttribute("href", "/flows/a1?panel=test");
    const broken = screen.getByRole("link", { name: "Broken" }).closest("li") as HTMLElement;
    expect(within(broken).getByText("missing_configuration")).toBeVisible();
    expect(within(broken).queryByRole("link", { name: /^Test/ })).toBeNull();
    expect(screen.getByText("2 enabled · 1 not ready")).toBeVisible();
  });

  it("toggles Enabled by sending only enabled, never a config", async () => {
    const updateFlow = vi.fn().mockResolvedValue(flow({ enabled: false }));
    const { user } = setup({ updateFlow });
    await user.click(await screen.findByRole("checkbox", { name: "Front desk enabled" }));
    expect(updateFlow).toHaveBeenCalledWith("a1", { enabled: false });
    expect(Object.keys(updateFlow.mock.calls[0][1])).toEqual(["enabled"]);
  });

  it("shows a failed toggle instead of swallowing it", async () => {
    const { user } = setup({ updateFlow: vi.fn().mockRejectedValue(new ApiError("nope", "conflict", 409, "r")) });
    await user.click(await screen.findByRole("checkbox", { name: "Front desk enabled" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("nope");
  });

  it("filters by name", async () => {
    const { user } = setup({ getFlows: vi.fn().mockResolvedValue([flow(), flow({ id: "a2", name: "Appointments" })]) });
    await screen.findByRole("link", { name: "Front desk" });
    await user.type(screen.getByLabelText("Filter flows"), "appoint");
    expect(screen.queryByRole("link", { name: "Front desk" })).toBeNull();
    expect(screen.getByRole("link", { name: "Appointments" })).toBeVisible();
    await user.clear(screen.getByLabelText("Filter flows"));
    await user.type(screen.getByLabelText("Filter flows"), "zzz");
    expect(screen.getByText("No flow matches “zzz”.")).toBeVisible();
  });

  it("copies a flow from its row and stays on the list", async () => {
    const duplicateFlow = vi.fn().mockResolvedValue(flow({ id: "a9", name: "Front desk copy" }));
    const { user } = setup({ duplicateFlow });
    await user.click(await screen.findByRole("button", { name: "Duplicate Front desk" }));
    expect(duplicateFlow).toHaveBeenCalledWith("a1", undefined);
    expect(screen.queryByTestId("where")).toBeNull();
  });

  it("archives only after confirmation, and a failure stays in the dialog", async () => {
    const archiveFlow = vi.fn().mockRejectedValueOnce(new ApiError("busy", "conflict", 409, "r")).mockResolvedValue({ archived: true });
    const { user } = setup({ archiveFlow });
    await user.click(await screen.findByRole("button", { name: "Archive Front desk" }));
    expect(archiveFlow).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Archive flow" }));
    expect(await within(screen.getByRole("dialog")).findByRole("alert")).toHaveTextContent("busy");
    await user.click(screen.getByRole("button", { name: "Archive flow" }));
    await waitFor(() => expect(archiveFlow).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("opens the editor on a blank flow, carrying a name with spaces and &", async () => {
    const { user } = setup({});
    await user.click(await screen.findByRole("button", { name: "New flow" }));
    await user.type(screen.getByLabelText("Name"), "R&D line ñ");
    await user.click(screen.getByRole("button", { name: "Create and open editor" }));
    expect(screen.getByTestId("where")).toHaveTextContent("/flows/new?name=R%26D%20line%20%C3%B1");
  });

  it("copies from the dialog and opens the copy", async () => {
    const duplicateFlow = vi.fn().mockResolvedValue(flow({ id: "a9", name: "Night line" }));
    const { user } = setup({ duplicateFlow });
    await user.click(await screen.findByRole("button", { name: "New flow" }));
    await user.click(screen.getByRole("radio", { name: /Copy an existing flow/ }));
    await user.type(screen.getByLabelText("Name"), "Night line");
    await user.click(screen.getByRole("button", { name: "Copy and open editor" }));
    expect(duplicateFlow).toHaveBeenCalledWith("a1", { name: "Night line" });
    expect(await screen.findByTestId("where")).toHaveTextContent("/flows/a9");
  });

  it("teaches the vocabulary when there are no flows yet", async () => {
    setup({ getFlows: vi.fn().mockResolvedValue([]) });
    expect(await screen.findByRole("heading", { name: "No flows yet." })).toBeVisible();
    for (const term of ["Flow", "Steps", "Routes", "Flow settings"]) expect(screen.getByText(term, { selector: "dt" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Use guided setup" })).toHaveAttribute("href", "/onboarding");
    // One label for the action in both states, so it is always "New flow".
    expect(screen.getByRole("button", { name: "New flow" })).toBeVisible();
  });

  it("hides archived flows, and says when the list was cut at the API's page size", async () => {
    const many = Array.from({ length: 100 }, (_, i) => flow({ id: `f${i}`, name: `Flow ${i}` }));
    many[0] = flow({ id: "f0", name: "Gone", archived: true });
    setup({ getFlows: vi.fn().mockResolvedValue(many) });
    expect(await screen.findByText("Showing the first 100 flows.")).toBeVisible();
    expect(screen.queryByRole("link", { name: "Gone" })).toBeNull();
  });

  it("names each missing variable once, beside the reasons", async () => {
    setup({
      getFlows: vi.fn().mockResolvedValue([
        flow({ readiness: { state: "not_ready", reasons: ["missing_credential_llm"], missing_env: ["XAI_API_KEY", "XAI_API_KEY"] } }),
        flow({ id: "a2", name: "Off", enabled: false, readiness: { state: "not_ready", reasons: ["disabled"], missing_env: ["OFF_KEY"] } }),
      ]),
    });
    const row = (await screen.findByRole("link", { name: "Front desk" })).closest("li") as HTMLElement;
    expect(within(row).getByText("missing_credential_llm")).toBeVisible();
    expect(within(row).getAllByText("XAI_API_KEY")).toHaveLength(1);
    expect(within(row).getByText("XAI_API_KEY").closest(".reason")).toHaveTextContent("Set XAI_API_KEY");
    expect(screen.queryByText("OFF_KEY")).toBeNull();
  });

  it("shows a load failure with a retry", async () => {
    setup({ getFlows: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    expect(await screen.findByRole("alert")).toHaveTextContent("down");
    expect(screen.getByRole("button", { name: "Try again" })).toBeVisible();
  });

  it("hands the copy to the editor's cache before opening it, so it never flashes as missing", async () => {
    const copy = flow({ id: "a9", name: "Night line" });
    const { user, queryClient } = setup({ duplicateFlow: vi.fn().mockResolvedValue(copy) });
    await user.click(await screen.findByRole("button", { name: "New flow" }));
    await user.click(screen.getByRole("radio", { name: /Copy an existing flow/ }));
    await user.type(screen.getByLabelText("Name"), "Night line");
    await user.click(screen.getByRole("button", { name: "Copy and open editor" }));
    await screen.findByTestId("where");
    expect(queryClient.getQueryData(["flows", "a9"])).toEqual(copy);
  });

  it("keeps the list when a background refresh fails", async () => {
    const getFlows = vi.fn().mockResolvedValueOnce([flow({}, graph)]).mockRejectedValue(new ApiError("down", "internal", 500, "r"));
    const { user } = setup({ getFlows, updateFlow: vi.fn().mockResolvedValue(flow({ enabled: false })) });
    await user.click(await screen.findByRole("checkbox", { name: "Front desk enabled" }));
    await waitFor(() => expect(getFlows).toHaveBeenCalledTimes(2));
    expect(screen.getByRole("link", { name: "Front desk" })).toBeVisible();
    expect(screen.queryByText("Failed to load flows")).toBeNull();
  });

  it("does not repeat 'disabled' as a reason chip for a flow that is switched off", async () => {
    setup({ getFlows: vi.fn().mockResolvedValue([flow({ enabled: false, readiness: { state: "not_ready", reasons: ["disabled"] } })]) });
    const row = (await screen.findByRole("link", { name: "Front desk" })).closest("li") as HTMLElement;
    expect(within(row).getByText("Disabled")).toBeVisible();
    expect(within(row).queryByText("disabled")).toBeNull();
  });

  it("puts the summary beside the heading, with the filter and New flow in the same header", async () => {
    setup({});
    const head = (await screen.findByRole("heading", { name: "Flows", level: 1 })).closest("header") as HTMLElement;
    expect(await within(head).findByText("1 enabled · 0 not ready")).toBeVisible();
    expect(within(head).getByLabelText("Filter flows")).toBeVisible();
    expect(within(head).getByRole("button", { name: "New flow" })).toBeVisible();
  });

  it("names the providers that are ready in the empty state", async () => {
    const p = (id: string, name: string, detected: boolean) => ({ id, display_name: name, capabilities: [], detected, env: [], services: {} });
    setup({ getFlows: vi.fn().mockResolvedValue([]), getProviders: vi.fn().mockResolvedValue([p("google", "Google", true), p("xai", "xAI", false), p("cartesia", "Cartesia", true)]) });
    expect(await screen.findByText("Google and Cartesia are ready to use.")).toBeVisible();
  });

  it("says when no provider is set up, with a way to see them", async () => {
    setup({ getFlows: vi.fn().mockResolvedValue([]) });
    expect(await screen.findByText(/No provider is set up yet\./)).toBeVisible();
    expect(screen.getByRole("link", { name: "See providers" })).toHaveAttribute("href", "/providers");
  });

  it("leaves the provider line out when providers cannot be loaded", async () => {
    setup({ getFlows: vi.fn().mockResolvedValue([]), getProviders: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    await screen.findByRole("heading", { name: "No flows yet." });
    expect(screen.queryByText(/ready to use|No provider is set up/)).toBeNull();
  });
});
