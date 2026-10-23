import { ApiError, type AgentDTO, type MCPServerDTO, type ToolDependencyDTO, type ToolSelection } from "../../api/client";

type Tone = "ok" | "bad" | "idle";

/** The server's own words for its state, in ours. Only `healthy` is Connected; an unknown state is shown as it is. */
export function serverStatus(s: MCPServerDTO): { tone: Tone; label: string } {
  if (s.pending_deletion) return { tone: "idle", label: "Being removed" };
  if (!s.enabled || s.validation_state === "disabled") return { tone: "idle", label: "Disabled" };
  if (s.auth_env && !s.auth_env_set) return { tone: "bad", label: `Token variable ${s.auth_env} is not set on the server` };
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

const live = (agents: AgentDTO[]) => agents.filter((f) => !f.archived);

export function agentsUsingServer(agents: AgentDTO[], serverId: string): AgentDTO[] {
  return live(agents).filter((f) => (f.current_version?.mcp_tools ?? []).some((t) => t.mcp_server_id === serverId));
}

export function agentsUsingTool(agents: AgentDTO[], serverId: string, remoteName: string): AgentDTO[] {
  return live(agents).filter((f) => (f.current_version?.mcp_tools ?? []).some((t) => t.mcp_server_id === serverId && t.remote_name === remoteName));
}

export type Drift = { agent: AgentDTO; dep: ToolDependencyDTO; kind: "changed" | "missing" };

/** Pinned tools of this server whose schema changed, or that the server no longer offers. */
export function drifts(agents: AgentDTO[], serverId: string): Drift[] {
  return live(agents).flatMap((agent) =>
    (agent.readiness.tool_dependencies ?? [])
      .filter((dep) => dep.mcp_server_id === serverId && (dep.state === "schema_drift" || dep.state === "tool_missing"))
      .map((dep): Drift => ({ agent, dep, kind: dep.state === "schema_drift" ? "changed" : "missing" })),
  );
}

type Repin = {
  /** The agent's whole tool list with every changed tool re-pinned; `null` when nothing can be re-pinned. */
  selection: ToolSelection[] | null;
  /** The tools that get re-pinned. */
  tools: string[];
  /** Dependencies that would still make the server refuse the list: a tool that is gone, a server that is down. */
  blockedBy: ToolDependencyDTO[];
};

/**
 * What accepting the new schemas means for one agent. The server checks every tool in the list against its latest
 * discovery, so a list that leaves any changed tool on its old hash is refused: all of them are re-pinned together.
 */
export function repin(agent: AgentDTO): Repin {
  const deps = agent.readiness.tool_dependencies ?? [];
  const tools = agent.current_version?.mcp_tools ?? [];
  const changed = deps.filter((d) => d.state === "schema_drift" && d.current_schema_hash && tools.some((t) => t.mcp_server_id === d.mcp_server_id && t.remote_name === d.remote_name));
  const blockedBy = deps.filter((d) => d.state !== "ready" && d.state !== "schema_drift");
  if (changed.length === 0) return { selection: null, tools: [], blockedBy };
  const selection = tools.map((t) => {
    const d = changed.find((c) => c.mcp_server_id === t.mcp_server_id && c.remote_name === t.remote_name);
    return d ? { ...t, schema_hash: d.current_schema_hash! } : t;
  });
  return { selection, tools: changed.map((d) => d.remote_name), blockedBy };
}

/** The agents a refused delete names (`details` of the 409: every current agent that pins a tool of the server, archived or not). */
export function referencedAgents(details: unknown): { id: string; name: string }[] {
  if (!Array.isArray(details)) return [];
  const seen = new Map<string, string>();
  for (const r of details as { agent_id?: unknown; agent_name?: unknown }[]) {
    if (r && typeof r.agent_id === "string" && typeof r.agent_name === "string" && !seen.has(r.agent_id)) seen.set(r.agent_id, r.agent_name);
  }
  return [...seen].map(([id, name]) => ({ id, name }));
}

// One sentence per refusal reason (create and edit) and per tool-test failure category.
const REASONS: Record<string, string> = {
  invalid_endpoint: "Enter a full server address, such as https://mcp.example.com/mcp.",
  destination_denied: "That address is not allowed: MCP servers must be public HTTPS addresses.",
  invalid_auth_env: "The token variable must be named MCP_TOKEN_ followed by A-Z, 0-9 or _ (max 48).",
  invalid_metadata: "The name is required (up to 128 characters) and the description can be up to 1024 characters.",
  transport_unsupported: "Only streamable HTTP servers are supported.",
};
const CATEGORIES: Record<string, string> = {
  invalid_arguments: "The tool rejected the arguments",
  timeout: "The tool did not answer in time",
  unavailable: "The server could not be reached",
  result_too_large: "The tool's result was too large",
};

/** The sentence for an error's `details` (`reason` or `category`), if it carries a known one. */
function detailSentence(details: unknown): string | undefined {
  const d = (details && typeof details === "object" ? details : {}) as { reason?: string; category?: string; message?: string };
  if (d.reason && REASONS[d.reason]) return REASONS[d.reason];
  if (d.category === "tool_error") return `The tool reported an error${d.message ? `: ${d.message}` : ""}`;
  return d.category ? CATEGORIES[d.category] : undefined;
}

export function formatApiError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    const text = detailSentence(err.details) ?? err.message;
    return err.requestId ? `${text} (Request ID: ${err.requestId})` : text;
  }
  if (err instanceof Error) return err.message;
  return fallback;
}
