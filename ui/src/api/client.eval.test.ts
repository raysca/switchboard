import { describe, expect, it, vi } from "vitest";
import { ApiError, createApiClient } from "./client";

const run = {
  id: "r1", agent_id: "a1", version: 7, draft: false, config_hash: "h", scope: "suite", providers: "offline",
  runs: 1, min_pass: 1, status: "pending", triggered_by: "console", created_at: "2026-10-24T10:00:00Z",
};

describe("evaluation client", () => {
  it("reads the fleet page with limit and offset", async () => {
    const fleet = {
      agents: [{ agent_id: "a1", name: "Support", threshold: 0.9, latest: { run_id: "r1", version: 7, passed: 4, total: 5, pass_rate: 0.8, ended_at: "2026-10-24T10:00:00Z" }, trend: [0.6, 0.8], below_threshold: true }],
      total: 1,
      below_threshold: 1,
    };
    const fetcher = vi.fn<typeof fetch>(async () => new Response(JSON.stringify({ data: fleet })));
    const client = createApiClient(() => ({ key: "k", generation: 1 }), fetcher);
    await expect(client.getEvalFleet(25, 50)).resolves.toEqual(fleet);
    expect(fetcher).toHaveBeenCalledWith("/api/v1/eval/fleet?limit=25&offset=50", expect.any(Object));
  });

  it("starts a run with the body and parses the run", async () => {
    const fetcher = vi.fn<typeof fetch>(async () => new Response(JSON.stringify({ data: run }), { status: 202 }));
    const client = createApiClient(() => ({ key: "k", generation: 1 }), fetcher);
    await expect(client.startEvalRun("a/1", { version: 7, providers: "offline" })).resolves.toMatchObject({ id: "r1", version: 7 });
    const [url, init] = fetcher.mock.calls[0]!;
    expect(url).toBe("/api/v1/agents/a%2F1/eval/runs");
    expect(init).toMatchObject({ method: "POST", body: JSON.stringify({ version: 7, providers: "offline" }) });
  });

  it("keeps the server's code for a refused paid run", async () => {
    const fetcher = vi.fn<typeof fetch>(async () => new Response(JSON.stringify({ error: { code: "eval_live_required", message: "needs GOVOICE_LIVE=1", request_id: "q" } }), { status: 403 }));
    const client = createApiClient(() => ({ key: "k", generation: 1 }), fetcher);
    await expect(client.startEvalRun("a1", { providers: "keep" })).rejects.toMatchObject({ code: "eval_live_required", status: 403 } satisfies Partial<ApiError>);
  });
});
