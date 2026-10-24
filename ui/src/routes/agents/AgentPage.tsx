import { useState } from "react";
import { Link, useLocation, useNavigate, useParams, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, asApiError, type AgentDTO, type ApiClient, type CreateAgentInput } from "../../api/client";
import { EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { AgentEditor } from "./editor/AgentEditor";
import { PublishedBanner } from "./PublishedBanner";
import { liveCalls } from "./agentStats";
import { useActiveSessions } from "./useActiveSessions";

export type AgentPageProps = { client: ApiClient; keyValue: string | null };

/** /agents/new and /agents/:agentId: the agent editor as a page of its own. */
export function AgentPage({ client, keyValue }: AgentPageProps) {
  const { agentId } = useParams();
  const [search] = useSearchParams();
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [formError, setFormError] = useState<ApiError | null>(null);
  const [dismissedAt, setDismissedAt] = useState<string | null>(null);

  // The one agent, not the list: the list is capped at 100 and an agent can sort past the cap.
  const agentQuery = useQuery({
    queryKey: ["agents", agentId],
    queryFn: () => client.getAgent(agentId!),
    enabled: !!keyValue && !!agentId,
    retry: false,
  });
  const providersQuery = useQuery({ queryKey: ["providers"], queryFn: () => client.getProviders(), enabled: !!keyValue, retry: false });
  // Placeholders and the sound list in Agent settings; the editor works without them.
  const serverConfigQuery = useQuery({ queryKey: ["server-config"], queryFn: () => client.getServerConfig(), enabled: !!keyValue, retry: false });
  const active = useActiveSessions(client, keyValue);

  const agent: AgentDTO | undefined = agentQuery.data && !agentQuery.data.archived ? agentQuery.data : undefined;
  const notFound = agentQuery.error instanceof ApiError && agentQuery.error.status === 404;

  const published = (saved: AgentDTO) => {
    setFormError(null);
    // Put the saved agent in the cache before moving to its address: a refetch is still in flight, and
    // without this the new address would flash "That agent was not found." and remount the editor.
    queryClient.setQueryData(["agents", saved.id], saved);
    void queryClient.invalidateQueries({ queryKey: ["agents"] });
    // Same address for an update: the state change is what raises the banner.
    navigate(`/agents/${encodeURIComponent(saved.id)}`, { replace: true, state: { published: saved.current_version?.number ?? 1 } });
  };
  const createMutation = useMutation({
    mutationFn: (payload: CreateAgentInput) => client.createAgent(payload),
    onSuccess: published,
    onError: (err) => setFormError(asApiError(err, "Publishing failed")),
  });
  const updateMutation = useMutation({
    mutationFn: ({ id, payload }: { id: string; payload: CreateAgentInput }) => client.updateAgent(id, payload),
    onSuccess: published,
    onError: (err) => setFormError(asApiError(err, "Publishing failed")),
  });

  if (!keyValue) return <EmptyState title="Connect to edit agents">Enter a management key above to configure voice agents.</EmptyState>;
  if (agentId && agentQuery.isPending) return <LoadingState label="Loading agent" />;
  // A failed refresh keeps the editor (and its unsaved edits): only an agent we never loaded is an error.
  if (agentId && !agentQuery.data && agentQuery.isError && !notFound) {
    return <ErrorState title="Failed to load the agent" error={agentQuery.error as ApiError} onRetry={() => void agentQuery.refetch()} reassure />;
  }
  if (agentId && !agent) {
    return (
      <section className="agent-missing">
        <p role="status">That agent was not found.</p>
        <Link to="/agents" className="action-button-small">
          Back to agents
        </Link>
      </section>
    );
  }

  const state = location.state as { published?: number } | null;
  const publishedVersion = typeof state?.published === "number" ? state.published : null;
  const providers = providersQuery.data ?? [];

  return (
    <div className="agent-page agent-page--editor">
      {agent && publishedVersion !== null && dismissedAt !== location.key && (
        <PublishedBanner
          agentId={agent.id}
          agentName={agent.name}
          version={publishedVersion}
          olderCalls={liveCalls(active.data ?? [], agent).onOlderVersion}
          onDismiss={() => setDismissedAt(location.key)}
        />
      )}
      {formError && <ErrorState title="Publishing failed" error={formError} onRetry={() => setFormError(null)} />}
      {providersQuery.isError && (
        <ErrorState title="Failed to load providers" error={providersQuery.error as ApiError} onRetry={() => void providersQuery.refetch()} reassure />
      )}
      <AgentEditor
        key={agent?.id ?? "new"}
        agent={agent ?? null}
        initialName={search.get("name") ?? undefined}
        initialPanel={search.get("panel") === "test" ? "call" : undefined}
        providers={providers}
        serverConfig={serverConfigQuery.data}
        client={client}
        keyValue={keyValue}
        pending={createMutation.isPending || updateMutation.isPending}
        onSubmit={(input) => (agent ? updateMutation.mutate({ id: agent.id, payload: input }) : createMutation.mutate(input))}
        onCancel={() => navigate("/agents")}
        onRestored={published}
        callsInProgress={agent ? (({ live, onOlderVersion }) => live - onOlderVersion)(liveCalls(active.data ?? [], agent)) : 0}
      />
    </div>
  );
}
