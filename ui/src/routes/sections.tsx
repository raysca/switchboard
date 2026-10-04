import { useQuery } from "@tanstack/react-query";
import type { ApiError, ServerConfig } from "../api/client";
import {
  DataTable,
  EmptyState,
  ErrorState,
  LoadingState,
  Status,
} from "../components/primitives";

export type Section = {
  path: string;
  name: string;
  purpose: string;
  emptyTitle: string;
  emptyBody: string;
};
export const sections: Section[] = [
  {
    path: "/dashboard",
    name: "Dashboard",
    purpose: "Operational signals for voice sessions and agents.",
    emptyTitle: "Signals start with sessions",
    emptyBody:
      "Session rate, latency, and provider failures need the upcoming telemetry endpoint.",
  },
  {
    path: "/agents",
    name: "Agents",
    purpose: "Configure the agents people speak with.",
    emptyTitle: "Agent catalog is not connected",
    emptyBody: "Agent versions and voice tests need the agent management API.",
  },
  {
    path: "/providers",
    name: "Providers",
    purpose: "Manage the services that power listening and speech.",
    emptyTitle: "Provider connections are not exposed yet",
    emptyBody:
      "Credential status and detected capabilities need the provider management API.",
  },
  {
    path: "/mcp-servers",
    name: "MCP Servers",
    purpose: "Connect tools that agents can call.",
    emptyTitle: "Tool discovery needs an endpoint",
    emptyBody:
      "Connected servers and tool bindings will be shown when the MCP API is available.",
  },
  {
    path: "/sessions",
    name: "Sessions",
    purpose: "Inspect calls and their traces.",
    emptyTitle: "Traces need session history",
    emptyBody: "Recent calls and turn timelines need the session history API.",
  },
  {
    path: "/server",
    name: "Server",
    purpose: "Inspect the safe, read-only runtime configuration.",
    emptyTitle: "Connect to inspect the server",
    emptyBody:
      "Enter a management key to load the public configuration projection.",
  },
];

export function SectionPage({
  section,
  keyValue,
  readConfig,
}: {
  section: Section;
  keyValue: string | null;
  readConfig: () => Promise<ServerConfig>;
}) {
  const isServer = section.path === "/server";
  const query = useQuery({
    queryKey: ["server-config"],
    queryFn: readConfig,
    enabled: isServer && !!keyValue,
    retry: false,
  });
  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>{section.name}</h1>
        <p>{section.purpose}</p>
      </header>
      {isServer && keyValue ? (
        query.isPending ? (
          <LoadingState label="Loading server configuration" />
        ) : query.isError ? (
          <ErrorState
            error={query.error as ApiError}
            onRetry={() => {
              void query.refetch();
            }}
          />
        ) : (
          <>
            <Status tone="good">Configuration available</Status>
            <DataTable
              caption="Server configuration"
              columns={["Setting", "Value"]}
              rows={[
                ["Profile", query.data.profile],
                ["Maximum sessions", query.data.max_sessions],
                [
                  "Maximum call duration",
                  `${query.data.max_call_duration_seconds} seconds`,
                ],
                [
                  "Maximum voice token lifetime",
                  `${query.data.voice_token_max_lifetime_seconds} seconds`,
                ],
              ]}
            />
          </>
        )
      ) : (
        <EmptyState title={section.emptyTitle}>{section.emptyBody}</EmptyState>
      )}
    </>
  );
}
