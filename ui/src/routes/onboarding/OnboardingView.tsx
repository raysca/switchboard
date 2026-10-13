import { useState, type FormEvent } from "react";
import { Link } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ApiClient,
  type FlowDTO,
  type CreateFlowInput,
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

  // Step 2: Flow form state
  const [flowNameDraft, setFlowNameDraft] = useState("");
  const [flowDescDraft, setFlowDescDraft] = useState("");
  const [flowSttDraft, setFlowSttDraft] = useState("chirp_2");
  const [flowLlmDraft, setFlowLlmDraft] = useState("gemini-2.5-flash");
  const [flowTtsDraft, setFlowTtsDraft] = useState("en-US-Chirp3-HD-Aoede");
  const [flowError, setFlowError] = useState<ApiError | null>(null);

  const providersQuery = useQuery({
    queryKey: ["providers"],
    queryFn: client.getProviders,
    enabled: !!keyValue,
    retry: false,
  });

  const flowsQuery = useQuery({
    queryKey: ["flows"],
    queryFn: () => client.getFlows(),
    enabled: !!keyValue,
    retry: false,
  });

  const createFlowMutation = useMutation({
    mutationFn: (payload: CreateFlowInput) => client.createFlow(payload),
    onSuccess: () => {
      setFlowError(null);
      void queryClient.invalidateQueries({ queryKey: ["flows"] });
    },
    onError: (err) => {
      setFlowError(asApiError(err, "Failed to create flow"));
    },
  });

  function handleCreateFlow(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const name = flowNameDraft.trim();
    const stt = flowSttDraft.trim();
    const llm = flowLlmDraft.trim();
    const tts = flowTtsDraft.trim();
    if (!name || !stt || !llm || !tts) return;
    if (!flowProvider) {
      setFlowError(
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
    // ponytail: onboarding uses one detected provider for every stage; the Flows page edits each stage.
    const provider = flowProvider;
    createFlowMutation.mutate({
      name,
      description: flowDescDraft.trim(),
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
  const flowProvider = detected.find((p) =>
    ["stt", "llm", "tts"].every((cap) => p.capabilities.includes(cap)),
  )?.id;
  const flows = (flowsQuery.data ?? []).filter((a) => !a.archived);

  const hasProvider = detected.length > 0;
  const hasFlows = flows.length > 0;
  const isFullyConfigured = hasProvider && hasFlows;

  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>Onboarding setup</h1>
        <p>First-run guided setup for providers and voice flows.</p>
      </header>

      {!keyValue ? (
        <EmptyState title="Connect to start onboarding">
          Enter a management key above to begin onboarding setup.
        </EmptyState>
      ) : providersQuery.isPending || flowsQuery.isPending ? (
        <LoadingState label="Checking system configuration" />
      ) : providersQuery.isError ? (
        <ErrorState
          title="Setup check failed"
          error={providersQuery.error as ApiError}
          onRetry={() => {
            void providersQuery.refetch();
          }}
        />
      ) : flowsQuery.isError ? (
        <ErrorState
          title="Setup check failed"
          error={flowsQuery.error as ApiError}
          onRetry={() => {
            void flowsQuery.refetch();
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
              {flows.length} configured flow(s).
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
                Configured Flows ({flows.length})
              </h3>
              <ul style={{ paddingLeft: "18px", margin: "0 0 16px" }}>
                {flows.map((a: FlowDTO) => (
                  <li key={a.id} style={{ marginBottom: "6px" }}>
                    <strong>{a.name}</strong> —{" "}
                    <Status tone={a.readiness.state === "ready" ? "good" : "bad"}>
                      {a.readiness.state === "ready" ? "Ready" : a.readiness.state}
                    </Status>
                  </li>
                ))}
              </ul>
              <Link to="/flows" className="action-button-small">
                Manage flows
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
              Voice flows need a provider for speech recognition, LLM reasoning, and speech
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
        /* Step 2: Create First Flow */
        <div className="onboarding-step" style={{ marginTop: "32px" }}>
          <section aria-labelledby="step2-heading">
            <h2 id="step2-heading">Step 2: Create your first flow</h2>
            <p style={{ color: "var(--ink-2)", marginBottom: "16px" }}>
              Provider detected: <strong>{flowProvider ?? detected.map((p) => p.display_name).join(", ")}</strong>.
              Now configure your first voice flow.
            </p>

            {flowError && (
              <ErrorState
                title="Failed to create flow"
                error={flowError}
                onRetry={() => setFlowError(null)}
              />
            )}

            <form onSubmit={handleCreateFlow} className="form-card">
              <label className="form-field" htmlFor="onboard-flow-name">
                <span>Flow name</span>
                <input
                  id="onboard-flow-name"
                  type="text"
                  value={flowNameDraft}
                  onChange={(e) => setFlowNameDraft(e.target.value)}
                  placeholder="e.g. Concierge"
                  maxLength={128}
                  required
                />
              </label>

              <label className="form-field" htmlFor="onboard-flow-desc">
                <span>Description</span>
                <input
                  id="onboard-flow-desc"
                  type="text"
                  value={flowDescDraft}
                  onChange={(e) => setFlowDescDraft(e.target.value)}
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
                  Provider for all stages: <code>{flowProvider ?? "none detected"}</code>
                </p>

                <label className="form-field" htmlFor="onboard-flow-stt">
                  <span>STT model</span>
                  <input
                    id="onboard-flow-stt"
                    type="text"
                    value={flowSttDraft}
                    onChange={(e) => setFlowSttDraft(e.target.value)}
                    required
                  />
                </label>

                <label className="form-field" htmlFor="onboard-flow-llm">
                  <span>LLM model</span>
                  <input
                    id="onboard-flow-llm"
                    type="text"
                    value={flowLlmDraft}
                    onChange={(e) => setFlowLlmDraft(e.target.value)}
                    required
                  />
                </label>

                <label className="form-field" htmlFor="onboard-flow-tts">
                  <span>TTS voice</span>
                  <input
                    id="onboard-flow-tts"
                    type="text"
                    value={flowTtsDraft}
                    onChange={(e) => setFlowTtsDraft(e.target.value)}
                    required
                  />
                </label>
              </div>

              <ActionButton type="submit" disabled={createFlowMutation.isPending}>
                {createFlowMutation.isPending ? "Creating…" : "Create flow"}
              </ActionButton>
            </form>
          </section>
        </div>
      )}
    </>
  );
}
