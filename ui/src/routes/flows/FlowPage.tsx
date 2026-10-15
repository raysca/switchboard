import { useState } from "react";
import { Link, useLocation, useNavigate, useParams, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, asApiError, type FlowDTO, type ApiClient, type CreateFlowInput } from "../../api/client";
import { EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { FlowEditor } from "./editor/FlowEditor";
import { PublishedBanner } from "./PublishedBanner";
import { liveCalls } from "./flowStats";
import { useActiveSessions } from "./useActiveSessions";

export type FlowPageProps = { client: ApiClient; keyValue: string | null };

/** /flows/new and /flows/:flowId: the flow editor as a page of its own. */
export function FlowPage({ client, keyValue }: FlowPageProps) {
  const { flowId } = useParams();
  const [search] = useSearchParams();
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [formError, setFormError] = useState<ApiError | null>(null);
  const [dismissedAt, setDismissedAt] = useState<string | null>(null);

  // The one flow, not the list: the list is capped at 100 and a flow can sort past the cap.
  const flowQuery = useQuery({
    queryKey: ["flows", flowId],
    queryFn: () => client.getFlow(flowId!),
    enabled: !!keyValue && !!flowId,
    retry: false,
  });
  const providersQuery = useQuery({ queryKey: ["providers"], queryFn: () => client.getProviders(), enabled: !!keyValue, retry: false });
  // Placeholders and the sound list in Flow settings; the editor works without them.
  const serverConfigQuery = useQuery({ queryKey: ["server-config"], queryFn: () => client.getServerConfig(), enabled: !!keyValue, retry: false });
  const active = useActiveSessions(client, keyValue);

  const flow: FlowDTO | undefined = flowQuery.data && !flowQuery.data.archived ? flowQuery.data : undefined;
  const notFound = flowQuery.error instanceof ApiError && flowQuery.error.status === 404;

  const published = (saved: FlowDTO) => {
    setFormError(null);
    // Put the saved flow in the cache before moving to its address: a refetch is still in flight, and
    // without this the new address would flash "That flow was not found." and remount the editor.
    queryClient.setQueryData(["flows", saved.id], saved);
    void queryClient.invalidateQueries({ queryKey: ["flows"] });
    // Same address for an update: the state change is what raises the banner.
    navigate(`/flows/${encodeURIComponent(saved.id)}`, { replace: true, state: { published: saved.current_version?.number ?? 1 } });
  };
  const createMutation = useMutation({
    mutationFn: (payload: CreateFlowInput) => client.createFlow(payload),
    onSuccess: published,
    onError: (err) => setFormError(asApiError(err, "Publishing failed")),
  });
  const updateMutation = useMutation({
    mutationFn: ({ id, payload }: { id: string; payload: CreateFlowInput }) => client.updateFlow(id, payload),
    onSuccess: published,
    onError: (err) => setFormError(asApiError(err, "Publishing failed")),
  });

  if (!keyValue) return <EmptyState title="Connect to edit flows">Enter a management key above to configure voice flows.</EmptyState>;
  if (flowId && flowQuery.isPending) return <LoadingState label="Loading flow" />;
  // A failed refresh keeps the editor (and its unsaved edits): only a flow we never loaded is an error.
  if (flowId && !flowQuery.data && flowQuery.isError && !notFound) {
    return <ErrorState title="Failed to load the flow" error={flowQuery.error as ApiError} onRetry={() => void flowQuery.refetch()} reassure />;
  }
  if (flowId && !flow) {
    return (
      <section className="flow-missing">
        <p role="status">That flow was not found.</p>
        <Link to="/flows" className="action-button-small">
          Back to flows
        </Link>
      </section>
    );
  }

  const state = location.state as { published?: number } | null;
  const publishedVersion = typeof state?.published === "number" ? state.published : null;
  const providers = providersQuery.data ?? [];

  return (
    <div className="flow-page flow-page--editor">
      {flow && publishedVersion !== null && dismissedAt !== location.key && (
        <PublishedBanner
          flowId={flow.id}
          flowName={flow.name}
          version={publishedVersion}
          olderCalls={liveCalls(active.data ?? [], flow).onOlderVersion}
          onDismiss={() => setDismissedAt(location.key)}
        />
      )}
      {formError && <ErrorState title="Publishing failed" error={formError} onRetry={() => setFormError(null)} />}
      {providersQuery.isError && (
        <ErrorState title="Failed to load providers" error={providersQuery.error as ApiError} onRetry={() => void providersQuery.refetch()} reassure />
      )}
      <FlowEditor
        key={flow?.id ?? "new"}
        flow={flow ?? null}
        initialName={search.get("name") ?? undefined}
        initialPanel={search.get("panel") === "test" ? "call" : undefined}
        providers={providers}
        serverConfig={serverConfigQuery.data}
        client={client}
        keyValue={keyValue}
        pending={createMutation.isPending || updateMutation.isPending}
        onSubmit={(input) => (flow ? updateMutation.mutate({ id: flow.id, payload: input }) : createMutation.mutate(input))}
        onCancel={() => navigate("/flows")}
        callsInProgress={flow ? (({ live, onOlderVersion }) => live - onOlderVersion)(liveCalls(active.data ?? [], flow)) : 0}
      />
    </div>
  );
}
