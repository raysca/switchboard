import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import type { ApiClient, ApiError } from "../../api/client";
import { ReplyChart } from "./ReplyChart";
import {
  DataTable,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../../components/primitives";

/** The dashboard loads this many recent calls once: the first few fill the table, all of them draw the reply-time chart. */
const RECENT_LIMIT = 40;

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

  const sessionsQuery = useQuery({
    queryKey: ["dashboard-sessions"],
    queryFn: () => client.getSessions({ limit: RECENT_LIMIT }),
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
        <div className="message-panel">
          <h2>First run</h2>
          <p>
            Connect your first provider and configure a voice agent with the guided setup.
          </p>
          <Link to="/onboarding" className="action-button-small">
            Start first run
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
          reassure
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
  const sessions = sessionsQuery.data ?? [];
  const recent = sessions.slice(0, 5);

  // Readiness calculations
  const readyAgentsCount = agents.filter((a) => a.readiness?.state === "ready").length;
  const totalAgents = agents.length;

  const detectedProviders = providers.filter((p) => p.detected).length;
  const detectedList = providers.filter((p) => p.detected);
  const known = providersQuery.isSuccess && agentsQuery.isSuccess;
  const stepsLeft = (detectedList.length === 0 ? 1 : 0) + (agents.length === 0 ? 1 : 0);

  const nearCapacity = op.capacity_utilization >= 0.9 || (op.max_sessions > 0 && op.active_sessions >= op.max_sessions);
  const utilPct = Math.round(op.capacity_utilization * 100);

  const tone = op.health === "healthy" ? "ok" : op.health === "degraded" ? "warn" : "bad";
  const healthText =
    op.health === "degraded"
      ? "System is operating under degraded conditions or near capacity limit."
      : "Critical operational issues or database storage failures detected.";
  const turnsNote = op.sessions_total > 0 ? `${(op.turns_total / op.sessions_total).toFixed(1)} per call` : "No calls yet";
  const errorsNote = op.sessions_total > 0 ? `${(op.errors_total / op.sessions_total).toFixed(2)} per call` : "No calls yet";

  return (
    <>
      <header className="dash-head">
        <div>
          <h1 tabIndex={-1}>Dashboard</h1>
          <p>Operational signals for voice sessions and agents.</p>
        </div>
        <p className="health-pill" data-tone={tone} role="status">
          <span className="lamp" aria-hidden="true" />
          System {op.health} · up {formatUptime(op.uptime_seconds)}
        </p>
      </header>

      {tone !== "ok" && <p className="subtext">{healthText}</p>}

      {known && stepsLeft > 0 && (
        <section aria-labelledby="setup-title" className="setup-checklist">
          <h2 id="setup-title">{stepsLeft === 2 ? "Two steps to your first call." : "One step to your first call."}</h2>
          <ol aria-label="Setup checklist">
            <li data-done={detectedList.length > 0 || undefined}>
              <span className="setup-checklist__mark" aria-hidden="true">
                {detectedList.length > 0 ? "✓" : "1"}
              </span>
              <span>
                <b>Set a provider key</b>
                <span className="subtext">
                  {detectedList.length > 0 ? `${detectedList.map((p) => p.display_name).join(", ")} ${detectedList.length === 1 ? "is" : "are"} ready.` : "Set one of the provider variables on the server and restart it."}
                </span>
              </span>
              {detectedList.length === 0 && (
                <Link to="/providers" className="action-button-small">
                  See providers
                </Link>
              )}
            </li>
            <li data-done={agents.length > 0 || undefined}>
              <span className="setup-checklist__mark" aria-hidden="true">
                {agents.length > 0 ? "✓" : "2"}
              </span>
              <span>
                <b>Create your first agent</b>
                <span className="subtext">Guided setup takes about three minutes.</span>
              </span>
              {agents.length === 0 && (
                <Link to="/onboarding" className="action-button-small">
                  Start first run
                </Link>
              )}
            </li>
          </ol>
        </section>
      )}

      {nearCapacity && (
        <section className="alert-box" role="alert" aria-label="Capacity warning">
          <h3>Capacity warning</h3>
          <p>
            Active sessions are at {utilPct}% capacity ({op.active_sessions} of {op.max_sessions} maximum concurrent calls).
          </p>
        </section>
      )}

      <section className="dash-metrics" aria-label="Operational metrics">
        <div className="capacity-card">
          <div className="capacity-card__top">
            <span className="kpi__label">Live calls</span>
            <span>{utilPct}% of capacity</span>
          </div>
          <p className="capacity-card__value">
            <b>{op.active_sessions}</b> / {op.max_sessions} concurrent
          </p>
          <div className="meter" data-tone={nearCapacity ? "warn" : undefined} role="meter" aria-label="Capacity in use" aria-valuenow={utilPct} aria-valuemin={0} aria-valuemax={100}>
            <span style={{ width: `${Math.min(100, Math.max(0, op.capacity_utilization * 100))}%` }} />
          </div>
        </div>
        <div className="kpi">
          <dl>
            <dt className="kpi__label">Total sessions</dt>
            <dd>{op.sessions_total}</dd>
          </dl>
          <span className="kpi__note">Since the server started</span>
        </div>
        <div className="kpi">
          <dl>
            <dt className="kpi__label">Turns</dt>
            <dd>{op.turns_total}</dd>
          </dl>
          <span className="kpi__note">{turnsNote}</span>
        </div>
        <div className="kpi" data-tone={op.errors_total > 0 ? "bad" : undefined}>
          <dl>
            <dt className="kpi__label">Errors</dt>
            <dd>{op.errors_total}</dd>
          </dl>
          <span className="kpi__note">{errorsNote}</span>
        </div>
      </section>

      <ReplyChart sessions={sessions} />

      <div className="dash-grid">
        <section className="dash-card dash-grid__main" aria-label="Recent calls">
          <div className="section-row">
            <h2>Recent calls</h2>
            <Link to="/sessions" className="ledger__link">
              View all calls
            </Link>
          </div>

          {recent.length === 0 ? (
            <EmptyState title="No recent calls">
              No calls recorded yet. Test an agent to see one here.
            </EmptyState>
          ) : (
            <DataTable
              caption="Recent voice sessions"
              columns={["Session ID", "Status", "Turns", "Started", "Action"]}
              rows={recent.map((s) => [
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

        <div className="dash-grid__side">
          <section className="dash-card" aria-label="Readiness">
            <div className="section-row">
              <h2>Readiness</h2>
            </div>
            <ul className="ledger" aria-label="Service readiness">
              <li className="ledger__row">
                <span className="lamp" data-tone={readyAgentsCount > 0 ? "ok" : "idle"} aria-hidden="true" />
                <span className="ledger__name">Agents</span>
                <span className="ledger__value">
                  {readyAgentsCount} of {totalAgents} ready
                </span>
                {totalAgents === 0 && <span className="subtext">No agents created yet.</span>}
                <Link to="/agents" className="ledger__link">
                  View agents
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
          </section>
        </div>
      </div>
    </>
  );
}
