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

export const demoStatusSchema = z.object({
  dental: z.object({ available: z.boolean(), installed: z.boolean(), flow_id: z.string().optional() }),
});
export type DemoStatus = z.infer<typeof demoStatusSchema>;
export const demoInstallSchema = z.object({
  mcp_server_id: z.string(),
  mcp_created: z.boolean(),
  tools: z.number(),
  flow_id: z.string(),
  flow_created: z.boolean(),
});
export type DemoInstall = z.infer<typeof demoInstallSchema>;

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

// Flow schemas & types
export const toolSelectionSchema = z.object({
  mcp_server_id: z.string(),
  remote_name: z.string(),
  schema_hash: z.string(),
  required: z.boolean(),
});
export type ToolSelection = z.infer<typeof toolSelectionSchema>;

// Versions stored before the flow rename say entry/nodes/edges/handoff_cue (the server reads both).
const LEGACY_KEYS: Record<string, string> = { entry: "start", nodes: "steps", edges: "routes", handoff_cue: "route_cue" };

export const flowVersionSchema = z.object({
  id: z.string(),
  number: z.number(),
  schema_version: z.number(),
  config: z
    .record(z.string(), z.unknown())
    .transform((c) => (Array.isArray(c.nodes) ? Object.fromEntries(Object.entries(c).map(([k, v]) => [LEGACY_KEYS[k] ?? k, v])) : c)),
  config_hash: z.string(),
  created_at: z.string(),
  mcp_tools: z.array(toolSelectionSchema),
  mcp_review_required: z.boolean().optional(),
});
export type FlowVersionDTO = z.infer<typeof flowVersionSchema>;

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

export const flowSchema = z.object({
  id: z.string(),
  name: z.string(),
  description: z.string(),
  enabled: z.boolean(),
  archived: z.boolean(),
  current_version: flowVersionSchema.nullish(),
  readiness: readinessSchema,
  created_at: z.string(),
  updated_at: z.string(),
});
export type FlowDTO = z.infer<typeof flowSchema>;

const flowsEnvelope = z.object({
  data: z.array(flowSchema),
});
const flowVersionEnvelope = z.object({ data: flowVersionSchema });
const flowEnvelope = z.object({
  data: flowSchema,
});
const archiveEnvelope = z.object({
  data: z.object({
    archived: z.boolean(),
  }),
});

// Flow config schema v2: one typed block per stage. Every stage names its
// provider; secrets come from server environment variables, never the config.
const nonEmpty = z.string().trim().min(1);
export const stepConfigV2Schema = z.object({
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
export type StepConfigV2 = z.infer<typeof stepConfigV2Schema>;

/**
 * What the flows API accepts as `config`: the flat stage config, or a flow
 * ({start, steps, routes, route_cue}) whose steps carry flat stage configs.
 * The server validates flows (server/flow/definition.go); the designer mirrors it.
 */
export type FlowConfigPayload = StepConfigV2 | Record<string, unknown>;

export type CreateFlowInput = {
  name: string;
  description: string;
  enabled?: boolean;
  config: FlowConfigPayload;
};

export type UpdateFlowInput = {
  name?: string;
  description?: string;
  enabled?: boolean;
  config?: FlowConfigPayload;
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

/** A span's attributes, or an empty object when it has none. */
export function spanAttrs(span: TraceSpanDTO): Record<string, unknown> {
  return (span.attributes && typeof span.attributes === "object" ? span.attributes : {}) as Record<string, unknown>;
}

/**
 * A route span's event ("preparing", "active", "failed") and the steps it joins, or null for other spans.
 * Sessions stored before the flow rename name them handoff.* with from_node/to_node.
 */
export function routeSpan(span: TraceSpanDTO): { event: string; from: string; to: string } | null {
  const event = /^(?:route|handoff)\.(\w+)$/.exec(span.name)?.[1];
  const a = spanAttrs(span);
  const from = a.from_step ?? a.from_node;
  const to = a.to_step ?? a.to_node;
  return event && typeof from === "string" && typeof to === "string" ? { event, from, to } : null;
}

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
  flow_id: z.string(),
  flow_version_id: z.string(),
  config_hash: z.string(),
  started_at: z.string(),
  ended_at: z.string().nullish(),
  status: z.string(),
  turn_count: z.number(),
  safe_summary: z.string().optional().default(""),
  draft: z.boolean().optional().default(false),
  provider_stack: z.unknown().optional(),
  runtime_snapshot: z.unknown().optional(),
  runtime_tools: z.array(sessionRuntimeToolSchema).nullish(),
});
export type SessionDTO = z.infer<typeof sessionSchema>;

export type SessionFilterParams = {
  limit?: number;
  offset?: number;
  status?: string;
  flow_id?: string;
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
    issueVoiceToken: async (input: { principal: string; flow_id: string }) =>
      (await request("/voice-tokens", tokenEnvelope, { method: "POST", body: input })).data,
    getProviders: async () =>
      (await request("/providers", providersEnvelope)).data,
    // ponytail: one page of the server max (100); page through when someone has more flows.
    // Includes archived flows (sessions still name them); list views filter them out.
    getFlows: async () =>
      (await request("/flows?limit=100", flowsEnvelope)).data,
    getFlow: async (id: string) =>
      (await request(`/flows/${encodeURIComponent(id)}`, flowEnvelope)).data,
    createFlow: async (input: CreateFlowInput) =>
      (
        await request("/flows", flowEnvelope, {
          method: "POST",
          body: input,
        })
      ).data,
    updateFlow: async (id: string, input: UpdateFlowInput) =>
      (
        await request(`/flows/${encodeURIComponent(id)}`, flowEnvelope, {
          method: "PUT",
          body: input,
        })
      ).data,
    // Replaces the tool selection and publishes a new version with the same config.
    setFlowMCPTools: async (id: string, tools: ToolSelection[]) =>
      (
        await request(`/flows/${encodeURIComponent(id)}/mcp-tools`, flowVersionEnvelope, {
          method: "PUT",
          body: { tools },
        })
      ).data,
    archiveFlow: async (id: string) =>
      (
        await request(
          `/flows/${encodeURIComponent(id)}/archive`,
          archiveEnvelope,
          { method: "POST" },
        )
      ).data,
    duplicateFlow: async (id: string, input?: { name?: string }) =>
      (
        await request(
          `/flows/${encodeURIComponent(id)}/duplicate`,
          flowEnvelope,
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
      if (params?.flow_id) searchParams.set("flow_id", params.flow_id);
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

    // Demo content
    getDemo: async () => (await request("/demo", z.object({ data: demoStatusSchema }))).data,
    installDentalDemo: async () =>
      (await request("/demo/dental", z.object({ data: demoInstallSchema }), { method: "POST" })).data,
  };
}

export type ApiClient = ReturnType<typeof createApiClient>;

