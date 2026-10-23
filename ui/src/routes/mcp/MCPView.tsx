import { useState, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ApiClient, ApiError, CreateMCPServerInput } from "../../api/client";
import { EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { ConnectServerDialog } from "./ConnectServerDialog";
import { ServerDetail } from "./ServerDetail";
import { ServerList } from "./ServerList";
import { drifts, formatApiError } from "./mcpStats";

export type MCPViewProps = { client: ApiClient; keyValue: string | null };

// GET /agents returns at most this many agents; a list that reaches it may be missing some.
const AGENT_CAP = 100;

/** MCP servers on the left, the selected one on the right: its tools, and a review of changed schemas. */
export function MCPView({ client, keyValue }: MCPViewProps) {
  const { serverId } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [connecting, setConnecting] = useState(false);
  const [connectError, setConnectError] = useState<string | null>(null);

  const serversQuery = useQuery({ queryKey: ["mcp-servers"], queryFn: () => client.getMCPServers(), enabled: !!keyValue, retry: false });
  const agentsQuery = useQuery({ queryKey: ["agents"], queryFn: () => client.getAgents(), enabled: !!keyValue, retry: false });
  const servers = serversQuery.data ?? [];
  // The list endpoint has no tool counts, so each server's detail is read; the selected one reuses the same cache entry.
  const details = useQueries({
    queries: servers.map((s) => ({ queryKey: ["mcp-server", s.id], queryFn: () => client.getMCPServer(s.id), retry: false })),
  });

  const connect = useMutation({
    mutationFn: (input: CreateMCPServerInput) => client.createMCPServer(input),
    onSuccess: (created) => {
      setConnecting(false);
      setConnectError(null);
      void queryClient.invalidateQueries({ queryKey: ["mcp-servers"] });
      navigate(`/mcp-servers/${encodeURIComponent(created.id)}`);
    },
    onError: (err) => setConnectError(formatApiError(err, "Could not connect the server")),
  });

  const head = (
    <header className="page-head">
      <h1 tabIndex={-1}>MCP tools</h1>
      <span className="subtext">Remote servers whose tools agents can call mid-conversation.</span>
      <span className="grow" />
      {keyValue && (
        <button
          type="button"
          className="action-button"
          onClick={() => {
            setConnectError(null);
            setConnecting(true);
          }}
        >
          Connect server
        </button>
      )}
    </header>
  );
  const dialog = connecting && <ConnectServerDialog pending={connect.isPending} error={connectError} onSubmit={(input) => connect.mutate(input)} onCancel={() => setConnecting(false)} />;

  // The page uses the whole workspace, like Providers: the server list as a column, the selected server beside it.
  const page = (body: ReactNode) => (
    <>
      <div className="mcp-page">
        {head}
        {body}
      </div>
      {dialog}
    </>
  );

  if (!keyValue) {
    return page(<EmptyState title="Connect to see MCP tools">Enter a management key above to see MCP servers and the tools they offer.</EmptyState>);
  }
  if (serversQuery.isPending) return page(<LoadingState label="Loading MCP servers" />);
  if (serversQuery.isError) {
    return page(<ErrorState title="Could not load MCP servers" error={serversQuery.error as ApiError} onRetry={() => void serversQuery.refetch()} reassure />);
  }
  if (servers.length === 0) {
    return page(
      <section className="message-panel">
        <h2>No MCP servers yet.</h2>
        <p>Connect a remote MCP server by its endpoint to let agents call its tools. Tokens are read only from MCP_TOKEN_* environment variables.</p>
      </section>,
    );
  }

  const agents = agentsQuery.data;
  const now = Date.now();
  const selectedIndex = serverId ? servers.findIndex((s) => s.id === serverId) : 0;
  const selected = selectedIndex >= 0 ? servers[selectedIndex] : undefined;
  const selectedDetail = selectedIndex >= 0 ? details[selectedIndex] : undefined;
  const toolCounts = Object.fromEntries(servers.map((s, i) => [s.id, details[i]?.data?.discovery?.tools.length ?? (details[i]?.isSuccess ? 0 : undefined)]));
  const changed = Object.fromEntries(servers.map((s) => [s.id, agents ? new Set(drifts(agents, s.id).filter((d) => d.kind === "changed").map((d) => d.dep.remote_name)).size : 0]));

  return page(
      <div className="mcp-layout">
        <nav aria-label="MCP servers" className="mcp-side">
          <ServerList servers={servers} toolCounts={toolCounts} changed={changed} selectedId={selected?.id} now={now} />
        </nav>
        <div className="mcp-main">
          {!selected ? (
            <section className="agent-missing">
              <p role="status">That MCP server was not found.</p>
              <Link to="/mcp-servers" className="action-button-small">
                Back to MCP tools
              </Link>
            </section>
          ) : selectedDetail?.isPending ? (
            <LoadingState label="Loading MCP server" />
          ) : selectedDetail?.isError || !selectedDetail?.data ? (
            <ErrorState title="Could not load the server" error={selectedDetail?.error as ApiError} onRetry={() => void selectedDetail?.refetch()} reassure />
          ) : (
            <ServerDetail
              key={selected.id}
              client={client}
              server={selectedDetail.data}
              agents={agents}
              agentsFailed={agentsQuery.isError}
              agentsCapped={(agents?.length ?? 0) >= AGENT_CAP}
              now={now}
              onDeleted={() => navigate("/mcp-servers")}
            />
          )}
        </div>
      </div>,
  );
}
