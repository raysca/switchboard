import { useState, type FormEvent } from "react";
import { Link } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ApiClient,
  type CredentialDTO,
  type AgentDTO,
  type CreateAgentInput,
  ApiError,
} from "../../api/client";
import {
  ActionButton,
  EmptyState,
  ErrorState,
  LoadingState,
  Status,
} from "../../components/primitives";

export type OnboardingViewProps = {
  client: ApiClient;
  keyValue: string | null;
};

export function OnboardingView({ client, keyValue }: OnboardingViewProps) {
  const queryClient = useQueryClient();

  // Step 1: Provider form state
  const [providerChoice, setProviderChoice] = useState("google");
  const [secretDraft, setSecretDraft] = useState("");
  const [providerError, setProviderError] = useState<ApiError | null>(null);

  // Step 2: Agent form state
  const [agentNameDraft, setAgentNameDraft] = useState("");
  const [agentDescDraft, setAgentDescDraft] = useState("");
  const [agentSttDraft, setAgentSttDraft] = useState("chirp_2");
  const [agentLlmDraft, setAgentLlmDraft] = useState("gemini-2.5-flash");
  const [agentTtsDraft, setAgentTtsDraft] = useState("en-US-Journey-F");
  const [agentError, setAgentError] = useState<ApiError | null>(null);

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

  const agentsQuery = useQuery({
    queryKey: ["agents"],
    queryFn: () => client.getAgents(),
    enabled: !!keyValue,
    retry: false,
  });

  const createCredMutation = useMutation({
    mutationFn: (payload: { provider: string; secret: string }) =>
      client.createCredential(payload),
    onSuccess: () => {
      setSecretDraft("");
      setProviderError(null);
      void queryClient.invalidateQueries({ queryKey: ["credentials"] });
    },
    onError: (err) => {
      if (err instanceof ApiError) {
        setProviderError(err);
      } else {
        setProviderError(
          new ApiError("Failed to connect provider", "credential_failed", 500, ""),
        );
      }
    },
  });

  const createAgentMutation = useMutation({
    mutationFn: (payload: CreateAgentInput) => client.createAgent(payload),
    onSuccess: () => {
      setAgentError(null);
      void queryClient.invalidateQueries({ queryKey: ["agents"] });
    },
    onError: (err) => {
      if (err instanceof ApiError) {
        setAgentError(err);
      } else {
        setAgentError(
          new ApiError("Failed to create agent", "create_failed", 500, ""),
        );
      }
    },
  });

  function handleConnectProvider(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const secret = secretDraft.trim();
    if (!secret) return;
    createCredMutation.mutate({ provider: providerChoice, secret });
  }

  function handleCreateAgent(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const name = agentNameDraft.trim();
    const stt = agentSttDraft.trim();
    const llm = agentLlmDraft.trim();
    const tts = agentTtsDraft.trim();
    if (!name || !stt || !llm || !tts) return;
    if (!agentCredential) {
      setAgentError(
        new ApiError(
          providersQuery.isSuccess
            ? "No connected credential can run speech-to-text, LLM and text-to-speech. Add one (for example Google) on the Providers page."
            : "The provider list has not loaded yet. Reload the page and try again.",
          "no_credential",
          0,
          "",
        ),
      );
      return;
    }
    // ponytail: onboarding uses the step-1 credential for every stage; the Agents page edits each stage.
    const { provider, id: credential_id } = agentCredential;
    createAgentMutation.mutate({
      name,
      description: agentDescDraft.trim(),
      enabled: true,
      config: {
        stt: { provider, credential_id, model: stt, language: "en-US" },
        llm: { provider, credential_id, model: llm },
        tts: { provider, credential_id, voice: tts },
      },
    });
  }

  // Onboarding uses one provider for every stage, so offer only providers that
  // can do speech-to-text, LLM and text-to-speech (never the "mcp" token type).
  const providers = (providersQuery.data ?? []).filter((p) =>
    ["stt", "llm", "tts"].every((cap) => p.capabilities.includes(cap)),
  );
  const credentials = credentialsQuery.data ?? [];
  const agents = (agentsQuery.data ?? []).filter((a) => !a.archived);
  // The credential connected in step 1: the first one whose provider can run every stage.
  const agentCredential = credentials.find((c) => providers.some((p) => p.id === c.provider));

  const hasCredentials = credentials.length > 0;
  const hasAgents = agents.length > 0;
  const isFullyConfigured = hasCredentials && hasAgents;

  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>Onboarding setup</h1>
        <p>First-run guided setup for providers and voice agents.</p>
      </header>

      {!keyValue ? (
        <EmptyState title="Connect to start onboarding">
          Enter a management key below to begin onboarding setup.
        </EmptyState>
      ) : credentialsQuery.isPending || agentsQuery.isPending ? (
        <LoadingState label="Checking system configuration" />
      ) : credentialsQuery.isError ? (
        <ErrorState
          title="Setup check failed"
          error={credentialsQuery.error as ApiError}
          onRetry={() => {
            void credentialsQuery.refetch();
          }}
        />
      ) : agentsQuery.isError ? (
        <ErrorState
          title="Setup check failed"
          error={agentsQuery.error as ApiError}
          onRetry={() => {
            void agentsQuery.refetch();
          }}
        />
      ) : isFullyConfigured ? (
        /* Completed State */
        <div className="onboarding-complete" style={{ marginTop: "32px" }}>
          <div
            className="message-panel"
            style={{ borderColor: "#007e82", marginBottom: "28px" }}
            role="status"
          >
            <h2>System configured and ready</h2>
            <p>
              Your setup is complete with {credentials.length} provider credential(s) and{" "}
              {agents.length} configured agent(s).
            </p>
          </div>

          <div
            className="summary-cards"
            style={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fit, minmax(260px, 1fr))",
              gap: "20px",
              marginBottom: "32px",
            }}
          >
            <div
              style={{
                background: "#ffffff",
                border: "1px solid #c9d3d2",
                padding: "20px",
              }}
            >
              <h3 style={{ margin: "0 0 12px", fontSize: "1.1rem" }}>
                Connected Providers ({credentials.length})
              </h3>
              <ul style={{ paddingLeft: "18px", margin: "0 0 16px" }}>
                {credentials.map((c: CredentialDTO) => (
                  <li key={c.id} style={{ marginBottom: "6px" }}>
                    <strong>{c.provider}</strong>{" "}
                    <code style={{ fontSize: "0.85rem" }}>{c.masked_value}</code> (
                    {c.validation_state})
                  </li>
                ))}
              </ul>
              <Link to="/providers" className="action-button-small">
                Manage providers
              </Link>
            </div>

            <div
              style={{
                background: "#ffffff",
                border: "1px solid #c9d3d2",
                padding: "20px",
              }}
            >
              <h3 style={{ margin: "0 0 12px", fontSize: "1.1rem" }}>
                Configured Agents ({agents.length})
              </h3>
              <ul style={{ paddingLeft: "18px", margin: "0 0 16px" }}>
                {agents.map((a: AgentDTO) => (
                  <li key={a.id} style={{ marginBottom: "6px" }}>
                    <strong>{a.name}</strong> —{" "}
                    <Status tone={a.readiness.state === "ready" ? "good" : "bad"}>
                      {a.readiness.state === "ready" ? "Ready" : a.readiness.state}
                    </Status>
                  </li>
                ))}
              </ul>
              <Link to="/agents" className="action-button-small">
                Manage agents
              </Link>
            </div>
          </div>
        </div>
      ) : !hasCredentials ? (
        /* Step 1: Connect Provider */
        <div className="onboarding-step" style={{ marginTop: "32px" }}>
          <section aria-labelledby="step1-heading">
            <h2 id="step1-heading">Step 1: Connect your first provider</h2>
            <p style={{ color: "#53666b", marginBottom: "20px" }}>
              Voice agents need a provider (like Google or xAI) for speech recognition, LLM
              reasoning, and speech synthesis.
            </p>

            {providerError && (
              <ErrorState
                title="Failed to connect provider"
                error={providerError}
                onRetry={() => setProviderError(null)}
              />
            )}

            <form onSubmit={handleConnectProvider} className="form-card">
              <label className="form-field" htmlFor="onboard-provider-select">
                <span>Select provider</span>
                <select
                  id="onboard-provider-select"
                  value={providerChoice}
                  onChange={(e) => setProviderChoice(e.target.value)}
                >
                  {providers.length > 0 ? (
                    providers.map((p) => (
                      <option key={p.id} value={p.id}>
                        {p.display_name}
                      </option>
                    ))
                  ) : (
                    <option value="google">Google</option>
                  )}
                </select>
              </label>

              <label className="form-field" htmlFor="onboard-secret-input">
                <span>API key / secret</span>
                <input
                  id="onboard-secret-input"
                  type="password"
                  autoComplete="off"
                  value={secretDraft}
                  onChange={(e) => setSecretDraft(e.target.value)}
                  placeholder="Enter API key or secret token"
                  required
                />
              </label>

              <ActionButton type="submit" disabled={createCredMutation.isPending}>
                {createCredMutation.isPending ? "Connecting…" : "Connect provider"}
              </ActionButton>
            </form>
          </section>
        </div>
      ) : (
        /* Step 2: Create First Agent */
        <div className="onboarding-step" style={{ marginTop: "32px" }}>
          <section aria-labelledby="step2-heading">
            <h2 id="step2-heading">Step 2: Create your first agent</h2>
            <p style={{ color: "#53666b", marginBottom: "16px" }}>
              Provider connected: <strong>{agentCredential?.provider}</strong> (
              <code>{agentCredential?.masked_value}</code>). Now configure your first voice agent.
            </p>

            {agentError && (
              <ErrorState
                title="Failed to create agent"
                error={agentError}
                onRetry={() => setAgentError(null)}
              />
            )}

            <form onSubmit={handleCreateAgent} className="form-card">
              <label className="form-field" htmlFor="onboard-agent-name">
                <span>Agent name</span>
                <input
                  id="onboard-agent-name"
                  type="text"
                  value={agentNameDraft}
                  onChange={(e) => setAgentNameDraft(e.target.value)}
                  placeholder="e.g. Concierge"
                  maxLength={128}
                  required
                />
              </label>

              <label className="form-field" htmlFor="onboard-agent-desc">
                <span>Description</span>
                <input
                  id="onboard-agent-desc"
                  type="text"
                  value={agentDescDraft}
                  onChange={(e) => setAgentDescDraft(e.target.value)}
                  placeholder="Primary assistant bot"
                  maxLength={1024}
                />
              </label>

              <div
                className="voice-stack-settings"
                style={{
                  border: "1px solid #c9d3d2",
                  padding: "16px",
                  marginTop: "16px",
                  marginBottom: "16px",
                  background: "#ffffff",
                }}
              >
                <h3 style={{ margin: "0 0 12px", fontSize: "1rem" }}>
                  Voice stack configuration
                </h3>
                <p style={{ margin: "0 0 12px" }}>
                  Provider for all stages: <code>{agentCredential?.provider ?? "none connected"}</code>
                </p>

                <label className="form-field" htmlFor="onboard-agent-stt">
                  <span>STT model</span>
                  <input
                    id="onboard-agent-stt"
                    type="text"
                    value={agentSttDraft}
                    onChange={(e) => setAgentSttDraft(e.target.value)}
                    required
                  />
                </label>

                <label className="form-field" htmlFor="onboard-agent-llm">
                  <span>LLM model</span>
                  <input
                    id="onboard-agent-llm"
                    type="text"
                    value={agentLlmDraft}
                    onChange={(e) => setAgentLlmDraft(e.target.value)}
                    required
                  />
                </label>

                <label className="form-field" htmlFor="onboard-agent-tts">
                  <span>TTS voice</span>
                  <input
                    id="onboard-agent-tts"
                    type="text"
                    value={agentTtsDraft}
                    onChange={(e) => setAgentTtsDraft(e.target.value)}
                    required
                  />
                </label>
              </div>

              <ActionButton type="submit" disabled={createAgentMutation.isPending}>
                {createAgentMutation.isPending ? "Creating…" : "Create agent"}
              </ActionButton>
            </form>
          </section>
        </div>
      )}
    </>
  );
}
