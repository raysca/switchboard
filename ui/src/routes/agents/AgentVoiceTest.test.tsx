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

    render(
      <MemoryRouter>
        <AgentVoiceTest agent={agent} client={client} keyValue="mgmt-key-1" />
      </MemoryRouter>,
    );

    const user = userEvent.setup();
    await user.click(screen.getByTestId("start-voice-test-btn"));

    expect(await screen.findByRole("alert")).toBeVisible();
    expect(screen.getByText(/Token Creation Failed/i)).toBeVisible();
    expect(screen.getByText(/Token service unavailable/i)).toBeVisible();
  });

  it("executes normal conversational flow: connect -> bot-ready -> user speaks -> bot speaks -> barge-in -> disconnect -> trace link", async () => {
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
    expect(muteBtn).toHaveTextContent("Mute mic");
    await user.click(muteBtn);
    expect(screen.getByText(/\[Mic Muted\]/i)).toBeVisible();
    expect(muteBtn).toHaveTextContent("Unmute mic");
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
});
