import { useQuery } from "@tanstack/react-query";
import type { ApiClient, ApiError, ProviderStatus } from "../../api/client";
import { EmptyState, ErrorState, LoadingState, Status } from "../../components/primitives";

export type ProvidersViewProps = {
  client: ApiClient;
  keyValue: string | null;
};

const SERVICES = ["stt", "llm", "tts"] as const;

// One pill per service the catalog lists for the provider: what it can do now, or what is missing.
function servicePills(p: ProviderStatus) {
  return SERVICES.flatMap((s) => {
    const svc = p.services[s];
    if (!svc) return [];
    const name = s.toUpperCase();
    if (svc.available) return [{ key: s, state: "ok", text: `${name} available` }];
    if (svc.reason === "missing_env") return [{ key: s, state: "missing", text: `${name} needs ${(svc.missing_env ?? []).join(", ")}` }];
    return [{ key: s, state: "absent", text: `${name} not in this build` }];
  });
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
          Enter a management key above to see which providers the server detected.
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
        <div className="provider-views">
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
                </tr>
              </thead>
              <tbody>
                {providers.map((p) => (
                  <tr key={p.id}>
                    <td>
                      <strong>{p.display_name}</strong>
                      <div className="subtext">{p.id}</div>
                    </td>
                    <td>
                      <Status tone={p.detected ? "good" : "neutral"}>{p.detected ? "Detected" : "Not detected"}</Status>
                    </td>
                    <td>
                      <div className="pills">
                        {servicePills(p).map((pill) => (
                          <span key={pill.key} className="pill" data-state={pill.state}>
                            {pill.text}
                          </span>
                        ))}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </>
  );
}
