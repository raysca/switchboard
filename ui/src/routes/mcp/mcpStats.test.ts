import { describe, expect, it } from "vitest";
import { ApiError, type AgentDTO, type MCPServerDTO, type ToolDependencyDTO } from "../../api/client";
import { acceptedSelection, ago, drifts, flowsUsingServer, flowsUsingTool, formatApiError, serverStatus } from "./mcpStats";

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
const flow = (id: string, tools: { server: string; name: string; hash: string; required: boolean }[], deps: ToolDependencyDTO[] = [], archived = false): AgentDTO => ({
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
  const a = flow("a", [{ server: "crm", name: "lookup", hash: "h1", required: true }, { server: "billing", name: "pay", hash: "h2", required: false }]);
  const b = flow("b", [{ server: "crm", name: "notes", hash: "h3", required: false }]);
  const gone = flow("gone", [{ server: "crm", name: "lookup", hash: "h1", required: true }], [], true);
  const bare = { ...flow("bare", []), current_version: undefined } as AgentDTO;
  it("lists the flows that pin a server's tools, once each, never an archived one", () => {
    expect(flowsUsingServer([a, b, gone, bare], "crm").map((f) => f.id)).toEqual(["a", "b"]);
    expect(flowsUsingServer([a, b], "billing").map((f) => f.id)).toEqual(["a"]);
  });
  it("lists the flows that pin one tool", () => {
    expect(flowsUsingTool([a, b], "crm", "lookup").map((f) => f.id)).toEqual(["a"]);
    expect(flowsUsingTool([a, b], "crm", "nope")).toEqual([]);
  });
});

describe("drifts and acceptedSelection", () => {
  const changed = dep({ state: "schema_drift", current_schema_hash: "bbbbbbbb22" });
  const missing = dep({ remote_name: "gone", state: "tool_missing" });
  const ok = dep({ remote_name: "fine" });
  const other = dep({ mcp_server_id: "billing", state: "schema_drift", current_schema_hash: "z" });
  const f = flow("f", [{ server: "crm", name: "lookup", hash: "aaaaaaaa11", required: true }, { server: "billing", name: "pay", hash: "p1", required: false }], [changed, missing, ok, other]);
  it("finds the changed and missing tools of one server, for live flows only", () => {
    const archived = flow("old", [], [changed], true);
    expect(drifts([f, archived], "crm").map((d) => [d.flow.id, d.dep.remote_name, d.kind])).toEqual([
      ["f", "lookup", "changed"],
      ["f", "gone", "missing"],
    ]);
    expect(drifts([f], "billing").map((d) => d.kind)).toEqual(["changed"]);
  });
  it("accepts a changed schema by replacing only that tool's hash and keeping the rest", () => {
    expect(acceptedSelection(f, changed)).toEqual([
      { mcp_server_id: "crm", remote_name: "lookup", schema_hash: "bbbbbbbb22", required: true },
      { mcp_server_id: "billing", remote_name: "pay", schema_hash: "p1", required: false },
    ]);
  });
  it("refuses to accept when the current hash is unknown or the flow does not pin the tool", () => {
    expect(acceptedSelection(f, dep({ state: "schema_drift" }))).toBeNull();
    expect(acceptedSelection(f, dep({ remote_name: "other", current_schema_hash: "x" }))).toBeNull();
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
