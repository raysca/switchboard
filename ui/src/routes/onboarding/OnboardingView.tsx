import { useState, type FormEvent } from "react";
import { Link } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ApiClient,
  type AgentDTO,
  type CreateAgentInput,
  ApiError,
  asApiError,
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

  // Step 2: Agent form state
  const [agentNameDraft, setAgentNameDraft] = useState("");
  const [agentDescDraft, setAgentDescDraft] = useState("");
  const [agentSttDraft, setAgentSttDraft] = useState("chirp_2");
  const [agentLlmDraft, setAgentLlmDraft] = useState("gemini-2.5-flash");
  const [agentTtsDraft, setAgentTtsDraft] = useState("en-US-Chirp3-HD-Aoede");
  const [agentError, setAgentError] = useState<ApiError | null>(null);

  const providersQuery = useQuery({
    queryKey: ["providers"],
    queryFn: client.getProviders,
    enabled: !!keyValue,
    retry: false,
  });

  const agentsQuery = useQuery({
    queryKey: ["agents"],
    queryFn: () => client.getAgents(),
    enabled: !!keyValue,
    retry: false,
  });

  const createAgentMutation = useMutation({
    mutationFn: (payload: CreateAgentInput) => client.createAgent(payload),
    onSuccess: () => {
      setAgentError(null);
      void queryClient.invalidateQueries({ queryKey: ["agents"] });
    },
    onError: (err) => {
      setAgentError(asApiError(err, "Failed to create agent"));
    },
  });

  function handleCreateAgent(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const name = agentNameDraft.trim();
    const stt = agentSttDraft.trim();
    const llm = agentLlmDraft.trim();
    const tts = agentTtsDraft.trim();
    if (!name || !stt || !llm || !tts) return;
    if (!agentProvider) {
      setAgentError(
        new ApiError(
          providersQuery.isSuccess
            ? "No detected provider can run speech-to-text, LLM and text-to-speech. Set the variables for one on the server (see the Providers page)."
            : "The provider list has not loaded yet. Reload the page and try again.",
          "no_provider",
          0,
          "",
        ),
      );
      return;
    }
    // ponytail: onboarding uses one detected provider for every stage; the Agents page edits each stage.
    const provider = agentProvider;
    createAgentMutation.mutate({
      name,
      description: agentDescDraft.trim(),
      enabled: true,
      config: {
        stt: { provider, model: stt, language: "en-US" },
        llm: { provider, model: llm },
        tts: { provider, voice: tts },
      },
    });
  }

  const providers = providersQuery.data ?? [];
  const detected = providers.filter((p) => p.detected);
  // Onboarding uses one provider for every stage: a detected one that can run all three.
  const agentProvider = detected.find((p) =>
    ["stt", "llm", "tts"].every((cap) => p.capabilities.includes(cap)),
  )?.id;
  const agents = (agentsQuery.data ?? []).filter((a) => !a.archived);

  const hasProvider = detected.length > 0;
  const hasAgents = agents.length > 0;
  const isFullyConfigured = hasProvider && hasAgents;

  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>Onboarding setup</h1>
        <p>First-run guided setup for providers and voice agents.</p>
      </header>

      {!keyValue ? (
        <EmptyState title="Connect to start onboarding">
          Enter a management key above to begin onboarding setup.
        </EmptyState>
      ) : providersQuery.isPending || agentsQuery.isPending ? (
        <LoadingState label="Checking system configuration" />
      ) : providersQuery.isError ? (
        <ErrorState
          title="Setup check failed"
          error={providersQuery.error as ApiError}
          onRetry={() => {
            void providersQuery.refetch();
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
            style={{ borderColor: "var(--caller)", marginBottom: "28px" }}
            role="status"
          >
            <h2>System configured and ready</h2>
            <p>
              Your setup is complete with {detected.length} detected provider(s) and{" "}
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
                background: "var(--surface)",
                border: "1px solid var(--rule)",
                padding: "20px",
              }}
            >
              <h3 style={{ margin: "0 0 12px", fontSize: "1.1rem" }}>
                Detected Providers ({detected.length})
              </h3>
              <ul style={{ paddingLeft: "18px", margin: "0 0 16px" }}>
                {detected.map((p) => (
                  <li key={p.id} style={{ marginBottom: "6px" }}>
                    <strong>{p.display_name}</strong> ({p.capabilities.join(", ") || "no services"})
                  </li>
                ))}
              </ul>
              <Link to="/providers" className="action-button-small">
                View providers
              </Link>
            </div>

            <div
              style={{
                background: "var(--surface)",
                border: "1px solid var(--rule)",
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
      ) : !hasProvider ? (
        /* Step 1: Set a provider key */
        <div className="onboarding-step" style={{ marginTop: "32px" }}>
          <section aria-labelledby="step1-heading">
            <h2 id="step1-heading">Step 1: Set a provider key</h2>
            <p style={{ color: "var(--ink-2)", marginBottom: "20px" }}>
              Voice agents need a provider for speech recognition, LLM reasoning, and speech
              synthesis. Set one of these environment variables on the server and restart it:
            </p>
            <ul style={{ paddingLeft: "18px" }}>
              {providers.map((p) => (
                <li key={p.id}>
                  {p.display_name}: <code>{p.env.join(", ")}</code>
                </li>
              ))}
            </ul>
          </section>
        </div>
      ) : (
        /* Step 2: Create First Agent */
        <div className="onboarding-step" style={{ marginTop: "32px" }}>
          <section aria-labelledby="step2-heading">
            <h2 id="step2-heading">Step 2: Create your first agent</h2>
            <p style={{ color: "var(--ink-2)", marginBottom: "16px" }}>
              Provider detected: <strong>{agentProvider ?? detected.map((p) => p.display_name).join(", ")}</strong>.
              Now configure your first voice agent.
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
                  border: "1px solid var(--rule)",
                  padding: "16px",
                  marginTop: "16px",
                  marginBottom: "16px",
                  background: "var(--surface)",
                }}
              >
                <h3 style={{ margin: "0 0 12px", fontSize: "1rem" }}>
                  Voice stack configuration
                </h3>
                <p style={{ margin: "0 0 12px" }}>
                  Provider for all stages: <code>{agentProvider ?? "none detected"}</code>
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
