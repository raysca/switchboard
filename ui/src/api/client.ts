import { z } from "zod";

const errorEnvelope = z.object({
  error: z.object({
    code: z.string(),
    message: z.string(),
    request_id: z.string(),
    details: z.unknown().optional(),
  }),
});
const tokenEnvelope = z.object({
  data: z.object({
    token: z.string(),
    expires_at: z.string(),
    scope: z.string(),
  }),
});

// Operations & telemetry schemas & types
export const databaseStatusSchema = z.object({
  dialect: z.string(),
  status: z.string(),
  open_connections: z.number(),
  in_use: z.number(),
  migration_version: z.number().optional(),
});
export type DatabaseStatus = z.infer<typeof databaseStatusSchema>;

export const vadStatusSchema = z.object({
  worker_status: z.string(),
  sample_rate: z.number(),
  frame_size: z.number(),
});
export type VADStatus = z.infer<typeof vadStatusSchema>;

// The server always sends every non-optional field; a missing one is an error,
// never a made-up healthy value.
export const operationsStatusSchema = z.object({
  uptime_seconds: z.number(),
  started_at: z.string(),
  profile: z.string(),
  active_sessions: z.number(),
  max_sessions: z.number(),
  capacity_utilization: z.number(),
  sessions_total: z.number(),
  turns_total: z.number(),
  errors_total: z.number(),
  session_rate: z.number().optional(),
  max_call_duration_seconds: z.number().optional(),
  voice_token_max_lifetime_seconds: z.number().optional(),
  database: databaseStatusSchema,
  vad: vadStatusSchema,
  health: z.enum(["healthy", "degraded", "unhealthy"]),
});
export type OperationsStatus = z.infer<typeof operationsStatusSchema>;

const operationsStatusEnvelope = z.object({
  data: operationsStatusSchema,
});


// Provider schemas & types
const catalogEntrySchema = z.object({
  id: z.string(),
  label: z.string(),
  default: z.boolean().optional(),
});
export const serviceStatusSchema = z.object({
  available: z.boolean(),
  reason: z.enum(["missing_env", "not_in_build"]).optional(),
  missing_env: z.array(z.string()).optional(),
  models: z.array(catalogEntrySchema).optional(),
  voices: z.array(catalogEntrySchema).optional(),
  languages: z.array(catalogEntrySchema).optional(),
  custom: z.boolean().optional().default(true),
});
export const providerStatusSchema = z.object({
  id: z.string(),
  display_name: z.string(),
  capabilities: z.array(z.string()),
  detected: z.boolean(),
  env: z.array(z.string()),
  // partialRecord: the server omits services a provider lacks.
  services: z.partialRecord(z.enum(["stt", "llm", "tts"]), serviceStatusSchema),
});
export type ProviderStatus = z.infer<typeof providerStatusSchema>;

const providersEnvelope = z.object({
  data: z.array(providerStatusSchema),
});

// Agent schemas & types
export const toolSelectionSchema = z.object({
  mcp_server_id: z.string(),
  remote_name: z.string(),
  schema_hash: z.string(),
  required: z.boolean(),
});
export type ToolSelection = z.infer<typeof toolSelectionSchema>;

export const agentVersionSchema = z.object({
  id: z.string(),
  number: z.number(),
  schema_version: z.number(),
  config: z.record(z.string(), z.unknown()),
  config_hash: z.string(),
  created_at: z.string(),
  mcp_tools: z.array(toolSelectionSchema),
  mcp_review_required: z.boolean().optional(),
});
export type AgentVersionDTO = z.infer<typeof agentVersionSchema>;

export const toolDependencySchema = z.object({
  mcp_server_id: z.string(),
  remote_name: z.string(),
  required: z.boolean(),
  state: z.string(),
  selected_schema_hash: z.string(),
  current_schema_hash: z.string().optional(),
});
export type ToolDependencyDTO = z.infer<typeof toolDependencySchema>;

export const readinessSchema = z.object({
  state: z.string(),
  reasons: z.array(z.string()),
  tool_dependencies: z.array(toolDependencySchema).optional(),
});
export type ReadinessDTO = z.infer<typeof readinessSchema>;

export const agentSchema = z.object({
  id: z.string(),
  name: z.string(),
  description: z.string(),
  enabled: z.boolean(),
  archived: z.boolean(),
  current_version: agentVersionSchema.nullish(),
  readiness: readinessSchema,
  created_at: z.string(),
  updated_at: z.string(),
});
export type AgentDTO = z.infer<typeof agentSchema>;

const agentsEnvelope = z.object({
  data: z.array(agentSchema),
});
const agentVersionEnvelope = z.object({ data: agentVersionSchema });
const agentEnvelope = z.object({
  data: agentSchema,
});
const archiveEnvelope = z.object({
  data: z.object({
    archived: z.boolean(),
  }),
});

// Agent config schema v2: one typed block per stage. Every stage names its
// provider; secrets come from server environment variables, never the config.
const nonEmpty = z.string().trim().min(1);
export const agentConfigV2Schema = z.object({
  // Mirrors the server: provider is required per stage and
  // llm.model is required; stt.model and tts.voice fall back to provider defaults.
  stt: z.object({
    provider: nonEmpty,
    model: z.string().optional(),
    language: z.string().optional(),
  }),
  llm: z.object({
    provider: nonEmpty,
    model: nonEmpty,
    system_prompt: z.string().optional(),
  }),
  tts: z.object({
    provider: nonEmpty,
    voice: z.string().optional(),
  }),
  context: z
    .object({
      max_messages: z.number().int().min(2).optional(),
      max_tokens: z.number().int().positive().optional(),
    })
    .optional(),
});
export type AgentConfigV2 = z.infer<typeof agentConfigV2Schema>;

/**
 * What the agents API accepts as `config`: the flat stage config, or a graph
 * ({entry, nodes, edges, handoff_cue}) whose nodes carry flat stage configs.
 * The server validates graphs (server/agent/graph.go); the designer mirrors it.
 */
export type AgentConfigPayload = AgentConfigV2 | Record<string, unknown>;

export type CreateAgentInput = {
  name: string;
  description: string;
  enabled?: boolean;
  config: AgentConfigPayload;
};

export type UpdateAgentInput = {
  name?: string;
  description?: string;
  enabled?: boolean;
  config?: AgentConfigPayload;
};

// MCP schemas & types
export const mcpServerSchema = z.object({
  id: z.string(),
  name: z.string(),
  description: z.string().optional().default(""),
  endpoint: z.string(),
  transport: z.string().optional().default("streamable-http"),
  validation_state: z.string(),
  latest_discovery_id: z.string().nullish(),
  auth_env: z.string().optional().default(""),
  auth_env_set: z.boolean().optional().default(false),
  enabled: z.boolean(),
  pending_deletion: z.boolean().optional(),
  last_connected_at: z.string().nullish(),
  last_discovered_at: z.string().nullish(),
});
export type MCPServerDTO = z.infer<typeof mcpServerSchema>;

export const mcpToolSchema = z.object({
  remote_name: z.string(),
  description: z.string().optional().default(""),
  input_schema: z.unknown().optional(),
  schema_hash: z.string(),
});
export type MCPToolDTO = z.infer<typeof mcpToolSchema>;

export const mcpDiscoverySchema = z.object({
  id: z.string(),
  discovery_hash: z.string(),
  created_at: z.string(),
  tools: z.array(mcpToolSchema),
});
export type MCPDiscoveryDTO = z.infer<typeof mcpDiscoverySchema>;

export const mcpServerDetailSchema = mcpServerSchema.extend({
  discovery: mcpDiscoverySchema.nullish(),
});
export type MCPServerDetailDTO = z.infer<typeof mcpServerDetailSchema>;

export const mcpExecutionSchema = z.object({
  execution_id: z.string(),
  mcp_server_id: z.string(),
  tool_name: z.string(),
  status: z.string(),
  duration_ms: z.number(),
  result: z.unknown().optional(),
});
export type MCPToolExecutionResultDTO = z.infer<typeof mcpExecutionSchema>;

export const mcpDetachResultSchema = z.object({
  pending: z.boolean().optional(),
  deleted: z.boolean().optional(),
  server: mcpServerSchema.optional(),
});
export type MCPDetachResultDTO = z.infer<typeof mcpDetachResultSchema>;

export type CreateMCPServerInput = {
  name: string;
  description?: string;
  endpoint: string;
  transport?: string;
  auth_env?: string;
  enabled?: boolean;
};

const mcpServersEnvelope = z.object({
  data: z.array(mcpServerSchema),
});
const mcpServerEnvelope = z.object({
  data: mcpServerSchema,
});
const mcpServerDetailEnvelope = z.object({
  data: mcpServerDetailSchema,
});
const mcpExecutionEnvelope = z.object({
  data: mcpExecutionSchema,
});
const mcpDetachResultEnvelope = z.object({
  data: mcpDetachResultSchema,
});

// Session & Trace schemas & types
export const sessionTraceSpanSchema = z.object({
  id: z.string(),
  session_id: z.string().optional(),
  turn_id: z.string().nullish(),
  parent_id: z.string().nullish(),
  name: z.string(),
  started_at: z.string(),
  ended_at: z.string().nullish(),
  attributes: z.unknown().optional(),
});
export type TraceSpanDTO = z.infer<typeof sessionTraceSpanSchema>;

export const sessionTraceEventSchema = z.object({
  id: z.string(),
  span_id: z.string().optional(),
  session_id: z.string().optional(),
  turn_id: z.string().nullish(),
  name: z.string(),
  occurred_at: z.string(),
  attributes: z.unknown().optional(),
});
export type TraceEventDTO = z.infer<typeof sessionTraceEventSchema>;

export const sessionToolCallSchema = z.object({
  id: z.string(),
  session_id: z.string(),
  turn_id: z.string().nullish(),
  mcp_server_id: z.string().optional(),
  remote_name: z.string(),
  schema_hash: z.string(),
  started_at: z.string(),
  ended_at: z.string().nullish(),
  status: z.string(),
  arguments_summary: z.string().optional().default(""),
  result_summary: z.string().optional().default(""),
});
export type ToolCallDTO = z.infer<typeof sessionToolCallSchema>;

export const sessionTracesSchema = z.object({
  session_id: z.string(),
  spans: z.array(sessionTraceSpanSchema),
  events: z.array(sessionTraceEventSchema),
  tool_calls: z.array(sessionToolCallSchema),
});
export type SessionTracesDTO = z.infer<typeof sessionTracesSchema>;

export const turnSchema = z.object({
  id: z.string(),
  session_id: z.string(),
  ordinal: z.number(),
  started_at: z.string(),
  ended_at: z.string().nullish(),
  transcript: z.string().optional().default(""),
  assistant_text: z.string().optional().default(""),
  interrupted: z.boolean().default(false),
  latencies: z.unknown().optional(),
  tools: z.unknown().optional(),
});
export type TurnDTO = z.infer<typeof turnSchema>;

export const sessionRuntimeToolSchema = z.object({
  mcp_server_id: z.string(),
  remote_name: z.string(),
  schema_hash: z.string(),
  required: z.boolean(),
});
export type SessionRuntimeToolDTO = z.infer<typeof sessionRuntimeToolSchema>;

export const sessionSchema = z.object({
  id: z.string(),
  agent_id: z.string(),
  agent_version_id: z.string(),
  config_hash: z.string(),
  started_at: z.string(),
  ended_at: z.string().nullish(),
  status: z.string(),
  turn_count: z.number(),
  safe_summary: z.string().optional().default(""),
  provider_stack: z.unknown().optional(),
  runtime_snapshot: z.unknown().optional(),
  runtime_tools: z.array(sessionRuntimeToolSchema).nullish(),
});
export type SessionDTO = z.infer<typeof sessionSchema>;

export type SessionFilterParams = {
  limit?: number;
  offset?: number;
  status?: string;
  agent_id?: string;
};

const sessionsEnvelope = z.object({
  data: z.array(sessionSchema),
});
const sessionEnvelope = z.object({
  data: sessionSchema,
});
const turnsEnvelope = z.object({
  data: z.array(turnSchema),
});
const sessionTracesEnvelope = z.object({
  data: sessionTracesSchema,
});

export type VoiceToken = z.infer<typeof tokenEnvelope>["data"];
export type AuthSnapshot = { key: string | null; generation: number };

export class ApiError extends Error {
  constructor(
    message: string,
    public code: string,
    public status: number,
    public requestId: string,
    public details?: unknown,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

/** Keeps an ApiError as is; wraps anything else (a bug, a network oddity) with a readable message. */
export function asApiError(err: unknown, message: string): ApiError {
  return err instanceof ApiError ? err : new ApiError(message, "client_error", 0, "");
}

export function createApiClient(
  getAuth: () => AuthSnapshot,
  fetcher: typeof fetch = fetch,
  onUnauthorized?: (error: ApiError, generation: number) => void,
) {
  async function request<T>(
    path: string,
    schema: z.ZodType<T>,
    options?: {
      method?: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
      body?: unknown;
    },
  ): Promise<T> {
    const { key, generation } = getAuth();
    if (!key)
      throw new ApiError(
        "Enter a management key to continue.",
        "missing_key",
        0,
        "",
      );
    const method =
      options?.method ?? (options?.body !== undefined ? "POST" : "GET");
    let response: Response;
    try {
      response = await fetcher(`/api/v1${path}`, {
        method,
        headers: {
          Authorization: `Bearer ${key}`,
          ...(options?.body !== undefined
            ? { "Content-Type": "application/json" }
            : {}),
        },
        ...(options?.body !== undefined
          ? { body: JSON.stringify(options.body) }
          : {}),
        cache: "no-store",
      });
    } catch {
      throw new ApiError(
        "Cannot reach the management API. Check the admin listener.",
        "network_error",
        0,
        "",
      );
    }
    const requestId = response.headers.get("X-Request-ID") ?? "";
    let payload: unknown;
    try {
      payload = await response.json();
    } catch {
      payload = null;
    }
    if (!response.ok) {
      const parsed = errorEnvelope.safeParse(payload);
      const error = new ApiError(
        parsed.success
          ? parsed.data.error.message
          : "The management API returned an unreadable error.",
        parsed.success ? parsed.data.error.code : "invalid_error",
        response.status,
        requestId || (parsed.success ? parsed.data.error.request_id : ""),
        parsed.success ? parsed.data.error.details : undefined,
      );
      if (response.status === 401) onUnauthorized?.(error, generation);
      throw error;
    }
    const parsed = schema.safeParse(payload);
    if (!parsed.success)
      throw new ApiError(
        "The management API returned an unexpected response.",
        "invalid_response",
        response.status,
        requestId,
      );
    return parsed.data;
  }
  return {
    issueVoiceToken: async (input: { principal: string; agent_id: string }) =>
      (await request("/voice-tokens", tokenEnvelope, { method: "POST", body: input })).data,
    getProviders: async () =>
      (await request("/providers", providersEnvelope)).data,
    // ponytail: one page of the server max (100); page through when someone has more agents.
    // Includes archived agents (sessions still name them); list views filter them out.
    getAgents: async () =>
      (await request("/agents?limit=100", agentsEnvelope)).data,
    getAgent: async (id: string) =>
      (await request(`/agents/${encodeURIComponent(id)}`, agentEnvelope)).data,
    createAgent: async (input: CreateAgentInput) =>
      (
        await request("/agents", agentEnvelope, {
          method: "POST",
          body: input,
        })
      ).data,
    updateAgent: async (id: string, input: UpdateAgentInput) =>
      (
        await request(`/agents/${encodeURIComponent(id)}`, agentEnvelope, {
          method: "PUT",
          body: input,
        })
      ).data,
    // Replaces the tool selection and publishes a new version with the same config.
    setAgentMCPTools: async (id: string, tools: ToolSelection[]) =>
      (
        await request(`/agents/${encodeURIComponent(id)}/mcp-tools`, agentVersionEnvelope, {
          method: "PUT",
          body: { tools },
        })
      ).data,
    archiveAgent: async (id: string) =>
      (
        await request(
          `/agents/${encodeURIComponent(id)}/archive`,
          archiveEnvelope,
          { method: "POST" },
        )
      ).data,
    duplicateAgent: async (id: string, input?: { name?: string }) =>
      (
        await request(
          `/agents/${encodeURIComponent(id)}/duplicate`,
          agentEnvelope,
          {
            method: "POST",
            body: input,
          },
        )
      ).data,

    // MCP endpoints
    getMCPServers: async () =>
      (await request("/mcp-servers", mcpServersEnvelope)).data,
    getMCPServer: async (id: string) =>
      (
        await request(
          `/mcp-servers/${encodeURIComponent(id)}`,
          mcpServerDetailEnvelope,
        )
      ).data,
    createMCPServer: async (payload: CreateMCPServerInput) =>
      (
        await request("/mcp-servers", mcpServerEnvelope, {
          method: "POST",
          body: payload,
        })
      ).data,
    deleteMCPServer: async (id: string, confirmedDetach?: boolean) => {
      if (confirmedDetach) {
        return (
          await request(
            `/mcp-servers/${encodeURIComponent(id)}/detach-delete`,
            mcpDetachResultEnvelope,
            { method: "POST", body: { confirm: true } },
          )
        ).data;
      }
      await request(
        `/mcp-servers/${encodeURIComponent(id)}`,
        z.unknown(),
        { method: "DELETE" },
      );
      return { deleted: true };
    },
    disableMCPServer: async (id: string) =>
      (
        await request(
          `/mcp-servers/${encodeURIComponent(id)}/disable`,
          mcpServerEnvelope,
          { method: "POST" },
        )
      ).data,
    enableMCPServer: async (id: string) =>
      (
        await request(
          `/mcp-servers/${encodeURIComponent(id)}/enable`,
          mcpServerEnvelope,
          { method: "POST" },
        )
      ).data,
    discoverMCPServer: async (id: string) =>
      (
        await request(
          `/mcp-servers/${encodeURIComponent(id)}/validate`,
          mcpServerEnvelope,
          { method: "POST" },
        )
      ).data,
    testMCPTool: async (id: string, toolName: string, args: Record<string, unknown> = {}) =>
      (
        await request(
          `/mcp-servers/${encodeURIComponent(id)}/tools/${encodeURIComponent(toolName)}/test`,
          mcpExecutionEnvelope,
          { method: "POST", body: { arguments: args } },
        )
      ).data,

    // Session & Trace endpoints
    getSessions: async (params?: SessionFilterParams) => {
      const searchParams = new URLSearchParams();
      if (params?.limit !== undefined)
        searchParams.set("limit", String(params.limit));
      if (params?.offset !== undefined)
        searchParams.set("offset", String(params.offset));
      if (params?.status) searchParams.set("status", params.status);
      if (params?.agent_id) searchParams.set("agent_id", params.agent_id);
      const qs = searchParams.toString();
      return (
        await request(`/sessions${qs ? `?${qs}` : ""}`, sessionsEnvelope)
      ).data;
    },
    getSession: async (id: string) =>
      (
        await request(
          `/sessions/${encodeURIComponent(id)}`,
          sessionEnvelope,
        )
      ).data,
    getSessionTurns: async (id: string) =>
      (
        await request(
          `/sessions/${encodeURIComponent(id)}/turns`,
          turnsEnvelope,
        )
      ).data,
    getSessionTraces: async (id: string, turnId?: string) => {
      const qs = turnId ? `?turn_id=${encodeURIComponent(turnId)}` : "";
      return (
        await request(
          `/sessions/${encodeURIComponent(id)}/traces${qs}`,
          sessionTracesEnvelope,
        )
      ).data;
    },

    // Operations & telemetry
    getOperationsStatus: async () =>
      (
        await request(
          "/operations/status",
          operationsStatusEnvelope,
        )
      ).data,
  };
}

export type ApiClient = ReturnType<typeof createApiClient>;

