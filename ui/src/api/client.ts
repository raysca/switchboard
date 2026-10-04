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

export type ServerConfig = z.infer<typeof configEnvelope>["data"];
export type VoiceToken = z.infer<typeof tokenEnvelope>["data"];

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
  getKey: () => string | null,
  fetcher: typeof fetch = fetch,
  onUnauthorized?: () => void,
) {
  async function request<T>(
    path: string,
    schema: z.ZodType<T>,
    body?: object,
  ): Promise<T> {
    const key = getKey();
    if (!key)
      throw new ApiError(
        "Enter a management key to continue.",
        "missing_key",
        0,
        "",
      );
    let response: Response;
    try {
      response = await fetcher(`/api/v1${path}`, {
        method: body ? "POST" : "GET",
        headers: {
          Authorization: `Bearer ${key}`,
          ...(body ? { "Content-Type": "application/json" } : {}),
        },
        ...(body ? { body: JSON.stringify(body) } : {}),
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
      if (response.status === 401) onUnauthorized?.();
      const parsed = errorEnvelope.safeParse(payload);
      throw new ApiError(
        parsed.success
          ? parsed.data.error.message
          : "The management API returned an unreadable error.",
        parsed.success ? parsed.data.error.code : "invalid_error",
        response.status,
        requestId || (parsed.success ? parsed.data.error.request_id : ""),
      );
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
      (await request("/voice-tokens", tokenEnvelope, input)).data,
  };
}
