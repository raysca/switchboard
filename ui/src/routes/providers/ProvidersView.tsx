import { useState, type FormEvent } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ApiClient,
  type CredentialDTO,
  type DiscoveryResult,
  ApiError,
} from "../../api/client";
import {
  ActionButton,
  DataTable,
  EmptyState,
  ErrorState,
  LoadingState,
  Status,
} from "../../components/primitives";

export type ProvidersViewProps = {
  client: ApiClient;
  keyValue: string | null;
};

export function ProvidersView({ client, keyValue }: ProvidersViewProps) {
  const queryClient = useQueryClient();
  const [selectedProvider, setSelectedProvider] = useState("google");
  const [secretDraft, setSecretDraft] = useState("");
  const [addError, setAddError] = useState<ApiError | null>(null);
  const [actionError, setActionError] = useState<ApiError | null>(null);

  // Discovery state
  const [discoverProvider, setDiscoverProvider] = useState("google");
  const [discoverCapability, setDiscoverCapability] = useState("llm");
  const [discoveryResult, setDiscoveryResult] = useState<DiscoveryResult | null>(null);
  const [discoveryLoading, setDiscoveryLoading] = useState(false);
  const [discoveryError, setDiscoveryError] = useState<ApiError | null>(null);

  // Updating secret state
  const [editingCredId, setEditingCredId] = useState<string | null>(null);
  const [updateSecretDraft, setUpdateSecretDraft] = useState("");

  const providersQuery = useQuery({
    queryKey: ["providers"],
    queryFn: client.getProviders,
    enabled: !!keyValue,
    retry: false,
  });

  const credentialsQuery = useQuery({
    queryKey: ["credentials"],
    queryFn: client.getCredentials,
    enabled: !!keyValue,
    retry: false,
  });

  const addMutation = useMutation({
    mutationFn: (payload: { provider: string; secret: string }) =>
      client.createCredential(payload),
    onSuccess: () => {
      setSecretDraft("");
      setAddError(null);
      void queryClient.invalidateQueries({ queryKey: ["credentials"] });
    },
    onError: (err) => {
      if (err instanceof ApiError) {
        setAddError(err);
      } else {
        setAddError(new ApiError("Failed to add credential", "unknown_error", 500, ""));
      }
    },
  });

  const validateMutation = useMutation({
    mutationFn: (id: string) => client.validateCredential(id),
    onSuccess: () => {
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["credentials"] });
    },
    onError: (err) => {
      if (err instanceof ApiError) {
        setActionError(err);
      } else {
        setActionError(new ApiError("Validation failed", "validation_failed", 500, ""));
      }
    },
  });

  const rotateMutation = useMutation({
    mutationFn: (id: string) => client.rotateCredential(id),
    onSuccess: () => {
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["credentials"] });
    },
    onError: (err) => {
      if (err instanceof ApiError) {
        setActionError(err);
      } else {
        setActionError(new ApiError("Rotate failed", "rotate_failed", 500, ""));
      }
    },
  });

  const updateMutation = useMutation({
    mutationFn: ({ id, secret }: { id: string; secret: string }) =>
      client.updateCredential(id, { secret }),
    onSuccess: () => {
      setEditingCredId(null);
      setUpdateSecretDraft("");
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["credentials"] });
    },
    onError: (err) => {
      if (err instanceof ApiError) {
        setActionError(err);
      } else {
        setActionError(new ApiError("Update secret failed", "update_failed", 500, ""));
      }
    },
  });

  const deleteMutation = useMutation({
    mutationFn: (id: string) => client.deleteCredential(id),
    onSuccess: () => {
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["credentials"] });
    },
    onError: (err) => {
      if (err instanceof ApiError) {
        setActionError(err);
      } else {
        setActionError(new ApiError("Delete failed", "delete_failed", 500, ""));
      }
    },
  });

  async function handleAddCredential(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const secret = secretDraft.trim();
    if (!secret) return;
    addMutation.mutate({ provider: selectedProvider, secret });
  }

  async function handleDiscover(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setDiscoveryLoading(true);
    setDiscoveryError(null);
    try {
      const res = await client.getProviderDiscovery(
        discoverProvider,
        discoverCapability,
      );
      setDiscoveryResult(res);
    } catch (err) {
      if (err instanceof ApiError) {
        setDiscoveryError(err);
      } else {
        setDiscoveryError(
          new ApiError("Discovery failed", "discovery_failed", 500, ""),
        );
      }
      setDiscoveryResult(null);
    } finally {
      setDiscoveryLoading(false);
    }
  }

  const providers = providersQuery.data ?? [];
  const credentials = credentialsQuery.data ?? [];

  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>Providers</h1>
        <p>Manage the services that power listening and speech.</p>
      </header>

      {!keyValue ? (
        <EmptyState title="Connect to manage providers">
          Enter a management key below to view provider credentials and capabilities.
        </EmptyState>
      ) : providersQuery.isPending || credentialsQuery.isPending ? (
        <LoadingState label="Loading providers and credentials" />
      ) : providersQuery.isError ? (
        <ErrorState
          title="Failed to load providers"
          error={providersQuery.error as ApiError}
          onRetry={() => {
            void providersQuery.refetch();
          }}
        />
      ) : credentialsQuery.isError ? (
        <ErrorState
          title="Failed to load credentials"
          error={credentialsQuery.error as ApiError}
          onRetry={() => {
            void credentialsQuery.refetch();
          }}
        />
      ) : (
        <div className="provider-views">
          {actionError && (
            <div className="section-banner" style={{ marginTop: "24px" }}>
              <ErrorState
                title="Provider action failed"
                error={actionError}
                onRetry={() => setActionError(null)}
              />
            </div>
          )}

          {/* Credentials List Section */}
          <section
            className="credentials-section"
            aria-labelledby="credentials-heading"
            style={{ marginTop: "32px" }}
          >
            <h2 id="credentials-heading">Configured credentials</h2>
            {credentials.length === 0 ? (
              <EmptyState title="No provider credentials configured">
                Add a credential below to enable agent capabilities.
              </EmptyState>
            ) : (
              <div className="table-scroll">
                <table>
                  <caption>Provider API credentials and validation states</caption>
                  <thead>
                    <tr>
                      <th scope="col">Provider</th>
                      <th scope="col">Masked key</th>
                      <th scope="col">Capabilities</th>
                      <th scope="col">Validation status</th>
                      <th scope="col">Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {credentials.map((cred: CredentialDTO) => {
                      const matched = providers.find((p) => p.id === cred.provider);
                      const providerName = matched?.display_name ?? cred.provider;
                      const tone =
                        cred.validation_state === "valid"
                          ? "good"
                          : cred.validation_state === "invalid"
                            ? "bad"
                            : "neutral";
                      return (
                        <tr key={cred.id}>
                          <td>
                            <strong>{providerName}</strong>
                            <div className="subtext">{cred.provider}</div>
                          </td>
                          <td>
                            <code className="masked-value">{cred.masked_value}</code>
                          </td>
                          <td>
                            {cred.capabilities && cred.capabilities.length > 0
                              ? cred.capabilities.join(", ")
                              : "—"}
                          </td>
                          <td>
                            <Status tone={tone}>{cred.validation_state}</Status>
                          </td>
                          <td>
                            <div className="action-buttons-cell">
                              <button
                                className="action-button-small"
                                onClick={() => validateMutation.mutate(cred.id)}
                                disabled={validateMutation.isPending}
                              >
                                Validate
                              </button>
                              <button
                                className="action-button-small"
                                onClick={() => rotateMutation.mutate(cred.id)}
                                disabled={rotateMutation.isPending}
                              >
                                Rotate
                              </button>
                              <button
                                className="action-button-small"
                                onClick={() => {
                                  if (editingCredId === cred.id) {
                                    setEditingCredId(null);
                                    setUpdateSecretDraft("");
                                  } else {
                                    setEditingCredId(cred.id);
                                    setUpdateSecretDraft("");
                                  }
                                }}
                              >
                                {editingCredId === cred.id ? "Cancel" : "Update"}
                              </button>
                              <button
                                className="action-button-small action-button-danger"
                                onClick={() => deleteMutation.mutate(cred.id)}
                                disabled={deleteMutation.isPending}
                              >
                                Delete
                              </button>
                            </div>
                            {editingCredId === cred.id && (
                              <form
                                className="inline-update-form"
                                onSubmit={(e) => {
                                  e.preventDefault();
                                  if (updateSecretDraft.trim()) {
                                    updateMutation.mutate({
                                      id: cred.id,
                                      secret: updateSecretDraft.trim(),
                                    });
                                  }
                                }}
                                style={{ marginTop: "8px" }}
                              >
                                <label
                                  className="form-field"
                                  htmlFor={`update-secret-${cred.id}`}
                                >
                                  <span>New API secret</span>
                                  <input
                                    id={`update-secret-${cred.id}`}
                                    type="password"
                                    value={updateSecretDraft}
                                    onChange={(e) =>
                                      setUpdateSecretDraft(e.target.value)
                                    }
                                    placeholder="Enter new secret"
                                    required
                                  />
                                </label>
                                <button
                                  type="submit"
                                  className="action-button-small"
                                  disabled={updateMutation.isPending}
                                  style={{ marginTop: "4px" }}
                                >
                                  Save new secret
                                </button>
                              </form>
                            )}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </section>

          {/* Add Credential Section */}
          <section
            className="add-credential-section"
            aria-labelledby="add-credential-heading"
            style={{ marginTop: "48px" }}
          >
            <h2 id="add-credential-heading">Add provider credential</h2>
            {addError && (
              <ErrorState
                title="Failed to add credential"
                error={addError}
                onRetry={() => setAddError(null)}
              />
            )}
            <form onSubmit={handleAddCredential} className="form-card">
              <label className="form-field" htmlFor="provider-select">
                <span>Provider service</span>
                <select
                  id="provider-select"
                  value={selectedProvider}
                  onChange={(e) => setSelectedProvider(e.target.value)}
                >
                  {providers.length > 0 ? (
                    providers.map((p) => (
                      <option key={p.id} value={p.id}>
                        {p.display_name} ({p.id})
                      </option>
                    ))
                  ) : (
                    <>
                      <option value="google">Google</option>
                      <option value="xai">xAI</option>
                    </>
                  )}
                </select>
              </label>

              <label className="form-field" htmlFor="credential-secret">
                <span>API key / secret</span>
                <input
                  id="credential-secret"
                  type="password"
                  autoComplete="off"
                  value={secretDraft}
                  onChange={(e) => setSecretDraft(e.target.value)}
                  placeholder="Enter secret token"
                  required
                />
              </label>

              <ActionButton type="submit" disabled={addMutation.isPending}>
                {addMutation.isPending ? "Adding…" : "Add credential"}
              </ActionButton>
            </form>
          </section>

          {/* Capability Discovery Section */}
          <section
            className="discovery-section"
            aria-labelledby="discovery-heading"
            style={{ marginTop: "48px" }}
          >
            <h2 id="discovery-heading">Test capability discovery</h2>
            <p className="discovery-desc" style={{ color: "#53666b", marginBottom: "16px" }}>
              Query the provider catalog for supported models and voices.
            </p>

            {discoveryError && (
              <ErrorState
                title="Capability discovery failed"
                error={discoveryError}
                onRetry={() => setDiscoveryError(null)}
              />
            )}

            <form onSubmit={handleDiscover} className="form-card">
              <label className="form-field" htmlFor="discover-provider-select">
                <span>Discovery provider</span>
                <select
                  id="discover-provider-select"
                  value={discoverProvider}
                  onChange={(e) => setDiscoverProvider(e.target.value)}
                >
                  {providers.length > 0 ? (
                    providers.map((p) => (
                      <option key={p.id} value={p.id}>
                        {p.display_name}
                      </option>
                    ))
                  ) : (
                    <>
                      <option value="google">Google</option>
                      <option value="xai">xAI</option>
                    </>
                  )}
                </select>
              </label>

              <label className="form-field" htmlFor="discover-capability-select">
                <span>Discovery capability</span>
                <select
                  id="discover-capability-select"
                  value={discoverCapability}
                  onChange={(e) => setDiscoverCapability(e.target.value)}
                >
                  <option value="llm">LLM Models</option>
                  <option value="stt">Speech-to-Text (STT)</option>
                  <option value="tts">Text-to-Speech (TTS)</option>
                </select>
              </label>

              <ActionButton type="submit" disabled={discoveryLoading}>
                {discoveryLoading ? "Discovering…" : "Discover"}
              </ActionButton>
            </form>

            {discoveryLoading && (
              <LoadingState label="Discovering provider capabilities" />
            )}

            {discoveryResult && (
              <div className="discovery-results" style={{ marginTop: "24px" }}>
                {discoveryResult.models && discoveryResult.models.length > 0 && (
                  <DataTable
                    caption={`Discovered ${discoverCapability.toUpperCase()} models`}
                    columns={["Display Name", "Model ID"]}
                    rows={discoveryResult.models.map((m) => [
                      m.display_name,
                      <code>{m.id}</code>,
                    ])}
                  />
                )}
                {discoveryResult.voices && discoveryResult.voices.length > 0 && (
                  <DataTable
                    caption="Discovered TTS voices"
                    columns={["Voice Name", "Language", "Voice ID"]}
                    rows={discoveryResult.voices.map((v) => [
                      v.display_name,
                      v.language ?? "—",
                      <code>{v.id}</code>,
                    ])}
                  />
                )}
                {(!discoveryResult.models || discoveryResult.models.length === 0) &&
                  (!discoveryResult.voices || discoveryResult.voices.length === 0) && (
                    <p style={{ marginTop: "16px", color: "#53666b" }}>
                      No models or voices returned for this capability.
                    </p>
                  )}
              </div>
            )}
          </section>
        </div>
      )}
    </>
  );
}
