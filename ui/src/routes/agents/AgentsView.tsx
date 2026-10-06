import { useState, type FormEvent } from "react";
import { Link } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ApiClient,
  type AgentDTO,
  type AgentConfigV2,
  type CreateAgentInput,
  type CredentialDTO,
  type ProviderDefinition,
  type UpdateAgentInput,
  agentConfigV2Schema,
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
import { AgentVoiceTest } from "./AgentVoiceTest";

export type AgentsViewProps = {
  client: ApiClient;
  keyValue: string | null;
};

type ViewMode = "list" | "create" | "detail" | "edit";

function getReadinessTone(state: string): "good" | "bad" | "neutral" {
  switch (state) {
    case "ready":
      return "good";
    case "configured":
    case "ready_with_tool_warning":
      return "neutral";
    case "not_ready":
    case "needs_tool_review":
    case "tool_dependency_unavailable":
    default:
      return "bad";
  }
}

function getReadinessLabel(state: string): string {
  switch (state) {
    case "ready":
      return "Ready";
    case "configured":
      return "Configured";
    case "ready_with_tool_warning":
      return "Ready (tool warning)";
    case "needs_tool_review":
      return "Needs tool review";
    case "tool_dependency_unavailable":
      return "Tool unavailable";
    case "not_ready":
      return "Not ready";
    default:
      return state;
  }
}

type Capability = "stt" | "llm" | "tts";

type AgentDraft = {
  name: string;
  description: string;
  enabled: boolean;
  sttProvider: string;
  sttModel: string;
  sttLanguage: string;
  sttCredential: string;
  llmProvider: string;
  llmModel: string;
  llmPrompt: string;
  llmCredential: string;
  ttsProvider: string;
  ttsVoice: string;
  ttsCredential: string;
  maxMessages: string;
  maxTokens: string;
};

/**
 * Returns the typed v2 config, or null when the stored config predates schema v2
 * or lacks a stage credential; either way the agent needs reconfiguration.
 */
function agentConfigV2(agent: AgentDTO): AgentConfigV2 | null {
  const parsed = agentConfigV2Schema.safeParse(agent.current_version?.config);
  return parsed.success ? parsed.data : null;
}

function draftFromAgent(agent: AgentDTO | null): AgentDraft {
  const cfg = agent ? agentConfigV2(agent) : null;
  return {
    name: agent?.name ?? "",
    description: agent?.description ?? "",
    enabled: agent?.enabled ?? true,
    sttProvider: cfg?.stt.provider ?? "",
    sttModel: cfg?.stt.model ?? "",
    sttLanguage: cfg?.stt.language ?? "en-US",
    sttCredential: cfg?.stt.credential_id ?? "",
    llmProvider: cfg?.llm.provider ?? "",
    llmModel: cfg?.llm.model ?? "",
    llmPrompt: cfg?.llm.system_prompt ?? "",
    llmCredential: cfg?.llm.credential_id ?? "",
    ttsProvider: cfg?.tts.provider ?? "",
    ttsVoice: cfg?.tts.voice ?? "",
    ttsCredential: cfg?.tts.credential_id ?? "",
    maxMessages: cfg?.context?.max_messages?.toString() ?? "",
    maxTokens: cfg?.context?.max_tokens?.toString() ?? "",
  };
}

/** Validates the draft and builds the v2 config; returns error messages on failure. */
function buildAgentConfig(d: AgentDraft): { config: AgentConfigV2 } | { errors: string[] } {
  const errors: string[] = [];
  const need = (value: string, label: string) => {
    if (!value.trim()) errors.push(`${label} is required.`);
  };
  need(d.name, "Agent name");
  need(d.sttProvider, "STT provider");
  need(d.sttCredential, "STT credential");
  need(d.llmProvider, "LLM provider");
  need(d.llmModel, "LLM model");
  need(d.llmCredential, "LLM credential");
  need(d.ttsProvider, "TTS provider");
  need(d.ttsCredential, "TTS credential");
  const limit = (value: string, label: string) => {
    if (!value.trim()) return undefined;
    const n = Number(value);
    if (!Number.isInteger(n) || n <= 0) {
      errors.push(`${label} must be a positive whole number.`);
      return undefined;
    }
    return n;
  };
  const maxMessages = limit(d.maxMessages, "Max messages");
  if (maxMessages === 1) errors.push("Max messages must be at least 2.");
  const maxTokens = limit(d.maxTokens, "Max tokens");
  if (errors.length) return { errors };

  const config: AgentConfigV2 = {
    stt: {
      provider: d.sttProvider,
      ...(d.sttModel.trim() ? { model: d.sttModel.trim() } : {}),
      ...(d.sttLanguage.trim() ? { language: d.sttLanguage.trim() } : {}),
      credential_id: d.sttCredential,
    },
    llm: {
      provider: d.llmProvider,
      model: d.llmModel.trim(),
      ...(d.llmPrompt.trim() ? { system_prompt: d.llmPrompt.trim() } : {}),
      credential_id: d.llmCredential,
    },
    tts: {
      provider: d.ttsProvider,
      ...(d.ttsVoice.trim() ? { voice: d.ttsVoice.trim() } : {}),
      credential_id: d.ttsCredential,
    },
  };
  if (maxMessages !== undefined || maxTokens !== undefined) {
    config.context = {
      ...(maxMessages !== undefined ? { max_messages: maxMessages } : {}),
      ...(maxTokens !== undefined ? { max_tokens: maxTokens } : {}),
    };
  }
  return { config };
}

type AgentFormProps = {
  agent: AgentDTO | null;
  providers: ProviderDefinition[];
  credentials: CredentialDTO[];
  pending: boolean;
  submitLabel: string;
  onSubmit: (input: CreateAgentInput) => void;
  onCancel: () => void;
};

function AgentForm({
  agent,
  providers,
  credentials,
  pending,
  submitLabel,
  onSubmit,
  onCancel,
}: AgentFormProps) {
  const [draft, setDraft] = useState(() => draftFromAgent(agent));
  const [errors, setErrors] = useState<string[]>([]);
  const set = (patch: Partial<AgentDraft>) => setDraft((d) => ({ ...d, ...patch }));

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const result = buildAgentConfig(draft);
    if ("errors" in result) {
      setErrors(result.errors);
      return;
    }
    setErrors([]);
    onSubmit({
      name: draft.name.trim(),
      description: draft.description.trim(),
      enabled: draft.enabled,
      config: result.config,
    });
  }

  // "mcp" holds MCP tokens only; never a voice stage provider.
  const providerOptions = (cap: Capability) =>
    providers.filter((p) => p.id !== "mcp" && p.capabilities.includes(cap));

  const providerSelect = (cap: Capability, value: string, onChange: (v: string) => void) => (
    <label className="form-field" htmlFor={`agent-${cap}-provider`}>
      <span>{cap.toUpperCase()} provider</span>
      <select id={`agent-${cap}-provider`} value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">Select a provider</option>
        {providerOptions(cap).map((p) => (
          <option key={p.id} value={p.id}>
            {p.display_name}
          </option>
        ))}
      </select>
    </label>
  );

  const credentialSelect = (
    cap: Capability,
    provider: string,
    value: string,
    onChange: (v: string) => void,
  ) => {
    const options = credentials.filter((c) => c.provider === provider);
    return (
      <>
        <label className="form-field" htmlFor={`agent-${cap}-credential`}>
          <span>{cap.toUpperCase()} credential</span>
          <select
            id={`agent-${cap}-credential`}
            value={value}
            onChange={(e) => onChange(e.target.value)}
            disabled={!provider}
            required
          >
            <option value="">Select a credential</option>
            {options.map((c) => (
              <option key={c.id} value={c.id}>
                {c.masked_value} ({c.validation_state})
              </option>
            ))}
          </select>
        </label>
        {provider && options.length === 0 && (
          <p className="subtext" role="note">
            No {cap.toUpperCase()} credentials for this provider.{" "}
            <Link to="/providers">Add one on the Providers page</Link>.
          </p>
        )}
      </>
    );
  };

  const stageBox = {
    border: "1px solid #c9d3d2",
    padding: "16px",
    marginTop: "16px",
    background: "#ffffff",
  } as const;

  return (
    <form onSubmit={handleSubmit} className="form-card" noValidate>
      {errors.length > 0 && (
        <div role="alert" className="message-panel" style={{ borderColor: "#a43e30" }}>
          <ul style={{ margin: 0 }}>
            {errors.map((msg) => (
              <li key={msg}>{msg}</li>
            ))}
          </ul>
        </div>
      )}

      <label className="form-field" htmlFor="agent-name-input">
        <span>Agent name</span>
        <input
          id="agent-name-input"
          type="text"
          value={draft.name}
          onChange={(e) => set({ name: e.target.value })}
          placeholder="e.g. Front Desk Assistant"
          maxLength={128}
          required
        />
      </label>

      <label className="form-field" htmlFor="agent-desc-input">
        <span>Description</span>
        <textarea
          id="agent-desc-input"
          value={draft.description}
          onChange={(e) => set({ description: e.target.value })}
          placeholder="Short summary of what this agent does"
          maxLength={1024}
          rows={3}
        />
      </label>

      <label
        className="form-field"
        htmlFor="agent-enabled-checkbox"
        style={{ display: "flex", alignItems: "center", gap: "8px" }}
      >
        <input
          id="agent-enabled-checkbox"
          type="checkbox"
          checked={draft.enabled}
          onChange={(e) => set({ enabled: e.target.checked })}
        />
        <span>Enabled</span>
      </label>

      <fieldset style={stageBox}>
        <legend>Speech-to-text</legend>
        {providerSelect("stt", draft.sttProvider, (v) => set({ sttProvider: v, sttCredential: "" }))}
        <label className="form-field" htmlFor="agent-stt-model">
          <span>STT model</span>
          <input
            id="agent-stt-model"
            type="text"
            value={draft.sttModel}
            onChange={(e) => set({ sttModel: e.target.value })}
            placeholder="Provider default (e.g. chirp_2)"
          />
        </label>
        <label className="form-field" htmlFor="agent-stt-language">
          <span>STT language</span>
          <input
            id="agent-stt-language"
            type="text"
            value={draft.sttLanguage}
            onChange={(e) => set({ sttLanguage: e.target.value })}
            placeholder="e.g. en-US"
          />
        </label>
        {credentialSelect("stt", draft.sttProvider, draft.sttCredential, (v) => set({ sttCredential: v }))}
      </fieldset>

      <fieldset style={stageBox}>
        <legend>Language model</legend>
        {providerSelect("llm", draft.llmProvider, (v) => set({ llmProvider: v, llmCredential: "" }))}
        <label className="form-field" htmlFor="agent-llm-model">
          <span>LLM model</span>
          <input
            id="agent-llm-model"
            type="text"
            value={draft.llmModel}
            onChange={(e) => set({ llmModel: e.target.value })}
            placeholder="e.g. gemini-2.0-flash"
            required
          />
        </label>
        <label className="form-field" htmlFor="agent-llm-prompt">
          <span>System prompt</span>
          <textarea
            id="agent-llm-prompt"
            value={draft.llmPrompt}
            onChange={(e) => set({ llmPrompt: e.target.value })}
            placeholder="Instructions for the agent's personality and behavior"
            rows={4}
          />
        </label>
        {credentialSelect("llm", draft.llmProvider, draft.llmCredential, (v) => set({ llmCredential: v }))}
      </fieldset>

      <fieldset style={stageBox}>
        <legend>Text-to-speech</legend>
        {providerSelect("tts", draft.ttsProvider, (v) => set({ ttsProvider: v, ttsCredential: "" }))}
        <label className="form-field" htmlFor="agent-tts-voice">
          <span>TTS voice</span>
          <input
            id="agent-tts-voice"
            type="text"
            value={draft.ttsVoice}
            onChange={(e) => set({ ttsVoice: e.target.value })}
            placeholder="Provider default (e.g. en-US-Journey-F)"
          />
        </label>
        {credentialSelect("tts", draft.ttsProvider, draft.ttsCredential, (v) => set({ ttsCredential: v }))}
      </fieldset>

      <fieldset style={{ ...stageBox, marginBottom: "16px" }}>
        <legend>Context limits (optional)</legend>
        <label className="form-field" htmlFor="agent-max-messages">
          <span>Max messages</span>
          <input
            id="agent-max-messages"
            type="number"
            min={1}
            value={draft.maxMessages}
            onChange={(e) => set({ maxMessages: e.target.value })}
            placeholder="server default"
          />
        </label>
        <label className="form-field" htmlFor="agent-max-tokens">
          <span>Max tokens</span>
          <input
            id="agent-max-tokens"
            type="number"
            min={1}
            value={draft.maxTokens}
            onChange={(e) => set({ maxTokens: e.target.value })}
            placeholder="server default"
          />
        </label>
      </fieldset>

      <div style={{ display: "flex", gap: "10px", marginTop: "16px" }}>
        <ActionButton type="submit" disabled={pending}>
          {pending ? "Saving…" : submitLabel}
        </ActionButton>
        <button type="button" className="action-button-small" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

export function AgentsView({ client, keyValue }: AgentsViewProps) {
  const queryClient = useQueryClient();
  const [mode, setMode] = useState<ViewMode>("list");
  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null);

  const [formError, setFormError] = useState<ApiError | null>(null);
  const [actionError, setActionError] = useState<ApiError | null>(null);
  const [saveSuccessMessage, setSaveSuccessMessage] = useState<string | null>(null);

  const agentsQuery = useQuery({
    queryKey: ["agents"],
    queryFn: () => client.getAgents(),
    enabled: !!keyValue,
    retry: false,
  });

  const createMutation = useMutation({
    mutationFn: (payload: CreateAgentInput) => client.createAgent(payload),
    onSuccess: (newAgent) => {
      setFormError(null);
      void queryClient.invalidateQueries({ queryKey: ["agents"] });
      setSelectedAgentId(newAgent.id);
      setMode("detail");
    },
    onError: (err) => {
      if (err instanceof ApiError) {
        setFormError(err);
      } else {
        setFormError(new ApiError("Failed to create agent", "create_failed", 500, ""));
      }
    },
  });

  const updateMutation = useMutation({
    mutationFn: ({ id, payload }: { id: string; payload: UpdateAgentInput }) =>
      client.updateAgent(id, payload),
    onSuccess: (updatedAgent) => {
      setFormError(null);
      void queryClient.invalidateQueries({ queryKey: ["agents"] });
      const versionNum = updatedAgent.current_version?.number ?? 1;
      const hash = updatedAgent.current_version?.config_hash ?? "";
      setSaveSuccessMessage(
        `Agent updated. Published immutable version v${versionNum} with hash ${hash}.`,
      );
      setSelectedAgentId(updatedAgent.id);
      setMode("detail");
    },
    onError: (err) => {
      if (err instanceof ApiError) {
        setFormError(err);
      } else {
        setFormError(new ApiError("Failed to update agent", "update_failed", 500, ""));
      }
    },
  });

  const duplicateMutation = useMutation({
    mutationFn: (id: string) => client.duplicateAgent(id),
    onSuccess: () => {
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["agents"] });
    },
    onError: (err) => {
      if (err instanceof ApiError) {
        setActionError(err);
      } else {
        setActionError(new ApiError("Duplicate failed", "duplicate_failed", 500, ""));
      }
    },
  });

  const archiveMutation = useMutation({
    mutationFn: (id: string) => client.archiveAgent(id),
    onSuccess: () => {
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["agents"] });
    },
    onError: (err) => {
      if (err instanceof ApiError) {
        setActionError(err);
      } else {
        setActionError(new ApiError("Archive failed", "archive_failed", 500, ""));
      }
    },
  });

  function startCreate() {
    setFormError(null);
    setSaveSuccessMessage(null);
    setMode("create");
  }

  function startEdit(agent: AgentDTO) {
    setSelectedAgentId(agent.id);
    setFormError(null);
    setSaveSuccessMessage(null);
    setMode("edit");
  }

  const formOpen = mode === "create" || mode === "edit";
  const providersQuery = useQuery({
    queryKey: ["providers"],
    queryFn: () => client.getProviders(),
    enabled: !!keyValue && formOpen,
    retry: false,
  });
  const credentialsQuery = useQuery({
    queryKey: ["credentials"],
    queryFn: () => client.getCredentials(),
    enabled: !!keyValue && formOpen,
    retry: false,
  });
  const providers = providersQuery.data ?? [];
  const credentials = credentialsQuery.data ?? [];
  const optionsFailure = providersQuery.error ?? credentialsQuery.error;
  const optionsError = optionsFailure ? (
    <ErrorState
      title="Failed to load providers or credentials"
      error={optionsFailure as ApiError}
      onRetry={() => {
        void providersQuery.refetch();
        void credentialsQuery.refetch();
      }}
    />
  ) : null;

  const agents = agentsQuery.data ?? [];
  const selectedAgent = agents.find((a) => a.id === selectedAgentId);

  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>Agents</h1>
        <p>Configure the agents people speak with.</p>
      </header>

      {!keyValue ? (
        <EmptyState title="Connect to manage agents">
          Enter a management key below to configure voice agents.
        </EmptyState>
      ) : agentsQuery.isPending ? (
        <LoadingState label="Loading agents" />
      ) : agentsQuery.isError ? (
        <ErrorState
          title="Failed to load agents"
          error={agentsQuery.error as ApiError}
          onRetry={() => {
            void agentsQuery.refetch();
          }}
        />
      ) : (
        <div className="agents-view" style={{ marginTop: "24px" }}>
          {actionError && (
            <div className="section-banner" style={{ marginBottom: "20px" }}>
              <ErrorState
                title="Agent action failed"
                error={actionError}
                onRetry={() => setActionError(null)}
              />
            </div>
          )}

          {saveSuccessMessage && (
            <div
              className="message-panel"
              style={{ marginBottom: "20px", borderColor: "#007e82" }}
              role="status"
            >
              <h2>Version published</h2>
              <p>{saveSuccessMessage}</p>
            </div>
          )}

          {/* List View */}
          {mode === "list" && (
            <section aria-labelledby="agents-list-heading">
              <div
                style={{
                  display: "flex",
                  justifyContent: "space-between",
                  alignItems: "center",
                  marginBottom: "20px",
                }}
              >
                <h2 id="agents-list-heading">Configured agents ({agents.length})</h2>
                <ActionButton onClick={startCreate}>+ Create agent</ActionButton>
              </div>

              {agents.length === 0 ? (
                <EmptyState title="No agents configured">
                  Create your first agent to begin configuring voice interactions.
                </EmptyState>
              ) : (
                <div className="table-scroll">
                  <table>
                    <caption>Persistent voice agent configurations</caption>
                    <thead>
                      <tr>
                        <th scope="col">Name</th>
                        <th scope="col">Readiness</th>
                        <th scope="col">Version</th>
                        <th scope="col">Status</th>
                        <th scope="col">Actions</th>
                      </tr>
                    </thead>
                    <tbody>
                      {agents.map((agent: AgentDTO) => {
                        const tone = getReadinessTone(agent.readiness.state);
                        const label = getReadinessLabel(agent.readiness.state);
                        const versionNum = agent.current_version?.number ?? 1;

                        return (
                          <tr key={agent.id}>
                            <td>
                              <strong>{agent.name}</strong>
                              {agent.description && (
                                <div className="subtext">{agent.description}</div>
                              )}
                            </td>
                            <td>
                              <Status tone={tone}>{label}</Status>
                              {!agentConfigV2(agent) && (
                                <div className="subtext">Needs reconfiguration</div>
                              )}
                              {agent.readiness.reasons &&
                                agent.readiness.reasons.length > 0 && (
                                  <div
                                    className="reasons-list"
                                    style={{
                                      display: "flex",
                                      flexWrap: "wrap",
                                      gap: "4px",
                                      marginTop: "4px",
                                    }}
                                  >
                                    {agent.readiness.reasons.map((r) => (
                                      <span
                                        key={r}
                                        style={{
                                          fontSize: "0.75rem",
                                          color: "#a43e30",
                                          background: "#fbebe8",
                                          padding: "2px 6px",
                                          borderRadius: "2px",
                                        }}
                                      >
                                        {r}
                                      </span>
                                    ))}
                                  </div>
                                )}
                            </td>
                            <td>
                              <code>v{versionNum}</code>
                            </td>
                            <td>
                              {agent.archived ? (
                                <Status tone="bad">Archived</Status>
                              ) : agent.enabled ? (
                                <Status tone="good">Enabled</Status>
                              ) : (
                                <Status tone="neutral">Disabled</Status>
                              )}
                            </td>
                            <td>
                              <div className="action-buttons-cell">
                                <button
                                  className="action-button-small"
                                  onClick={() => {
                                    setSelectedAgentId(agent.id);
                                    setSaveSuccessMessage(null);
                                    setMode("detail");
                                  }}
                                >
                                  View detail
                                </button>
                                <button
                                  className="action-button-small"
                                  onClick={() => startEdit(agent)}
                                >
                                  Edit
                                </button>
                                <button
                                  className="action-button-small"
                                  onClick={() => duplicateMutation.mutate(agent.id)}
                                  disabled={duplicateMutation.isPending}
                                >
                                  Duplicate
                                </button>
                                {!agent.archived && (
                                  <button
                                    className="action-button-small action-button-danger"
                                    onClick={() => archiveMutation.mutate(agent.id)}
                                    disabled={archiveMutation.isPending}
                                  >
                                    Archive
                                  </button>
                                )}
                              </div>
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              )}
            </section>
          )}

          {/* Create View */}
          {mode === "create" && (
            <section aria-labelledby="create-agent-heading">
              <h2 id="create-agent-heading">Create new voice agent</h2>
              <p style={{ color: "#53666b", marginBottom: "20px" }}>
                Configure the speech-to-text, language model and text-to-speech stages. Calls run
                this stored agent directly.
              </p>

              {formError && (
                <ErrorState
                  title="Failed to create agent"
                  error={formError}
                  onRetry={() => setFormError(null)}
                />
              )}
              {optionsError}

              <AgentForm
                agent={null}
                providers={providers}
                credentials={credentials}
                pending={createMutation.isPending}
                submitLabel="Save agent"
                onSubmit={(input) => createMutation.mutate(input)}
                onCancel={() => setMode("list")}
              />
            </section>
          )}

          {/* Edit View */}
          {mode === "edit" && selectedAgent && (
            <section aria-labelledby="edit-agent-heading">
              <h2 id="edit-agent-heading">Edit {selectedAgent.name}</h2>
              <div
                className="message-panel"
                style={{ marginBottom: "20px", borderColor: "#007e82" }}
              >
                <p>
                  <strong>Notice:</strong> Saving changes will publish a new immutable version
                  (v{(selectedAgent.current_version?.number ?? 1) + 1}) with a new config hash.
                  Historical sessions remain pinned to their past versions.
                </p>
                {!agentConfigV2(selectedAgent) && (
                  <p>
                    This agent's stored configuration uses an old format or is missing stage
                    credentials. Choose a provider and credential for every stage to reconfigure it.
                  </p>
                )}
              </div>

              {formError && (
                <ErrorState
                  title="Failed to update agent"
                  error={formError}
                  onRetry={() => setFormError(null)}
                />
              )}
              {optionsError}

              <AgentForm
                key={selectedAgent.id}
                agent={selectedAgent}
                providers={providers}
                credentials={credentials}
                pending={updateMutation.isPending}
                submitLabel="Save changes"
                onSubmit={(input) =>
                  updateMutation.mutate({ id: selectedAgent.id, payload: input })
                }
                onCancel={() => setMode("detail")}
              />
            </section>
          )}

          {/* Detail View */}
          {mode === "detail" && selectedAgent && (
            <section aria-labelledby="agent-detail-heading">
              <div
                style={{
                  display: "flex",
                  justifyContent: "space-between",
                  alignItems: "flex-start",
                  marginBottom: "24px",
                }}
              >
                <div>
                  <h2 id="agent-detail-heading">{selectedAgent.name}</h2>
                  {selectedAgent.description && (
                    <p style={{ color: "#53666b", margin: "4px 0 0" }}>
                      {selectedAgent.description}
                    </p>
                  )}
                </div>
                <div style={{ display: "flex", gap: "8px" }}>
                  <button
                    className="action-button-small"
                    onClick={() => startEdit(selectedAgent)}
                  >
                    Edit
                  </button>
                  <button
                    className="action-button-small"
                    onClick={() => duplicateMutation.mutate(selectedAgent.id)}
                  >
                    Duplicate
                  </button>
                  {!selectedAgent.archived && (
                    <button
                      className="action-button-small action-button-danger"
                      onClick={() => archiveMutation.mutate(selectedAgent.id)}
                    >
                      Archive
                    </button>
                  )}
                  <button
                    className="action-button-small"
                    onClick={() => setMode("list")}
                  >
                    Back to list
                  </button>
                </div>
              </div>

              {/* Status and Readiness overview */}
              <div
                className="detail-grid"
                style={{
                  display: "grid",
                  gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))",
                  gap: "16px",
                  marginBottom: "24px",
                }}
              >
                <div
                  style={{
                    background: "#ffffff",
                    border: "1px solid #c9d3d2",
                    padding: "16px",
                  }}
                >
                  <span style={{ fontSize: "0.8rem", color: "#53666b" }}>Readiness State</span>
                  <div style={{ marginTop: "6px" }}>
                    <Status tone={getReadinessTone(selectedAgent.readiness.state)}>
                      {getReadinessLabel(selectedAgent.readiness.state)}
                    </Status>
                  </div>
                  {!agentConfigV2(selectedAgent) && (
                    <div style={{ marginTop: "8px", fontSize: "0.8rem" }}>
                      Needs reconfiguration: this configuration predates typed agent configs or
                      lacks stage credentials. Edit the agent to choose a provider and credential
                      for each stage.
                    </div>
                  )}
                  {selectedAgent.readiness.reasons &&
                    selectedAgent.readiness.reasons.length > 0 && (
                      <div style={{ marginTop: "8px", fontSize: "0.8rem", color: "#a43e30" }}>
                        Reasons: {selectedAgent.readiness.reasons.join(", ")}
                      </div>
                    )}
                </div>

                <div
                  style={{
                    background: "#ffffff",
                    border: "1px solid #c9d3d2",
                    padding: "16px",
                  }}
                >
                  <span style={{ fontSize: "0.8rem", color: "#53666b" }}>Lifecycle</span>
                  <div style={{ marginTop: "6px" }}>
                    {selectedAgent.archived ? (
                      <Status tone="bad">Archived</Status>
                    ) : selectedAgent.enabled ? (
                      <Status tone="good">Enabled</Status>
                    ) : (
                      <Status tone="neutral">Disabled</Status>
                    )}
                  </div>
                </div>

                <div
                  style={{
                    background: "#ffffff",
                    border: "1px solid #c9d3d2",
                    padding: "16px",
                  }}
                >
                  <span style={{ fontSize: "0.8rem", color: "#53666b" }}>Agent ID</span>
                  <div style={{ marginTop: "6px" }}>
                    <code style={{ fontSize: "0.85rem" }}>{selectedAgent.id}</code>
                  </div>
                </div>
              </div>

              {/* Current Immutable Version Details */}
              {selectedAgent.current_version && (
                <div
                  className="version-card"
                  style={{
                    background: "#ffffff",
                    border: "1px solid #c9d3d2",
                    padding: "20px",
                    marginBottom: "24px",
                  }}
                >
                  <div
                    style={{
                      display: "flex",
                      justifyContent: "space-between",
                      alignItems: "center",
                      borderBottom: "1px solid #c9d3d2",
                      paddingBottom: "12px",
                      marginBottom: "16px",
                    }}
                  >
                    <h3 style={{ margin: 0, fontSize: "1.1rem" }}>
                      Current Immutable Version: v{selectedAgent.current_version.number}
                    </h3>
                    <span style={{ fontSize: "0.8rem", color: "#53666b" }}>
                      Published:{" "}
                      {new Date(selectedAgent.current_version.created_at).toLocaleString()}
                    </span>
                  </div>

                  <div
                    style={{
                      display: "grid",
                      gridTemplateColumns: "1fr 1fr",
                      gap: "12px",
                      marginBottom: "16px",
                    }}
                  >
                    <div>
                      <span style={{ fontSize: "0.8rem", color: "#53666b" }}>Version ID:</span>
                      <div>
                        <code>{selectedAgent.current_version.id}</code>
                      </div>
                    </div>
                    <div>
                      <span style={{ fontSize: "0.8rem", color: "#53666b" }}>Config Hash:</span>
                      <div>
                        <code>{selectedAgent.current_version.config_hash}</code>
                      </div>
                    </div>
                  </div>

                  <div style={{ marginTop: "16px" }}>
                    <h4 style={{ margin: "0 0 8px", fontSize: "0.95rem" }}>
                      Active Voice Configuration
                    </h4>
                    <pre
                      style={{
                        background: "#f3f5f4",
                        padding: "12px",
                        fontSize: "0.85rem",
                        overflowX: "auto",
                        border: "1px solid #c9d3d2",
                      }}
                    >
                      {JSON.stringify(selectedAgent.current_version.config, null, 2)}
                    </pre>
                  </div>

                  {/* Bound MCP Tools */}
                  <div style={{ marginTop: "16px" }}>
                    <h4 style={{ margin: "0 0 8px", fontSize: "0.95rem" }}>
                      Bound MCP Tools (
                      {selectedAgent.current_version.mcp_tools?.length ?? 0})
                    </h4>
                    {selectedAgent.current_version.mcp_tools &&
                    selectedAgent.current_version.mcp_tools.length > 0 ? (
                      <DataTable
                        caption="Tools attached to immutable version snapshot"
                        columns={["Remote Name", "Server ID", "Schema Hash", "Required"]}
                        rows={selectedAgent.current_version.mcp_tools.map((t) => [
                          <strong>{t.remote_name}</strong>,
                          <code>{t.mcp_server_id}</code>,
                          <code>{t.schema_hash}</code>,
                          t.required ? "Required" : "Optional",
                        ])}
                      />
                    ) : (
                      <p style={{ color: "#53666b", fontSize: "0.85rem" }}>
                        No external MCP tools bound.
                      </p>
                    )}
                  </div>
                </div>
              )}

              {/* Live Voice Test Panel */}
              <AgentVoiceTest
                agent={selectedAgent}
                client={client}
                keyValue={keyValue}
              />

              {/* Immutable Snapshot Explanation */}
              <div
                className="message-panel"
                style={{ borderColor: "#007e82", marginTop: "24px" }}
              >
                <h2>Immutable snapshot architecture</h2>
                <p>
                  Every configuration change writes an immutable snapshot with a cryptographic
                  content hash. Historical and live voice sessions pin to a specific version ID and
                  hash, ensuring past interactions can always be precisely audited and cannot be
                  altered by ongoing edits.
                </p>
              </div>
            </section>
          )}
        </div>
      )}
    </>
  );
}
