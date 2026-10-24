import { describe, expect, it } from "vitest";
import { ApiError, type AgentDTO, type MCPServerDTO, type ToolDependencyDTO } from "../../api/client";
import { ago, drifts, agentsUsingServer, agentsUsingTool, formatApiError, referencedAgents, repin, serverStatus } from "./mcpStats";

const server = (over: Partial<MCPServerDTO> = {}): MCPServerDTO => ({
  id: "crm",
  name: "crm",
  description: "",
  endpoint: "https://mcp.example.com",
  transport: "streamable-http",
  validation_state: "healthy",
  latest_discovery_id: "d1",
  auth_env: "",
  auth_env_set: false,
  enabled: true,
  ...over,
});
const dep = (over: Partial<ToolDependencyDTO> = {}): ToolDependencyDTO => ({
  mcp_server_id: "crm",
  remote_name: "lookup",
  required: true,
  state: "ready",
  selected_schema_hash: "aaaaaaaa11",
  ...over,
});
const agent = (id: string, tools: { server: string; name: string; hash: string; required: boolean }[], deps: ToolDependencyDTO[] = [], archived = false): AgentDTO => ({
  id,
  name: id,
  description: "",
  enabled: true,
  archived,
  current_version: {
    id: `${id}-v`,
    number: 1,
    schema_version: 1,
    config: {},
    config_hash: "h",
    created_at: "",
    mcp_tools: tools.map((t) => ({ mcp_server_id: t.server, remote_name: t.name, schema_hash: t.hash, required: t.required })),
  },
  readiness: { state: "ready", reasons: [], tool_dependencies: deps },
  created_at: "",
  updated_at: "",
});

describe("serverStatus", () => {
  it("says Connected only for the state the server calls healthy", () => {
    expect(serverStatus(server())).toEqual({ tone: "ok", label: "Connected" });
  });
  it("names each failure the server can report, as a failure", () => {
    expect(serverStatus(server({ validation_state: "authentication_failed" }))).toEqual({ tone: "bad", label: "Authentication failed" });
    expect(serverStatus(server({ validation_state: "connection_failed" }))).toEqual({ tone: "bad", label: "Unreachable" });
    expect(serverStatus(server({ validation_state: "discovery_failed" }))).toEqual({ tone: "bad", label: "Could not list tools" });
    expect(serverStatus(server({ validation_state: "tool_schema_invalid" }))).toEqual({ tone: "bad", label: "A tool schema is invalid" });
  });
  it("keeps disabled, unchecked and being-removed neutral, whatever the stored state says", () => {
    expect(serverStatus(server({ enabled: false }))).toEqual({ tone: "idle", label: "Disabled" });
    expect(serverStatus(server({ validation_state: "disabled" }))).toEqual({ tone: "idle", label: "Disabled" });
    expect(serverStatus(server({ validation_state: "not_validated" }))).toEqual({ tone: "idle", label: "Not checked yet" });
    expect(serverStatus(server({ pending_deletion: true }))).toEqual({ tone: "idle", label: "Being removed" });
  });
  it("shows a state it has never heard of as it is, never as connected", () => {
    expect(serverStatus(server({ validation_state: "valid" }))).toEqual({ tone: "idle", label: "valid" });
  });
});

describe("ago", () => {
  const now = Date.parse("2026-10-12T15:00:00Z");
  it("says how long ago, in the largest sensible unit", () => {
    expect(ago("2026-10-12T14:59:40Z", now)).toBe("just now");
    expect(ago("2026-10-12T14:56:00Z", now)).toBe("4 min ago");
    expect(ago("2026-10-12T12:00:00Z", now)).toBe("3 h ago");
    expect(ago("2026-10-09T15:00:00Z", now)).toBe("3 d ago");
  });
  it("is empty without a time, and never says the future", () => {
    expect(ago(null, now)).toBe("");
    expect(ago(undefined, now)).toBe("");
    expect(ago("2026-10-12T15:05:00Z", now)).toBe("just now");
  });
});

describe("usage", () => {
  const a = agent("a", [{ server: "crm", name: "lookup", hash: "h1", required: true }, { server: "billing", name: "pay", hash: "h2", required: false }]);
  const b = agent("b", [{ server: "crm", name: "notes", hash: "h3", required: false }]);
  const gone = agent("gone", [{ server: "crm", name: "lookup", hash: "h1", required: true }], [], true);
  const bare = { ...agent("bare", []), current_version: undefined } as AgentDTO;
  it("lists the agents that pin a server's tools, once each, never an archived one", () => {
    expect(agentsUsingServer([a, b, gone, bare], "crm").map((f) => f.id)).toEqual(["a", "b"]);
    expect(agentsUsingServer([a, b], "billing").map((f) => f.id)).toEqual(["a"]);
  });
  it("lists the agents that pin one tool", () => {
    expect(agentsUsingTool([a, b], "crm", "lookup").map((f) => f.id)).toEqual(["a"]);
    expect(agentsUsingTool([a, b], "crm", "nope")).toEqual([]);
  });
});

describe("drifts and acceptedSelection", () => {
  const changed = dep({ state: "schema_drift", current_schema_hash: "bbbbbbbb22" });
  const missing = dep({ remote_name: "gone", state: "tool_missing" });
  const ok = dep({ remote_name: "fine" });
  const other = dep({ mcp_server_id: "billing", state: "schema_drift", current_schema_hash: "z" });
  const f = agent("f", [{ server: "crm", name: "lookup", hash: "aaaaaaaa11", required: true }, { server: "billing", name: "pay", hash: "p1", required: false }], [changed, missing, ok, other]);
  it("finds the changed and missing tools of one server, for live agents only", () => {
    const archived = agent("old", [], [changed], true);
    expect(drifts([f, archived], "crm").map((d) => [d.agent.id, d.dep.remote_name, d.kind])).toEqual([
      ["f", "lookup", "changed"],
      ["f", "gone", "missing"],
    ]);
    expect(drifts([f], "billing").map((d) => d.kind)).toEqual(["changed"]);
  });
});

describe("repin", () => {
  const two = agent(
    "two",
    [
      { server: "crm", name: "lookup", hash: "aaaaaaaa11", required: true },
      { server: "crm", name: "notes", hash: "n1", required: false },
      { server: "billing", name: "pay", hash: "p1", required: false },
    ],
    [
      dep({ state: "schema_drift", current_schema_hash: "bbbbbbbb22" }),
      dep({ remote_name: "notes", required: false, state: "schema_drift", selected_schema_hash: "n1", current_schema_hash: "n2" }),
      dep({ mcp_server_id: "billing", remote_name: "pay", required: false, state: "ready", selected_schema_hash: "p1" }),
    ],
  );
  it("re-pins every changed tool of the agent in one list, and keeps the rest as they were", () => {
    const plan = repin(two);
    expect(plan.tools).toEqual(["lookup", "notes"]);
    expect(plan.blockedBy).toEqual([]);
    expect(plan.selection).toEqual([
      { mcp_server_id: "crm", remote_name: "lookup", schema_hash: "bbbbbbbb22", required: true },
      { mcp_server_id: "crm", remote_name: "notes", schema_hash: "n2", required: false },
      { mcp_server_id: "billing", remote_name: "pay", schema_hash: "p1", required: false },
    ]);
  });
  it("names what would still make the server refuse: a tool that is gone, or a server that is down", () => {
    const f = agent("blocked", [{ server: "crm", name: "lookup", hash: "a", required: true }, { server: "crm", name: "gone", hash: "g", required: false }], [
      dep({ state: "schema_drift", current_schema_hash: "b" }),
      dep({ remote_name: "gone", required: false, state: "tool_missing" }),
    ]);
    expect(repin(f).blockedBy.map((d) => d.remote_name)).toEqual(["gone"]);
  });
  it("has nothing to re-pin when no tool changed, or the current hash is unknown", () => {
    expect(repin(agent("none", [{ server: "crm", name: "lookup", hash: "a", required: true }], [dep()])).selection).toBeNull();
    expect(repin(agent("nohash", [{ server: "crm", name: "lookup", hash: "a", required: true }], [dep({ state: "schema_drift" })])).selection).toBeNull();
  });
});

describe("referencedAgents", () => {
  it("reads the agents the server names when it refuses a delete, once each", () => {
    const details = [
      { agent_id: "a1", agent_name: "Front desk", remote_name: "lookup", required: true },
      { agent_id: "a1", agent_name: "Front desk", remote_name: "notes", required: false },
      { agent_id: "a2", agent_name: "Old line", remote_name: "lookup", required: false },
    ];
    expect(referencedAgents(details)).toEqual([{ id: "a1", name: "Front desk" }, { id: "a2", name: "Old line" }]);
  });
  it("is empty for anything else", () => {
    expect(referencedAgents(null)).toEqual([]);
    expect(referencedAgents("x")).toEqual([]);
    expect(referencedAgents([{ nope: 1 }, null])).toEqual([]);
    // the Go-field-name keys the server used to send are not read any more
    expect(referencedAgents([{ ["Agent" + "ID"]: "a1", ["Agent" + "Name"]: "Front desk" }, { AgentID: "a1", AgentName: "Front desk" }])).toEqual([]);
  });
});

describe("formatApiError", () => {
  it("keeps the request id of an API error, the message of any error, else the fallback", () => {
    expect(formatApiError(new ApiError("boom", "internal", 500, "req-9"), "x")).toBe("boom (Request ID: req-9)");
    expect(formatApiError(new ApiError("boom", "internal", 500, ""), "x")).toBe("boom");
    expect(formatApiError(new Error("plain"), "x")).toBe("plain");
    expect(formatApiError("odd", "fallback")).toBe("fallback");
  });
});

describe("serverStatus with a missing token variable", () => {
  it("names the variable", () => {
    const s = { enabled: true, validation_state: "healthy", auth_env: "MCP_TOKEN_X", auth_env_set: false } as MCPServerDTO;
    expect(serverStatus(s)).toEqual({ tone: "bad", label: "Secret MCP_TOKEN_X is not set on the server" });
  });
});
