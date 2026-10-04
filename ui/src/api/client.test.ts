import { describe, expect, it, vi } from "vitest";
import { ApiError, createApiClient } from "./client";

describe("management client", () => {
  it("sends an in-memory bearer and reads the sanitized server projection", async () => {
    const fetcher = vi.fn<typeof fetch>(async (_input, init) => {
      expect(init?.headers).toMatchObject({
        Authorization: "Bearer management-key",
      });
      return new Response(
        JSON.stringify({
          data: {
            profile: "production",
            max_sessions: 50,
            max_call_duration_seconds: 300,
            voice_token_max_lifetime_seconds: 60,
          },
        }),
        { headers: { "X-Request-ID": "req-1" } },
      );
    });
    const client = createApiClient(
      () => ({ key: "management-key", generation: 1 }),
      fetcher,
    );
    await expect(client.serverConfig()).resolves.toMatchObject({
      profile: "production",
      max_sessions: 50,
    });
    expect(fetcher).toHaveBeenCalledWith(
      "/api/v1/server-config",
      expect.any(Object),
    );
  });

  it("preserves API code and request ID on a failed response", async () => {
    const fetcher = vi.fn<typeof fetch>(
      async () =>
        new Response(
          JSON.stringify({
            error: {
              code: "unavailable",
              message: "Management is unavailable",
              request_id: "req-body",
            },
          }),
          { status: 503, headers: { "X-Request-ID": "req-header" } },
        ),
    );
    await expect(
      createApiClient(
        () => ({ key: "key", generation: 1 }),
        fetcher,
      ).serverConfig(),
    ).rejects.toMatchObject({
      code: "unavailable",
      requestId: "req-header",
      status: 503,
    });
  });

  it("clears the key on 401 without leaking it into the error", async () => {
    const clear = vi.fn();
    const fetcher = vi.fn<typeof fetch>(
      async () =>
        new Response(
          JSON.stringify({
            error: {
              code: "unauthorized",
              message: "Unauthorized",
              request_id: "req-2",
            },
          }),
          { status: 401, headers: { "X-Request-ID": "req-2" } },
        ),
    );
    const client = createApiClient(
      () => ({ key: "private-key", generation: 1 }),
      fetcher,
      clear,
    );
    await expect(client.serverConfig()).rejects.toBeInstanceOf(ApiError);
    expect(clear).toHaveBeenCalledOnce();
    await expect(client.serverConfig()).rejects.not.toHaveProperty(
      "message",
      expect.stringContaining("private-key"),
    );
  });

  it("issues a scoped voice token through the documented JSON endpoint", async () => {
    const fetcher = vi.fn<typeof fetch>(async (_input, init) => {
      expect(init?.method).toBe("POST");
      expect(init?.headers).toMatchObject({
        "Content-Type": "application/json",
      });
      expect(init?.body).toBe(
        JSON.stringify({ principal: "operator-1", pipeline: "default" }),
      );
      return new Response(
        JSON.stringify({
          data: {
            token: "scoped-token",
            expires_at: "2026-10-04T23:00:00Z",
            scope: "voice:connect",
          },
        }),
        { status: 201 },
      );
    });
    const token = await createApiClient(
      () => ({ key: "management-key", generation: 1 }),
      fetcher,
    ).issueVoiceToken({ principal: "operator-1", pipeline: "default" });
    expect(token.scope).toBe("voice:connect");
    expect(fetcher).toHaveBeenCalledWith(
      "/api/v1/voice-tokens",
      expect.any(Object),
    );
  });

  it("reports the generation captured when a request starts", async () => {
    let respond!: (response: Response) => void;
    const pending = new Promise<Response>((resolve) => {
      respond = resolve;
    });
    const fetcher = vi.fn<typeof fetch>(async () => pending);
    const onUnauthorized = vi.fn();
    let generation = 7;
    const client = createApiClient(
      () => ({ key: "management-key", generation }),
      fetcher,
      onUnauthorized,
    );
    const request = client.serverConfig();
    generation = 8;
    respond(
      new Response(
        JSON.stringify({
          error: {
            code: "unauthorized",
            message: "Unauthorized",
            request_id: "req-old",
          },
        }),
        { status: 401, headers: { "X-Request-ID": "req-old" } },
      ),
    );
    await expect(request).rejects.toBeInstanceOf(ApiError);
    expect(onUnauthorized).toHaveBeenCalledWith(
      expect.objectContaining({ requestId: "req-old" }),
      7,
    );
  });
});
