import { describe, expect, it, vi } from "vitest";
import { ApiError, FLOW_PAGES_MAX, PROXIED_ACCESS, probeProxiedAccess, createApiClient, flowSchema, flowVersionSchema, routeSpan, sessionSchema, type TraceSpanDTO } from "./client";

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

  it("sends a draft with the voice token request only when there is one", async () => {
    const bodies: string[] = [];
    const fetcher = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      bodies.push(String(init?.body));
      return new Response(
        JSON.stringify({ data: { token: "t", expires_at: "2026-10-04T23:00:00Z", scope: "voice:connect" } }),
        { status: 201 },
      );
    });
    const api = createApiClient(() => ({ key: "k", generation: 1 }), fetcher);
    await api.issueVoiceToken({ principal: "p", agent_id: "f", draft: { config: { a: 1 } } });
    await api.issueVoiceToken({ principal: "p", agent_id: "f" });
    expect(JSON.parse(bodies[0])).toEqual({ principal: "p", agent_id: "f", draft: { config: { a: 1 } } });
    expect(JSON.parse(bodies[1])).not.toHaveProperty("draft");
  });

  it("tests a provider on demand with a body-less POST", async () => {
    const calls: { url: string; method?: string; body?: unknown }[] = [];
    const fetcher = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(url), method: init?.method, body: init?.body });
      return new Response(
        JSON.stringify({ data: { provider: "google", checked_at: "2026-10-14T10:00:00Z", results: [
          { stage: "llm", status: "ok", latency_ms: 410, circuit: "closed" },
          { stage: "stt", status: "ok", circuit: "unknown" },
        ] } }),
        { status: 200 },
      );
    });
    const out = await createApiClient(() => ({ key: "k", generation: 1 }), fetcher).testProvider("google");
    expect(calls).toEqual([{ url: "/api/v1/providers/google/test", method: "POST", body: undefined }]);
    expect(out.results[0].latency_ms).toBe(410);
    expect(out.results[1].latency_ms).toBeUndefined();
  });

  it("sends a flow's MCP tool selection with a create or update, and with a draft token", async () => {
    const bodies: Record<string, unknown>[] = [];
    const flowBody = { data: { id: "f", name: "n", description: "", enabled: true, archived: false, current_version: null, readiness: { state: "ready", reasons: [] }, created_at: "", updated_at: "" } };
    const fetcher = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      bodies.push({ url: String(url), ...JSON.parse(String(init?.body)) });
      return new Response(JSON.stringify(String(url).includes("voice-tokens") ? { data: { token: "t", expires_at: "2026-10-04T23:00:00Z", scope: "voice:connect" } } : flowBody), { status: 200 });
    });
    const api = createApiClient(() => ({ key: "k", generation: 1 }), fetcher);
    const tools = [{ mcp_server_id: "crm", remote_name: "echo", schema_hash: "h", required: false }];
    await api.updateFlow("f", { config: { a: 1 }, mcp_tools: tools });
    await api.createFlow({ name: "n", description: "", config: { a: 1 }, mcp_tools: tools });
    await api.issueVoiceToken({ principal: "p", agent_id: "f", draft: { config: { a: 1 }, mcp_tools: tools } });
    expect(bodies.map((b) => b.mcp_tools ?? (b.draft as { mcp_tools?: unknown }).mcp_tools)).toEqual([tools, tools, tools]);
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
    const svc = (extra = {}) => ({ available: true, custom: true, ...extra });
    const sample = [
      {
        id: "google", display_name: "Google", capabilities: ["stt", "llm", "tts"],
        detected: true, env: ["GOOGLE_API_KEY"],
        services: {
          stt: svc({ languages: [{ id: "en-US", label: "English", default: true }] }),
          llm: svc({ models: [{ id: "gemini-2.5-flash", label: "Gemini Flash" }] }),
          tts: svc({ voices: [{ id: "aoede", label: "Aoede" }] }),
        },
      },
      {
        id: "xai", display_name: "xAI", capabilities: [], detected: false, env: ["XAI_API_KEY"],
        services: { llm: { available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], custom: false } },
      },
      {
        id: "deepgram", display_name: "Deepgram", capabilities: [], detected: true, env: ["DEEPGRAM_API_KEY"],
        services: { stt: { available: false, reason: "not_in_build" } },
      },
    ];

    it("parses detected, undetected and not_in_build providers", async () => {
      const fetcher = vi.fn<typeof fetch>(async () =>
        new Response(JSON.stringify({ data: sample }), { status: 200 }),
      );
      const client = createApiClient(() => ({ key: "test-key", generation: 1 }), fetcher);
      const providers = await client.getProviders();
      expect(providers).toHaveLength(3);
      expect(providers[0].detected).toBe(true);
      expect(providers[0].services.llm?.models?.[0].id).toBe("gemini-2.5-flash");
      expect(providers[1].services.llm?.missing_env).toEqual(["XAI_API_KEY"]);
      expect(providers[2].services.stt?.reason).toBe("not_in_build");
      expect(providers[2].services.stt?.custom).toBe(true);
      expect(fetcher).toHaveBeenCalledWith("/api/v1/providers", expect.any(Object));
    });

    it("parses entries with and without default (the server omits default=false)", async () => {
      const fetcher = vi.fn<typeof fetch>(async () =>
        new Response(JSON.stringify({ data: [{
          id: "g", display_name: "G", capabilities: ["llm"], detected: true, env: [],
          services: { llm: { available: true, custom: false, models: [
            { id: "a", label: "A", default: true }, { id: "b", label: "B" },
          ] } },
        }] }), { status: 200 }),
      );
      const client = createApiClient(() => ({ key: "k", generation: 1 }), fetcher);
      const models = (await client.getProviders())[0].services.llm?.models;
      expect(models).toEqual([{ id: "a", label: "A", default: true }, { id: "b", label: "B" }]);
    });

    it("has no credential methods", () => {
      const client = createApiClient(() => ({ key: "k", generation: 1 }), vi.fn<typeof fetch>());
      for (const m of ["getCredentials", "createCredential", "updateCredential", "deleteCredential", "rotateCredential", "validateCredential"]) {
        expect(m in client).toBe(false);
      }
    });
  });

  describe("flows API", () => {
    it("handles flow CRUD, duplicate, and archive", async () => {
      const flow = {
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
          new Response(JSON.stringify({ data: [flow] }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: flow }), { status: 201 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: { ...flow, name: "Updated" } }), { status: 200 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: { ...flow, id: "agent-2", name: "Customer Support Copy" } }), { status: 201 }),
        )
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ data: { archived: true } }), { status: 200 }),
        );

      const client = createApiClient(
        () => ({ key: "test-key", generation: 1 }),
        fetcher,
      );

      const flows = await client.getFlows();
      expect(flows).toHaveLength(1);
      expect(fetcher).toHaveBeenLastCalledWith("/api/v1/agents?limit=100&offset=0", expect.any(Object));

      const created = await client.createFlow({
        name: "Customer Support",
        description: "Assists customers",
        config: {
          stt: { provider: "google", model: "chirp_2", language: "en-US" },
          llm: { provider: "google", model: "gemini-2.0-flash" },
          tts: { provider: "google", voice: "en-US-Journey-F" },
        },
      });
      expect(created.id).toBe("agent-1");

      const updated = await client.updateFlow("agent-1", { name: "Updated" });
      expect(updated.name).toBe("Updated");

      const duplicated = await client.duplicateFlow("agent-1", { name: "Customer Support Copy" });
      expect(duplicated.id).toBe("agent-2");

      const archived = await client.archiveFlow("agent-1");
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
    });

    it("parses the tool calls a turn made", async () => {
      const turn = { id: "t1", session_id: "s", ordinal: 1, started_at: "2026-10-05T12:00:01Z", interrupted: false, tools: [{ name: "lookup_order", status: "succeeded", duration_ms: 42 }] };
      const fetcher = vi.fn<typeof fetch>().mockResolvedValueOnce(new Response(JSON.stringify({ data: [turn] }), { status: 200 }));
      const turns = await createApiClient(() => ({ key: "k", generation: 1 }), fetcher).getSessionTurns("s");
      expect(turns[0].tools).toEqual([{ name: "lookup_order", status: "succeeded", duration_ms: 42 }]);
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

describe("demo", () => {
  it("reads the demo status and installs the dental demo", async () => {
    const fetcher = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(new Response(JSON.stringify({ data: { dental: { available: true, installed: false } } }), { status: 200 }))
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ data: { mcp_server_id: "m", mcp_created: true, tools: 7, agent_id: "f", agent_created: true } }), { status: 200 }),
      );
    const client = createApiClient(() => ({ key: "test-key", generation: 1 }), fetcher);
    expect((await client.getDemo()).dental.available).toBe(true);
    expect((await client.installDentalDemo()).agent_id).toBe("f");
    expect(fetcher.mock.calls[1][0]).toBe("/api/v1/demo/dental");
    expect((fetcher.mock.calls[1][1] as RequestInit).method).toBe("POST");
  });
});

describe("session draft flag", () => {
  const base = { id: "s1", agent_id: "f1", agent_version_id: "v1", config_hash: "h", started_at: "2026-10-14T10:00:00Z", status: "ended", turn_count: 0 };
  it("is false when an older server leaves it out, and true when set", () => {
    expect(sessionSchema.parse(base).draft).toBe(false);
    expect(sessionSchema.parse({ ...base, draft: true }).draft).toBe(true);
  });
});

describe("access without a pasted key", () => {
  it("marks every request as coming from the dashboard", async () => {
    const fetcher = vi.fn<typeof fetch>(async (_input, init) => {
      expect(init?.headers).toMatchObject({ "X-Switchboard-UI": "1" });
      return new Response(JSON.stringify({ data: [] }));
    });
    const client = createApiClient(() => ({ key: "k", generation: 1 }), fetcher);
    await client.getProviders();
    expect(fetcher).toHaveBeenCalledOnce();
  });

  it("sends no Authorization header when the dev proxy supplies the credentials", async () => {
    const fetcher = vi.fn<typeof fetch>(async (_input, init) => {
      expect(init?.headers).not.toHaveProperty("Authorization");
      expect(init?.headers).toMatchObject({ "X-Switchboard-UI": "1" });
      return new Response(JSON.stringify({ data: [] }));
    });
    const client = createApiClient(() => ({ key: PROXIED_ACCESS, generation: 1 }), fetcher);
    await expect(client.getProviders()).resolves.toEqual([]);
    expect(fetcher).toHaveBeenCalledOnce();
  });

  it("detects server-provided access with a keyless request", async () => {
    const ok = vi.fn<typeof fetch>(async (input, init) => {
      expect(input).toBe("/api/v1/server-config");
      expect(init?.headers).not.toHaveProperty("Authorization");
      return new Response("{}", { status: 200 });
    });
    await expect(probeProxiedAccess(ok)).resolves.toBe(true);
    await expect(probeProxiedAccess(async () => new Response("{}", { status: 401 }))).resolves.toBe(false);
    await expect(probeProxiedAccess(async () => { throw new TypeError("offline"); })).resolves.toBe(false);
  });

  describe("live-call controls", () => {
    const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status });
    it("posts a note to the encoded id and a run request without a body", async () => {
      const fetcher = vi.fn<typeof fetch>(async () => json(202, { data: { accepted: 1 } }));
      const client = createApiClient(() => ({ key: "k", generation: 1 }), fetcher);
      await expect(client.addSessionNote("s/1", "vip", true)).resolves.toEqual({ accepted: 1 });
      const [url, init] = fetcher.mock.calls[0];
      expect(url).toBe("/api/v1/sessions/s%2F1/context");
      expect(init?.method).toBe("POST");
      expect(init?.body).toBe('{"messages":[{"content":"vip"}],"run":true}');
      await expect(client.runSession("s/1")).resolves.toEqual({ accepted: 1 });
      const [runUrl, runInit] = fetcher.mock.calls[1];
      expect(runUrl).toBe("/api/v1/sessions/s%2F1/run");
      expect(runInit?.method).toBe("POST");
      expect(runInit?.body).toBeUndefined();
    });
    it("turns a 429 too_many_pending into an ApiError with status and code", async () => {
      const fetcher = vi.fn<typeof fetch>(async () => json(429, { error: { code: "too_many_pending", message: "busy", request_id: "r" } }));
      const client = createApiClient(() => ({ key: "k", generation: 1 }), fetcher);
      await expect(client.addSessionNote("s1", "x", false)).rejects.toMatchObject({ status: 429, code: "too_many_pending" });
    });
  });
});

describe("getFlows follows the server's pages", () => {
  const dto = (id: string) => ({
    id, name: id, description: "", enabled: true, archived: false, current_version: null,
    readiness: { state: "ready", reasons: [] }, created_at: "", updated_at: "",
  });
  const ids = (from: number, n: number) => Array.from({ length: n }, (_, i) => dto(`f${from + i}`));
  const clientFor = (pageFor: (offset: number) => Response) => {
    const fetcher = vi.fn<typeof fetch>(async (input) => pageFor(Number(new URL(String(input), "http://x").searchParams.get("offset"))));
    return { fetcher, client: createApiClient(() => ({ key: "k", generation: 1 }), fetcher) };
  };
  const ok = (data: unknown[]) => new Response(JSON.stringify({ data }), { status: 200 });
  const urls = (f: ReturnType<typeof vi.fn>) => f.mock.calls.map((c) => c[0]);

  it("reads a second page when the first is full", async () => {
    const { fetcher, client } = clientFor((o) => ok(o === 0 ? ids(0, 100) : ids(100, 1)));
    await expect(client.getFlows()).resolves.toHaveLength(101);
    expect(urls(fetcher)).toEqual(["/api/v1/agents?limit=100&offset=0", "/api/v1/agents?limit=100&offset=100"]);
  });

  it("asks once more after exactly 100 flows, and gets an empty page", async () => {
    const { fetcher, client } = clientFor((o) => ok(o === 0 ? ids(0, 100) : []));
    await expect(client.getFlows()).resolves.toHaveLength(100);
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it("stops at the page cap", async () => {
    const { fetcher, client } = clientFor((o) => ok(ids(o, 100)));
    await expect(client.getFlows()).resolves.toHaveLength(FLOW_PAGES_MAX * 100);
    expect(fetcher).toHaveBeenCalledTimes(FLOW_PAGES_MAX);
  });

  it("lists a flow once when pages overlap", async () => {
    const { client } = clientFor((o) => ok(o === 0 ? ids(0, 100) : ids(99, 2)));
    const flows = await client.getFlows();
    expect(flows).toHaveLength(101);
    expect(new Set(flows.map((f) => f.id)).size).toBe(101);
  });

  it("rejects, with no partial list, when a later page fails", async () => {
    const { client } = clientFor((o) => (o === 0 ? ok(ids(0, 100)) : new Response("{}", { status: 500 })));
    await expect(client.getFlows()).rejects.toBeInstanceOf(ApiError);
  });
});

describe("flow readiness", () => {
  const parse = (r: object) => flowSchema.parse({ id: "a", name: "A", description: "", enabled: true, archived: false, current_version: null, readiness: r, created_at: "", updated_at: "" });
  it("keeps the names of the missing variables", () => {
    expect(parse({ state: "not_ready", reasons: ["missing_credential_llm"], missing_env: ["XAI_API_KEY"] }).readiness.missing_env).toEqual(["XAI_API_KEY"]);
  });
  it("accepts a readiness without them", () => {
    expect(parse({ state: "ready", reasons: [] }).readiness.missing_env).toBeUndefined();
  });
});
