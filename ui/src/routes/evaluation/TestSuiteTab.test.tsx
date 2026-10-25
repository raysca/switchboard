import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { TestSuiteTab } from "./TestSuiteTab";
import { ApiError, type ApiClient } from "../../api/client";

const kase = { id: "c1", name: "books-a-visit", caller: { script: ["hi"] }, expect: {}, yaml: "name: books-a-visit\n", created_at: "", updated_at: "" };
const run = (over = {}) => ({
  id: "r1", agent_id: "a1", version: 3, draft: false, config_hash: "h", scope: "suite", providers: "offline", runs: 1, min_pass: 1,
  status: "done", triggered_by: "console", passed_cases: 0, total_cases: 1, created_at: "2026-10-24T10:00:00Z", ...over,
});
const detail = {
  run: run(),
  case_runs: [
    {
      case_id: "c1", case_name: "books-a-visit", repeat: 0, status: "done", session_id: "s9", passed: false,
      scores: [
        { scorer: "expectations", score: 0, outcome: "fail", reason: "never_say: (?i)you said" },
        { scorer: "correctness", score: 0, outcome: "skipped", reason: "no judge configured" },
      ],
    },
    { case_id: "c1", case_name: "books-a-visit", repeat: 1, status: "dead", error_category: "dial", passed: false, scores: [] },
  ],
};

function setup(over: Record<string, unknown> = {}) {
  const client = {
    listEvalCases: vi.fn().mockResolvedValue([kase]),
    listEvalRuns: vi.fn().mockResolvedValue([run(), run({ id: "r2", draft: true, version: undefined })]),
    getEvalRun: vi.fn().mockResolvedValue(detail),
    getProviders: vi.fn().mockResolvedValue([]),
    getEvalSuite: vi.fn().mockResolvedValue({ runs: 1, min_pass: 1, pass_threshold: 1, judge: null }),
    getEvalScorers: vi.fn().mockResolvedValue([{ name: "expectations", judged: false, default_threshold: 1 }]),
    createEvalCases: vi.fn().mockResolvedValue([kase]),
    startEvalRun: vi.fn().mockResolvedValue(run({ id: "r3", status: "pending" })),
    ...over,
  };
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <TestSuiteTab client={client as unknown as ApiClient} agentId="a1" agentName="Support" currentVersion={3} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return client;
}

describe("TestSuiteTab", () => {
  it("always warns that eval calls run real tools", async () => {
    setup();
    expect(await screen.findByRole("note")).toHaveTextContent("Eval calls run this agent's real tools.");
  });

  it("posts the YAML from the textarea", async () => {
    const c = setup();
    const u = userEvent.setup();
    await u.type(await screen.findByLabelText("Add cases"), "name: x");
    await u.click(screen.getByRole("button", { name: "Add cases" }));
    await waitFor(() => expect(c.createEvalCases).toHaveBeenCalledWith("a1", "name: x"));
  });

  it("shows a server validation error from saving a case", async () => {
    setup({ createEvalCases: vi.fn().mockRejectedValue(new ApiError("cases.yaml#2: caller: set exactly one of script or persona", "invalid_case", 400, "q")) });
    const u = userEvent.setup();
    await u.type(await screen.findByLabelText("Add cases"), "name: x");
    await u.click(screen.getByRole("button", { name: "Add cases" }));
    expect(await screen.findByText(/cases.yaml#2: caller/)).toBeVisible();
  });

  it("runs the suite on the offline model at the current version by default", async () => {
    const c = setup();
    await userEvent.setup().click(await screen.findByRole("button", { name: "Run suite" }));
    await waitFor(() => expect(c.startEvalRun).toHaveBeenCalledWith("a1", { version: 3, providers: "offline" }));
  });

  it("runs one case and shows the paid note for the agent's own models", async () => {
    const c = setup();
    const u = userEvent.setup();
    await u.selectOptions(await screen.findByLabelText("Models"), "keep");
    expect(screen.getByText(/may spend money/)).toBeVisible();
    await u.click(screen.getByRole("button", { name: "Run books-a-visit" }));
    await waitFor(() => expect(c.startEvalRun).toHaveBeenCalledWith("a1", { version: 3, providers: "keep", case_id: "c1" }));
  });

  it("shows the server's message for eval_live_required and a clear one for suite_too_large", async () => {
    const start = vi
      .fn()
      .mockRejectedValueOnce(new ApiError("needs GOVOICE_LIVE=1", "eval_live_required", 403, "q"))
      .mockRejectedValueOnce(new ApiError("raw", "suite_too_large", 409, "q"));
    setup({ startEvalRun: start });
    const u = userEvent.setup();
    await u.click(await screen.findByRole("button", { name: "Run suite" }));
    expect(await screen.findByText("needs GOVOICE_LIVE=1")).toBeVisible();
    await u.click(screen.getByRole("button", { name: "Run suite" }));
    expect(await screen.findByText(/over 200/)).toBeVisible();
  });

  it("lists runs with draft, and details with scores, skipped reason and harness error", async () => {
    setup();
    const row = await screen.findByRole("row", { name: /draft/ });
    expect(within(row).getByText("draft")).toBeVisible();
    expect(await screen.findByText("correctness: skipped — no judge configured")).toBeVisible();
    expect(screen.getByText("expectations: fail — never_say: (?i)you said")).toBeVisible();
    expect(screen.getByText("Harness error: dial")).toBeVisible();
    expect(screen.getByRole("link", { name: "Open call" })).toHaveAttribute("href", "/sessions/s9");
    expect(screen.getByText("0 of 2 runs passed")).toBeVisible();
  });

  it("polls an active run on a bounded interval", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const get = vi.fn().mockResolvedValue({ ...detail, run: run({ status: "running" }) });
      setup({ listEvalRuns: vi.fn().mockResolvedValue([run({ status: "running" })]), getEvalRun: get });
      await screen.findByText(/Run detail: running/);
      const before = get.mock.calls.length;
      await vi.advanceTimersByTimeAsync(2100);
      expect(get.mock.calls.length).toBeGreaterThan(before);
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps each case's suite result after a single-case run and ignores drafts", async () => {
    const suite = run({ id: "rs", scope: "suite", created_at: "2026-10-24T09:00:00Z" });
    const single = run({ id: "r1c", scope: "case", created_at: "2026-10-24T10:00:00Z" });
    const draftRun = run({ id: "rd", draft: true, created_at: "2026-10-24T11:00:00Z" });
    const two = { ...kase, id: "c2", name: "cancels" };
    const get = vi.fn(async (_a: string, id: string) => ({
      run: run({ id }),
      case_runs: [{ case_id: "c1", case_name: "books-a-visit", repeat: 0, status: "done", passed: id === "rs", scores: [] }, ...(id === "rs" ? [{ case_id: "c2", case_name: "cancels", repeat: 0, status: "done", passed: true, scores: [] }] : [])],
    }));
    setup({ listEvalCases: vi.fn().mockResolvedValue([kase, two]), listEvalRuns: vi.fn().mockResolvedValue([draftRun, single, suite]), getEvalRun: get });
    const cases = within(await screen.findByRole("table", { name: "Test cases" }));
    expect(await cases.findAllByText("1 of 1 runs passed")).toHaveLength(2);
    expect(get).toHaveBeenCalledWith("a1", "rs");
  });

  it("refuses an emptied suite field instead of saving 0", async () => {
    const put = vi.fn().mockResolvedValue({});
    setup({ putEvalSuite: put });
    const u = userEvent.setup();
    await u.clear(await screen.findByLabelText("Pass threshold (%)"));
    await u.click(screen.getByRole("button", { name: "Save suite settings" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Pass threshold must be a number");
    expect(put).not.toHaveBeenCalled();
  });
});

describe("TestSuiteTab polling", () => {
  const withTimers = async (fn: () => Promise<void>) => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      await fn();
    } finally {
      vi.useRealTimers();
    }
  };

  it("polls the runs list every 2000 ms while a run is active and stops when it is done", async () => {
    await withTimers(async () => {
      const list = vi.fn().mockResolvedValueOnce([run({ status: "running" })]).mockResolvedValue([run()]);
      setup({ listEvalRuns: list });
      await waitFor(() => expect(list).toHaveBeenCalledTimes(1));
      await vi.advanceTimersByTimeAsync(1900);
      expect(list).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(200);
      await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
      await vi.advanceTimersByTimeAsync(10000);
      expect(list).toHaveBeenCalledTimes(2);
    });
  });

  it("stops polling the run detail at a terminal status", async () => {
    await withTimers(async () => {
      const get = vi.fn().mockResolvedValueOnce({ ...detail, run: run({ status: "running" }) }).mockResolvedValue(detail);
      setup({ listEvalRuns: vi.fn().mockResolvedValue([run({ status: "running" })]), getEvalRun: get });
      await waitFor(() => expect(get).toHaveBeenCalledTimes(1));
      await vi.advanceTimersByTimeAsync(2100);
      await waitFor(() => expect(get).toHaveBeenCalledTimes(2));
      await vi.advanceTimersByTimeAsync(10000);
      expect(get).toHaveBeenCalledTimes(2);
    });
  });

  it("does not poll the run detail after an error", async () => {
    await withTimers(async () => {
      const get = vi.fn().mockRejectedValue(new ApiError("gone", "not_found", 404, "q"));
      setup({ getEvalRun: get });
      await waitFor(() => expect(get).toHaveBeenCalled());
      const n = get.mock.calls.length;
      await vi.advanceTimersByTimeAsync(10000);
      expect(get.mock.calls.length).toBe(n);
    });
  });
});
