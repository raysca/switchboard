import { useEffect, useState, type FormEvent } from "react";
import { useLocation, useParams } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ApiClient,
  type MCPServerDTO,
  type MCPToolDTO,
  type CreateMCPServerInput,
  ApiError,
} from "../../api/client";
import {
  ActionButton,
  DataTable,
  EmptyState,
  ErrorState,
  FormField,
  LoadingState,
  Status,
} from "../../components/primitives";

export type MCPViewProps = {
  client: ApiClient;
  keyValue: string | null;
};

type ViewMode = "list" | "create" | "detail";

export function getMCPStatusTone(server: MCPServerDTO): "good" | "bad" | "neutral" {
  if (!server.enabled || server.validation_state === "disabled") {
    return "neutral";
  }
  if (server.validation_state === "valid" || server.validation_state === "connected") {
    return "good";
  }
  if (
    server.validation_state === "invalid" ||
    server.validation_state === "schema_invalid" ||
    server.validation_state === "authentication_failed" ||
    server.validation_state === "unreachable" ||
    server.validation_state === "error"
  ) {
    return "bad";
  }
  return "neutral";
}

export function getMCPStatusLabel(server: MCPServerDTO): string {
  if (!server.enabled || server.validation_state === "disabled") {
    return "disabled";
  }
  if (server.validation_state === "valid" || server.validation_state === "connected") {
    return "connected";
  }
  if (
    server.validation_state === "invalid" ||
    server.validation_state === "schema_invalid" ||
    server.validation_state === "authentication_failed" ||
    server.validation_state === "unreachable"
  ) {
    return "error";
  }
  return server.validation_state || "not_validated";
}

export function formatApiError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    return err.requestId ? `${err.message} (Request ID: ${err.requestId})` : err.message;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return fallback;
}

export function MCPView({ client, keyValue }: MCPViewProps) {
  const { serverId: initialServerId } = useParams();
  const queryClient = useQueryClient();
  const [mode, setMode] = useState<ViewMode>(initialServerId ? "detail" : "list");
  const [selectedServerId, setSelectedServerId] = useState<string | null>(initialServerId ?? null);
  // Follow the URL: /mcp-servers/:serverId opens that server, /mcp-servers the list.
  const { key: locationKey } = useLocation();
  useEffect(() => {
    setMode(initialServerId ? "detail" : "list");
    setSelectedServerId(initialServerId ?? null);
  }, [initialServerId, locationKey]);

  // Create server form state
  const [nameDraft, setNameDraft] = useState("");
  const [descriptionDraft, setDescriptionDraft] = useState("");
  const [endpointDraft, setEndpointDraft] = useState("");
  const [transportDraft, setTransportDraft] = useState("streamable-http");
  const [credentialDraft, setCredentialDraft] = useState("");
  const [enabledDraft, setEnabledDraft] = useState(true);
  const [formError, setFormError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  // Conflict warning state for delete protection
  const [conflictAlert, setConflictAlert] = useState<{
    serverId: string;
    serverName: string;
    message: string;
    details?: unknown;
  } | null>(null);

  // Active testing state for tool console
  const [activeTestTool, setActiveTestTool] = useState<string | null>(null);
  const [testArgsDraft, setTestArgsDraft] = useState<string>("{}");
  const [testResult, setTestResult] = useState<{
    executionId: string;
    status: string;
    durationMs: number;
    result: unknown;
  } | null>(null);
  const [testError, setTestError] = useState<string | null>(null);
  const [testLoading, setTestLoading] = useState(false);

  // Expanded tool schemas
  const [expandedSchemas, setExpandedSchemas] = useState<Record<string, boolean>>({});

  const serversQuery = useQuery({
    queryKey: ["mcp-servers"],
    queryFn: () => client.getMCPServers(),
    enabled: !!keyValue,
    retry: false,
  });

  const credentialsQuery = useQuery({
    queryKey: ["credentials"],
    queryFn: () => client.getCredentials(),
    enabled: !!keyValue,
    retry: false,
  });

  const agentsQuery = useQuery({
    queryKey: ["agents"],
    queryFn: () => client.getAgents(),
    enabled: !!keyValue,
    retry: false,
  });

  const serverDetailQuery = useQuery({
    queryKey: ["mcp-server", selectedServerId],
    queryFn: () => client.getMCPServer(selectedServerId!),
    enabled: !!keyValue && !!selectedServerId && mode === "detail",
    retry: false,
  });

  const createMutation = useMutation({
    mutationFn: (payload: CreateMCPServerInput) => client.createMCPServer(payload),
    onSuccess: (newServer) => {
      setFormError(null);
      void queryClient.invalidateQueries({ queryKey: ["mcp-servers"] });
      setSelectedServerId(newServer.id);
      setMode("detail");
    },
    onError: (err) => {
      setFormError(formatApiError(err, "Failed to create MCP server"));
    },
  });

  const enableMutation = useMutation({
    mutationFn: (id: string) => client.enableMCPServer(id),
    onSuccess: () => {
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["mcp-servers"] });
      if (selectedServerId) {
        void queryClient.invalidateQueries({ queryKey: ["mcp-server", selectedServerId] });
      }
    },
    onError: (err) => {
      setActionError(formatApiError(err, "Failed to enable MCP server"));
    },
  });

  const disableMutation = useMutation({
    mutationFn: (id: string) => client.disableMCPServer(id),
    onSuccess: () => {
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["mcp-servers"] });
      if (selectedServerId) {
        void queryClient.invalidateQueries({ queryKey: ["mcp-server", selectedServerId] });
      }
    },
    onError: (err) => {
      setActionError(formatApiError(err, "Failed to disable MCP server"));
    },
  });

  const discoverMutation = useMutation({
    mutationFn: (id: string) => client.discoverMCPServer(id),
    onSuccess: () => {
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["mcp-servers"] });
      if (selectedServerId) {
        void queryClient.invalidateQueries({ queryKey: ["mcp-server", selectedServerId] });
      }
    },
    onError: (err) => {
      setActionError(formatApiError(err, "Failed to discover MCP server"));
    },
  });

  const deleteMutation = useMutation({
    mutationFn: async (id: string) => {
      return await client.deleteMCPServer(id);
    },
    onSuccess: () => {
      setConflictAlert(null);
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["mcp-servers"] });
      if (selectedServerId) {
        setSelectedServerId(null);
        setMode("list");
      }
    },
    onError: (err, id) => {
      const server = (serversQuery.data ?? []).find((s) => s.id === id);
      if (err instanceof ApiError && (err.status === 409 || err.code === "mcp_server_in_use")) {
        setConflictAlert({
          serverId: id,
          serverName: server?.name ?? id,
          message: formatApiError(err, err.message),
          details: err.details,
        });
      } else {
        setActionError(formatApiError(err, "Failed to delete MCP server"));
      }
    },
  });

  const detachDeleteMutation = useMutation({
    mutationFn: async (id: string) => {
      return await client.deleteMCPServer(id, true);
    },
    onSuccess: () => {
      setConflictAlert(null);
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["mcp-servers"] });
      void queryClient.invalidateQueries({ queryKey: ["agents"] });
      if (selectedServerId) {
        setSelectedServerId(null);
        setMode("list");
      }
    },
    onError: (err) => {
      setActionError(formatApiError(err, "Failed to detach and delete MCP server"));
    },
  });

  if (!keyValue) {
    return (
      <>
        <header className="section-heading">
          <h1 tabIndex={-1}>MCP Servers</h1>
          <p>Connect and inspect Model Context Protocol tools for voice agents.</p>
        </header>
        <EmptyState title="Connect to inspect MCP servers">
          Enter a management key to inspect MCP servers and discovered tools.
        </EmptyState>
      </>
    );
  }

  function handleCreateSubmit(e: FormEvent) {
    e.preventDefault();
    if (!nameDraft.trim()) {
      setFormError("Server name is required");
      return;
    }
    if (!endpointDraft.trim()) {
      setFormError("Endpoint URL is required");
      return;
    }
    // onError reports failures.
    createMutation.mutate({
      name: nameDraft.trim(),
      description: descriptionDraft.trim() || undefined,
      endpoint: endpointDraft.trim(),
      transport: transportDraft,
      credential_id: credentialDraft || undefined,
      enabled: enabledDraft,
    });
  }

  async function handleExecuteTool(serverId: string, toolName: string) {
    setTestError(null);
    setTestResult(null);

    let parsed: unknown;
    try {
      parsed = JSON.parse(testArgsDraft || "{}");
    } catch {
      setTestError("Arguments must be valid JSON");
      return;
    }

    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      setTestError("Arguments must be a valid JSON object");
      return;
    }

    // Enforce 64 KiB bounded execution limit
    if (new TextEncoder().encode(testArgsDraft).length > 65536) {
      setTestError("Arguments exceed the 64 KiB execution limit");
      return;
    }

    const parsedArgs = parsed as Record<string, unknown>;

    setTestLoading(true);
    try {
      const res = await client.testMCPTool(serverId, toolName, parsedArgs);
      setTestResult({
        executionId: res.execution_id,
        status: res.status,
        durationMs: res.duration_ms,
        result: res.result,
      });
    } catch (err) {
      setTestError(formatApiError(err, "Tool execution failed"));
    } finally {
      setTestLoading(false);
    }
  }

  // Determine if a tool schema has drifted compared to what agents have bound
  const getToolDriftInfo = (tool: MCPToolDTO, serverId: string): boolean => {
    if (!agentsQuery.data) return false;
    for (const agent of agentsQuery.data) {
      if (agent.archived) continue;
      const selections = agent.current_version?.mcp_tools ?? [];
      for (const sel of selections) {
        if (sel.mcp_server_id === serverId && sel.remote_name === tool.remote_name) {
          if (sel.schema_hash !== tool.schema_hash) {
            return true;
          }
        }
      }
      const deps = agent.readiness.tool_dependencies ?? [];
      for (const dep of deps) {
        if (dep.mcp_server_id === serverId && dep.remote_name === tool.remote_name) {
          if (dep.state === "schema_drift" || dep.state === "needs_review") {
            return true;
          }
        }
      }
    }
    return false;
  };

  const servers = serversQuery.data ?? [];

  return (
    <>
      <header className="section-heading">
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", flexWrap: "wrap", gap: 16 }}>
          <div>
            <h1 tabIndex={-1}>MCP Servers</h1>
            <p>Connect and inspect Model Context Protocol tools for voice agents.</p>
          </div>
          {mode === "list" && (
            <ActionButton
              onClick={() => {
                setNameDraft("");
                setDescriptionDraft("");
                setEndpointDraft("");
                setTransportDraft("streamable-http");
                setCredentialDraft("");
                setEnabledDraft(true);
                setFormError(null);
                setActionError(null);
                setMode("create");
              }}
            >
              Add MCP Server
            </ActionButton>
          )}
        </div>
      </header>

      {/* Action Error Alert Box */}
      {actionError && (
        <section className="alert-box" style={{ borderColor: "#a43e30" }} role="alert" aria-label="Action error">
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
            <p style={{ color: "#a43e30", margin: 0 }}>{actionError}</p>
            <button
              type="button"
              className="action-button-small"
              onClick={() => setActionError(null)}
            >
              Dismiss
            </button>
          </div>
        </section>
      )}

      {/* Delete Conflict Warning Alert Box */}
      {conflictAlert && (
        <section className="alert-box" role="alert" aria-label="Delete conflict warning" data-testid="delete-conflict-alert">
          <h3>Server in Use: Deletion Blocked</h3>
          <p>
            The server <strong>{conflictAlert.serverName}</strong> cannot be deleted directly because it is
            referenced by existing agents or active sessions.
          </p>
          {conflictAlert.message && <p className="subtext">{conflictAlert.message}</p>}
          <div style={{ display: "flex", gap: 8, marginTop: 12 }}>
            <button
              type="button"
              className="action-button-small action-button-danger"
              disabled={detachDeleteMutation.isPending}
              onClick={() => detachDeleteMutation.mutate(conflictAlert.serverId)}
            >
              {detachDeleteMutation.isPending ? "Detaching…" : "Confirm Detach & Delete"}
            </button>
            <button
              type="button"
              className="action-button-small"
              onClick={() => setConflictAlert(null)}
            >
              Cancel
            </button>
          </div>
        </section>
      )}

      {/* Mode 1: Create Form */}
      {mode === "create" && (
        <section aria-label="Create MCP Server">
          <div style={{ marginBottom: 16 }}>
            <button
              type="button"
              className="subtext"
              style={{ background: "none", border: "none", cursor: "pointer", padding: 0 }}
              onClick={() => setMode("list")}
            >
              ← Back to MCP Servers
            </button>
          </div>
          <h2>Add MCP Server</h2>
          {formError && (
            <div className="alert-box" style={{ borderColor: "#a43e30" }} role="alert">
              <p style={{ color: "#a43e30", margin: 0 }}>{formError}</p>
            </div>
          )}
          <form className="form-card" onSubmit={handleCreateSubmit}>
            <FormField
              id="mcp-name"
              label="Server Name"
              required
              value={nameDraft}
              placeholder="e.g. Order Service"
              onChange={(e) => setNameDraft(e.target.value)}
            />
            <FormField
              id="mcp-desc"
              label="Description (optional)"
              value={descriptionDraft}
              placeholder="e.g. Order lookup and status checking"
              onChange={(e) => setDescriptionDraft(e.target.value)}
            />
            <FormField
              id="mcp-endpoint"
              label="Endpoint URL"
              required
              type="url"
              value={endpointDraft}
              placeholder="https://mcp.example.com/rpc"
              onChange={(e) => setEndpointDraft(e.target.value)}
            />
            <label className="form-field" htmlFor="mcp-transport">
              <span>Transport</span>
              <select
                id="mcp-transport"
                value={transportDraft}
                onChange={(e) => setTransportDraft(e.target.value)}
              >
                <option value="streamable-http">Streamable HTTP (recommended)</option>
              </select>
            </label>
            <label className="form-field" htmlFor="mcp-credential">
              <span>Authentication Credential (optional)</span>
              <select
                id="mcp-credential"
                value={credentialDraft}
                onChange={(e) => setCredentialDraft(e.target.value)}
              >
                <option value="">(None / No credential)</option>
                {(credentialsQuery.data ?? []).map((cred) => (
                  <option key={cred.id} value={cred.id}>
                    {cred.provider} ({cred.masked_value})
                  </option>
                ))}
              </select>
            </label>
            <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: "0.9rem" }}>
              <input
                type="checkbox"
                checked={enabledDraft}
                onChange={(e) => setEnabledDraft(e.target.checked)}
              />
              Enabled for agent assignment
            </label>
            <div style={{ display: "flex", gap: 8, marginTop: 8 }}>
              <ActionButton type="submit" disabled={createMutation.isPending}>
                {createMutation.isPending ? "Connecting…" : "Create Server"}
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

      {/* Mode 2: Server Detail View */}
      {mode === "detail" && selectedServerId && (
        <section aria-label="MCP Server Detail">
          <div style={{ marginBottom: 16 }}>
            <button
              type="button"
              className="subtext"
              style={{ background: "none", border: "none", cursor: "pointer", padding: 0 }}
              onClick={() => {
                setSelectedServerId(null);
                setMode("list");
              }}
            >
              ← Back to MCP Servers
            </button>
          </div>

          {serverDetailQuery.isPending ? (
            <LoadingState label="Loading MCP server detail" />
          ) : serverDetailQuery.isError ? (
            <ErrorState
              title="Could not load server"
              error={serverDetailQuery.error as ApiError}
              onRetry={() => {
                void serverDetailQuery.refetch();
              }}
            />
          ) : (
            (() => {
              const server = serverDetailQuery.data;
              const statusLabel = getMCPStatusLabel(server);
              const statusTone = getMCPStatusTone(server);

              return (
                <>
                  <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", flexWrap: "wrap", gap: 16 }}>
                    <div>
                      <h2 style={{ fontSize: "1.8rem", margin: "0 0 6px" }}>{server.name}</h2>
                      {server.description && <p className="subtext" style={{ fontSize: "0.95rem" }}>{server.description}</p>}
                    </div>
                    <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
                      <span className={`badge badge--${statusTone}`} role="status">
                        {statusLabel}
                      </span>
                      {server.enabled ? (
                        <button
                          type="button"
                          className="action-button-small"
                          onClick={() => disableMutation.mutate(server.id)}
                          disabled={disableMutation.isPending}
                        >
                          Disable
                        </button>
                      ) : (
                        <button
                          type="button"
                          className="action-button-small"
                          onClick={() => enableMutation.mutate(server.id)}
                          disabled={enableMutation.isPending}
                        >
                          Enable
                        </button>
                      )}
                      <button
                        type="button"
                        className="action-button-small"
                        onClick={() => discoverMutation.mutate(server.id)}
                        disabled={discoverMutation.isPending}
                      >
                        {discoverMutation.isPending ? "Discovering…" : "Refresh Discovery"}
                      </button>
                      <button
                        type="button"
                        className="action-button-small action-button-danger"
                        onClick={() => deleteMutation.mutate(server.id)}
                        disabled={deleteMutation.isPending}
                      >
                        Delete
                      </button>
                    </div>
                  </div>

                  {/* Metadata Cards */}
                  <div className="meta-grid">
                    <div className="meta-card">
                      <div className="label">Endpoint</div>
                      <div className="value" style={{ fontSize: "0.9rem", wordBreak: "break-all" }}>
                        <code>{server.endpoint}</code>
                      </div>
                    </div>
                    <div className="meta-card">
                      <div className="label">Transport</div>
                      <div className="value">{server.transport}</div>
                    </div>
                    <div className="meta-card">
                      <div className="label">Credential</div>
                      <div className="value" style={{ fontSize: "0.85rem" }}>
                        {server.credential_id ? <code>{server.credential_id}</code> : "None"}
                      </div>
                    </div>
                    <div className="meta-card">
                      <div className="label">Discovery Snapshot Hash</div>
                      <div className="value">
                        <code className="masked-value" style={{ fontSize: "0.78rem" }}>
                          {server.discovery?.discovery_hash
                            ? server.discovery.discovery_hash.slice(0, 16) + "…"
                            : "none"}
                        </code>
                      </div>
                    </div>
                  </div>

                  {/* Discovered Tools List */}
                  <section style={{ marginTop: 32 }} aria-label="Discovered tools">
                    <h3 style={{ fontSize: "1.3rem", fontWeight: 600, marginBottom: 16 }}>
                      Discovered Tools ({server.discovery?.tools.length ?? 0})
                    </h3>

                    {!server.discovery || server.discovery.tools.length === 0 ? (
                      <EmptyState title="No tools discovered">
                        No tools were discovered from this MCP server. Click "Refresh Discovery" to connect and introspect.
                      </EmptyState>
                    ) : (
                      <div style={{ display: "grid", gap: 16 }}>
                        {server.discovery.tools.map((tool) => {
                          const hasDrift = getToolDriftInfo(tool, server.id);
                          const isTesting = activeTestTool === tool.remote_name;
                          const isSchemaExpanded = !!expandedSchemas[tool.remote_name];

                          return (
                            <div key={tool.remote_name} className="turn-card" data-testid={`tool-${tool.remote_name}`}>
                              <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", flexWrap: "wrap", gap: 12 }}>
                                <div>
                                  <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                                    <h4 style={{ margin: 0, fontSize: "1.1rem" }}>🔧 {tool.remote_name}</h4>
                                    {hasDrift && (
                                      <span
                                        className="badge badge--warning"
                                        data-testid="schema-drift-badge"
                                        role="status"
                                      >
                                        Schema drift
                                      </span>
                                    )}
                                  </div>
                                  {tool.description && (
                                    <p className="subtext" style={{ margin: "4px 0 0" }}>{tool.description}</p>
                                  )}
                                  <div className="subtext" style={{ marginTop: 6 }}>
                                    Canonical schema hash: <code>{tool.schema_hash.slice(0, 12)}…</code>
                                  </div>
                                </div>
                                <div style={{ display: "flex", gap: 8 }}>
                                  <button
                                    type="button"
                                    className="action-button-small"
                                    onClick={() =>
                                      setExpandedSchemas((prev) => ({
                                        ...prev,
                                        [tool.remote_name]: !prev[tool.remote_name],
                                      }))
                                    }
                                  >
                                    {isSchemaExpanded ? "Hide Schema" : "View Schema"}
                                  </button>
                                  <button
                                    type="button"
                                    className="action-button-small"
                                    onClick={() => {
                                      if (isTesting) {
                                        setActiveTestTool(null);
                                        setTestResult(null);
                                        setTestError(null);
                                      } else {
                                        setActiveTestTool(tool.remote_name);
                                        setTestResult(null);
                                        setTestError(null);
                                        setTestArgsDraft("{}");
                                      }
                                    }}
                                  >
                                    {isTesting ? "Close Test Console" : "Test Console"}
                                  </button>
                                </div>
                              </div>

                              {/* Collapsible Schema Viewer */}
                              {isSchemaExpanded && (
                                <div style={{ marginTop: 16 }} data-testid="tool-schema-viewer">
                                  <div className="subtext" style={{ fontWeight: 600, marginBottom: 4 }}>
                                    JSON Schema:
                                  </div>
                                  <pre
                                    className="console-box"
                                    style={{ background: "#f8faf9", color: "#15262a", border: "1px solid #c9d3d2" }}
                                  >
                                    <code>{JSON.stringify(tool.input_schema, null, 2)}</code>
                                  </pre>
                                </div>
                              )}

                              {/* Interactive Test Console */}
                              {isTesting && (
                                <section
                                  style={{ marginTop: 16, borderTop: "1px dashed #c9d3d2", paddingTop: 16 }}
                                  aria-label={`Test console for ${tool.remote_name}`}
                                  data-testid="test-console"
                                >
                                  <h5 style={{ margin: "0 0 8px", fontSize: "0.95rem" }}>
                                    Interactive Test Console: {tool.remote_name}
                                  </h5>
                                  <p className="subtext" style={{ margin: "0 0 8px" }}>
                                    Execute this tool with bounded test arguments outside a conversation.
                                  </p>

                                  <label htmlFor={`test-args-${tool.remote_name}`} className="subtext" style={{ display: "block", marginBottom: 4 }}>
                                    JSON Arguments (max 64 KiB):
                                  </label>
                                  <textarea
                                    id={`test-args-${tool.remote_name}`}
                                    className="console-input"
                                    rows={4}
                                    value={testArgsDraft}
                                    onChange={(e) => setTestArgsDraft(e.target.value)}
                                    placeholder='{ "param": "value" }'
                                    data-testid="test-arguments-input"
                                  />

                                  <div style={{ marginTop: 8 }}>
                                    <button
                                      type="button"
                                      className="action-button-small"
                                      disabled={testLoading}
                                      onClick={() => handleExecuteTool(server.id, tool.remote_name)}
                                      data-testid="execute-tool-button"
                                    >
                                      {testLoading ? "Executing…" : "Execute Tool"}
                                    </button>
                                  </div>

                                  {testError && (
                                    <div className="alert-box" style={{ borderColor: "#a43e30", marginTop: 12 }} role="alert">
                                      <p style={{ color: "#a43e30", margin: 0 }}>{testError}</p>
                                    </div>
                                  )}

                                  {testResult && (
                                    <div style={{ marginTop: 12 }} data-testid="execution-result">
                                      <div style={{ display: "flex", gap: 12, alignItems: "center", marginBottom: 6 }}>
                                        <Status tone={testResult.status === "completed" || testResult.status === "success" ? "good" : "bad"}>
                                          {testResult.status}
                                        </Status>
                                        <span className="subtext">
                                          Duration: {testResult.durationMs}ms
                                        </span>
                                        <span className="subtext">
                                          Execution ID: <code>{testResult.executionId.slice(0, 8)}…</code>
                                        </span>
                                      </div>
                                      <pre className="console-box" data-testid="result-json">
                                        <code>{JSON.stringify(testResult.result, null, 2)}</code>
                                      </pre>
                                    </div>
                                  )}
                                </section>
                              )}
                            </div>
                          );
                        })}
                      </div>
                    )}
                  </section>
                </>
              );
            })()
          )}
        </section>
      )}

      {/* Mode 3: Server List View */}
      {mode === "list" && (
        <section aria-label="MCP Server list">
          {serversQuery.isPending ? (
            <LoadingState label="Loading MCP servers" />
          ) : serversQuery.isError ? (
            <ErrorState
              title="Could not load MCP servers"
              error={serversQuery.error as ApiError}
              onRetry={() => {
                void serversQuery.refetch();
              }}
            />
          ) : servers.length === 0 ? (
            <EmptyState title="No MCP servers configured">
              No Model Context Protocol servers have been connected yet. Click "Add MCP Server" above to register an endpoint.
            </EmptyState>
          ) : (
            <DataTable
              caption="Configured MCP servers"
              columns={["Name", "Endpoint", "Transport", "Status", "Discovery Hash", "Actions"]}
              rows={servers.map((s) => {
                const statusLabel = getMCPStatusLabel(s);
                const statusTone = getMCPStatusTone(s);

                return [
                  <span key="name" style={{ fontWeight: 600 }}>
                    {s.name}
                  </span>,
                  <code key="endpoint" style={{ fontSize: "0.85rem" }}>
                    {s.endpoint}
                  </code>,
                  s.transport,
                  <span key="status" className={`badge badge--${statusTone}`} role="status">
                    {statusLabel}
                  </span>,
                  <code key="hash" className="masked-value" style={{ fontSize: "0.75rem" }}>
                    {s.latest_discovery_id ? s.latest_discovery_id.slice(0, 8) + "…" : "none"}
                  </code>,
                  <div key="actions" className="action-buttons-cell">
                    <button
                      type="button"
                      className="action-button-small"
                      onClick={() => {
                        setSelectedServerId(s.id);
                        setMode("detail");
                      }}
                    >
                      Inspect
                    </button>
                    {s.enabled ? (
                      <button
                        type="button"
                        className="action-button-small"
                        disabled={disableMutation.isPending}
                        onClick={() => disableMutation.mutate(s.id)}
                      >
                        Disable
                      </button>
                    ) : (
                      <button
                        type="button"
                        className="action-button-small"
                        disabled={enableMutation.isPending}
                        onClick={() => enableMutation.mutate(s.id)}
                      >
                        Enable
                      </button>
                    )}
                    <button
                      type="button"
                      className="action-button-small"
                      disabled={discoverMutation.isPending}
                      onClick={() => discoverMutation.mutate(s.id)}
                    >
                      Discover
                    </button>
                    <button
                      type="button"
                      className="action-button-small action-button-danger"
                      disabled={deleteMutation.isPending}
                      onClick={() => deleteMutation.mutate(s.id)}
                    >
                      Delete
                    </button>
                  </div>,
                ];
              })}
            />
          )}
        </section>
      )}
    </>
  );
}
