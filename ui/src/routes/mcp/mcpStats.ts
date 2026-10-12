import { ApiError, type AgentDTO, type MCPServerDTO, type ToolDependencyDTO, type ToolSelection } from "../../api/client";

export type Tone = "ok" | "bad" | "idle";

/** The server's own words for its state, in ours. Only `healthy` is Connected; an unknown state is shown as it is. */
export function serverStatus(s: MCPServerDTO): { tone: Tone; label: string } {
  if (s.pending_deletion) return { tone: "idle", label: "Being removed" };
  if (!s.enabled || s.validation_state === "disabled") return { tone: "idle", label: "Disabled" };
  switch (s.validation_state) {
    case "healthy":
      return { tone: "ok", label: "Connected" };
    case "not_validated":
      return { tone: "idle", label: "Not checked yet" };
    case "authentication_failed":
      return { tone: "bad", label: "Authentication failed" };
    case "connection_failed":
      return { tone: "bad", label: "Unreachable" };
    case "discovery_failed":
      return { tone: "bad", label: "Could not list tools" };
    case "tool_schema_invalid":
      return { tone: "bad", label: "A tool schema is invalid" };
    default:
      return { tone: "idle", label: s.validation_state };
  }
}

/** "4 min ago": the largest sensible unit; "" without a time. `now` is ms since the epoch. */
export function ago(iso: string | null | undefined, now: number): string {
  if (!iso) return "";
  const seconds = (now - Date.parse(iso)) / 1000;
  if (seconds < 60) return "just now";
  if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} h ago`;
  return `${Math.floor(seconds / 86400)} d ago`;
}

const live = (flows: AgentDTO[]) => flows.filter((f) => !f.archived);

export function flowsUsingServer(flows: AgentDTO[], serverId: string): AgentDTO[] {
  return live(flows).filter((f) => (f.current_version?.mcp_tools ?? []).some((t) => t.mcp_server_id === serverId));
}

export function flowsUsingTool(flows: AgentDTO[], serverId: string, remoteName: string): AgentDTO[] {
  return live(flows).filter((f) => (f.current_version?.mcp_tools ?? []).some((t) => t.mcp_server_id === serverId && t.remote_name === remoteName));
}

export type Drift = { flow: AgentDTO; dep: ToolDependencyDTO; kind: "changed" | "missing" };

/** Pinned tools of this server whose schema changed, or that the server no longer offers. */
export function drifts(flows: AgentDTO[], serverId: string): Drift[] {
  return live(flows).flatMap((flow) =>
    (flow.readiness.tool_dependencies ?? [])
      .filter((dep) => dep.mcp_server_id === serverId && (dep.state === "schema_drift" || dep.state === "tool_missing"))
      .map((dep): Drift => ({ flow, dep, kind: dep.state === "schema_drift" ? "changed" : "missing" })),
  );
}

export type Repin = {
  /** The flow's whole tool list with every changed tool re-pinned; `null` when nothing can be re-pinned. */
  selection: ToolSelection[] | null;
  /** The tools that get re-pinned. */
  tools: string[];
  /** Dependencies that would still make the server refuse the list: a tool that is gone, a server that is down. */
  blockedBy: ToolDependencyDTO[];
};

/**
 * What accepting the new schemas means for one flow. The server checks every tool in the list against its latest
 * discovery, so a list that leaves any changed tool on its old hash is refused: all of them are re-pinned together.
 */
export function repin(flow: AgentDTO): Repin {
  const deps = flow.readiness.tool_dependencies ?? [];
  const tools = flow.current_version?.mcp_tools ?? [];
  const changed = deps.filter((d) => d.state === "schema_drift" && d.current_schema_hash && tools.some((t) => t.mcp_server_id === d.mcp_server_id && t.remote_name === d.remote_name));
  const blockedBy = deps.filter((d) => d.state !== "ready" && d.state !== "schema_drift");
  if (changed.length === 0) return { selection: null, tools: [], blockedBy };
  const selection = tools.map((t) => {
    const d = changed.find((c) => c.mcp_server_id === t.mcp_server_id && c.remote_name === t.remote_name);
    return d ? { ...t, schema_hash: d.current_schema_hash! } : t;
  });
  return { selection, tools: changed.map((d) => d.remote_name), blockedBy };
}

/** The flows a refused delete names (`details` of the 409: every current flow that pins a tool of the server, archived or not). */
export function referencedFlows(details: unknown): { id: string; name: string }[] {
  if (!Array.isArray(details)) return [];
  const seen = new Map<string, string>();
  for (const r of details as { AgentID?: unknown; AgentName?: unknown }[]) {
    if (r && typeof r.AgentID === "string" && typeof r.AgentName === "string" && !seen.has(r.AgentID)) seen.set(r.AgentID, r.AgentName);
  }
  return [...seen].map(([id, name]) => ({ id, name }));
}

export function formatApiError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.requestId ? `${err.message} (Request ID: ${err.requestId})` : err.message;
  if (err instanceof Error) return err.message;
  return fallback;
}
