import { describe, expect, it } from "vitest";
import type { AgentDTO, SessionDTO } from "../../api/client";
import { canTest, liveCalls, readiness, reasonText, handoffCount, stepCount, toolCount } from "./agentStats";

const agent = (over: Partial<AgentDTO> = {}, config: Record<string, unknown> = {}): AgentDTO => ({
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
  caller: "",
  end_reason: "",
  id: "s",
  agent_id: "a1",
  agent_version_id: "v2",
  config_hash: "h",
  started_at: "",
  status: "active",
  turn_count: 0,
  safe_summary: "",
  ...over,
});

describe("agent figures", () => {
  it("counts steps, routes and tools; a flat config is one step", () => {
    expect(stepCount(agent())).toBe(1);
    const graph = agent({}, { start: "a", steps: [{ id: "a" }, { id: "b" }, { id: "c" }], handoffs: [{ id: "e1" }, { id: "e2" }] });
    expect([stepCount(graph), handoffCount(graph)]).toEqual([3, 2]);
    expect(handoffCount(agent())).toBe(0);
    const withTools = agent();
    withTools.current_version!.mcp_tools = [{ mcp_server_id: "crm", remote_name: "lookup", schema_hash: "h", required: false }];
    expect(toolCount(withTools)).toBe(1);
    expect(toolCount(agent({ current_version: null }))).toBe(0);
  });

  it("maps readiness to a lamp tone and a plain label; disabled wins", () => {
    expect(readiness(agent())).toEqual({ tone: "ok", label: "Ready" });
    expect(readiness(agent({ readiness: { state: "ready_with_tool_warning", reasons: [] } }))).toEqual({ tone: "ok", label: "Ready (tool warning)" });
    expect(readiness(agent({ readiness: { state: "needs_tool_review", reasons: [] } }))).toEqual({ tone: "bad", label: "Needs tool review" });
    expect(readiness(agent({ readiness: { state: "tool_dependency_unavailable", reasons: [] } }))).toEqual({ tone: "bad", label: "Tool unavailable" });
    expect(readiness(agent({ readiness: { state: "not_ready", reasons: ["no_provider"] } }))).toEqual({ tone: "bad", label: "Not ready" });
    expect(readiness(agent({ enabled: false, readiness: { state: "not_ready", reasons: [] } }))).toEqual({ tone: "idle", label: "Disabled" });
  });

  it("only offers a test call for an enabled, ready flow", () => {
    expect(canTest(agent())).toBe(true);
    expect(canTest(agent({ enabled: false }))).toBe(false);
    expect(canTest(agent({ readiness: { state: "not_ready", reasons: [] } }))).toBe(false);
  });

  it("counts a flow's active calls and the ones still on an earlier version", () => {
    const sessions = [
      session({ id: "1" }),
      session({ id: "2", agent_version_id: "v1" }),
      session({ id: "3", agent_version_id: "v1" }),
      session({ id: "4", agent_id: "other", agent_version_id: "v1" }),
      session({ id: "5", status: "completed", agent_version_id: "v1" }),
    ];
    expect(liveCalls(sessions, agent())).toEqual({ live: 3, onOlderVersion: 2 });
    expect(liveCalls([], agent())).toEqual({ live: 0, onOlderVersion: 0 });
  });
});

describe("reasonText", () => {
  it("words the codes it knows and keeps any other raw", () => {
    expect(reasonText("smart_turn_unavailable")).toBe("Smart end-of-turn needs turn.analyzer.for_agents on this server");
    expect(reasonText("missing_credential_llm")).toBe("missing_credential_llm");
  });
});
