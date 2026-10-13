import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import type { ApiClient, ApiError } from "../../api/client";
import {
  DataTable,
  EmptyState,
  ErrorState,
  LoadingState,
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

  const flowsQuery = useQuery({
    queryKey: ["flows"],
    queryFn: () => client.getFlows(),
    enabled: !!keyValue,
    retry: false,
  });

  const providersQuery = useQuery({
    queryKey: ["providers"],
    queryFn: () => client.getProviders(),
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
          <p>Operational signals for voice sessions and flows.</p>
        </header>
        <EmptyState title="Connect to view operations">
          Enter a management key to inspect operational health, session telemetry, and provider readiness.
        </EmptyState>
        <div className="message-panel">
          <h2>First-run onboarding</h2>
          <p>
            Connect your first provider and configure a voice flow with the guided setup.
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
          <p>Operational signals for voice sessions and flows.</p>
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
          <p>Operational signals for voice sessions and flows.</p>
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
  const flows = (flowsQuery.data ?? []).filter((a) => !a.archived);
  const providers = providersQuery.data ?? [];
  const sessions = sessionsQuery.data ?? [];

  // Readiness calculations
  const readyFlowsCount = flows.filter((a) => a.readiness?.state === "ready").length;
  const totalFlows = flows.length;

  const detectedProviders = providers.filter((p) => p.detected).length;

  const nearCapacity = op.capacity_utilization >= 0.9 || (op.max_sessions > 0 && op.active_sessions >= op.max_sessions);

  const tone = op.health === "healthy" ? "ok" : op.health === "degraded" ? "warn" : "bad";
  const healthText =
    op.health === "healthy"
      ? "All core components and voice runtime pipelines are operating normally."
      : op.health === "degraded"
        ? "System is operating under degraded conditions or near capacity limit."
        : "Critical operational issues or database storage failures detected.";

  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>Dashboard</h1>
        <p>Operational signals for voice sessions and flows.</p>
      </header>

      <section className="state-line" role="status" aria-label="System health status">
        <p className="state-line__headline">
          <span className="lamp" data-tone={tone} aria-hidden="true" />
          System {op.health}. {op.active_sessions} of {op.max_sessions} sessions active.
        </p>
        <p className="state-line__desc">{healthText}</p>
      </section>

      {nearCapacity && (
        <section className="alert-box" role="alert" aria-label="Capacity warning">
          <h3>Capacity warning</h3>
          <p>
            Active sessions are at {(op.capacity_utilization * 100).toFixed(0)}% capacity ({op.active_sessions} of {op.max_sessions} maximum concurrent calls).
          </p>
        </section>
      )}

      <dl className="figures" aria-label="Operational metrics">
        <div>
          <dt>Total sessions</dt>
          <dd>{op.sessions_total}</dd>
        </div>
        <div>
          <dt>Turns</dt>
          <dd>{op.turns_total}</dd>
        </div>
        <div>
          <dt>Errors</dt>
          <dd>{op.errors_total}</dd>
        </div>
        <div>
          <dt>Utilization</dt>
          <dd>{(op.capacity_utilization * 100).toFixed(1)}%</dd>
        </div>
        <div>
          <dt>Uptime</dt>
          <dd>{formatUptime(op.uptime_seconds)}</dd>
        </div>
      </dl>

      <ul className="ledger" aria-label="Service readiness">
        <li className="ledger__row">
          <span className="lamp" data-tone={readyFlowsCount > 0 ? "ok" : "idle"} aria-hidden="true" />
          <span className="ledger__name">Flows</span>
          <span className="ledger__value">
            {readyFlowsCount} of {totalFlows} ready
          </span>
          {totalFlows === 0 && <span className="subtext">No flows created yet.</span>}
          <Link to="/flows" className="ledger__link">
            View flows
          </Link>
        </li>
        <li className="ledger__row">
          <span className="lamp" data-tone={detectedProviders > 0 ? "ok" : "bad"} aria-hidden="true" />
          <span className="ledger__name">Providers</span>
          <span className="ledger__value">
            {detectedProviders} of {providers.length} detected
          </span>
          {detectedProviders === 0 && <span className="subtext">No provider keys found in the server environment.</span>}
          <Link to="/providers" className="ledger__link">
            View providers
          </Link>
        </li>
      </ul>

      <section className="section-block" aria-label="Recent calls">
        <div className="section-row">
          <h2>Recent calls</h2>
          <Link to="/sessions" className="ledger__link">
            View all calls
          </Link>
        </div>

        {sessions.length === 0 ? (
          <EmptyState title="No recent calls">
            No calls recorded yet. Test a flow to see one here.
          </EmptyState>
        ) : (
          <DataTable
            caption="Recent voice sessions"
            columns={["Session ID", "Status", "Turns", "Started", "Action"]}
            rows={sessions.map((s) => [
              <span key="id" className="masked-value">
                {s.id.slice(0, 8)}…
              </span>,
              <span key="status" className={`badge badge--${s.status === "active" ? "good" : s.status === "failed" ? "bad" : "neutral"}`}>
                {s.status}
              </span>,
              s.turn_count,
              formatRelativeTime(s.started_at),
              <Link key="action" to={`/sessions/${s.id}`} className="action-button-small">
                Inspect
              </Link>,
            ])}
          />
        )}
      </section>
    </>
  );
}
