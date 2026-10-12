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

/** The flow's tool list with one tool re-pinned to the schema the server offers now. `null` when that cannot be done. */
export function acceptedSelection(flow: AgentDTO, dep: ToolDependencyDTO): ToolSelection[] | null {
  const current = dep.current_schema_hash;
  const tools = flow.current_version?.mcp_tools ?? [];
  const pinned = tools.some((t) => t.mcp_server_id === dep.mcp_server_id && t.remote_name === dep.remote_name);
  if (!current || !pinned) return null;
  return tools.map((t) => (t.mcp_server_id === dep.mcp_server_id && t.remote_name === dep.remote_name ? { ...t, schema_hash: current } : t));
}

export function formatApiError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.requestId ? `${err.message} (Request ID: ${err.requestId})` : err.message;
  if (err instanceof Error) return err.message;
  return fallback;
}
