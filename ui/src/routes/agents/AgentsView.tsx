import { useState, type FormEvent } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ApiClient,
  type AgentDTO,
  type CreateAgentInput,
  type UpdateAgentInput,
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

export function AgentsView({ client, keyValue }: AgentsViewProps) {
  const queryClient = useQueryClient();
  const [mode, setMode] = useState<ViewMode>("list");
  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null);

  // Form draft state
  const [nameDraft, setNameDraft] = useState("");
  const [descriptionDraft, setDescriptionDraft] = useState("");
  const [enabledDraft, setEnabledDraft] = useState(true);
  const [sttDraft, setSttDraft] = useState("google");
  const [llmDraft, setLlmDraft] = useState("gemini-2.0-flash");
  const [ttsDraft, setTtsDraft] = useState("en-US-Journey-F");
  const [promptDraft, setPromptDraft] = useState("");

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
    setNameDraft("");
    setDescriptionDraft("");
    setEnabledDraft(true);
    setSttDraft("");
    setLlmDraft("");
    setTtsDraft("");
    setPromptDraft("");
    setFormError(null);
    setSaveSuccessMessage(null);
    setMode("create");
  }

  function startEdit(agent: AgentDTO) {
    setSelectedAgentId(agent.id);
    setNameDraft(agent.name);
    setDescriptionDraft(agent.description);
    setEnabledDraft(agent.enabled);
    const cfg = (agent.current_version?.config ?? {}) as Record<string, string>;
    setSttDraft(cfg.stt ?? "google");
    setLlmDraft(cfg.llm ?? "google");
    setTtsDraft(cfg.tts ?? "google");
    setPromptDraft(cfg.system_prompt ?? "");
    setFormError(null);
    setSaveSuccessMessage(null);
    setMode("edit");
  }

  function handleCreateSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const name = nameDraft.trim();
    if (!name) return;
    const config: Record<string, unknown> = {
      stt: sttDraft.trim(),
      llm: llmDraft.trim(),
      tts: ttsDraft.trim(),
    };
    if (promptDraft.trim()) {
      config.system_prompt = promptDraft.trim();
    }
    createMutation.mutate({
      name,
      description: descriptionDraft.trim(),
      enabled: enabledDraft,
      config,
    });
  }

  function handleEditSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (!selectedAgentId) return;
    const name = nameDraft.trim();
    if (!name) return;
    const config: Record<string, unknown> = {
      stt: sttDraft.trim(),
      llm: llmDraft.trim(),
      tts: ttsDraft.trim(),
    };
    if (promptDraft.trim()) {
      config.system_prompt = promptDraft.trim();
    }
    updateMutation.mutate({
      id: selectedAgentId,
      payload: {
        name,
        description: descriptionDraft.trim(),
        enabled: enabledDraft,
        config,
      },
    });
  }

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
                Configure an agent with speech-to-text, reasoning model, and speech synthesis voice.
              </p>

              {formError && (
                <ErrorState
                  title="Failed to create agent"
                  error={formError}
                  onRetry={() => setFormError(null)}
                />
              )}

              <form onSubmit={handleCreateSubmit} className="form-card">
                <label className="form-field" htmlFor="agent-name-input">
                  <span>Agent name</span>
                  <input
                    id="agent-name-input"
                    type="text"
                    value={nameDraft}
                    onChange={(e) => setNameDraft(e.target.value)}
                    placeholder="e.g. Front Desk Assistant"
                    maxLength={128}
                    required
                  />
                </label>

                <label className="form-field" htmlFor="agent-desc-input">
                  <span>Description</span>
                  <textarea
                    id="agent-desc-input"
                    value={descriptionDraft}
                    onChange={(e) => setDescriptionDraft(e.target.value)}
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
                    checked={enabledDraft}
                    onChange={(e) => setEnabledDraft(e.target.checked)}
                  />
                  <span>Enabled</span>
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

                  <label className="form-field" htmlFor="agent-stt-input">
                    <span>STT (Speech-to-Text)</span>
                    <input
                      id="agent-stt-input"
                      type="text"
                      value={sttDraft}
                      onChange={(e) => setSttDraft(e.target.value)}
                      placeholder="e.g. google"
                      required
                    />
                  </label>

                  <label className="form-field" htmlFor="agent-llm-input">
                    <span>LLM Model</span>
                    <input
                      id="agent-llm-input"
                      type="text"
                      value={llmDraft}
                      onChange={(e) => setLlmDraft(e.target.value)}
                      placeholder="e.g. gemini-2.0-flash"
                      required
                    />
                  </label>

                  <label className="form-field" htmlFor="agent-tts-input">
                    <span>TTS (Text-to-Speech)</span>
                    <input
                      id="agent-tts-input"
                      type="text"
                      value={ttsDraft}
                      onChange={(e) => setTtsDraft(e.target.value)}
                      placeholder="e.g. en-US-Journey-F"
                      required
                    />
                  </label>

                  <label className="form-field" htmlFor="agent-prompt-input">
                    <span>System prompt (optional)</span>
                    <textarea
                      id="agent-prompt-input"
                      value={promptDraft}
                      onChange={(e) => setPromptDraft(e.target.value)}
                      placeholder="Instructions for the agent voice personality and behavior"
                      rows={3}
                    />
                  </label>
                </div>

                <div style={{ display: "flex", gap: "10px", marginTop: "16px" }}>
                  <ActionButton type="submit" disabled={createMutation.isPending}>
                    {createMutation.isPending ? "Saving…" : "Save agent"}
                  </ActionButton>
                  <button
                    type="button"
                    className="action-button-small"
                    onClick={() => setMode("list")}
                  >
                    Cancel
                  </button>
                </div>
              </form>
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
              </div>

              {formError && (
                <ErrorState
                  title="Failed to update agent"
                  error={formError}
                  onRetry={() => setFormError(null)}
                />
              )}

              <form onSubmit={handleEditSubmit} className="form-card">
                <label className="form-field" htmlFor="edit-agent-name-input">
                  <span>Agent name</span>
                  <input
                    id="edit-agent-name-input"
                    type="text"
                    value={nameDraft}
                    onChange={(e) => setNameDraft(e.target.value)}
                    maxLength={128}
                    required
                  />
                </label>

                <label className="form-field" htmlFor="edit-agent-desc-input">
                  <span>Description</span>
                  <textarea
                    id="edit-agent-desc-input"
                    value={descriptionDraft}
                    onChange={(e) => setDescriptionDraft(e.target.value)}
                    maxLength={1024}
                    rows={3}
                  />
                </label>

                <label
                  className="form-field"
                  htmlFor="edit-agent-enabled-checkbox"
                  style={{ display: "flex", alignItems: "center", gap: "8px" }}
                >
                  <input
                    id="edit-agent-enabled-checkbox"
                    type="checkbox"
                    checked={enabledDraft}
                    onChange={(e) => setEnabledDraft(e.target.checked)}
                  />
                  <span>Enabled</span>
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

                  <label className="form-field" htmlFor="edit-agent-stt-input">
                    <span>STT (Speech-to-Text)</span>
                    <input
                      id="edit-agent-stt-input"
                      type="text"
                      value={sttDraft}
                      onChange={(e) => setSttDraft(e.target.value)}
                      required
                    />
                  </label>

                  <label className="form-field" htmlFor="edit-agent-llm-input">
                    <span>LLM Model</span>
                    <input
                      id="edit-agent-llm-input"
                      type="text"
                      value={llmDraft}
                      onChange={(e) => setLlmDraft(e.target.value)}
                      required
                    />
                  </label>

                  <label className="form-field" htmlFor="edit-agent-tts-input">
                    <span>TTS (Text-to-Speech)</span>
                    <input
                      id="edit-agent-tts-input"
                      type="text"
                      value={ttsDraft}
                      onChange={(e) => setTtsDraft(e.target.value)}
                      required
                    />
                  </label>

                  <label className="form-field" htmlFor="edit-agent-prompt-input">
                    <span>System prompt (optional)</span>
                    <textarea
                      id="edit-agent-prompt-input"
                      value={promptDraft}
                      onChange={(e) => setPromptDraft(e.target.value)}
                      rows={3}
                    />
                  </label>
                </div>

                <div style={{ display: "flex", gap: "10px", marginTop: "16px" }}>
                  <ActionButton type="submit" disabled={updateMutation.isPending}>
                    {updateMutation.isPending ? "Saving…" : "Save changes"}
                  </ActionButton>
                  <button
                    type="button"
                    className="action-button-small"
                    onClick={() => setMode("detail")}
                  >
                    Cancel
                  </button>
                </div>
              </form>
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
