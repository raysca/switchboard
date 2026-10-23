import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { AgentVoiceTest } from "./AgentVoiceTest";
import {
  type ApiClient,
  type AgentDTO,
} from "../../api/client";
import { BrowserAudioController, BrowserAudioError } from "../../voice/audio";
import {
  decodeBinaryFrame,
  encodeAudioFrame,
  encodeRTVIMessage,
  RTVI_TYPE_BOT_LLM_TEXT,
  RTVI_TYPE_BOT_READY,
  RTVI_TYPE_BOT_STARTED_SPEAKING,
  RTVI_TYPE_BOT_STOPPED_SPEAKING,
  RTVI_TYPE_CLIENT_READY,
  RTVI_TYPE_DISCONNECT,
  RTVI_TYPE_PUSH_TO_TALK,
  RTVI_TYPE_SEND_TEXT,
  RTVI_TYPE_SET_MUTED,
  RTVI_TYPE_USER_STARTED_SPEAKING,
  RTVI_TYPE_USER_STOPPED_SPEAKING,
  RTVI_TYPE_USER_TRANSCRIPTION,
} from "../../voice/protocol";

// Mock WebSocket class
class MockWebSocket {
  public static instances: MockWebSocket[] = [];
  public url: string;
  public readyState: number = WebSocket.CONNECTING;
  public binaryType = "blob";
  public onopen: (() => void) | null = null;
  public onmessage: ((event: MessageEvent) => void) | null = null;
  public onerror: ((event: Event) => void) | null = null;
  public onclose: ((event: CloseEvent) => void) | null = null;
  public sentData: Uint8Array[] = [];

  constructor(url: string) {
    this.url = url;
    MockWebSocket.instances.push(this);
    // Simulate async open
    setTimeout(() => {
      if (this.readyState === WebSocket.CONNECTING) {
        this.readyState = WebSocket.OPEN;
        this.onopen?.();
      }
    }, 10);
  }

  send(data: unknown) {
    if (data instanceof Uint8Array) {
      this.sentData.push(data);
    } else if (ArrayBuffer.isView(data)) {
      this.sentData.push(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
    }
  }

  close(code = 1000, reason = "") {
    this.readyState = WebSocket.CLOSED;
    this.onclose?.({ code, reason } as CloseEvent);
  }

  receiveRTVIMessage(type: string, data?: unknown) {
    const frame = encodeRTVIMessage(type, data);
    this.onmessage?.({ data: frame.buffer } as MessageEvent);
  }

  receiveAudioFrame(samples: Uint8Array, rate = 24000) {
    const frame = encodeAudioFrame(samples, rate, 1);
    this.onmessage?.({ data: frame.buffer } as MessageEvent);
  }
}

function createMockAgent(overrides: Partial<AgentDTO> = {}): AgentDTO {
  return {
    id: "agent-alpha",
    name: "Customer Assistant",
    description: "Helps users with account questions",
    enabled: true,
    archived: false,
    current_version: {
      id: "ver-1",
      number: 1,
      schema_version: 1,
      config: { stt: "google", llm: "gemini-2.0-flash", tts: "journey" },
      config_hash: "hash-1",
      created_at: "2026-10-05T12:00:00Z",
      mcp_tools: [],
    },
    readiness: {
      state: "ready",
      reasons: [],
    },
    created_at: "2026-10-05T12:00:00Z",
    updated_at: "2026-10-05T12:00:00Z",
    ...overrides,
  };
}

function createMockClient(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    issueVoiceToken: vi.fn().mockResolvedValue({
      // Token contains jti: "test-session-uuid-999"
      token:
        "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJqdGkiOiJ0ZXN0LXNlc3Npb24tdXVpZC05OTkiLCJzdWIiOiJhbGljZSIsInBpcGVsaW5lIjoiZGVmYXVsdCJ9.sig",
      expires_at: "2026-10-05T13:00:00Z",
      scope: "voice:connect",
    }),
    getProviders: vi.fn(),
    getProviderDiscovery: vi.fn(),
    getAgents: vi.fn(),
    createAgent: vi.fn(),
    updateAgent: vi.fn(),
    archiveAgent: vi.fn(),
    duplicateAgent: vi.fn(),
    ...overrides,
  } as ApiClient;
}

describe("AgentVoiceTest", () => {
  let originalWebSocket: typeof WebSocket;

  beforeEach(() => {
    MockWebSocket.instances = [];
    originalWebSocket = globalThis.WebSocket;
    globalThis.WebSocket = MockWebSocket as unknown as typeof WebSocket;
  });

  afterEach(() => {
    globalThis.WebSocket = originalWebSocket;
  });

  it("handles microphone denied permission error gracefully with retry", async () => {
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockRejectedValue(
      new BrowserAudioError(
        "Microphone permission was denied. Please allow microphone access in your browser settings.",
        "permission_denied",
      ),
    );

    const client = createMockClient();
    const agent = createMockAgent();

    render(
      <MemoryRouter>
        <AgentVoiceTest
          agent={agent}
          client={client}
          keyValue="mgmt-key-1"
          audioController={mockAudio}
        />
      </MemoryRouter>,
    );

    const user = userEvent.setup();
    const startBtn = screen.getByTestId("start-voice-test-btn");
    await user.click(startBtn);

    expect(await screen.findByRole("alert")).toBeVisible();
    expect(screen.getByText(/Microphone Access Denied/i)).toBeVisible();
    expect(
      screen.getByText(/Microphone permission was denied/i),
    ).toBeVisible();

    // Has retry button
    const retryBtn = screen.getByRole("button", { name: /retry connection/i });
    expect(retryBtn).toBeVisible();

    // Audio capture mock now succeeds on retry
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);
    await user.click(retryBtn);

    await waitFor(() => {
      expect(screen.queryByText(/Microphone Access Denied/i)).toBeNull();
    });
  });

  it("handles token creation or WebSocket connection failure gracefully", async () => {
    const client = createMockClient({
      issueVoiceToken: vi.fn().mockRejectedValue(new Error("Token service unavailable")),
    });
    const agent = createMockAgent();
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);

    render(
      <MemoryRouter>
        <AgentVoiceTest agent={agent} client={client} keyValue="mgmt-key-1" audioController={mockAudio} />
      </MemoryRouter>,
    );

    const user = userEvent.setup();
    await user.click(screen.getByTestId("start-voice-test-btn"));

    expect(await screen.findByRole("alert")).toBeVisible();
    expect(screen.getByText(/Token Creation Failed/i)).toBeVisible();
    expect(screen.getByText(/Token service unavailable/i)).toBeVisible();
  });

  it("shows the server's reply times for the call: a strip with the last and median reply, and a note under the reply it measured", async () => {
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);
    const getSessionTurns = vi.fn().mockResolvedValue([
      {
        id: "t1",
        session_id: "test-session-uuid-999",
        ordinal: 1,
        started_at: "2026-10-12T14:00:00Z",
        ended_at: "2026-10-12T14:00:02Z",
        transcript: "hello",
        assistant_text: "Hi, how can I help?",
        interrupted: false,
        latencies: { voice_to_voice_ms: 640 },
      },
    ]);
    const client = createMockClient({ getSessionTurns });
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={createMockAgent()} client={client} keyValue="mgmt-key-1" audioController={mockAudio} wsBaseUrl="ws://127.0.0.1:9090/ws" />
      </MemoryRouter>,
    );
    expect(screen.queryByText("Last reply")).toBeNull();
    await userEvent.setup().click(screen.getByTestId("start-voice-test-btn"));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_READY, { version: "2.1.0" });
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_LLM_TEXT, { text: "Hi, how can I help?" });
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_STARTED_SPEAKING);
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_STOPPED_SPEAKING);
    expect(await screen.findByText("Last reply")).toBeVisible();
    expect(getSessionTurns).toHaveBeenCalledWith("test-session-uuid-999");
    expect(screen.getAllByText("640 ms").length).toBeGreaterThan(0);
    expect(await screen.findByText("Replied in 640 ms")).toBeVisible();
  });

  it("executes normal conversational agent: connect -> bot-ready -> user speaks -> bot speaks -> barge-in -> disconnect -> trace link", async () => {
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);
    const stopPlaybackSpy = vi.spyOn(mockAudio, "stopPlayback");
    const playChunkSpy = vi.spyOn(mockAudio, "playAudioChunk");

    const client = createMockClient();
    const agent = createMockAgent();

    render(
      <MemoryRouter initialEntries={["/agents/agent-alpha"]}>
        <Routes>
          <Route
            path="/agents/agent-alpha"
            element={
              <AgentVoiceTest
                agent={agent}
                client={client}
                keyValue="mgmt-key-1"
                audioController={mockAudio}
                wsBaseUrl="ws://127.0.0.1:9090/ws"
              />
            }
          />
          <Route
            path="/sessions/:sessionId"
            element={<div data-testid="sessions-page">Session Details Page</div>}
          />
        </Routes>
      </MemoryRouter>,
    );

    const user = userEvent.setup();

    // 1. Connect
    await user.click(screen.getByTestId("start-voice-test-btn"));

    expect(client.issueVoiceToken).toHaveBeenCalledWith(
      expect.objectContaining({
        principal: "agent-test-agent-alpha",
        agent_id: "agent-alpha",
      }),
    );

    // Wait for WS instance
    await waitFor(() => {
      expect(MockWebSocket.instances.length).toBe(1);
    });
    const ws = MockWebSocket.instances[0];

    // Verify WebSocket connected with token query param
    expect(ws.url).toContain("token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9");
    expect(ws.url).not.toContain("pipeline");

    // Client sends client-ready frame
    await waitFor(() => {
      expect(ws.sentData.length).toBeGreaterThan(0);
    });
    const firstSent = decodeBinaryFrame(ws.sentData[0]);
    expect(firstSent?.kind).toBe("message");
    if (firstSent?.kind === "message") {
      expect(firstSent.parsed?.type).toBe(RTVI_TYPE_CLIENT_READY);
    }

    // 2. Server sends bot-ready frame
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_READY, { version: "2.1.0" });

    // Status becomes READY / Live call
    expect(await screen.findByText("READY")).toBeVisible();
    expect(screen.getByText(/Live call active/i)).toBeVisible();
    expect(screen.getByTestId("disconnect-btn")).toBeVisible();
    expect(screen.getByTestId("mute-btn")).toBeVisible();

    // 3. User speaks: user-started-speaking, interim transcript, user-stopped-speaking, final transcript
    ws.receiveRTVIMessage(RTVI_TYPE_USER_STARTED_SPEAKING);
    expect(await screen.findByText(/Listening \(user speaking\)…/i)).toBeVisible();

    // Interim transcript
    ws.receiveRTVIMessage(RTVI_TYPE_USER_TRANSCRIPTION, {
      text: "How do I reset my",
      final: false,
    });
    expect(await screen.findByTestId("interim-transcript")).toHaveTextContent(
      "How do I reset my",
    );

    // User stops speaking
    ws.receiveRTVIMessage(RTVI_TYPE_USER_STOPPED_SPEAKING);

    // Final transcript
    ws.receiveRTVIMessage(RTVI_TYPE_USER_TRANSCRIPTION, {
      text: "How do I reset my password?",
      final: true,
    });
    expect(await screen.findByText("How do I reset my password?")).toBeVisible();
    expect(screen.queryByTestId("interim-transcript")).toBeNull();

    // 4. Bot speaks: bot-llm-text, bot-started-speaking, audio frames, bot-stopped-speaking
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_LLM_TEXT, {
      text: "You can reset your password ",
    });
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_LLM_TEXT, {
      text: "in account settings.",
    });
    expect(await screen.findByTestId("bot-streaming-text")).toHaveTextContent(
      "You can reset your password in account settings.",
    );

    ws.receiveRTVIMessage(RTVI_TYPE_BOT_STARTED_SPEAKING);
    expect(await screen.findByText(/Assistant speaking…/i)).toBeVisible();

    // Server sends audio samples
    const fakeSamples = new Uint8Array(480);
    ws.receiveAudioFrame(fakeSamples, 24000);
    expect(playChunkSpy).toHaveBeenCalled();

    ws.receiveRTVIMessage(RTVI_TYPE_BOT_STOPPED_SPEAKING);
    await waitFor(() => {
      expect(screen.queryByTestId("bot-streaming-text")).toBeNull();
    });
    expect(
      screen.getByText("You can reset your password in account settings."),
    ).toBeVisible();

    // 5. Barge-in test: while bot speaks, user speaks
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_STARTED_SPEAKING);
    expect(await screen.findByText(/Assistant speaking…/i)).toBeVisible();

    // User interrupts bot!
    ws.receiveRTVIMessage(RTVI_TYPE_USER_STARTED_SPEAKING);
    expect(
      await screen.findByText(/Barge-in: user interrupted assistant!/i),
    ).toBeVisible();
    expect(stopPlaybackSpy).toHaveBeenCalled();
    expect(screen.getByText(/Interrupted: 1x/i)).toBeVisible();

    ws.receiveRTVIMessage(RTVI_TYPE_USER_STOPPED_SPEAKING);

    // 6. Mic mute toggle
    const muteBtn = screen.getByTestId("mute-btn");
    expect(muteBtn).toHaveTextContent("Mute");
    await user.click(muteBtn);
    expect(screen.getByText(/\[Mic Muted\]/i)).toBeVisible();
    expect(muteBtn).toHaveTextContent("Unmute");
    const muteMessages = () =>
      ws.sentData
        .map((d) => decodeBinaryFrame(d))
        .filter((f) => f?.kind === "message" && f.parsed?.type === RTVI_TYPE_SET_MUTED)
        .map((f) => (f?.kind === "message" ? f.parsed?.data : null));
    expect(muteMessages()).toEqual([{ muted: true }]);
    await user.click(muteBtn);
    expect(screen.queryByText(/\[Mic Muted\]/i)).toBeNull();
    expect(muteMessages()).toEqual([{ muted: true }, { muted: false }]);

    // 7. Session summary & Trace timeline link verification
    const sessionSummary = screen.getByTestId("session-summary");
    expect(sessionSummary).toBeVisible();
    expect(sessionSummary).toHaveTextContent("test-session-uuid-999");

    const traceLink = screen.getByTestId("view-trace-link");
    expect(traceLink).toBeVisible();
    expect(traceLink).toHaveAttribute("href", "/sessions/test-session-uuid-999");

    // 8. Disconnect
    const disconnectBtn = screen.getByTestId("disconnect-btn");
    await user.click(disconnectBtn);

    // Status becomes DISCONNECTED
    expect(await screen.findByText("DISCONNECTED")).toBeVisible();
    expect(screen.getByTestId("start-voice-test-btn")).toBeVisible();

    // Verify disconnect-bot was sent
    const lastSent = decodeBinaryFrame(ws.sentData[ws.sentData.length - 1]);
    expect(lastSent?.kind).toBe("message");
    if (lastSent?.kind === "message") {
      expect(lastSent.parsed?.type).toBe(RTVI_TYPE_DISCONNECT);
    }

    // Trace link remains accessible after call disconnect
    expect(screen.getByTestId("view-trace-link")).toBeVisible();

    // Clicking trace link routes to /sessions/:sessionId
    await user.click(screen.getByTestId("view-trace-link"));
    expect(await screen.findByTestId("sessions-page")).toBeVisible();
  });

  it("push-to-talk: opts in at client-ready and sends press and release", async () => {
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={createMockAgent()} client={createMockClient()} keyValue="mgmt-key-1"
          audioController={mockAudio} wsBaseUrl="ws://127.0.0.1:9090/ws" />
      </MemoryRouter>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByLabelText("Push to talk"));
    await user.click(screen.getByRole("button", { name: "Start voice test" }));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];
    await waitFor(() => expect(ws.sentData.length).toBeGreaterThan(0));
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_READY, { version: "2.1.0" });
    await waitFor(() => expect(screen.getByTestId("connection-state")).toHaveTextContent("READY"));

    const messages = (type: string) =>
      ws.sentData
        .map((d) => decodeBinaryFrame(d))
        .filter((f) => f?.kind === "message" && f.parsed?.type === type)
        .map((f) => (f?.kind === "message" ? f.parsed?.data : null));
    expect(messages(RTVI_TYPE_CLIENT_READY)[0]).toMatchObject({ push_to_talk: true });
    expect(screen.getByLabelText("Push to talk")).toBeDisabled();

    const talk = screen.getByTestId("ptt-btn");
    await user.pointer({ keys: "[MouseLeft>]", target: talk });
    expect(talk).toHaveAttribute("aria-pressed", "true");
    await user.pointer({ keys: "[/MouseLeft]", target: talk });
    expect(talk).toHaveAttribute("aria-pressed", "false");
    talk.focus();
    await user.keyboard("[Space>]");
    await user.keyboard("[/Space]");
    expect(messages(RTVI_TYPE_PUSH_TO_TALK)).toEqual([
      { pressed: true }, { pressed: false }, { pressed: true }, { pressed: false },
    ]);
  });

  async function connectPushToTalk() {
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={createMockAgent()} client={createMockClient()} keyValue="mgmt-key-1"
          audioController={mockAudio} wsBaseUrl="ws://127.0.0.1:9090/ws" />
      </MemoryRouter>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByLabelText("Push to talk"));
    await user.click(screen.getByRole("button", { name: "Start voice test" }));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];
    await waitFor(() => expect(ws.sentData.length).toBeGreaterThan(0));
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_READY, { version: "2.1.0" });
    await waitFor(() => expect(screen.getByTestId("connection-state")).toHaveTextContent("READY"));
    const presses = () =>
      ws.sentData
        .map((d) => decodeBinaryFrame(d))
        .filter((f) => f?.kind === "message" && f.parsed?.type === RTVI_TYPE_PUSH_TO_TALK)
        .map((f) => (f?.kind === "message" ? f.parsed?.data : null));
    return { user, ws, talk: screen.getByTestId("ptt-btn"), presses };
  }

  it("push-to-talk: leaving the window while holding releases", async () => {
    const { user, talk, presses } = await connectPushToTalk();
    await user.pointer({ keys: "[MouseLeft>]", target: talk });
    expect(talk).toHaveAttribute("aria-pressed", "true");
    fireEvent.blur(window);
    await waitFor(() => expect(talk).toHaveAttribute("aria-pressed", "false"));
    expect(presses()).toEqual([{ pressed: true }, { pressed: false }]);
  });

  it("push-to-talk: a release over a closed socket still stops talking", async () => {
    const { user, ws, talk } = await connectPushToTalk();
    await user.pointer({ keys: "[MouseLeft>]", target: talk });
    ws.readyState = 3; // CLOSED; the mock global has no WebSocket.* constants
    await user.pointer({ keys: "[/MouseLeft]", target: talk });
    expect(talk).toHaveAttribute("aria-pressed", "false");
  });

  it("hands-free calls show no hold-to-talk button", async () => {
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={createMockAgent()} client={createMockClient()} keyValue="mgmt-key-1"
          audioController={mockAudio} wsBaseUrl="ws://127.0.0.1:9090/ws" />
      </MemoryRouter>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Start voice test" }));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];
    await waitFor(() => expect(ws.sentData.length).toBeGreaterThan(0));
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_READY, { version: "2.1.0" });
    await waitFor(() => expect(screen.getByTestId("connection-state")).toHaveTextContent("READY"));
    expect(screen.queryByTestId("ptt-btn")).toBeNull();
  });

  it("restates mute to the server after bot-ready", async () => {
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={createMockAgent()} client={createMockClient()} keyValue="mgmt-key-1"
          audioController={mockAudio} wsBaseUrl="ws://127.0.0.1:9090/ws" />
      </MemoryRouter>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Start voice test" }));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];
    await waitFor(() => expect(ws.sentData.length).toBeGreaterThan(0));
    await user.click(screen.getByTestId("mute-btn")); // muted before the session is ready
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_READY, { version: "2.1.0" });
    await waitFor(() => expect(screen.getByTestId("connection-state")).toHaveTextContent("READY"));
    const muted = ws.sentData
      .map((d) => decodeBinaryFrame(d))
      .filter((f) => f?.kind === "message" && f.parsed?.type === RTVI_TYPE_SET_MUTED);
    expect(muted.length).toBe(2); // the click, then the restatement after bot-ready
  });

  it("sends a typed text turn and shows user and bot transcript entries", async () => {
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);
    const client = createMockClient();

    render(
      <MemoryRouter>
        <AgentVoiceTest
          agent={createMockAgent()}
          client={client}
          keyValue="mgmt-key-1"
          audioController={mockAudio}
          wsBaseUrl="ws://127.0.0.1:9090/ws"
        />
      </MemoryRouter>,
    );
    const user = userEvent.setup();

    expect(screen.getByTestId("agent-readiness")).toHaveTextContent("ready");
    expect(screen.getByTestId("connection-state")).toHaveTextContent("DISCONNECTED");
    const input = screen.getByLabelText("Message");
    expect(input).toBeDisabled();
    expect(screen.getByRole("button", { name: "Send text" })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "Start voice test" }));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];
    await waitFor(() => expect(ws.sentData.length).toBeGreaterThan(0));
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_READY, { version: "2.1.0" });
    await waitFor(() =>
      expect(screen.getByTestId("connection-state")).toHaveTextContent("READY"),
    );

    await user.type(screen.getByLabelText("Message"), "What are your hours?");
    await user.click(screen.getByRole("button", { name: "Send text" }));

    const sent = ws.sentData
      .map((d) => decodeBinaryFrame(d))
      .find((f) => f?.kind === "message" && f.parsed?.type === RTVI_TYPE_SEND_TEXT);
    expect(sent?.kind === "message" && sent.parsed).toMatchObject({
      label: "rtvi-ai",
      type: RTVI_TYPE_SEND_TEXT,
      data: { content: "What are your hours?" },
    });
    expect(sent?.kind === "message" && sent.parsed?.id).toBeTruthy();
    expect(screen.getByLabelText("Message")).toHaveValue("");

    // Server answers like a voice turn.
    ws.receiveRTVIMessage(RTVI_TYPE_USER_TRANSCRIPTION, { text: "What are your hours?", final: true });
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_LLM_TEXT, { text: "We are open 9 to 5." });
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_STARTED_SPEAKING);
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_STOPPED_SPEAKING);

    expect(await screen.findByTestId("transcript-user")).toHaveTextContent("What are your hours?");
    expect(await screen.findByTestId("transcript-bot")).toHaveTextContent("We are open 9 to 5.");

    // Typing while the bot speaks interrupts it, like speech: local audio stops.
    const stopPlaybackSpy = vi.spyOn(mockAudio, "stopPlayback");
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_STARTED_SPEAKING);
    expect(await screen.findByText(/Assistant speaking…/i)).toBeVisible();
    await user.type(screen.getByLabelText("Message"), "Actually, never mind");
    await user.click(screen.getByRole("button", { name: "Send text" }));
    expect(stopPlaybackSpy).toHaveBeenCalled();
    expect(await screen.findByText(/Interrupted: 1x/i)).toBeVisible();
  });

  it("toggles Pipecat debug panel visibility", async () => {
    const client = createMockClient();
    const agent = createMockAgent();

    render(
      <MemoryRouter>
        <AgentVoiceTest agent={agent} client={client} keyValue="mgmt-key-1" />
      </MemoryRouter>,
    );

    const user = userEvent.setup();
    const toggleBtn = screen.getByTestId("toggle-debug-btn");

    expect(screen.queryByLabelText(/Pipecat RTVI Debug Panel/i)).toBeNull();

    await user.click(toggleBtn);
    expect(screen.getByLabelText(/Pipecat RTVI Debug Panel/i)).toBeVisible();

    await user.click(toggleBtn);
    expect(screen.queryByLabelText(/Pipecat RTVI Debug Panel/i)).toBeNull();
  });
  it("tells the user a call is on the version it started on after a newer one is published", async () => {
    const client = createMockClient();
    const v1 = createMockAgent();
    const v2 = createMockAgent();
    v2.current_version = { ...v1.current_version!, number: (v1.current_version!.number ?? 1) + 1 };
    const audio = new BrowserAudioController();
    vi.spyOn(audio, "startCapture").mockResolvedValue(undefined);
    const ui = (a: AgentDTO) => (
      <MemoryRouter>
        <AgentVoiceTest agent={a} client={client} keyValue="k" audioController={audio} wsBaseUrl="ws://127.0.0.1:9090/ws" />
      </MemoryRouter>
    );
    const { rerender } = render(ui(v1));
    await userEvent.setup().click(screen.getByTestId("start-voice-test-btn"));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    expect(screen.queryByText(/this call is on v/i)).toBeNull();

    rerender(ui(v2));
    expect(screen.getByText(/this call is on v1\. disconnect and reconnect to test v2/i)).toBeVisible();
    // The header names the version the call is on, not the newest one.
    expect(screen.getByText("Testing v1")).toBeVisible();
  });

  it("runs the draft it is given, names it, and never calls a draft call stale", async () => {
    const client = createMockClient();
    const v1 = createMockAgent();
    const v2 = createMockAgent();
    v2.current_version = { ...v1.current_version!, number: (v1.current_version!.number ?? 1) + 1 };
    const audio = new BrowserAudioController();
    vi.spyOn(audio, "startCapture").mockResolvedValue(undefined);
    const draft = { config: { a: 1 }, mcp_tools: [{ mcp_server_id: "crm", remote_name: "echo", schema_hash: "h", required: false }] };
    const ui = (f: AgentDTO) => (
      <MemoryRouter>
        <AgentVoiceTest agent={f} client={client} keyValue="k" audioController={audio} wsBaseUrl="ws://127.0.0.1:9090/ws" draft={draft} />
      </MemoryRouter>
    );
    const { rerender } = render(ui(v1));
    expect(screen.getByText("Testing draft (based on v1)")).toBeVisible();
    await userEvent.setup().click(screen.getByTestId("start-voice-test-btn"));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    expect(client.issueVoiceToken).toHaveBeenCalledWith(expect.objectContaining({ agent_id: v1.id, draft }));
    rerender(ui(v2));
    expect(screen.queryByText(/this call is on v/i)).toBeNull();
  });

  it("starts a text-only agent (silence speech-to-text) without a microphone, push to talk or mute", async () => {
    const client = createMockClient();
    const agent = createMockAgent();
    agent.current_version = { ...agent.current_version!, config: { ...agent.current_version!.config, stt: { provider: "silence", model: "text" } } };
    const audio = new BrowserAudioController();
    const capture = vi.spyOn(audio, "startCapture").mockResolvedValue(undefined);
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={agent} client={client} keyValue="k" audioController={audio} wsBaseUrl="ws://127.0.0.1:9090/ws" />
      </MemoryRouter>,
    );
    expect(screen.queryByText("Push to talk")).toBeNull();
    expect(screen.queryByTestId("start-voice-test-btn")).toHaveTextContent("Start text test");
    expect(screen.getByText(/Click "Start text test" to talk to this agent/)).toBeVisible(); // no voice wording on a text-only agent
    expect(screen.queryByText(/voice test/i)).toBeNull();
    await userEvent.setup().click(screen.getByTestId("start-voice-test-btn"));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    await screen.findByTestId("disconnect-btn");
    expect(capture).not.toHaveBeenCalled();
    expect(screen.queryByTestId("mute-btn")).toBeNull();
  });

  it("treats an agent with handoffs as text-only by its start step's speech-to-text", async () => {
    const client = createMockClient();
    const agent = createMockAgent();
    agent.current_version = {
      ...agent.current_version!,
      config: {
        start: "front",
        steps: [
          { id: "front", name: "Front desk", config: { stt: { provider: "silence", model: "text" }, llm: { provider: "echo", model: "m" }, tts: { provider: "silence", voice: "none" } } },
          { id: "sales", name: "Sales", config: { llm: { provider: "echo", model: "m" }, tts: { provider: "echo", voice: "v" } } },
        ],
        handoffs: [{ id: "e1", from: "front", to: "sales", condition: "buy" }],
      },
    };
    const audio = new BrowserAudioController();
    const capture = vi.spyOn(audio, "startCapture").mockResolvedValue(undefined);
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={agent} client={client} keyValue="k" audioController={audio} wsBaseUrl="ws://127.0.0.1:9090/ws" />
      </MemoryRouter>,
    );
    expect(screen.queryByText("Push to talk")).toBeNull();
    await userEvent.setup().click(screen.getByTestId("start-voice-test-btn"));
    await screen.findByTestId("disconnect-btn");
    expect(capture).not.toHaveBeenCalled(); // the later step has a real voice, but nobody speaks to it
  });

  it("a draft decides: a silence draft over a voice agent is text-only, and the reverse is not", async () => {
    const client = createMockClient();
    const audio = new BrowserAudioController();
    const capture = vi.spyOn(audio, "startCapture").mockResolvedValue(undefined);
    const draft = { config: { stt: { provider: "echo", model: "s" } } };
    const agent = createMockAgent();
    agent.current_version = { ...agent.current_version!, config: { stt: { provider: "silence", model: "text" } } };
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={agent} client={client} keyValue="k" audioController={audio} wsBaseUrl="ws://127.0.0.1:9090/ws" draft={draft} />
      </MemoryRouter>,
    );
    expect(screen.getByText("Push to talk")).toBeVisible(); // the draft listens, so the panel is the voice one
    await userEvent.setup().click(screen.getByTestId("start-voice-test-btn"));
    await waitFor(() => expect(capture).toHaveBeenCalled());
  });

  it("refuses to start while it is blocked, and says why", () => {
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={createMockAgent()} client={createMockClient()} keyValue="k" blocked="Fix the Reception step first." />
      </MemoryRouter>,
    );
    expect(screen.getByTestId("start-voice-test-btn")).toBeDisabled();
    expect(screen.getByText("Fix the Reception step first.")).toBeVisible();
  });

  async function connected(props: { versionNumber?: number; activeStep?: string } = {}) {
    const audio = new BrowserAudioController();
    vi.spyOn(audio, "startCapture").mockResolvedValue(undefined);
    const client = createMockClient();
    const agent = createMockAgent();
    if (props.versionNumber) agent.current_version = { ...agent.current_version!, number: props.versionNumber };
    const ui = (step?: string) => (
      <MemoryRouter>
        <AgentVoiceTest agent={agent} client={client} keyValue="k" audioController={audio} wsBaseUrl="ws://127.0.0.1:9090/ws" activeStep={step} />
      </MemoryRouter>
    );
    const view = render(ui(props.activeStep));
    const user = userEvent.setup();
    await user.click(screen.getByTestId("start-voice-test-btn"));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];
    await waitFor(() => expect(ws.sentData.length).toBeGreaterThan(0));
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_READY, { version: "2.1.0" });
    await waitFor(() => expect(screen.getByTestId("connection-state")).toHaveTextContent("READY"));
    return {
      user,
      ws,
      setActiveStep: (step: string) => view.rerender(ui(step)),
      botSays: async (text: string) => {
        ws.receiveRTVIMessage(RTVI_TYPE_BOT_LLM_TEXT, { text });
        ws.receiveRTVIMessage(RTVI_TYPE_BOT_STARTED_SPEAKING);
        ws.receiveRTVIMessage(RTVI_TYPE_BOT_STOPPED_SPEAKING);
        await screen.findByText(text);
      },
    };
  }

  it("shows the version and a clock in a header, and counts while the call is live", async () => {
    await connected({ versionNumber: 3 });
    expect(screen.getByText("Testing v3")).toBeVisible();
    expect(screen.getByTestId("call-clock")).toHaveTextContent(/^00:0\d$/);
    await waitFor(() => expect(screen.getByTestId("call-clock").textContent).not.toBe("00:00"), { timeout: 3000 });
  });

  it("offers End call and Mute with their new words, keeping the test ids", async () => {
    const { user } = await connected();
    expect(screen.getByTestId("disconnect-btn")).toHaveTextContent("End call");
    expect(screen.getByTestId("mute-btn")).toHaveTextContent("Mute");
    await user.click(screen.getByTestId("mute-btn"));
    expect(screen.getByTestId("mute-btn")).toHaveTextContent("Unmute");
  });

  it("labels each reply with the step that held the call when it arrived, and never relabels it", async () => {
    const h = await connected({ activeStep: "Reception" });
    await h.botSays("Hello, you're through to reception.");
    h.setActiveStep("Billing");
    await h.botSays("I can see your invoice.");
    const lines = await screen.findAllByTestId("transcript-bot");
    expect(lines[0]).toHaveTextContent("Reception");
    expect(lines[1]).toHaveTextContent("Billing");
    expect(lines[0]).not.toHaveTextContent("Billing");
  });

  it("relabels the incoming step's greeting once the step changes, keeping the old step's goodbye", async () => {
    const h = await connected({ activeStep: "Front desk" });
    h.ws.receiveRTVIMessage(RTVI_TYPE_USER_TRANSCRIPTION, { text: "I want to buy a car", final: true });
    await screen.findByText("I want to buy a car");
    await h.botSays("Connecting you to Sales."); // the old step's goodbye
    await h.botSays("Hi, which car interests you?"); // the greeting: spoken before the server reports Sales active
    let lines = await screen.findAllByTestId("transcript-bot");
    expect(lines[1]).toHaveTextContent("Front desk"); // still unknown: no step change yet
    h.setActiveStep("Sales");
    lines = await screen.findAllByTestId("transcript-bot");
    expect(lines[0]).toHaveTextContent("Front desk");
    expect(lines[1]).toHaveTextContent("Sales");
    // Later lines follow the active step, and a settled label never changes again.
    h.ws.receiveRTVIMessage(RTVI_TYPE_USER_TRANSCRIPTION, { text: "The electric one", final: true });
    await screen.findByText("The electric one");
    await h.botSays("It suits long trips.");
    h.setActiveStep("Service");
    lines = await screen.findAllByTestId("transcript-bot");
    expect(lines.map((l) => l.querySelector(".line__who")?.textContent)).toEqual(["Front desk", "Sales", "Sales"]);
  });

  it("does not relabel anything when the step changes without a caller turn to anchor it", async () => {
    const h = await connected({ activeStep: "Front desk" });
    await h.botSays("Welcome.");
    await h.botSays("How can I help?");
    h.setActiveStep("Sales");
    const lines = await screen.findAllByTestId("transcript-bot");
    expect(lines.map((l) => l.querySelector(".line__who")?.textContent)).toEqual(["Front desk", "Front desk"]);
  });

  it("falls back to the agent's name when no step is known", async () => {
    const h = await connected();
    await h.botSays("Hi there.");
    expect(await screen.findByTestId("transcript-bot")).toHaveTextContent("Customer Assistant");
  });
});
