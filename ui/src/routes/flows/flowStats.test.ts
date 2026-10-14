import { describe, expect, it } from "vitest";
import type { FlowDTO, SessionDTO } from "../../api/client";
import { canTest, liveCalls, readiness, routeCount, stepCount, toolCount } from "./flowStats";

const flow = (over: Partial<FlowDTO> = {}, config: Record<string, unknown> = {}): FlowDTO => ({
  id: "a1",
  name: "Front desk",
  description: "",
  enabled: true,
  archived: false,
  current_version: { id: "v2", number: 2, schema_version: 1, config, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
  ...over,
});
const session = (over: Partial<SessionDTO>): SessionDTO => ({
  draft: false,
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

describe("flow figures", () => {
  it("counts steps, routes and tools; a flat config is one step", () => {
    expect(stepCount(flow())).toBe(1);
    const graph = flow({}, { start: "a", steps: [{ id: "a" }, { id: "b" }, { id: "c" }], routes: [{ id: "e1" }, { id: "e2" }] });
    expect([stepCount(graph), routeCount(graph)]).toEqual([3, 2]);
    expect(routeCount(flow())).toBe(0);
    const withTools = flow();
    withTools.current_version!.mcp_tools = [{ mcp_server_id: "crm", remote_name: "lookup", schema_hash: "h", required: false }];
    expect(toolCount(withTools)).toBe(1);
    expect(toolCount(flow({ current_version: null }))).toBe(0);
  });

  it("maps readiness to a lamp tone and a plain label; disabled wins", () => {
    expect(readiness(flow())).toEqual({ tone: "ok", label: "Ready" });
    expect(readiness(flow({ readiness: { state: "ready_with_tool_warning", reasons: [] } }))).toEqual({ tone: "ok", label: "Ready (tool warning)" });
    expect(readiness(flow({ readiness: { state: "needs_tool_review", reasons: [] } }))).toEqual({ tone: "bad", label: "Needs tool review" });
    expect(readiness(flow({ readiness: { state: "tool_dependency_unavailable", reasons: [] } }))).toEqual({ tone: "bad", label: "Tool unavailable" });
    expect(readiness(flow({ readiness: { state: "not_ready", reasons: ["no_provider"] } }))).toEqual({ tone: "bad", label: "Not ready" });
    expect(readiness(flow({ enabled: false, readiness: { state: "not_ready", reasons: [] } }))).toEqual({ tone: "idle", label: "Disabled" });
  });

  it("only offers a test call for an enabled, ready flow", () => {
    expect(canTest(flow())).toBe(true);
    expect(canTest(flow({ enabled: false }))).toBe(false);
    expect(canTest(flow({ readiness: { state: "not_ready", reasons: [] } }))).toBe(false);
  });

  it("counts a flow's active calls and the ones still on an earlier version", () => {
    const sessions = [
      session({ id: "1" }),
      session({ id: "2", flow_version_id: "v1" }),
      session({ id: "3", flow_version_id: "v1" }),
      session({ id: "4", flow_id: "other", flow_version_id: "v1" }),
      session({ id: "5", status: "completed", flow_version_id: "v1" }),
    ];
    expect(liveCalls(sessions, flow())).toEqual({ live: 3, onOlderVersion: 2 });
    expect(liveCalls([], flow())).toEqual({ live: 0, onOlderVersion: 0 });
  });
});
