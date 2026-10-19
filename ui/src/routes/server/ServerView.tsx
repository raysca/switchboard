import { useQuery } from "@tanstack/react-query";
import type { ApiClient, ApiError } from "../../api/client";
import {
  DataTable,
  EmptyState,
  ErrorState,
  LoadingState,
  Status,
} from "../../components/primitives";

export type ServerViewProps = {
  client: ApiClient;
  keyValue: string | null;
};

function formatUptime(seconds: number): string {
  if (seconds < 60) return `${Math.floor(seconds)} seconds`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} minutes, ${Math.floor(seconds % 60)} seconds`;
  const hours = Math.floor(minutes / 60);
  const remMinutes = minutes % 60;
  if (hours < 24) return `${hours} hours, ${remMinutes} minutes`;
  const days = Math.floor(hours / 24);
  const remHours = hours % 24;
  return `${days} days, ${remHours} hours`;
}

export function ServerView({ client, keyValue }: ServerViewProps) {
  const operationsQuery = useQuery({
    queryKey: ["operations-status"],
    queryFn: () => client.getOperationsStatus(),
    enabled: !!keyValue,
    retry: false,
  });

  if (!keyValue) {
    return (
      <>
        <header className="section-heading">
          <h1 tabIndex={-1}>Server</h1>
          <p>Inspect the safe, read-only runtime configuration.</p>
        </header>
        <EmptyState title="Connect to inspect the server">
          Enter a management key to load the public configuration projection.
        </EmptyState>
      </>
    );
  }

  if (operationsQuery.isPending) {
    return (
      <>
        <header className="section-heading">
          <h1 tabIndex={-1}>Server</h1>
          <p>Inspect the safe, read-only runtime configuration.</p>
        </header>
        <LoadingState label="Loading server configuration" />
      </>
    );
  }

  if (operationsQuery.isError) {
    return (
      <>
        <header className="section-heading">
          <h1 tabIndex={-1}>Server</h1>
          <p>Inspect the safe, read-only runtime configuration.</p>
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

  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>Server</h1>
        <p>Inspect the safe, read-only runtime configuration.</p>
      </header>

      <Status tone={op.health === "healthy" ? "good" : "bad"}>
        Configuration available • System {op.health}
      </Status>

      <div className="server-grid">
      {/* System Overview */}
      <section className="server-card" aria-label="System overview">
        <DataTable
          caption="System overview"
          columns={["Setting", "Value"]}
          rows={[
            ["Profile", <span key="profile" className="badge badge--neutral">{op.profile}</span>],
            ["System health", <span key="health" className={`badge badge--${op.health === "healthy" ? "good" : op.health === "degraded" ? "warning" : "bad"}`}>{op.health}</span>],
            ["Uptime", formatUptime(op.uptime_seconds)],
            ["Started at", new Date(op.started_at).toUTCString()],
          ]}
        />
      </section>

      {/* Operational Limits */}
      <section className="server-card" aria-label="Operational limits">
        <DataTable
          caption="Operational limits"
          columns={["Limit", "Value"]}
          rows={[
            ["Maximum concurrent sessions", op.max_sessions],
            [
              "Capacity utilization",
              `${(op.capacity_utilization * 100).toFixed(1)}% (${op.active_sessions} active / ${op.max_sessions} max)`,
            ],
            [
              "Maximum call duration",
              `${op.max_call_duration_seconds ?? 300} seconds`,
            ],
            [
              "Maximum voice token lifetime",
              `${op.voice_token_max_lifetime_seconds ?? 60} seconds`,
            ],
          ]}
        />
      </section>

      {/* Storage Engine Diagnostics */}
      <section className="server-card" aria-label="Storage diagnostics">
        <DataTable
          caption="Storage diagnostics"
          columns={["Property", "Value"]}
          rows={[
            ["Database dialect", op.database.dialect],
            [
              "Connection status",
              <span
                key="db-status"
                className={`badge badge--${
                  op.database.status === "connected"
                    ? "good"
                    : op.database.status === "unconfigured"
                    ? "neutral"
                    : "bad"
                }`}
              >
                {op.database.status}
              </span>,
            ],
            [
              "Migration version",
              op.database.migration_version
                ? `Version ${op.database.migration_version}`
                : op.database.status === "connected"
                ? "Current"
                : "None",
            ],
            ["Open pool connections", op.database.open_connections],
            ["Active in-use connections", op.database.in_use],
          ]}
        />
      </section>

      {/* VAD Diagnostics */}
      <section className="server-card" aria-label="VAD diagnostics">
        <DataTable
          caption="VAD diagnostics"
          columns={["Parameter", "Value"]}
          rows={[
            [
              "Worker status",
              <span
                key="vad-status"
                className={`badge badge--${
                  op.vad.worker_status === "ready" || op.vad.worker_status === "active"
                    ? "good"
                    : op.vad.worker_status === "degraded"
                    ? "warning"
                    : "bad"
                }`}
              >
                {op.vad.worker_status}
              </span>,
            ],
            ["Sample rate", `${op.vad.sample_rate} Hz`],
            ["Frame size", `${op.vad.frame_size} samples`],
            [
              "Engine readiness",
              op.vad.worker_status === "unavailable" ? "Unavailable" : "Ready",
            ],
          ]}
        />
      </section>
      </div>
    </>
  );
}
