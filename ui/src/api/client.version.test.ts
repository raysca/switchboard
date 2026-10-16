import { describe, expect, it, vi } from "vitest";
import { createApiClient } from "./client";

describe("getFlowVersion", () => {
  it("reads one earlier publish by number", async () => {
    const version = { id: "v1", number: 1, schema_version: 1, config: { start: "a" }, config_hash: "h", created_at: "2026-10-01T10:00:00Z", mcp_tools: [] };
    const fetcher = vi.fn<typeof fetch>(async () => new Response(JSON.stringify({ data: version })));
    const client = createApiClient(() => ({ key: "k", generation: 1 }), fetcher);
    await expect(client.getFlowVersion("flow/1", 1)).resolves.toMatchObject({ number: 1, config_hash: "h" });
    expect(fetcher).toHaveBeenCalledWith("/api/v1/flows/flow%2F1/versions/1", expect.any(Object));
  });
});
