import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { type ApiClient, ApiError } from "../../api/client";
import {
  DataTable,
  EmptyState,
  ErrorState,
  FormField,
  LoadingState,
  Status,
} from "../../components/primitives";
import { formatDuration } from "../../components/timeline/TurnTimeline";

export type SessionsViewProps = {
  client: ApiClient;
  keyValue: string | null;
};

export function SessionsView({ client, keyValue }: SessionsViewProps) {
  const [limit, setLimit] = useState(10);
  const [offset, setOffset] = useState(0);
  const [statusFilter, setStatusFilter] = useState("");
  const [agentFilter, setAgentFilter] = useState("");

  const sessionsQuery = useQuery({
    queryKey: ["sessions", limit, offset, statusFilter, agentFilter],
    queryFn: () =>
      client.getSessions({
        limit,
        offset,
        status: statusFilter || undefined,
        agent_id: agentFilter.trim() || undefined,
      }),
    enabled: !!keyValue,
    retry: false,
  });

  const agentsQuery = useQuery({
    queryKey: ["agents"],
    queryFn: () => client.getAgents(),
    enabled: !!keyValue,
    retry: false,
  });

  if (!keyValue) {
    return (
      <>
        <header className="section-heading">
          <h1 tabIndex={-1}>Sessions</h1>
          <p>Inspect conversational voice sessions, turn latencies, and execution traces.</p>
        </header>
        <EmptyState title="Connect to inspect sessions">
          Enter a management key to inspect voice sessions and execution traces.
        </EmptyState>
      </>
    );
  }

  const agentNameMap = new Map<string, string>();
  if (agentsQuery.data) {
    for (const a of agentsQuery.data) {
      agentNameMap.set(a.id, a.name);
    }
  }

  const sessions = sessionsQuery.data ?? [];

  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>Sessions</h1>
        <p>Inspect conversational voice sessions, turn latencies, and execution traces.</p>
      </header>

      {/* Filter Bar */}
      <section className="filter-bar" aria-label="Session filters">
        <label className="form-field" htmlFor="status-filter">
          <span>Status</span>
          <select
            id="status-filter"
            value={statusFilter}
            onChange={(e) => {
              setStatusFilter(e.target.value);
              setOffset(0);
            }}
          >
            <option value="">All statuses</option>
            <option value="active">Active</option>
            <option value="completed">Completed</option>
            <option value="failed">Failed</option>
            <option value="terminated">Terminated</option>
          </select>
        </label>

        <FormField
          id="agent-filter"
          label="Filter by Agent ID"
          value={agentFilter}
          placeholder="e.g. agent-1"
          onChange={(e) => {
            setAgentFilter(e.target.value);
            setOffset(0);
          }}
        />

        {(statusFilter || agentFilter) && (
          <button
            type="button"
            className="action-button-small"
            style={{ marginBottom: 2 }}
            onClick={() => {
              setStatusFilter("");
              setAgentFilter("");
              setOffset(0);
            }}
          >
            Clear filters
          </button>
        )}
      </section>

      {sessionsQuery.isPending ? (
        <LoadingState label="Loading sessions" />
      ) : sessionsQuery.isError ? (
        <ErrorState
          title="Could not load sessions"
          error={sessionsQuery.error as ApiError}
          onRetry={() => {
            void sessionsQuery.refetch();
          }}
        />
      ) : sessions.length === 0 ? (
        <EmptyState title="No sessions found">
          {statusFilter || agentFilter
            ? "No sessions match the specified filters."
            : "No voice sessions have been recorded yet."}
        </EmptyState>
      ) : (
        <>
          <DataTable
            caption="Recorded voice sessions"
            columns={["Session ID", "Agent", "Status", "Turns", "Duration", "Started At", "Actions"]}
            rows={sessions.map((s) => [
              <Link key="id" to={`/sessions/${s.id}`} style={{ fontFamily: "IBM Plex Mono, monospace", fontWeight: 500 }}>
                {s.id.length > 12 ? `${s.id.slice(0, 12)}…` : s.id}
              </Link>,
              <span key="agent">
                {agentNameMap.get(s.agent_id) ?? s.agent_id}
                <span className="subtext" style={{ display: "block" }}>
                  {s.agent_version_id ? `ver: ${s.agent_version_id.slice(0, 8)}` : "v1"}
                </span>
              </span>,
              <Status
                key="status"
                tone={s.status === "completed" ? "good" : s.status === "active" ? "neutral" : "bad"}
              >
                {s.status}
              </Status>,
              s.turn_count,
              formatDuration(s.started_at, s.ended_at),
              new Date(s.started_at).toLocaleString(),
              <Link key="action" to={`/sessions/${s.id}`} className="action-button-small">
                Inspect Trace
              </Link>,
            ])}
          />

          {/* Pagination Bar */}
          <div className="pagination-bar" aria-label="Pagination">
            <span>
              Showing {sessions.length > 0 ? offset + 1 : 0}–{offset + sessions.length} sessions
            </span>
            <div className="pagination-controls">
              <label htmlFor="limit-select" style={{ fontSize: "0.85rem", marginRight: 8 }}>
                Per page:
                <select
                  id="limit-select"
                  value={limit}
                  style={{ marginLeft: 6, padding: "2px 6px" }}
                  onChange={(e) => {
                    setLimit(Number(e.target.value));
                    setOffset(0);
                  }}
                >
                  <option value={10}>10</option>
                  <option value={25}>25</option>
                  <option value={50}>50</option>
                </select>
              </label>

              <button
                type="button"
                className="action-button-small"
                disabled={offset === 0}
                onClick={() => setOffset(Math.max(0, offset - limit))}
              >
                Previous
              </button>
              <button
                type="button"
                className="action-button-small"
                disabled={sessions.length < limit}
                onClick={() => setOffset(offset + limit)}
              >
                Next
              </button>
            </div>
          </div>
        </>
      )}
    </>
  );
}
