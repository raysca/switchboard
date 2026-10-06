import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router";
import { type ApiClient, ApiError } from "../../api/client";
import {
  EmptyState,
  ErrorState,
  LoadingState,
  Status,
} from "../../components/primitives";
import { formatDuration, TurnTimeline } from "../../components/timeline/TurnTimeline";

export type SessionDetailViewProps = {
  client: ApiClient;
  keyValue: string | null;
};

export function SessionDetailView({ client, keyValue }: SessionDetailViewProps) {
  const { sessionId } = useParams<{ sessionId: string }>();

  const sessionQuery = useQuery({
    queryKey: ["session", sessionId],
    queryFn: () => client.getSession(sessionId!),
    enabled: !!keyValue && !!sessionId,
    retry: false,
  });

  const turnsQuery = useQuery({
    queryKey: ["session-turns", sessionId],
    queryFn: () => client.getSessionTurns(sessionId!),
    enabled: !!keyValue && !!sessionId,
    retry: false,
  });

  const tracesQuery = useQuery({
    queryKey: ["session-traces", sessionId],
    queryFn: () => client.getSessionTraces(sessionId!),
    enabled: !!keyValue && !!sessionId,
    retry: false,
  });

  if (!keyValue) {
    return (
      <>
        <header className="section-heading">
          <h1 tabIndex={-1}>Sessions</h1>
          <p>Inspect session metadata and chronological turn trace execution.</p>
        </header>
        <EmptyState title="Connect to inspect session">
          Enter a management key to inspect this session trace.
        </EmptyState>
      </>
    );
  }

  if (!sessionId) {
    return (
      <EmptyState title="Missing session ID">
        No session ID was provided. Return to <Link to="/sessions">Sessions</Link>.
      </EmptyState>
    );
  }

  if (sessionQuery.isPending) {
    return <LoadingState label="Loading session detail" />;
  }

  if (sessionQuery.isError) {
    return (
      <ErrorState
        title="Could not load session"
        error={sessionQuery.error as ApiError}
        onRetry={() => {
          void sessionQuery.refetch();
        }}
      />
    );
  }

  const session = sessionQuery.data;

  return (
    <>
      <div style={{ marginBottom: 16 }}>
        <Link to="/sessions" className="subtext" style={{ textDecoration: "none", display: "inline-flex", alignItems: "center", gap: 4 }}>
          ← Back to Sessions
        </Link>
      </div>

      <header className="section-heading">
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", flexWrap: "wrap", gap: 16 }}>
          <div>
            <h1 tabIndex={-1}>Sessions</h1>
            <p style={{ fontFamily: "IBM Plex Mono, monospace", fontSize: "0.9rem" }}>
              Session Trace: {session.id}
            </p>
          </div>
          <Status tone={session.status === "completed" ? "good" : session.status === "active" ? "neutral" : "bad"}>
            {session.status}
          </Status>
        </div>
      </header>

      {/* Metadata Overview Cards */}
      <section className="meta-grid" aria-label="Session summary metadata">
        <div className="meta-card">
          <div className="label">Agent & Snapshot</div>
          <div className="value" style={{ fontSize: "0.95rem" }}>
            ID: <code>{session.agent_id}</code>
            <br />
            Version: <code>{session.agent_version_id || "v1"}</code>
          </div>
        </div>

        <div className="meta-card">
          <div className="label">Config Hash</div>
          <div className="value">
            <code className="masked-value" style={{ fontSize: "0.78rem" }}>
              {session.config_hash ? session.config_hash.slice(0, 16) + "…" : "none"}
            </code>
          </div>
        </div>

        <div className="meta-card">
          <div className="label">Duration & Turns</div>
          <div className="value">
            {formatDuration(session.started_at, session.ended_at)} ({session.turn_count} turns)
          </div>
          <div className="subtext">
            Started: {new Date(session.started_at).toLocaleTimeString()}
          </div>
        </div>

        {session.safe_summary && (
          <div className="meta-card">
            <div className="label">Safe Summary</div>
            <div className="value" style={{ fontSize: "0.9rem", fontWeight: 400 }}>
              {session.safe_summary}
            </div>
          </div>
        )}
      </section>

      {/* Runtime MCP Tools */}
      {session.runtime_tools && session.runtime_tools.length > 0 && (
        <section style={{ marginBottom: 32 }} aria-label="Runtime MCP tools snapshot">
          <h2 style={{ fontSize: "1.2rem", fontWeight: 600, marginBottom: 12 }}>
            Captured MCP Tools ({session.runtime_tools.length})
          </h2>
          <div style={{ display: "grid", gap: 8 }}>
            {session.runtime_tools.map((t, idx) => (
              <div
                key={idx}
                className="trace-item"
                style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}
              >
                <div>
                  <strong>🔧 {t.remote_name}</strong>
                  <span className="subtext" style={{ marginLeft: 8 }}>
                    Server: <code>{t.mcp_server_id}</code>
                  </span>
                </div>
                <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
                  <code className="masked-value" style={{ fontSize: "0.75rem" }}>
                    {t.schema_hash.slice(0, 8)}…
                  </code>
                  <span className={`badge ${t.required ? "badge--good" : "badge--neutral"}`}>
                    {t.required ? "Required" : "Optional"}
                  </span>
                </div>
              </div>
            ))}
          </div>
        </section>
      )}

      {/* Turn Timeline */}
      <section aria-label="Chronological turn timeline">
        <h2 style={{ fontSize: "1.3rem", fontWeight: 600, marginBottom: 16 }}>
          Conversational Turns & Correlated Traces
        </h2>

        {turnsQuery.isError ? (
          <ErrorState
            title="Could not load turn timeline"
            error={turnsQuery.error as ApiError}
            onRetry={() => {
              void turnsQuery.refetch();
            }}
          />
        ) : (
          <TurnTimeline
            turns={turnsQuery.data ?? []}
            traces={tracesQuery.data ?? null}
            loading={turnsQuery.isPending}
          />
        )}
      </section>
    </>
  );
}
