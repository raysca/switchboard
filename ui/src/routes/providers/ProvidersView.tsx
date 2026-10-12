import { useQuery } from "@tanstack/react-query";
import type { ApiClient, ApiError, ProviderStatus } from "../../api/client";
import { EmptyState, ErrorState, LoadingState, Status } from "../../components/primitives";

export type ProvidersViewProps = {
  client: ApiClient;
  keyValue: string | null;
};

const SERVICES = ["stt", "llm", "tts"] as const;

// Services the provider can run now, and what is missing for the rest.
function describeServices(p: ProviderStatus) {
  const unlocked = SERVICES.filter((s) => p.services[s]?.available);
  const missing = new Set<string>();
  const notInBuild: string[] = [];
  for (const s of SERVICES) {
    const svc = p.services[s];
    if (!svc || svc.available) continue;
    if (svc.reason === "missing_env") svc.missing_env?.forEach((v) => missing.add(v));
    else notInBuild.push(s);
  }
  return { unlocked, missing: [...missing], notInBuild };
}

export function ProvidersView({ client, keyValue }: ProvidersViewProps) {
  const providersQuery = useQuery({
    queryKey: ["providers"],
    queryFn: client.getProviders,
    enabled: !!keyValue,
    retry: false,
  });
  const providers = providersQuery.data ?? [];

  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>Providers</h1>
        <p>Provider keys are read from the server environment. Set them there; they are never stored or shown here.</p>
      </header>

      {!keyValue ? (
        <EmptyState title="Connect to view providers">
          Enter the management key above to see which providers the server detected.
        </EmptyState>
      ) : providersQuery.isPending ? (
        <LoadingState label="Loading providers" />
      ) : providersQuery.isError ? (
        <ErrorState
          title="Failed to load providers"
          error={providersQuery.error as ApiError}
          onRetry={() => {
            void providersQuery.refetch();
          }}
        />
      ) : (
        <div className="provider-views" style={{ marginTop: "32px" }}>
          {!providers.some((p) => p.detected) && (
            <EmptyState title="No providers detected">
              Set at least one of these environment variables on the server and restart it:{" "}
              {[...new Set(providers.flatMap((p) => p.env))].join(", ")}.
            </EmptyState>
          )}
          <div className="table-scroll">
            <table>
              <caption>Providers and the services they unlock</caption>
              <thead>
                <tr>
                  <th scope="col">Provider</th>
                  <th scope="col">Status</th>
                  <th scope="col">Services</th>
                  <th scope="col">To enable</th>
                </tr>
              </thead>
              <tbody>
                {providers.map((p) => {
                  const { unlocked, missing, notInBuild } = describeServices(p);
                  return (
                    <tr key={p.id}>
                      <td>
                        <strong>{p.display_name}</strong>
                        <div className="subtext">{p.id}</div>
                      </td>
                      <td>
                        <Status tone={p.detected ? "good" : "neutral"}>
                          {p.detected ? "Detected" : "Not detected"}
                        </Status>
                      </td>
                      <td>{unlocked.length > 0 ? unlocked.join(", ") : "—"}</td>
                      <td>
                        {missing.length > 0 && (
                          <>
                            Set <code>{missing.join(", ")}</code>
                          </>
                        )}
                        {notInBuild.length > 0 && <div className="subtext">{notInBuild.join(", ")} not in this build</div>}
                        {missing.length === 0 && notInBuild.length === 0 && "—"}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </>
  );
}
