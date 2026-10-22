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
const databaseStatusSchema = z.object({
  dialect: z.string(),
  status: z.string(),
  open_connections: z.number(),
  in_use: z.number(),
  migration_version: z.number().optional(),
});
type DatabaseStatus = z.infer<typeof databaseStatusSchema>;

const vadStatusSchema = z.object({
  worker_status: z.string(),
  sample_rate: z.number(),
  frame_size: z.number(),
});
type VADStatus = z.infer<typeof vadStatusSchema>;

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

// What the server tells the editor about itself (GET /server-config). The editor reads the sound names
// and the quiet-caller defaults from it; the rest is for other screens.
export const serverConfigSchema = z.object({
  profile: z.string(),
  max_sessions: z.number(),
  max_call_duration_seconds: z.number(),
  voice_token_max_lifetime_seconds: z.number(),
  mcp_sync_interval_seconds: z.number().optional(),
  client_ip: z.string(),
  secure: z.boolean(),
  audio: z.object({ backgrounds: z.array(z.string()), cues: z.array(z.string()), allow_flow_background: z.boolean(), background_default: z.string() }),
  idle: z.object({ user_timeout_ms: z.number(), max_prompts: z.number(), prompt_mode: z.string() }),
  // The effective values a call uses when its flow says nothing (never a raw 0 for "default").
  turn: z.object({ manager: z.boolean(), min_words: z.number(), mute: z.array(z.string()), max_mute_ms: z.number(), speech_timeout_ms: z.number(), end_of_turn: z.string(), smart_turn_available: z.boolean(), smart_hold_ms: z.number() }),
});
export type ServerConfigDTO = z.infer<typeof serverConfigSchema>;
const serverConfigEnvelope = z.object({ data: serverConfigSchema });

const demoStatusSchema = z.object({
  dental: z.object({ available: z.boolean(), installed: z.boolean(), flow_id: z.string().optional() }),
});
type DemoStatus = z.infer<typeof demoStatusSchema>;
const demoInstallSchema = z.object({
  mcp_server_id: z.string(),
  mcp_created: z.boolean(),
  tools: z.number(),
  flow_id: z.string(),
  flow_created: z.boolean(),
});
type DemoInstall = z.infer<typeof demoInstallSchema>;

const operationsStatusEnvelope = z.object({
  data: operationsStatusSchema,
});


// Provider schemas & types
const catalogEntrySchema = z.object({
  id: z.string(),
  label: z.string(),
  default: z.boolean().optional(),
});
const serviceStatusSchema = z.object({
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

export const providerTestSchema = z.object({
  provider: z.string(),
  checked_at: z.string(),
  results: z.array(
    z.object({
      stage: z.enum(["stt", "llm", "tts"]),
      status: z.enum(["ok", "error", "skipped"]),
      latency_ms: z.number().optional(),
      detail: z.string().optional(),
      circuit: z.enum(["closed", "open", "half_open", "unknown"]),
    }),
  ),
});
export type ProviderTest = z.infer<typeof providerTestSchema>;

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

export const flowVersionSchema = z.object({
  id: z.string(),
  number: z.number(),
  schema_version: z.number(),
  config: z.record(z.string(), z.unknown()),
  config_hash: z.string(),
  created_at: z.string(),
  mcp_tools: z.array(toolSelectionSchema),
  mcp_review_required: z.boolean().optional(),
});
type FlowVersionDTO = z.infer<typeof flowVersionSchema>;

export const toolDependencySchema = z.object({
  mcp_server_id: z.string(),
  remote_name: z.string(),
  required: z.boolean(),
  state: z.string(),
  selected_schema_hash: z.string(),
  current_schema_hash: z.string().optional(),
});
export type ToolDependencyDTO = z.infer<typeof toolDependencySchema>;

const readinessSchema = z.object({
  state: z.string(),
  reasons: z.array(z.string()),
  missing_env: z.array(z.string()).optional(),
  tool_dependencies: z.array(toolDependencySchema).optional(),
});
type ReadinessDTO = z.infer<typeof readinessSchema>;

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

const FLOW_PAGE = 100; // the server's max page (server/api/api.go ParsePagination)
export const FLOW_PAGES_MAX = 10; // ponytail: 1000 flows, O(n²) de-dup is fine at that size; offsets can skip a flow deleted mid-walk until the next refetch; a server-side search replaces this if anyone gets near it

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

export type CreateFlowInput = {
  name: string;
  description: string;
  enabled?: boolean;
  config: Record<string, unknown>;
  mcp_tools?: ToolSelection[];
};

type UpdateFlowInput = {
  name?: string;
  description?: string;
  enabled?: boolean;
  config?: Record<string, unknown>;
  mcp_tools?: ToolSelection[];
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

const mcpToolSchema = z.object({
  remote_name: z.string(),
  description: z.string().optional().default(""),
  input_schema: z.unknown().optional(),
  schema_hash: z.string(),
});
type MCPToolDTO = z.infer<typeof mcpToolSchema>;

const mcpDiscoverySchema = z.object({
  id: z.string(),
  discovery_hash: z.string(),
  created_at: z.string(),
  tools: z.array(mcpToolSchema),
});
type MCPDiscoveryDTO = z.infer<typeof mcpDiscoverySchema>;

export const mcpServerDetailSchema = mcpServerSchema.extend({
  discovery: mcpDiscoverySchema.nullish(),
});
export type MCPServerDetailDTO = z.infer<typeof mcpServerDetailSchema>;

const mcpExecutionSchema = z.object({
  execution_id: z.string(),
  mcp_server_id: z.string(),
  tool_name: z.string(),
  status: z.string(),
  duration_ms: z.number(),
  result: z.unknown().optional(),
});
type MCPToolExecutionResultDTO = z.infer<typeof mcpExecutionSchema>;

const mcpDetachResultSchema = z.object({
  pending: z.boolean().optional(),
  deleted: z.boolean().optional(),
  server: mcpServerSchema.optional(),
});
type MCPDetachResultDTO = z.infer<typeof mcpDetachResultSchema>;

export type CreateMCPServerInput = {
  name: string;
  description?: string;
  endpoint: string;
  transport?: string;
  auth_env?: string;
  enabled?: boolean;
};

/** PATCH body: a field left out is unchanged; `auth_env: ""` clears the token variable. */
export type UpdateMCPServerInput = Partial<Pick<CreateMCPServerInput, "name" | "description" | "endpoint" | "auth_env">>;

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
 */
export function routeSpan(span: TraceSpanDTO): { event: string; from: string; to: string } | null {
  const event = /^route\.(\w+)$/.exec(span.name)?.[1];
  const a = spanAttrs(span);
  const from = a.from_step;
  const to = a.to_step;
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
  error_category: z.string().optional(),
});
export type ToolCallDTO = z.infer<typeof sessionToolCallSchema>;

export const sessionTracesSchema = z.object({
  session_id: z.string(),
  spans: z.array(sessionTraceSpanSchema),
  events: z.array(sessionTraceEventSchema),
  tool_calls: z.array(sessionToolCallSchema),
});
export type SessionTracesDTO = z.infer<typeof sessionTracesSchema>;

/** One tool call a turn made: never its arguments or result. */
const turnToolSchema = z.object({
  name: z.string(),
  status: z.string(),
  duration_ms: z.number(),
  error_category: z.string().optional(),
});
type TurnToolDTO = z.infer<typeof turnToolSchema>;

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
  tools: z.array(turnToolSchema).optional(),
});
export type TurnDTO = z.infer<typeof turnSchema>;

const sessionRuntimeToolSchema = z.object({
  mcp_server_id: z.string(),
  remote_name: z.string(),
  schema_hash: z.string(),
  required: z.boolean(),
});
type SessionRuntimeToolDTO = z.infer<typeof sessionRuntimeToolSchema>;

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
  caller: z.string().optional().default(""),
  end_reason: z.string().optional().default(""),
  median_reply_ms: z.number().nullish(),
  provider_stack: z.unknown().optional(),
  runtime_snapshot: z.unknown().optional(),
  runtime_tools: z.array(sessionRuntimeToolSchema).nullish(),
});
export type SessionDTO = z.infer<typeof sessionSchema>;

type SessionFilterParams = {
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

type VoiceToken = z.infer<typeof tokenEnvelope>["data"];
type AuthSnapshot = { key: string | null; generation: number };

const acceptedEnvelope = z.object({ data: z.object({ accepted: z.number() }) });

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

/**
 * Stands in for the management key when the server signs the dashboard in itself (the dev proxy adds the
 * credentials). It is not a secret and is never sent: requests then carry no Authorization header.
 */
export const PROXIED_ACCESS = "server-provided";

/** Whether the server already accepts keyless dashboard requests (a dev proxy or a front proxy signs them in). */
export async function probeProxiedAccess(fetcher: typeof fetch = fetch): Promise<boolean> {
  try {
    const response = await fetcher("/api/v1/server-config", { headers: { "X-Switchboard-UI": "1" }, cache: "no-store" });
    return response.ok;
  } catch {
    return false;
  }
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
          ...(key === PROXIED_ACCESS ? {} : { Authorization: `Bearer ${key}` }),
          // Marks the request as the dashboard's own: the dev proxy only signs in marked requests.
          "X-Switchboard-UI": "1",
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
    issueVoiceToken: async (input: { principal: string; flow_id: string; draft?: { config: unknown; mcp_tools?: ToolSelection[] } }) =>
      (await request("/voice-tokens", tokenEnvelope, { method: "POST", body: input })).data,
    testProvider: async (id: string) =>
      (await request(`/providers/${encodeURIComponent(id)}/test`, z.object({ data: providerTestSchema }), { method: "POST" })).data,
    getProviders: async () =>
      (await request("/providers", providersEnvelope)).data,
    // ponytail: one page of the server max (100); page through when someone has more flows.
    // Includes archived flows (sessions still name them); list views filter them out.
    getFlows: async () => {
      const all: FlowDTO[] = [];
      for (let page = 0; page < FLOW_PAGES_MAX; page++) {
        const batch = (await request(`/flows?limit=${FLOW_PAGE}&offset=${page * FLOW_PAGE}`, flowsEnvelope)).data;
        for (const f of batch) if (!all.some((g) => g.id === f.id)) all.push(f); // a flow created between pages shifts offsets
        if (batch.length < FLOW_PAGE) break;
      }
      return all;
    },
    getFlow: async (id: string) =>
      (await request(`/flows/${encodeURIComponent(id)}`, flowEnvelope)).data,
    /** One earlier publish of a flow (config and tools), read-only: a rollback publishes it again. */
    getFlowVersion: async (id: string, number: number) =>
      (await request(`/flows/${encodeURIComponent(id)}/versions/${number}`, flowVersionEnvelope)).data,
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
    updateMCPServer: async (id: string, patch: UpdateMCPServerInput) =>
      (
        await request(`/mcp-servers/${encodeURIComponent(id)}`, mcpServerEnvelope, {
          method: "PATCH",
          body: patch,
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
    addSessionNote: async (id: string, content: string, run: boolean) =>
      (await request(`/sessions/${encodeURIComponent(id)}/context`, acceptedEnvelope, { method: "POST", body: { messages: [{ content }], run } })).data,
    runSession: async (id: string) =>
      (await request(`/sessions/${encodeURIComponent(id)}/run`, acceptedEnvelope, { method: "POST" })).data,
    getSessionTraces: async (id: string) => {
      return (
        await request(
          `/sessions/${encodeURIComponent(id)}/traces`,
          sessionTracesEnvelope,
        )
      ).data;
    },

    getServerConfig: async () => (await request("/server-config", serverConfigEnvelope)).data,

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

