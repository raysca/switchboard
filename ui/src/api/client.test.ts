import { describe, expect, it, vi } from "vitest";
import { ApiError, agentConfigV2Schema, createApiClient } from "./client";

describe("management client", () => {
  it("sends an in-memory bearer and parses the response", async () => {
    const fetcher = vi.fn<typeof fetch>(async (_input, init) => {
      expect(init?.headers).toMatchObject({
        Authorization: "Bearer management-key",
      });
      return new Response(JSON.stringify({ data: [] }), {
        headers: { "X-Request-ID": "req-1" },
      });
    });
    const client = createApiClient(
      () => ({ key: "management-key", generation: 1 }),
      fetcher,
    );
    await expect(client.getProviders()).resolves.toEqual([]);
    expect(fetcher).toHaveBeenCalledWith(
      "/api/v1/providers",
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
      ).getProviders(),
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
    await expect(client.getProviders()).rejects.toBeInstanceOf(ApiError);
    expect(clear).toHaveBeenCalledOnce();
    await expect(client.getProviders()).rejects.not.toHaveProperty(
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
        JSON.stringify({ principal: "operator-1", agent_id: "agent-1" }),
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
    ).issueVoiceToken({ principal: "operator-1", agent_id: "agent-1" });
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
    const request = client.getProviders();
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

    it("handles credential lifecycle: list, create, rotate, update, delete", async () => {
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

      const rotated = await client.rotateCredential("cred-1");
      expect(rotated.id).toBe("cred-1");

      const updated = await client.updateCredential("cred-1", { secret: "new-secret" });
      expect(updated.id).toBe("cred-1");

      const deleted = await client.deleteCredential("cred-1");
      expect(deleted.deleted).toBe(true);
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
          new Response(JSON.stringify({ data: { ...agent, name: "Updated" } }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: { ...agent, id: "agent-2", name: "Customer Support Copy" } }), { status: 201 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: { archived: true } }), { status: 200 }),
        );

      const client = createApiClient(
        () => ({ key: "test-key", generation: 1 }),
        fetcher,
      );

      const agents = await client.getAgents();
      expect(agents).toHaveLength(1);
      expect(fetcher).toHaveBeenLastCalledWith("/api/v1/agents?limit=100", expect.any(Object));

      const created = await client.createAgent({
        name: "Customer Support",
        description: "Assists customers",
        config: {
          stt: { provider: "google", model: "chirp_2", language: "en-US", credential_id: "cred-1" },
          llm: { provider: "google", model: "gemini-2.0-flash", credential_id: "cred-1" },
          tts: { provider: "google", voice: "en-US-Journey-F", credential_id: "cred-1" },
        },
      });
      expect(created.id).toBe("agent-1");

      const updated = await client.updateAgent("agent-1", { name: "Updated" });
      expect(updated.name).toBe("Updated");

      const duplicated = await client.duplicateAgent("agent-1", { name: "Customer Support Copy" });
      expect(duplicated.id).toBe("agent-2");

      const archived = await client.archiveAgent("agent-1");
      expect(archived.archived).toBe(true);
    });

    it("handles MCP server management operations", async () => {
      const mockServer = {
        id: "mcp-1",
        name: "Order Service",
        description: "Order lookup and mutation",
        endpoint: "https://mcp.example.com/rpc",
        transport: "streamable-http",
        validation_state: "valid",
        enabled: true,
      };

      const fetcher = vi.fn<typeof fetch>()
        .mockResolvedValueOnce(new Response(JSON.stringify({ data: [mockServer] }), { status: 200 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({
          data: {
            ...mockServer,
            discovery: {
              id: "disc-1",
              discovery_hash: "dhash123",
              created_at: "2026-10-05T12:00:00Z",
              tools: [{
                remote_name: "lookup_order",
                description: "Find an order",
                input_schema: { type: "object", properties: { id: { type: "string" } } },
                schema_hash: "shash123",
              }],
            },
          },
        }), { status: 200 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({ data: mockServer }), { status: 201 }))
        .mockResolvedValueOnce(new Response(null, { status: 204 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({ data: { deleted: true } }), { status: 200 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({ data: { ...mockServer, enabled: false } }), { status: 200 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({ data: { ...mockServer, enabled: true } }), { status: 200 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({ data: mockServer }), { status: 200 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({
          data: {
            execution_id: "exec-1",
            mcp_server_id: "mcp-1",
            tool_name: "lookup_order",
            status: "completed",
            duration_ms: 45,
            result: { found: true },
          },
        }), { status: 200 }));

      const client = createApiClient(
        () => ({ key: "test-key", generation: 1 }),
        fetcher,
      );

      const servers = await client.getMCPServers();
      expect(servers).toHaveLength(1);
      expect(fetcher).toHaveBeenCalledWith("/api/v1/mcp-servers", expect.any(Object));

      const detail = await client.getMCPServer("mcp-1");
      expect(detail.discovery?.tools).toHaveLength(1);

      const created = await client.createMCPServer({
        name: "Order Service",
        endpoint: "https://mcp.example.com/rpc",
      });
      expect(created.id).toBe("mcp-1");

      const deleted = await client.deleteMCPServer("mcp-1");
      expect(deleted.deleted).toBe(true);

      const detached = await client.deleteMCPServer("mcp-1", true);
      expect(detached.deleted).toBe(true);

      const disabled = await client.disableMCPServer("mcp-1");
      expect(disabled.enabled).toBe(false);

      const enabled = await client.enableMCPServer("mcp-1");
      expect(enabled.enabled).toBe(true);

      const discovered = await client.discoverMCPServer("mcp-1");
      expect(discovered.id).toBe("mcp-1");

      const executed = await client.testMCPTool("mcp-1", "lookup_order", { id: "123" });
      expect(executed.status).toBe("completed");
      expect(executed.duration_ms).toBe(45);
    });

    it("handles sessions and traces query operations", async () => {
      const mockSession = {
        id: "sess-1",
        agent_id: "agent-1",
        agent_version_id: "ver-1",
        config_hash: "chash123",
        started_at: "2026-10-05T12:00:00Z",
        status: "completed",
        turn_count: 2,
        safe_summary: "Test session",
      };

      const mockTurn = {
        id: "turn-1",
        session_id: "sess-1",
        ordinal: 1,
        started_at: "2026-10-05T12:00:01Z",
        transcript: "Hello",
        assistant_text: "Hi there!",
        interrupted: false,
        latencies: { llm_ms: 120 },
      };

      const mockTraces = {
        session_id: "sess-1",
        spans: [{
          id: "span-1",
          session_id: "sess-1",
          turn_id: "turn-1",
          name: "llm.request",
          started_at: "2026-10-05T12:00:02Z",
        }],
        events: [{
          id: "event-1",
          session_id: "sess-1",
          turn_id: "turn-1",
          name: "first_token",
          occurred_at: "2026-10-05T12:00:02Z",
        }],
        tool_calls: [{
          id: "tc-1",
          session_id: "sess-1",
          turn_id: "turn-1",
          remote_name: "lookup_order",
          schema_hash: "sh1",
          started_at: "2026-10-05T12:00:02Z",
          status: "completed",
          arguments_summary: `{"id":"123"}`,
          result_summary: `{"found":true}`,
        }],
      };

      const fetcher = vi.fn<typeof fetch>()
        .mockResolvedValueOnce(new Response(JSON.stringify({ data: [mockSession] }), { status: 200 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({ data: mockSession }), { status: 200 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({ data: [mockTurn] }), { status: 200 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({ data: mockTraces }), { status: 200 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({ data: mockTraces }), { status: 200 }));

      const client = createApiClient(
        () => ({ key: "test-key", generation: 1 }),
        fetcher,
      );

      const sessions = await client.getSessions({ limit: 10, offset: 0, status: "completed", agent_id: "agent-1" });
      expect(sessions).toHaveLength(1);
      expect(fetcher).toHaveBeenCalledWith("/api/v1/sessions?limit=10&offset=0&status=completed&agent_id=agent-1", expect.any(Object));

      const single = await client.getSession("sess-1");
      expect(single.id).toBe("sess-1");

      const turns = await client.getSessionTurns("sess-1");
      expect(turns).toHaveLength(1);
      expect(turns[0].transcript).toBe("Hello");

      const traces = await client.getSessionTraces("sess-1");
      expect(traces.spans).toHaveLength(1);
      expect(traces.tool_calls).toHaveLength(1);

      const turnTraces = await client.getSessionTraces("sess-1", "turn-1");
      expect(turnTraces.events).toHaveLength(1);
      expect(fetcher).toHaveBeenLastCalledWith("/api/v1/sessions/sess-1/traces?turn_id=turn-1", expect.any(Object));
    });

    it("fetches operational status and diagnostics", async () => {
      const mockOperations = {
        uptime_seconds: 3600,
        started_at: "2026-10-05T12:00:00Z",
        profile: "development",
        active_sessions: 5,
        max_sessions: 50,
        capacity_utilization: 0.1,
        sessions_total: 100,
        turns_total: 450,
        errors_total: 2,
        p95_latency_ms: 180.5,
        average_latency_ms: 120.2,
        session_rate: 0.05,
        database: {
          dialect: "sqlite",
          status: "connected",
          open_connections: 1,
          in_use: 0,
          migration_version: 5,
        },
        vad: {
          worker_status: "ready",
          sample_rate: 16000,
          frame_size: 512,
        },
        health: "healthy",
      };

      const fetcher = vi.fn<typeof fetch>().mockResolvedValueOnce(
        new Response(JSON.stringify({ data: mockOperations }), { status: 200 }),
      );

      const client = createApiClient(
        () => ({ key: "test-key", generation: 1 }),
        fetcher,
      );

      const status = await client.getOperationsStatus();
      expect(status.profile).toBe("development");
      expect(status.active_sessions).toBe(5);
      expect(status.health).toBe("healthy");
      expect(status.database.dialect).toBe("sqlite");
      expect(status.vad.worker_status).toBe("ready");
      expect(fetcher).toHaveBeenCalledWith("/api/v1/operations/status", expect.any(Object));
    });
  });
});

describe("agentConfigV2Schema", () => {
  const stage = { provider: "google", credential_id: "cred-1" };
  const valid = { stt: stage, llm: { ...stage, model: "gemini-2.0-flash" }, tts: stage };

  it("requires a non-empty credential_id on every stage", () => {
    expect(agentConfigV2Schema.safeParse(valid).success).toBe(true);
    for (const key of ["stt", "llm", "tts"] as const) {
      const { credential_id: _, ...noCred } = valid[key];
      expect(agentConfigV2Schema.safeParse({ ...valid, [key]: noCred }).success).toBe(false);
      expect(
        agentConfigV2Schema.safeParse({ ...valid, [key]: { ...valid[key], credential_id: " " } }).success,
      ).toBe(false);
    }
  });
});
