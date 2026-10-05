import { z } from "zod";

const errorEnvelope = z.object({
  error: z.object({
    code: z.string(),
    message: z.string(),
    request_id: z.string(),
  }),
});
const configEnvelope = z.object({
  data: z.object({
    profile: z.string(),
    max_sessions: z.number(),
    max_call_duration_seconds: z.number(),
    voice_token_max_lifetime_seconds: z.number(),
  }),
});
const tokenEnvelope = z.object({
  data: z.object({
    token: z.string(),
    expires_at: z.string(),
    scope: z.string(),
  }),
});

// Provider schemas & types
export const providerDefinitionSchema = z.object({
  id: z.string(),
  display_name: z.string(),
  capabilities: z.array(z.string()),
  auth_required: z.boolean(),
});
export type ProviderDefinition = z.infer<typeof providerDefinitionSchema>;

const providersEnvelope = z.object({
  data: z.array(providerDefinitionSchema),
});

export const credentialDTOSchema = z.object({
  id: z.string(),
  provider: z.string(),
  masked_value: z.string(),
  validation_state: z.string(),
  validated_at: z.string().nullish(),
  capabilities: z.array(z.string()),
  created_at: z.string(),
  updated_at: z.string(),
});
export type CredentialDTO = z.infer<typeof credentialDTOSchema>;

const credentialsEnvelope = z.object({
  data: z.array(credentialDTOSchema),
});
const credentialEnvelope = z.object({
  data: credentialDTOSchema,
});

export const discoveryOptionSchema = z.object({
  id: z.string(),
  display_name: z.string(),
  language: z.string().optional(),
});
export type DiscoveryOption = z.infer<typeof discoveryOptionSchema>;

export const discoveryResultSchema = z.object({
  models: z.array(discoveryOptionSchema),
  voices: z.array(discoveryOptionSchema),
});
export type DiscoveryResult = z.infer<typeof discoveryResultSchema>;

const discoveryEnvelope = z.object({
  data: discoveryResultSchema,
});

const deleteEnvelope = z.object({
  data: z.object({
    deleted: z.boolean(),
  }),
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
const agentEnvelope = z.object({
  data: agentSchema,
});
const agentVersionEnvelope = z.object({
  data: agentVersionSchema,
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

export type UpdateAgentInput = {
  name?: string;
  description?: string;
  enabled?: boolean;
  config?: Record<string, unknown>;
};

export type ServerConfig = z.infer<typeof configEnvelope>["data"];
export type VoiceToken = z.infer<typeof tokenEnvelope>["data"];
export type AuthSnapshot = { key: string | null; generation: number };

export class ApiError extends Error {
  constructor(
    message: string,
    public code: string,
    public status: number,
    public requestId: string,
  ) {
    super(message);
    this.name = "ApiError";
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
    serverConfig: async () =>
      (await request("/server-config", configEnvelope)).data,
    issueVoiceToken: async (input: { principal: string; pipeline: string }) =>
      (await request("/voice-tokens", tokenEnvelope, { method: "POST", body: input })).data,
    getProviders: async () =>
      (await request("/providers", providersEnvelope)).data,
    getCredentials: async () =>
      (await request("/providers/credentials", credentialsEnvelope)).data,
    createCredential: async (input: { provider: string; secret: string }) =>
      (
        await request("/providers/credentials", credentialEnvelope, {
          method: "POST",
          body: input,
        })
      ).data,
    getCredential: async (id: string) =>
      (
        await request(
          `/providers/credentials/${encodeURIComponent(id)}`,
          credentialEnvelope,
        )
      ).data,
    updateCredential: async (id: string, input: { secret: string }) =>
      (
        await request(
          `/providers/credentials/${encodeURIComponent(id)}`,
          credentialEnvelope,
          { method: "PUT", body: input },
        )
      ).data,
    deleteCredential: async (id: string) =>
      (
        await request(
          `/providers/credentials/${encodeURIComponent(id)}`,
          deleteEnvelope,
          { method: "DELETE" },
        )
      ).data,
    validateCredential: async (id: string) =>
      (
        await request(
          `/providers/credentials/${encodeURIComponent(id)}/validate`,
          credentialEnvelope,
          { method: "POST" },
        )
      ).data,
    rotateCredential: async (id: string) =>
      (
        await request(
          `/providers/credentials/${encodeURIComponent(id)}/rotate`,
          credentialEnvelope,
          { method: "POST" },
        )
      ).data,
    getProviderDiscovery: async (provider: string, capability: string) =>
      (
        await request(
          `/providers/${encodeURIComponent(provider)}/discovery?capability=${encodeURIComponent(capability)}`,
          discoveryEnvelope,
        )
      ).data,
    getAgents: async (pagination?: { limit?: number; offset?: number }) => {
      const params = new URLSearchParams();
      if (pagination?.limit !== undefined)
        params.set("limit", String(pagination.limit));
      if (pagination?.offset !== undefined)
        params.set("offset", String(pagination.offset));
      const qs = params.toString();
      return (await request(`/agents${qs ? `?${qs}` : ""}`, agentsEnvelope))
        .data;
    },
    createAgent: async (input: CreateAgentInput) =>
      (
        await request("/agents", agentEnvelope, {
          method: "POST",
          body: input,
        })
      ).data,
    getAgent: async (id: string) =>
      (await request(`/agents/${encodeURIComponent(id)}`, agentEnvelope)).data,
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
    updateAgentMCPTools: async (id: string, tools: ToolSelection[]) =>
      (
        await request(
          `/agents/${encodeURIComponent(id)}/mcp-tools`,
          agentVersionEnvelope,
          {
            method: "PUT",
            body: { tools },
          },
        )
      ).data,
  };
}

export type ApiClient = ReturnType<typeof createApiClient>;

