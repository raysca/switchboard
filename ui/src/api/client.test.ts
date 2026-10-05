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

  describe("providers API", () => {
    it("lists provider definitions with capabilities", async () => {
      const fetcher = vi.fn<typeof fetch>(async () =>
        new Response(
          JSON.stringify({
            data: [
              {
                id: "google",
                display_name: "Google",
                capabilities: ["llm", "stt", "tts"],
                auth_required: true,
              },
            ],
          }),
          { status: 200 },
        ),
      );
      const client = createApiClient(
        () => ({ key: "test-key", generation: 1 }),
        fetcher,
      );
      const providers = await client.getProviders();
      expect(providers).toHaveLength(1);
      expect(providers[0].id).toBe("google");
      expect(fetcher).toHaveBeenCalledWith("/api/v1/providers", expect.any(Object));
    });

    it("handles credential lifecycle: list, create, validate, rotate, update, delete", async () => {
      const cred = {
        id: "cred-1",
        provider: "google",
        masked_value: "••••••••1234",
        validation_state: "unvalidated",
        capabilities: ["llm", "stt", "tts"],
        created_at: "2026-10-05T12:00:00Z",
        updated_at: "2026-10-05T12:00:00Z",
      };
      const fetcher = vi.fn<typeof fetch>()
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: [cred] }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: cred }), { status: 201 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: cred }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: { ...cred, validation_state: "valid" } }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: cred }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: cred }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: { deleted: true } }), { status: 200 }),
        );

      const client = createApiClient(
        () => ({ key: "test-key", generation: 1 }),
        fetcher,
      );

      const list = await client.getCredentials();
      expect(list).toEqual([cred]);

      const created = await client.createCredential({ provider: "google", secret: "super-secret" });
      expect(created.masked_value).toBe("••••••••1234");
      expect(fetcher).toHaveBeenLastCalledWith("/api/v1/providers/credentials", expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ provider: "google", secret: "super-secret" }),
      }));

      const single = await client.getCredential("cred-1");
      expect(single.id).toBe("cred-1");

      const validated = await client.validateCredential("cred-1");
      expect(validated.validation_state).toBe("valid");

      const rotated = await client.rotateCredential("cred-1");
      expect(rotated.id).toBe("cred-1");

      const updated = await client.updateCredential("cred-1", { secret: "new-secret" });
      expect(updated.id).toBe("cred-1");

      const deleted = await client.deleteCredential("cred-1");
      expect(deleted.deleted).toBe(true);
    });

    it("discovers provider capabilities (models and voices)", async () => {
      const fetcher = vi.fn<typeof fetch>(async () =>
        new Response(
          JSON.stringify({
            data: {
              models: [{ id: "gemini-2.0-flash", display_name: "Gemini 2.0 Flash" }],
              voices: [{ id: "en-US-Journey-F", display_name: "Journey (Female)", language: "en-US" }],
            },
          }),
          { status: 200 },
        ),
      );
      const client = createApiClient(
        () => ({ key: "test-key", generation: 1 }),
        fetcher,
      );
      const discovery = await client.getProviderDiscovery("google", "llm");
      expect(discovery.models).toHaveLength(1);
      expect(discovery.voices).toHaveLength(1);
      expect(fetcher).toHaveBeenCalledWith(
        "/api/v1/providers/google/discovery?capability=llm",
        expect.any(Object),
      );
    });
  });

  describe("agents API", () => {
    it("handles agent CRUD, duplicate, archive, and mcp-tools", async () => {
      const agent = {
        id: "agent-1",
        name: "Customer Support",
        description: "Assists customers",
        enabled: true,
        archived: false,
        current_version: {
          id: "ver-1",
          number: 1,
          schema_version: 1,
          config: { stt: "google", llm: "google", tts: "google" },
          config_hash: "hash123",
          created_at: "2026-10-05T12:00:00Z",
          mcp_tools: [],
        },
        readiness: {
          state: "ready",
          reasons: [],
        },
        created_at: "2026-10-05T12:00:00Z",
        updated_at: "2026-10-05T12:00:00Z",
      };

      const fetcher = vi.fn<typeof fetch>()
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: [agent] }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: agent }), { status: 201 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: agent }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: { ...agent, name: "Updated" } }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: { ...agent, id: "agent-2", name: "Customer Support Copy" } }), { status: 201 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: { archived: true } }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({
            data: {
              id: "ver-2",
              number: 2,
              schema_version: 1,
              config: { stt: "google", llm: "google", tts: "google" },
              config_hash: "hash456",
              created_at: "2026-10-05T12:05:00Z",
              mcp_tools: [{ mcp_server_id: "mcp-1", remote_name: "query", schema_hash: "sh1", required: true }],
            },
          }), { status: 200 }),
        );

      const client = createApiClient(
        () => ({ key: "test-key", generation: 1 }),
        fetcher,
      );

      const agents = await client.getAgents({ limit: 10, offset: 0 });
      expect(agents).toHaveLength(1);
      expect(fetcher).toHaveBeenLastCalledWith("/api/v1/agents?limit=10&offset=0", expect.any(Object));

      const created = await client.createAgent({
        name: "Customer Support",
        description: "Assists customers",
        config: { stt: "google", llm: "google", tts: "google" },
      });
      expect(created.id).toBe("agent-1");

      const single = await client.getAgent("agent-1");
      expect(single.name).toBe("Customer Support");

      const updated = await client.updateAgent("agent-1", { name: "Updated" });
      expect(updated.name).toBe("Updated");

      const duplicated = await client.duplicateAgent("agent-1", { name: "Customer Support Copy" });
      expect(duplicated.id).toBe("agent-2");

      const archived = await client.archiveAgent("agent-1");
      expect(archived.archived).toBe(true);

      const version = await client.updateAgentMCPTools("agent-1", [
        { mcp_server_id: "mcp-1", remote_name: "query", schema_hash: "sh1", required: true },
      ]);
      expect(version.number).toBe(2);
      expect(version.mcp_tools).toHaveLength(1);
    });
  });
});

