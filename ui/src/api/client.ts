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
  audio: z.object({ backgrounds: z.array(z.string()), cues: z.array(z.string()), allow_agent_background: z.boolean(), background_default: z.string() }),
  idle: z.object({ user_timeout_ms: z.number(), max_prompts: z.number(), prompt_mode: z.string() }),
  // The effective values a call uses when its agent says nothing (never a raw 0 for "default").
  turn: z.object({ manager: z.boolean(), min_words: z.number(), mute: z.array(z.string()), max_mute_ms: z.number(), speech_timeout_ms: z.number(), end_of_turn: z.string(), smart_turn_available: z.boolean(), smart_hold_ms: z.number() }),
});
export type ServerConfigDTO = z.infer<typeof serverConfigSchema>;
const serverConfigEnvelope = z.object({ data: serverConfigSchema });

const demoStatusSchema = z.object({
  dental: z.object({ available: z.boolean(), installed: z.boolean(), agent_id: z.string().optional() }),
});
type DemoStatus = z.infer<typeof demoStatusSchema>;
const demoInstallSchema = z.object({
  mcp_server_id: z.string(),
  mcp_created: z.boolean(),
  tools: z.number(),
  agent_id: z.string(),
  agent_created: z.boolean(),
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
type AgentVersionDTO = z.infer<typeof agentVersionSchema>;

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

const AGENT_PAGE = 100; // the server's max page (server/api/api.go ParsePagination)
export const AGENT_PAGES_MAX = 10; // ponytail: 1000 agents, O(n²) de-dup is fine at that size; offsets can skip an agent deleted mid-walk until the next refetch; a server-side search replaces this if anyone gets near it

const agentsEnvelope = z.object({
  data: z.array(agentSchema),
});
const agentVersionEnvelope = z.object({ data: agentVersionSchema });
/** One publish in an agent's history, without its config. The newest is the current version. */
export const agentVersionSummarySchema = z.object({
  number: z.number(),
  config_hash: z.string(),
  created_at: z.string(),
  tool_count: z.number(),
  mcp_review_required: z.boolean().optional(),
  current: z.boolean(),
});
export type AgentVersionSummary = z.infer<typeof agentVersionSummarySchema>;
const agentVersionsEnvelope = z.object({ data: z.array(agentVersionSummarySchema) });
const agentEnvelope = z.object({
  data: agentSchema,
});
const archiveEnvelope = z.object({
  data: z.object({
    archived: z.boolean(),
  }),
});

export type CreateAgentInput = {
  name: string;
  description: string;
  enabled?: boolean;
  config: Record<string, unknown>;
  mcp_tools?: ToolSelection[];
};

type UpdateAgentInput = {
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
  last_checked_at: z.string().nullish(),
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

// HTTP tools: one endpoint a step can call. A definition carries a secret's name, never its value.
export const httpToolDefinitionSchema = z.object({
  name: z.string(),
  description: z.string(),
  method: z.string(),
  url: z.string(),
  headers: z.record(z.string(), z.string()).nullish(),
  auth: z.object({ header: z.string().optional(), secret: z.string() }).nullish(),
  input_schema: z.unknown(),
  timeout_ms: z.number().optional(),
  max_response_bytes: z.number().optional(),
  response_pick: z.array(z.string()).nullish(),
});
export type HTTPToolDefinition = z.infer<typeof httpToolDefinitionSchema>;
export const httpToolSchema = z.object({
  id: z.string(),
  name: z.string(),
  enabled: z.boolean(),
  hash: z.string(),
  definition: httpToolDefinitionSchema,
  auth_secret_set: z.boolean().optional(),
  created_at: z.string(),
  updated_at: z.string(),
});
export type HTTPToolDTO = z.infer<typeof httpToolSchema>;
export const httpToolTestSchema = z.object({
  status: z.string(),
  duration_ms: z.number(),
  result: z.unknown().optional(),
  error_category: z.string().optional(),
  message: z.string().optional(),
});
export type HTTPToolTestDTO = z.infer<typeof httpToolTestSchema>;
const httpToolsEnvelope = z.object({ data: z.array(httpToolSchema) });
const httpToolEnvelope = z.object({ data: httpToolSchema });
const httpToolTestEnvelope = z.object({ data: httpToolTestSchema });

/** The id an agent's tool selection carries for an HTTP tool, in the place of an MCP server id. */
export const httpToolServerId = (toolId: string) => `http:${toolId}`;
export const isHTTPToolSelection = (t: { mcp_server_id: string }) => t.mcp_server_id.startsWith("http:");

// Retention: the policy in force, the last run, and (on request) what the next run would remove.
export const retentionStatusSchema = z.object({
  enabled: z.boolean(),
  interval_seconds: z.number(),
  content_retention_days: z.number(),
  metadata_retention_days: z.number(),
  batch_size: z.number(),
  recording_retention_days: z.number(),
  last_run: z
    .object({
      started_at: z.string(),
      duration_ms: z.number(),
      error: z.string().optional(),
      turns_purged: z.number(),
      sessions_deleted: z.number(),
      turns_deleted: z.number(),
      tool_calls_deleted: z.number(),
      spans_deleted: z.number(),
      events_deleted: z.number(),
    })
    .nullable(),
  dry_run: z
    .object({
      content_sessions: z.number(),
      content_turns: z.number(),
      metadata_sessions: z.number(),
      metadata_turns: z.number(),
      metadata_tool_calls: z.number(),
      metadata_spans: z.number(),
      metadata_events: z.number(),
    })
    .nullish(),
  held_sessions: z.number(),
  recent_actions: z.array(
    z.object({
      at: z.string(),
      action: z.enum(["hold", "release", "erase_call", "erase_caller"]),
      // A call id, or for erase_caller the hash of the caller label.
      subject: z.string(),
      request_id: z.string(),
    }),
  ),
});
export type RetentionStatus = z.infer<typeof retentionStatusSchema>;
const retentionEnvelope = z.object({ data: retentionStatusSchema });

// Secrets: names and where they live; a value is only ever sent, never received.
export const secretSchema = z.object({
  name: z.string(),
  source: z.enum(["environment", "directory", "store"]),
  writable: z.boolean(),
  updated_at: z.string().nullish(),
  last_used_at: z.string().nullish(),
});
export type SecretDTO = z.infer<typeof secretSchema>;
export const secretChangeSchema = z.object({ at: z.string(), action: z.enum(["create", "replace", "delete"]), name: z.string(), request_id: z.string() });
export type SecretChangeDTO = z.infer<typeof secretChangeSchema>;
const secretListEnvelope = z.object({ data: z.object({ store_enabled: z.boolean(), secrets: z.array(secretSchema), recent_changes: z.array(secretChangeSchema).optional().default([]) }) });
const secretEnvelope = z.object({ data: secretSchema });

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
 * A handoff span's event ("preparing", "active", "failed") and the steps it joins, or null for other spans.
 */
export function handoffSpan(span: TraceSpanDTO): { event: string; from: string; to: string } | null {
  const event = /^handoff\.(\w+)$/.exec(span.name)?.[1];
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
  agent_id: z.string(),
  agent_version_id: z.string(),
  config_hash: z.string(),
  started_at: z.string(),
  ended_at: z.string().nullish(),
  status: z.string(),
  turn_count: z.number(),
  safe_summary: z.string().optional().default(""),
  draft: z.boolean().optional().default(false),
  caller: z.string().optional().default(""),
  end_reason: z.string().optional().default(""),
  // On hold: retention and erasure leave the call alone. hold_reason is the operator's note.
  held: z.boolean().optional().default(false),
  hold_reason: z.string().optional().default(""),
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
  agent_id?: string;
  held?: boolean;
};

const sessionsEnvelope = z.object({
  data: z.array(sessionSchema),
});
const sessionEnvelope = z.object({
  data: sessionSchema,
});
const erasureEnvelope = z.object({ data: z.object({ turns: z.number(), tool_calls: z.number(), spans: z.number(), events: z.number(), recordings_removed: z.number() }) });
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

// Agent evaluation DTOs, mirrored by hand from docs/api/eval.md (server/api/eval.go).
const evalJudgeSchema = z.object({ provider: z.string(), model: z.string().optional() });
export const evalCaseSchema = z.object({
  id: z.string(),
  name: z.string(),
  caller: z.record(z.string(), z.unknown()),
  expect: z.record(z.string(), z.unknown()),
  expected_answer: z.string().optional(),
  yaml: z.string(),
  created_at: z.string(),
  updated_at: z.string(),
});
export type EvalCaseDTO = z.infer<typeof evalCaseSchema>;
export const evalSuiteSchema = z.object({
  runs: z.number(),
  min_pass: z.number(),
  pass_threshold: z.number(),
  judge: evalJudgeSchema.nullable(),
});
export type EvalSuiteDTO = z.infer<typeof evalSuiteSchema>;
export const evalScorerSchema = z.object({
  name: z.string(),
  judged: z.boolean(),
  default_threshold: z.number(),
  threshold: z.number().optional(),
  judge: evalJudgeSchema.optional(),
});
export type EvalScorerDTO = z.infer<typeof evalScorerSchema>;
export const evalRunSchema = z.object({
  id: z.string(),
  agent_id: z.string(),
  version: z.number().optional(),
  agent_version_id: z.string().optional(),
  draft: z.boolean(),
  config_hash: z.string(),
  scope: z.string(),
  case_id: z.string().optional(),
  providers: z.string(),
  runs: z.number(),
  min_pass: z.number(),
  status: z.string(),
  triggered_by: z.string(),
  passed_cases: z.number().optional(),
  total_cases: z.number().optional(),
  created_at: z.string(),
  started_at: z.string().optional(),
  ended_at: z.string().optional(),
});
export type EvalRunDTO = z.infer<typeof evalRunSchema>;
export const evalRunDetailSchema = z.object({
  run: evalRunSchema,
  case_runs: z.array(
    z.object({
      case_id: z.string(),
      case_name: z.string(),
      repeat: z.number(),
      status: z.string(),
      error_category: z.string().optional(),
      session_id: z.string().optional(),
      passed: z.boolean(),
      scores: z.array(
        z.object({ scorer: z.string(), score: z.number(), outcome: z.string(), reason: z.string(), judge_model: z.string().optional() }),
      ),
    }),
  ),
});
export type EvalRunDetailDTO = z.infer<typeof evalRunDetailSchema>;
export const evalFleetSchema = z.object({
  agents: z.array(
    z.object({
      agent_id: z.string(),
      name: z.string(),
      threshold: z.number(),
      latest: z
        .object({ run_id: z.string(), version: z.number(), passed: z.number(), total: z.number(), pass_rate: z.number().nullish(), ended_at: z.string() })
        .optional(),
      trend: z.array(z.number()),
      below_threshold: z.boolean(),
    }),
  ),
  total: z.number(),
  below_threshold: z.number(),
});
export type EvalFleetDTO = z.infer<typeof evalFleetSchema>;
export const evalHistorySchema = z.array(
  z.object({
    run_id: z.string(),
    version: z.number(),
    ended_at: z.string(),
    passed: z.number(),
    total: z.number(),
    pass_rate: z.number().nullish(),
    scorers: z.array(z.object({ scorer: z.string(), mean: z.number(), pass: z.number(), fail: z.number(), skipped: z.number(), errors: z.number() })),
  }),
);
export type EvalHistoryDTO = z.infer<typeof evalHistorySchema>;
export type EvalRunInput = { version?: number; draft?: { config: unknown; mcp_tools?: ToolSelection[] }; case_id?: string; providers?: "offline" | "echo" | "keep" };
const evalCasesEnvelope = z.object({ data: z.array(evalCaseSchema) });
const evalCaseEnvelope = z.object({ data: evalCaseSchema });

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
    issueVoiceToken: async (input: { principal: string; agent_id: string; draft?: { config: unknown; mcp_tools?: ToolSelection[] } }) =>
      (await request("/voice-tokens", tokenEnvelope, { method: "POST", body: input })).data,
    testProvider: async (id: string) =>
      (await request(`/providers/${encodeURIComponent(id)}/test`, z.object({ data: providerTestSchema }), { method: "POST" })).data,
    getProviders: async () =>
      (await request("/providers", providersEnvelope)).data,
    // ponytail: one page of the server max (100); page through when someone has more agents.
    // Includes archived agents (sessions still name them); list views filter them out.
    getAgents: async () => {
      const all: AgentDTO[] = [];
      for (let page = 0; page < AGENT_PAGES_MAX; page++) {
        const batch = (await request(`/agents?limit=${AGENT_PAGE}&offset=${page * AGENT_PAGE}`, agentsEnvelope)).data;
        for (const f of batch) if (!all.some((g) => g.id === f.id)) all.push(f); // an agent created between pages shifts offsets
        if (batch.length < AGENT_PAGE) break;
      }
      return all;
    },
    getAgent: async (id: string) =>
      (await request(`/agents/${encodeURIComponent(id)}`, agentEnvelope)).data,
    /** One earlier publish of an agent (config and tools), read-only: a rollback publishes it again. */
    getAgentVersions: async (id: string) => (await request(`/agents/${encodeURIComponent(id)}/versions`, agentVersionsEnvelope)).data,
    /** Publishes an earlier version's content as the next version and returns the agent. */
    restoreAgentVersion: async (id: string, number: number) =>
      (await request(`/agents/${encodeURIComponent(id)}/versions/${number}/restore`, agentEnvelope, { method: "POST" })).data,
    unarchiveAgent: async (id: string) =>
      (await request(`/agents/${encodeURIComponent(id)}/unarchive`, archiveEnvelope, { method: "POST" })).data,
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

    // HTTP tools endpoints
    getHTTPTools: async () => (await request("/http-tools", httpToolsEnvelope)).data,
    getHTTPTool: async (id: string) => (await request(`/http-tools/${encodeURIComponent(id)}`, httpToolEnvelope)).data,
    createHTTPTool: async (definition: HTTPToolDefinition) => (await request("/http-tools", httpToolEnvelope, { method: "POST", body: definition })).data,
    updateHTTPTool: async (id: string, definition: HTTPToolDefinition) => (await request(`/http-tools/${encodeURIComponent(id)}`, httpToolEnvelope, { method: "PUT", body: definition })).data,
    setHTTPToolEnabled: async (id: string, enabled: boolean) => (await request(`/http-tools/${encodeURIComponent(id)}/${enabled ? "enable" : "disable"}`, httpToolEnvelope, { method: "POST" })).data,
    deleteHTTPTool: async (id: string) => {
      await request(`/http-tools/${encodeURIComponent(id)}`, z.unknown(), { method: "DELETE" });
    },
    testHTTPTool: async (id: string, args: unknown) => (await request(`/http-tools/${encodeURIComponent(id)}/test`, httpToolTestEnvelope, { method: "POST", body: { arguments: args } })).data,

    // Retention status
    getRetention: async (dryRun = false) => (await request(`/retention${dryRun ? "?dry_run=true" : ""}`, retentionEnvelope)).data,

    // Secrets endpoints
    getSecrets: async () => (await request("/secrets", secretListEnvelope)).data,
    putSecret: async (name: string, value: string) =>
      (await request(`/secrets/${encodeURIComponent(name)}`, secretEnvelope, { method: "PUT", body: { value } })).data,
    deleteSecret: async (name: string) => {
      await request(`/secrets/${encodeURIComponent(name)}`, z.unknown(), { method: "DELETE" });
    },

    // Evaluation endpoints
    listEvalCases: async (agentId: string) => (await request(`/agents/${encodeURIComponent(agentId)}/eval/cases`, evalCasesEnvelope)).data,
    createEvalCases: async (agentId: string, yaml: string) =>
      (await request(`/agents/${encodeURIComponent(agentId)}/eval/cases`, evalCasesEnvelope, { method: "POST", body: { yaml } })).data,
    updateEvalCase: async (agentId: string, caseId: string, yaml: string) =>
      (await request(`/agents/${encodeURIComponent(agentId)}/eval/cases/${encodeURIComponent(caseId)}`, evalCaseEnvelope, { method: "PUT", body: { yaml } })).data,
    deleteEvalCase: async (agentId: string, caseId: string) => {
      await request(`/agents/${encodeURIComponent(agentId)}/eval/cases/${encodeURIComponent(caseId)}`, z.unknown(), { method: "DELETE" });
    },
    exportEvalCases: async (agentId: string) =>
      (await request(`/agents/${encodeURIComponent(agentId)}/eval/cases/export`, z.object({ data: z.object({ yaml: z.string() }) }))).data.yaml,
    getEvalSuite: async (agentId: string) => (await request(`/agents/${encodeURIComponent(agentId)}/eval/suite`, z.object({ data: evalSuiteSchema }))).data,
    putEvalSuite: async (agentId: string, body: EvalSuiteDTO) =>
      (await request(`/agents/${encodeURIComponent(agentId)}/eval/suite`, z.object({ data: evalSuiteSchema }), { method: "PUT", body })).data,
    getEvalScorers: async (agentId: string) =>
      (await request(`/agents/${encodeURIComponent(agentId)}/eval/scorers`, z.object({ data: z.array(evalScorerSchema) }))).data,
    /** Replaces one scorer's setting: a field left out goes back to the default. */
    putEvalScorer: async (agentId: string, name: string, body: { threshold?: number; judge?: { provider: string; model?: string } | null }) => {
      await request(`/agents/${encodeURIComponent(agentId)}/eval/scorers/${encodeURIComponent(name)}`, z.unknown(), { method: "PUT", body });
    },
    startEvalRun: async (agentId: string, body: EvalRunInput) =>
      (await request(`/agents/${encodeURIComponent(agentId)}/eval/runs`, z.object({ data: evalRunSchema }), { method: "POST", body })).data,
    listEvalRuns: async (agentId: string) => (await request(`/agents/${encodeURIComponent(agentId)}/eval/runs?include_drafts=true`, z.object({ data: z.array(evalRunSchema) }))).data,
    getEvalRun: async (agentId: string, runId: string) =>
      (await request(`/agents/${encodeURIComponent(agentId)}/eval/runs/${encodeURIComponent(runId)}`, z.object({ data: evalRunDetailSchema }))).data,
    getEvalHistory: async (agentId: string) => (await request(`/agents/${encodeURIComponent(agentId)}/eval/history`, z.object({ data: evalHistorySchema }))).data,
    getEvalFleet: async (limit: number, offset: number) => (await request(`/eval/fleet?limit=${limit}&offset=${offset}`, z.object({ data: evalFleetSchema }))).data,

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
      if (params?.agent_id) searchParams.set("agent_id", params.agent_id);
      if (params?.held !== undefined) searchParams.set("held", String(params.held));
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
    holdSession: async (id: string, reason: string) =>
      (await request(`/sessions/${encodeURIComponent(id)}/hold`, sessionEnvelope, { method: "PUT", body: { reason } })).data,
    releaseSession: async (id: string) =>
      (await request(`/sessions/${encodeURIComponent(id)}/hold`, sessionEnvelope, { method: "DELETE" })).data,
    eraseSession: async (id: string) => (await request(`/sessions/${encodeURIComponent(id)}`, erasureEnvelope, { method: "DELETE" })).data,
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

