import { useState } from "react";
import { Link, useNavigate } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { asApiError, type FlowDTO, type ApiClient, type ApiError } from "../../api/client";
import { EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { ArchiveFlowDialog } from "./ArchiveFlowDialog";
import { NewFlowDialog } from "./NewFlowDialog";
import { canTest, liveCalls, readiness, routeCount, stepCount, toolCount } from "./flowStats";
import { useActiveSessions } from "./useActiveSessions";

/** The management API returns at most this many flows (GET /flows?limit=100). */
const LIST_LIMIT = 100;
const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

export type FlowsViewProps = { client: ApiClient; keyValue: string | null };
type Dialog = { kind: "new" } | { kind: "archive"; flow: FlowDTO } | null;

export function FlowsView({ client, keyValue }: FlowsViewProps) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [filter, setFilter] = useState("");
  const [dialog, setDialog] = useState<Dialog>(null);
  const [actionError, setActionError] = useState<ApiError | null>(null);

  const flowsQuery = useQuery({ queryKey: ["flows"], queryFn: () => client.getFlows(), enabled: !!keyValue, retry: false });
  const active = useActiveSessions(client, keyValue);
  const providersQuery = useQuery({ queryKey: ["providers"], queryFn: client.getProviders, enabled: !!keyValue, retry: false });
  const sessions = active.data ?? [];

  const refresh = () => void queryClient.invalidateQueries({ queryKey: ["flows"] });
  const toggleMutation = useMutation({
    // Only `enabled`, never a config: the server publishes a version only when a config is sent.
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) => client.updateFlow(id, { enabled }),
    onSuccess: () => {
      setActionError(null);
      refresh();
    },
    onError: (err) => setActionError(asApiError(err, "Could not change whether the flow is enabled")),
  });
  const copyMutation = useMutation({
    mutationFn: ({ id, name }: { id: string; name?: string; open?: boolean }) => client.duplicateFlow(id, name ? { name } : undefined),
    onSuccess: (copy, vars) => {
      setActionError(null);
      setDialog(null);
      refresh();
      // Hand the copy to the editor's cache first, or it would look for a flow the list has not got yet.
      if (vars.open) queryClient.setQueryData(["flows", copy.id], copy);
      if (vars.open) navigate(`/flows/${encodeURIComponent(copy.id)}`);
    },
    onError: (err) => setActionError(asApiError(err, "Copy failed")),
  });
  const archiveMutation = useMutation({
    mutationFn: (id: string) => client.archiveFlow(id),
    onSuccess: () => {
      setActionError(null);
      setDialog(null);
      refresh();
    },
    onError: (err) => setActionError(asApiError(err, "Archive failed")),
  });

  const all = flowsQuery.data ?? [];
  const flows = all.filter((f) => !f.archived);
  const q = filter.trim().toLowerCase();
  const shown = q ? flows.filter((f) => f.name.toLowerCase().includes(q)) : flows;
  const enabledCount = flows.filter((f) => f.enabled).length;
  const notReady = flows.filter((f) => f.enabled && readiness(f).tone === "bad").length;
  const onCalls = flows.reduce((n, f) => n + liveCalls(sessions, f).live, 0);
  const ready = (providersQuery.data ?? []).filter((p) => p.detected).map((p) => p.display_name);
  const readyLine =
    ready.length === 0
      ? null
      : `${ready.length === 1 ? ready[0] : `${ready.slice(0, -1).join(", ")} and ${ready[ready.length - 1]}`} ${ready.length === 1 ? "is" : "are"} ready to use.`;
  const openDialog = (d: Dialog) => {
    setActionError(null);
    setDialog(d);
  };

  return (
    <>
      <header className="page-head">
        <h1 tabIndex={-1}>Flows</h1>
        {flows.length > 0 && (
          <span className="subtext" role="status">
            {enabledCount} enabled · {notReady} not ready{onCalls > 0 ? ` · ${plural(onCalls, "call", "calls")} in progress` : ""}
          </span>
        )}
        <span className="grow" />
        {flows.length > 0 && (
          <>
            <label className="form-field flows-filter" htmlFor="flows-filter">
              <span className="visually-hidden">Filter flows</span>
              <input id="flows-filter" type="search" placeholder="Filter by name" value={filter} onChange={(e) => setFilter(e.target.value)} />
            </label>
            <button type="button" className="action-button" onClick={() => openDialog({ kind: "new" })}>
              New flow
            </button>
          </>
        )}
      </header>

      {!keyValue ? (
        <EmptyState title="Connect to manage flows">Enter a management key above to configure voice flows.</EmptyState>
      ) : flowsQuery.isPending ? (
        <LoadingState label="Loading flows" />
      ) : flowsQuery.isError && !flowsQuery.data ? (
        <ErrorState title="Failed to load flows" error={flowsQuery.error as ApiError} onRetry={() => void flowsQuery.refetch()} />
      ) : flows.length === 0 ? (
        <section aria-labelledby="no-flows" className="flows-empty">
          <div className="flows-empty__lead">
            <h2 id="no-flows">No flows yet.</h2>
            <p>A flow is one voice line. Start with a single step that answers the phone; add more steps and routes when you need them.</p>
            {providersQuery.isSuccess &&
              (readyLine ? (
                <p className="subtext flows-empty__ready">
                  <span className="lamp" data-tone="ok" aria-hidden="true" /> {readyLine}
                </p>
              ) : (
                <p className="subtext">
                  No provider is set up yet. <Link to="/providers">See providers</Link>
                </p>
              ))}
            <div className="row">
              <button type="button" className="action-button" onClick={() => openDialog({ kind: "new" })}>
                New flow
              </button>
              <Link to="/onboarding" className="action-button-small">
                Use guided setup
              </Link>
            </div>
          </div>
          <dl className="flows-empty__terms">
            <div><dt>Flow</dt><dd>What callers reach. You create it, test it and publish it in versions.</dd></div>
            <div><dt>Steps</dt><dd>Parts of one flow, like Reception or Billing. Each has its own instructions, model, voice and tools. Up to 8.</dd></div>
            <div><dt>Routes</dt><dd>Plain-language conditions that move the caller from one step to another.</dd></div>
            <div><dt>Flow settings</dt><dd>Shared by the whole call: listening, turn-taking, memory, recording.</dd></div>
          </dl>
        </section>
      ) : (
        <div className="flows-view">
          {actionError && !dialog && <ErrorState title="Flow action failed" error={actionError} onRetry={() => setActionError(null)} />}
          {all.length >= LIST_LIMIT && <p className="subtext" role="note">Showing the first {LIST_LIMIT} flows.</p>}
          {shown.length === 0 && <p className="subtext">No flow matches “{filter.trim()}”.</p>}

          <ul className="flow-list">
            {shown.map((f) => {
              const r = readiness(f);
              const live = liveCalls(sessions, f);
              return (
                <li key={f.id} className="flow-row">
                  <div className="flow-row__main">
                    <div className="flow-row__title">
                      <span className="lamp" data-tone={r.tone === "idle" ? undefined : r.tone} aria-hidden="true" />
                      <Link to={`/flows/${encodeURIComponent(f.id)}`}>{f.name}</Link>
                      <span className="chip">v{f.current_version?.number ?? 1}</span>
                      <span className="flow-row__state">{r.label}</span>
                    </div>
                    {f.description && <p className="subtext">{f.description}</p>}
                    {f.enabled && f.readiness.reasons.length > 0 && (
                      <div className="reasons-list">
                        {f.readiness.reasons.map((reason) => (
                          <span key={reason} className="reason">
                            {reason}
                          </span>
                        ))}
                      </div>
                    )}
                  </div>
                  <div className="flow-row__figures">
                    <p>
                      {plural(stepCount(f), "step", "steps")} · {plural(routeCount(f), "route", "routes")} · {plural(toolCount(f), "tool", "tools")}
                    </p>
                    {live.live > 0 && (
                      <p>
                        <Link to={`/sessions?flow=${encodeURIComponent(f.id)}&outcome=live`}>
                          {plural(live.live, "call", "calls")} live{live.onOlderVersion > 0 ? `, ${live.onOlderVersion} on an earlier version` : ""}
                        </Link>
                      </p>
                    )}
                  </div>
                  <div className="flow-row__actions">
                    {canTest(f) && (
                      <Link to={`/flows/${encodeURIComponent(f.id)}?panel=test`} className="action-button-small" aria-label={`Test ${f.name}`}>
                        Test
                      </Link>
                    )}
                    <label className="check-row">
                      <input
                        type="checkbox"
                        aria-label={`${f.name} enabled`}
                        checked={f.enabled}
                        disabled={toggleMutation.isPending}
                        onChange={(e) => toggleMutation.mutate({ id: f.id, enabled: e.target.checked })}
                      />
                      Enabled
                    </label>
                    <button type="button" className="action-button-small" aria-label={`Duplicate ${f.name}`} disabled={copyMutation.isPending} onClick={() => copyMutation.mutate({ id: f.id })}>
                      Duplicate
                    </button>
                    <button type="button" className="action-button-small action-button-danger" aria-label={`Archive ${f.name}`} onClick={() => openDialog({ kind: "archive", flow: f })}>
                      Archive
                    </button>
                  </div>
                </li>
              );
            })}
          </ul>
        </div>
      )}

      {dialog?.kind === "new" && (
        <NewFlowDialog
          flows={flows.map((f) => ({ id: f.id, name: f.name }))}
          pending={copyMutation.isPending}
          error={actionError?.message ?? null}
          onBlank={(name) => navigate(`/flows/new?name=${encodeURIComponent(name)}`)}
          onCopy={(id, name) => copyMutation.mutate({ id, name, open: true })}
          onCancel={() => setDialog(null)}
        />
      )}
      {dialog?.kind === "archive" && (
        <ArchiveFlowDialog
          name={dialog.flow.name}
          pending={archiveMutation.isPending}
          error={actionError?.message ?? null}
          onConfirm={() => archiveMutation.mutate(dialog.flow.id)}
          onCancel={() => setDialog(null)}
        />
      )}
    </>
  );
}
