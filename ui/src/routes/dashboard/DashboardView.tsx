import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import type { ApiClient, ApiError } from "../../api/client";
import {
  DataTable,
  EmptyState,
  ErrorState,
  LoadingState,
  Status,
} from "../../components/primitives";

export type DashboardViewProps = {
  client: ApiClient;
  keyValue: string | null;
};

function formatUptime(seconds: number): string {
  if (seconds < 60) return `${Math.floor(seconds)}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${Math.floor(seconds % 60)}s`;
  const hours = Math.floor(minutes / 60);
  const remMinutes = minutes % 60;
  if (hours < 24) return `${hours}h ${remMinutes}m`;
  const days = Math.floor(hours / 24);
  const remHours = hours % 24;
  return `${days}d ${remHours}h`;
}

function formatRelativeTime(dateStr: string): string {
  const diffSec = Math.max(0, Math.floor((Date.now() - new Date(dateStr).getTime()) / 1000));
  if (diffSec < 60) return `${diffSec}s ago`;
  const diffMin = Math.floor(diffSec / 60);
  if (diffMin < 60) return `${diffMin}m ago`;
  const diffHours = Math.floor(diffMin / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  return `${Math.floor(diffHours / 24)}d ago`;
}

export function DashboardView({ client, keyValue }: DashboardViewProps) {
  const operationsQuery = useQuery({
    queryKey: ["operations-status"],
    queryFn: () => client.getOperationsStatus(),
    enabled: !!keyValue,
    retry: false,
    refetchInterval: 10000,
  });

  const agentsQuery = useQuery({
    queryKey: ["agents"],
    queryFn: () => client.getAgents(),
    enabled: !!keyValue,
    retry: false,
  });

  const providersQuery = useQuery({
    queryKey: ["providers"],
    queryFn: () => client.getProviders(),
    enabled: !!keyValue,
    retry: false,
  });

  const credentialsQuery = useQuery({
    queryKey: ["credentials"],
    queryFn: () => client.getCredentials(),
    enabled: !!keyValue,
    retry: false,
  });

  const sessionsQuery = useQuery({
    queryKey: ["dashboard-sessions"],
    queryFn: () => client.getSessions({ limit: 5 }),
    enabled: !!keyValue,
    retry: false,
  });

  if (!keyValue) {
    return (
      <>
        <header className="section-heading">
          <h1 tabIndex={-1}>Dashboard</h1>
          <p>Operational signals for voice sessions and agents.</p>
        </header>
        <EmptyState title="Connect to view operations">
          Enter a management key to inspect operational health, session telemetry, and provider readiness.
        </EmptyState>
        <div
          className="message-panel"
          style={{ marginTop: "24px", borderColor: "#007e82" }}
        >
          <h2>First-run onboarding</h2>
          <p style={{ marginBottom: "14px" }}>
            Connect your first provider and configure a voice agent with the guided setup.
          </p>
          <Link to="/onboarding" className="action-button-small">
            Start onboarding
          </Link>
        </div>
      </>
    );
  }

  if (operationsQuery.isPending) {
    return (
      <>
        <header className="section-heading">
          <h1 tabIndex={-1}>Dashboard</h1>
          <p>Operational signals for voice sessions and agents.</p>
        </header>
        <LoadingState label="Loading operational signals" />
      </>
    );
  }

  if (operationsQuery.isError) {
    return (
      <>
        <header className="section-heading">
          <h1 tabIndex={-1}>Dashboard</h1>
          <p>Operational signals for voice sessions and agents.</p>
        </header>
        <ErrorState
          error={operationsQuery.error as ApiError}
          onRetry={() => {
            void operationsQuery.refetch();
          }}
        />
      </>
    );
  }

  const op = operationsQuery.data;
  const agents = (agentsQuery.data ?? []).filter((a) => !a.archived);
  const providers = providersQuery.data ?? [];
  const credentials = credentialsQuery.data ?? [];
  const sessions = sessionsQuery.data ?? [];

  // Readiness calculations
  const readyAgentsCount = agents.filter((a) => a.readiness?.state === "ready").length;
  const configuredAgentsCount = agents.filter((a) => a.readiness?.state === "configured").length;
  const totalAgents = agents.length;

  const totalCredentials = credentials.length;
  const validatedCredentials = credentials.filter((c) => c.validation_state === "valid").length;

  const nearCapacity = op.capacity_utilization >= 0.9 || (op.max_sessions > 0 && op.active_sessions >= op.max_sessions);

  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>Dashboard</h1>
        <p>Operational signals for voice sessions and agents.</p>
      </header>

      {/* System Health Banner */}
      <section
        className={`health-banner health-banner--${op.health}`}
        role="status"
        aria-label="System health status"
      >
        <div className="health-banner-header">
          <Status tone={op.health === "healthy" ? "good" : "bad"}>
            System {op.health.charAt(0).toUpperCase() + op.health.slice(1)}
          </Status>
          <span className="uptime-pill">Uptime: {formatUptime(op.uptime_seconds)}</span>
        </div>
        <p className="health-banner-desc">
          {op.health === "healthy"
            ? "All core components and voice runtime pipelines are operating normally."
            : op.health === "degraded"
            ? "System is operating under degraded conditions or near capacity limit."
            : "Critical operational issues or database storage failures detected."}
        </p>
      </section>

      {/* Capacity Warning Alert if near capacity */}
      {nearCapacity && (
        <section className="alert-box" role="alert" aria-label="Capacity warning">
          <h3>Capacity Warning</h3>
          <p>
            Active sessions are at {((op.capacity_utilization) * 100).toFixed(0)}% capacity ({op.active_sessions} of {op.max_sessions} maximum concurrent calls).
          </p>
        </section>
      )}

      {/* Operational Metrics Grid */}
      <section className="meta-grid" aria-label="Operational metrics">
        <div className="meta-card">
          <div className="label">Active Sessions</div>
          <div className="value">
            {op.active_sessions} / {op.max_sessions}
          </div>
          <div className="subtext">
            Utilization: {(op.capacity_utilization * 100).toFixed(1)}%
          </div>
        </div>

        <div className="meta-card">
          <div className="label">Total Sessions</div>
          <div className="value">{op.sessions_total}</div>
          <div className="subtext">
            {op.session_rate !== undefined ? `${op.session_rate.toFixed(2)}/s rate` : "Historical total"}
          </div>
        </div>

        <div className="meta-card">
          <div className="label">Error Count</div>
          <div className="value">{op.errors_total}</div>
          <div className="subtext">
            {op.turns_total > 0 ? `${op.turns_total} total turns` : "0 turns processed"}
          </div>
        </div>
      </section>

      {/* Agent & Provider Readiness Summary Cards */}
      <section className="summary-grid" aria-label="Service readiness">
        <div className="summary-card">
          <div className="summary-card-header">
            <h2>Agent Readiness</h2>
            <Link to="/agents" className="action-button-small">
              View agents
            </Link>
          </div>
          <div className="summary-card-stats">
            <span className="stat-value">{totalAgents}</span>
            <span className="stat-label">total configured</span>
          </div>
          <div style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}>
            <span className={`badge ${readyAgentsCount > 0 ? "badge--good" : "badge--neutral"}`}>
              {readyAgentsCount} Ready
            </span>
            {configuredAgentsCount > 0 && (
              <span className="badge badge--warning">
                {configuredAgentsCount} Configured
              </span>
            )}
            {totalAgents === 0 && (
              <span className="subtext">No agents created yet.</span>
            )}
          </div>
        </div>

        <div className="summary-card">
          <div className="summary-card-header">
            <h2>Provider Readiness</h2>
            <Link to="/providers" className="action-button-small">
              View providers
            </Link>
          </div>
          <div className="summary-card-stats">
            <span className="stat-value">{providers.length}</span>
            <span className="stat-label">available providers</span>
          </div>
          <div style={{ display: "flex", gap: "8px", flexWrap: "wrap", alignItems: "center" }}>
            <span className={`badge ${validatedCredentials > 0 ? "badge--good" : "badge--neutral"}`}>
              {validatedCredentials} / {totalCredentials} Validated Keys
            </span>
            {totalCredentials === 0 && (
              <span className="subtext">No provider credentials saved.</span>
            )}
          </div>
        </div>
      </section>

      {/* Recent Session History Feed */}
      <section style={{ marginTop: "32px" }} aria-label="Recent session activity">
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: "12px" }}>
          <h2 style={{ fontSize: "1.2rem", margin: 0 }}>Recent Session Activity</h2>
          <Link to="/sessions" style={{ fontSize: "0.9rem", color: "#007e82", fontWeight: 500, textDecoration: "none" }}>
            View all sessions →
          </Link>
        </div>

        {sessions.length === 0 ? (
          <EmptyState title="No recent sessions">
            No voice sessions recorded yet. Start a session or voice test to view activity traces.
          </EmptyState>
        ) : (
          <DataTable
            caption="Recent voice sessions"
            columns={["Session ID", "Status", "Turns", "Started", "Action"]}
            rows={sessions.map((s) => [
              <span key="id" className="masked-value">
                {s.id.slice(0, 8)}…
              </span>,
              <span
                key="status"
                className={`badge badge--${
                  s.status === "active"
                    ? "good"
                    : s.status === "failed"
                    ? "bad"
                    : "neutral"
                }`}
              >
                {s.status}
              </span>,
              s.turn_count,
              formatRelativeTime(s.started_at),
              <Link
                key="action"
                to={`/sessions/${s.id}`}
                className="action-button-small"
              >
                Inspect
              </Link>,
            ])}
          />
        )}
      </section>
    </>
  );
}
