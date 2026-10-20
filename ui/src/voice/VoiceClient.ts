/**
 * RTVI Voice Client Controller.
 * Manages WebSocket connection to /ws, audio capture/playback, RTVI lifecycle, and metrics.
 */

import type { ApiClient, ToolSelection } from "../api/client";
import { BrowserAudioController, BrowserAudioError } from "./audio";
import {
  decodeBinaryFrame,
  encodeAudioFrame,
  encodeClientReadyFrame,
  encodeDisconnectFrame,
  encodeSendTextFrame,
  encodePushToTalkFrame,
  encodeSetMutedFrame,
  extractSessionIdFromToken,
  RTVI_TYPE_BOT_LLM_TEXT,
  RTVI_TYPE_BOT_READY,
  RTVI_TYPE_BOT_STARTED_SPEAKING,
  RTVI_TYPE_BOT_STOPPED_SPEAKING,
  RTVI_TYPE_CLIENT_READY,
  RTVI_TYPE_DISCONNECT,
  RTVI_TYPE_ERROR,
  RTVI_PROTOCOL_VERSION,
  RTVI_TYPE_PUSH_TO_TALK,
  RTVI_TYPE_SEND_TEXT,
  RTVI_TYPE_SET_MUTED,
  RTVI_TYPE_USER_STARTED_SPEAKING,
  RTVI_TYPE_USER_STOPPED_SPEAKING,
  RTVI_TYPE_USER_TRANSCRIPTION,
  type DecodedFrame,
  type RTVIBotTextData,
  type RTVIErrorData,
  type RTVIMessage,
  type RTVITranscriptionData,
} from "./protocol";

export type SessionState =
  | "disconnected"
  | "requesting_token"
  | "connecting"
  | "connected"
  | "ready"
  | "error";

export interface TranscriptItem {
  id: string;
  speaker: "user" | "bot";
  text: string;
  final: boolean;
  timestamp: number;
}

export interface RTVIDebugEvent {
  id: string;
  timestamp: number;
  direction: "in" | "out";
  type: string;
  label: string;
  raw: string;
  data?: unknown;
}

export interface VoiceMetrics {
  connectLatencyMs: number | null;
  readyLatencyMs: number | null;
  userSpeakingDurationMs: number;
  botSpeakingDurationMs: number;
  totalUserSpeakingMs: number;
  totalBotSpeakingMs: number;
  audioFramesIn: number;
  audioFramesOut: number;
  audioBytesIn: number;
  audioBytesOut: number;
  messageFramesIn: number;
  messageFramesOut: number;
  bargeInCount: number;
  currentFrameRate: number;
}

export interface VoiceClientError {
  category: "mic" | "connection" | "token" | "server" | "protocol";
  message: string;
  fatal?: boolean;
}

export interface VoiceClientState {
  sessionState: SessionState;
  sessionId: string | null;
  isUserSpeaking: boolean;
  isBotSpeaking: boolean;
  isMuted: boolean;
  pushToTalk: boolean;
  isTalking: boolean;
  isBargeInActive: boolean;
  currentInterimTranscript: string;
  currentBotStreamingText: string;
  transcripts: TranscriptItem[];
  events: RTVIDebugEvent[];
  metrics: VoiceMetrics;
  error: VoiceClientError | null;
}

type VoiceClientListener = (state: VoiceClientState) => void;

interface ConnectOptions {
  flowId: string;
  principal?: string;
  wsBaseUrl?: string;
  /** Turns start and end on the hold-to-talk button instead of voice detection. */
  pushToTalk?: boolean;
  /** An unpublished flow config to run instead of the published version (a test call). */
  draft?: { config: unknown; mcp_tools?: ToolSelection[] };
}

export class VoiceClient {
  private apiClient: ApiClient;
  private audioController: BrowserAudioController;
  private ws: WebSocket | null = null;
  private listeners = new Set<VoiceClientListener>();

  private state: VoiceClientState = {
    sessionState: "disconnected",
    sessionId: null,
    isUserSpeaking: false,
    isBotSpeaking: false,
    isMuted: false,
    pushToTalk: false,
    isTalking: false,
    isBargeInActive: false,
    currentInterimTranscript: "",
    currentBotStreamingText: "",
    transcripts: [],
    events: [],
    metrics: {
      connectLatencyMs: null,
      readyLatencyMs: null,
      userSpeakingDurationMs: 0,
      botSpeakingDurationMs: 0,
      totalUserSpeakingMs: 0,
      totalBotSpeakingMs: 0,
      audioFramesIn: 0,
      audioFramesOut: 0,
      audioBytesIn: 0,
      audioBytesOut: 0,
      messageFramesIn: 0,
      messageFramesOut: 0,
      bargeInCount: 0,
      currentFrameRate: 0,
    },
    error: null,
  };

  // Timers and metric trackers
  private connectStartTime = 0;
  private clientReadyTime = 0;
  private userSpeechStartTime = 0;
  private botSpeechStartTime = 0;
  private recentAudioFramesInTimestamps: number[] = [];
  private bargeInResetTimer: ReturnType<typeof setTimeout> | null = null;
  // Bumped by connect() and disconnect(); a connect() whose attempt is stale stops after its next await.
  private attempt = 0;

  constructor(apiClient: ApiClient, audioController?: BrowserAudioController) {
    this.apiClient = apiClient;
    this.audioController = audioController ?? new BrowserAudioController();
  }

  public subscribe(listener: VoiceClientListener): () => void {
    this.listeners.add(listener);
    listener(this.state);
    return () => {
      this.listeners.delete(listener);
    };
  }

  public getState(): VoiceClientState {
    return this.state;
  }

  private updateState(updates: Partial<VoiceClientState>): void {
    this.state = { ...this.state, ...updates };
    this.notify();
  }

  private notify(): void {
    for (const listener of this.listeners) {
      try {
        listener(this.state);
      } catch {
        // Ignore listener error
      }
    }
  }

  /**
   * Connects to the voice server: requests token, initializes mic, connects WS, performs RTVI handshake.
   */
  public async connect(options: ConnectOptions): Promise<void> {
    if (
      this.state.sessionState === "requesting_token" ||
      this.state.sessionState === "connecting" ||
      this.state.sessionState === "connected" ||
      this.state.sessionState === "ready"
    ) {
      return;
    }

    const flowId = options.flowId?.trim();
    if (!flowId) {
      this.updateState({
        sessionState: "error",
        error: { category: "token", message: "A flow is required to start a call.", fatal: true },
      });
      return;
    }
    const principal = options.principal || `operator-${Date.now()}`;
    const attempt = ++this.attempt;

    // Reset session errors and transient speaking states
    this.updateState({
      sessionState: "requesting_token",
      pushToTalk: options.pushToTalk === true,
      isTalking: false,
      error: null,
      isUserSpeaking: false,
      isBotSpeaking: false,
      isBargeInActive: false,
      currentInterimTranscript: "",
      currentBotStreamingText: "",
    });

    // Step 1: Initialize Microphone
    try {
      await this.audioController.startCapture((pcmChunk) => {
        this.sendAudioChunk(pcmChunk);
      });
    } catch (err: unknown) {
      if (attempt !== this.attempt) return;
      const audioErr = err as BrowserAudioError;
      const category =
        audioErr.type === "permission_denied" ? "mic" : "connection";
      this.updateState({
        sessionState: "error",
        error: { category, message: audioErr.message, fatal: true },
      });
      return;
    }
    // disconnect() ran while the mic was opening: it already released the mic,
    // and stopping capture here would stop a newer attempt's mic.
    if (attempt !== this.attempt) return;

    // Step 2: Request Voice Admission Token (after the mic: the token lives one minute, the permission prompt can outlast it)
    let tokenStr = "";
    try {
      const tokenResp = await this.apiClient.issueVoiceToken({
        principal,
        flow_id: flowId,
        ...(options.draft ? { draft: options.draft } : {}),
      });
      if (attempt !== this.attempt) return;
      tokenStr = tokenResp.token;
      this.updateState({
        sessionId: extractSessionIdFromToken(tokenStr),
        sessionState: "connecting",
      });
    } catch (err: unknown) {
      if (attempt !== this.attempt) return;
      this.audioController.stopCapture();
      const msg = (err as Error)?.message ?? "Failed to acquire voice token";
      this.updateState({
        sessionState: "error",
        error: { category: "token", message: msg, fatal: true },
      });
      return;
    }

    // Step 3: Connect WebSocket
    const wsUrl = this.resolveWsUrl(tokenStr, options.wsBaseUrl);
    this.connectStartTime = Date.now();

    try {
      const socket = new WebSocket(wsUrl);
      socket.binaryType = "arraybuffer";
      this.ws = socket;

      socket.onopen = () => {
        const connectLatency = Date.now() - this.connectStartTime;
        this.updateState({
          sessionState: "connected",
          metrics: {
            ...this.state.metrics,
            connectLatencyMs: connectLatency,
          },
        });

        // Send client-ready message
        this.sendClientReady();
      };

      socket.onmessage = (event) => {
        this.handleSocketMessage(event);
      };

      socket.onerror = () => {
        if (this.state.sessionState !== "disconnected") {
          this.updateState({
            sessionState: "error",
            error: {
              category: "connection",
              message:
                "WebSocket connection failed. Verify server is reachable.",
              fatal: true,
            },
          });
        }
      };

      socket.onclose = (event) => {
        this.handleSocketClose(event);
      };
    } catch (err: unknown) {
      this.audioController.stopCapture();
      this.updateState({
        sessionState: "error",
        error: {
          category: "connection",
          message: `Connection failed: ${(err as Error)?.message ?? String(err)}`,
          fatal: true,
        },
      });
    }
  }

  /**
   * Gracefully concludes the voice call and tears down resources.
   */
  public async disconnect(): Promise<void> {
    this.attempt++;
    if (this.state.sessionState === "disconnected") {
      return;
    }

    // Attempt to send disconnect-bot message
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      try {
        const discFrame = encodeDisconnectFrame();
        this.ws.send(discFrame as unknown as ArrayBufferView<ArrayBuffer>);
        this.recordEvent("out", RTVI_TYPE_DISCONNECT, JSON.stringify({ label: "rtvi-ai", type: RTVI_TYPE_DISCONNECT }));
      } catch {
        // Ignore send errors during disconnect
      }
    }

    this.teardownResources();
    this.updateState({
      sessionState: "disconnected",
      isTalking: false,
      isUserSpeaking: false,
      isBotSpeaking: false,
      isBargeInActive: false,
    });
  }

  /** Sends a typed user turn. Returns false when the socket is not open or the text is blank. */
  public sendText(text: string): boolean {
    const content = text.trim();
    if (!content || !this.ws || this.ws.readyState !== WebSocket.OPEN) return false;
    // A typed turn interrupts the reply like speech does; the server flushes its
    // side, this drops what is already buffered here.
    if (this.state.isBotSpeaking) this.triggerBargeIn();
    this.ws.send(encodeSendTextFrame(content) as unknown as ArrayBufferView<ArrayBuffer>);
    this.recordEvent(
      "out",
      RTVI_TYPE_SEND_TEXT,
      JSON.stringify({ label: "rtvi-ai", type: RTVI_TYPE_SEND_TEXT, data: { content } }),
      { content },
    );
    return true;
  }

  /**
   * Toggles microphone mute: disables the mic track and tells the server, which
   * then drops the caller's audio (no turns, STT, or recorded speech).
   */
  public toggleMute(): void {
    const nextMuted = !this.state.isMuted;
    this.audioController.setMuted(nextMuted);
    this.updateState({ isMuted: nextMuted });
    this.sendRTVI(RTVI_TYPE_SET_MUTED, encodeSetMutedFrame(nextMuted), { muted: nextMuted });
  }

  /** Push-to-talk: true when the hold-to-talk button goes down, false when it is released. */
  public setTalking(pressed: boolean): void {
    if (!this.state.pushToTalk || this.state.isTalking === pressed) return;
    const sent = this.sendRTVI(RTVI_TYPE_PUSH_TO_TALK, encodePushToTalkFrame(pressed), { pressed });
    // A release always stops talking here, even if the socket is gone; a press
    // only counts once the server was told.
    if (sent || !pressed) this.updateState({ isTalking: pressed });
  }

  /**
   * Clears event debug log.
   */
  public clearLogs(): void {
    this.updateState({ events: [] });
  }

  // --- Internal Socket & Audio Handlers ---

  private resolveWsUrl(
    token: string,
    customBaseUrl?: string,
  ): string {
    let base = customBaseUrl;
    if (!base) {
      const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
      base = `${proto}//${window.location.host}/ws`;
    }

    const url = new URL(base, "http://localhost");
    url.searchParams.set("token", token);
    return url.toString();
  }

  /** Sends one RTVI client message and logs it. Returns false when the socket is not open. */
  private sendRTVI(type: string, frame: Uint8Array, data: unknown): boolean {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return false;
    this.ws.send(frame as unknown as ArrayBufferView<ArrayBuffer>);
    this.recordEvent("out", type, JSON.stringify({ label: "rtvi-ai", type, data }), data);
    return true;
  }

  private sendClientReady(): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;

    this.clientReadyTime = Date.now();
    const readyFrame = encodeClientReadyFrame(RTVI_PROTOCOL_VERSION, this.state.pushToTalk);
    this.ws.send(readyFrame as unknown as ArrayBufferView<ArrayBuffer>);

    this.recordEvent(
      "out",
      RTVI_TYPE_CLIENT_READY,
      JSON.stringify({
        label: "rtvi-ai",
        type: RTVI_TYPE_CLIENT_READY,
        data: this.state.pushToTalk ? { version: "2.1.0", push_to_talk: true } : { version: "2.1.0" },
      }),
    );
  }

  private sendAudioChunk(pcmChunk: Uint8Array): void {
    if (
      !this.ws ||
      this.ws.readyState !== WebSocket.OPEN ||
      this.state.sessionState !== "ready"
    ) {
      return;
    }

    try {
      const audioFrame = encodeAudioFrame(pcmChunk, 16000, 1);
      this.ws.send(audioFrame as unknown as ArrayBufferView<ArrayBuffer>);

      this.updateState({
        metrics: {
          ...this.state.metrics,
          audioFramesOut: this.state.metrics.audioFramesOut + 1,
          audioBytesOut: this.state.metrics.audioBytesOut + audioFrame.length,
        },
      });
    } catch {
      // Ignore audio write errors
    }
  }

  private handleSocketMessage(event: MessageEvent): void {
    // binaryType is "arraybuffer", so binary frames are always ArrayBuffers.
    if (!(event.data instanceof ArrayBuffer)) return;

    let decoded: DecodedFrame | null = null;
    try {
      decoded = decodeBinaryFrame(new Uint8Array(event.data));
    } catch {
      return;
    }

    if (!decoded) return;

    if (decoded.kind === "audio") {
      this.handleIncomingAudio(decoded.audio, decoded.sampleRate);
    } else if (decoded.kind === "message") {
      this.handleIncomingMessage(decoded.data, decoded.parsed);
    }
  }

  private handleIncomingAudio(samples: Uint8Array, sampleRate: number): void {
    // Track frame rate & count
    const now = Date.now();
    this.recentAudioFramesInTimestamps.push(now);
    // Keep timestamps within last 1000ms
    const oneSecAgo = now - 1000;
    this.recentAudioFramesInTimestamps =
      this.recentAudioFramesInTimestamps.filter((t) => t > oneSecAgo);

    const frameRate = this.recentAudioFramesInTimestamps.length;

    this.updateState({
      metrics: {
        ...this.state.metrics,
        audioFramesIn: this.state.metrics.audioFramesIn + 1,
        audioBytesIn: this.state.metrics.audioBytesIn + samples.length,
        currentFrameRate: frameRate,
      },
    });

    // Play chunk through audio controller
    this.audioController.playAudioChunk(samples, sampleRate);
  }

  private handleIncomingMessage(
    rawJson: string,
    parsed?: RTVIMessage,
  ): void {
    if (!parsed) return;

    this.recordEvent("in", parsed.type, rawJson, parsed.data);

    this.updateState({
      metrics: {
        ...this.state.metrics,
        messageFramesIn: this.state.metrics.messageFramesIn + 1,
      },
    });

    switch (parsed.type) {
      case RTVI_TYPE_BOT_READY: {
        const readyLatency = Date.now() - this.clientReadyTime;
        this.updateState({
          sessionState: "ready",
          metrics: {
            ...this.state.metrics,
            readyLatencyMs: readyLatency,
          },
        });
        // A reconnect starts an unmuted server session; restate a mute the UI still shows.
        if (this.state.isMuted) {
          this.sendRTVI(RTVI_TYPE_SET_MUTED, encodeSetMutedFrame(true), { muted: true });
        }
        break;
      }

      case RTVI_TYPE_USER_STARTED_SPEAKING: {
        this.userSpeechStartTime = Date.now();

        // Check barge-in: If user speaks while bot was speaking, trigger barge-in!
        const wasBotSpeaking = this.state.isBotSpeaking;
        if (wasBotSpeaking) {
          this.triggerBargeIn();
        }

        this.updateState({
          isUserSpeaking: true,
        });
        break;
      }

      case RTVI_TYPE_USER_STOPPED_SPEAKING: {
        const duration = this.userSpeechStartTime
          ? Date.now() - this.userSpeechStartTime
          : 0;
        this.updateState({
          isUserSpeaking: false,
          metrics: {
            ...this.state.metrics,
            userSpeakingDurationMs: duration,
            totalUserSpeakingMs: this.state.metrics.totalUserSpeakingMs + duration,
          },
        });
        break;
      }

      case RTVI_TYPE_USER_TRANSCRIPTION: {
        const transcriptData = parsed.data as RTVITranscriptionData | undefined;
        if (!transcriptData) break;

        if (transcriptData.final) {
          // Commit final user transcript
          const newItem: TranscriptItem = {
            id: `user-${Date.now()}-${Math.random()}`,
            speaker: "user",
            text: transcriptData.text,
            final: true,
            timestamp: Date.now(),
          };
          this.updateState({
            currentInterimTranscript: "",
            transcripts: [...this.state.transcripts, newItem],
          });
        } else {
          // Update live interim transcript
          this.updateState({
            currentInterimTranscript: transcriptData.text,
          });
        }
        break;
      }

      case RTVI_TYPE_BOT_LLM_TEXT: {
        const textData = parsed.data as RTVIBotTextData | undefined;
        if (textData?.text) {
          const updated = this.state.currentBotStreamingText + textData.text;
          this.updateState({
            currentBotStreamingText: updated,
          });
        }
        break;
      }

      case RTVI_TYPE_BOT_STARTED_SPEAKING: {
        this.botSpeechStartTime = Date.now();
        this.updateState({
          isBotSpeaking: true,
        });
        break;
      }

      case RTVI_TYPE_BOT_STOPPED_SPEAKING: {
        const duration = this.botSpeechStartTime
          ? Date.now() - this.botSpeechStartTime
          : 0;

        // When bot finishes speaking, finalize any accumulated streaming assistant text
        let newTranscripts = this.state.transcripts;
        const currentText = this.state.currentBotStreamingText.trim();
        if (currentText) {
          newTranscripts = [
            ...newTranscripts,
            {
              id: `bot-${Date.now()}-${Math.random()}`,
              speaker: "bot",
              text: currentText,
              final: true,
              timestamp: Date.now(),
            },
          ];
        }

        this.updateState({
          isBotSpeaking: false,
          currentBotStreamingText: "",
          transcripts: newTranscripts,
          metrics: {
            ...this.state.metrics,
            botSpeakingDurationMs: duration,
            totalBotSpeakingMs: this.state.metrics.totalBotSpeakingMs + duration,
          },
        });
        break;
      }

      case RTVI_TYPE_ERROR: {
        const errData = parsed.data as RTVIErrorData | undefined;
        const errMsg = errData?.error ?? "Unknown server error";
        const isFatal = errData?.fatal ?? false;

        this.updateState({
          error: {
            category: "server",
            message: errMsg,
            fatal: isFatal,
          },
          ...(isFatal ? { sessionState: "error" } : {}),
        });

        if (isFatal) {
          this.teardownResources();
        }
        break;
      }

      case RTVI_TYPE_DISCONNECT: {
        this.teardownResources();
        this.updateState({
          sessionState: "disconnected",
          isUserSpeaking: false,
          isBotSpeaking: false,
          isBargeInActive: false,
        });
        break;
      }

      default:
        // Other RTVI or debug events
        break;
    }
  }

  private triggerBargeIn(): void {
    // 1. Immediately cut local audio playback
    this.audioController.stopPlayback();

    // 2. Increment barge-in metrics and set barge-in flag
    this.updateState({
      isBargeInActive: true,
      metrics: {
        ...this.state.metrics,
        bargeInCount: this.state.metrics.bargeInCount + 1,
      },
    });

    // 3. Clear barge-in pulse indicator after 1.5 seconds
    if (this.bargeInResetTimer) clearTimeout(this.bargeInResetTimer);
    this.bargeInResetTimer = setTimeout(() => {
      this.updateState({ isBargeInActive: false });
    }, 1500);
  }

  private handleSocketClose(event: CloseEvent): void {
    this.teardownResources();
    if (
      this.state.sessionState !== "disconnected" &&
      this.state.sessionState !== "error"
    ) {
      if (event.code === 1000) {
        // Normal closure
        this.updateState({ sessionState: "disconnected" });
      } else {
        // Abnormal closure
        this.updateState({
          sessionState: "error",
          error: {
            category: "connection",
            message: `Connection closed unexpectedly (code: ${event.code}${event.reason ? `, ${event.reason}` : ""})`,
            fatal: true,
          },
        });
      }
    }
  }

  private teardownResources(): void {
    if (this.bargeInResetTimer) {
      clearTimeout(this.bargeInResetTimer);
      this.bargeInResetTimer = null;
    }
    this.audioController.destroy();
    if (this.ws) {
      try {
        this.ws.onopen = null;
        this.ws.onmessage = null;
        this.ws.onerror = null;
        this.ws.onclose = null;
        this.ws.close();
      } catch {
        // Ignore
      }
      this.ws = null;
    }
  }

  private recordEvent(
    direction: "in" | "out",
    type: string,
    raw: string,
    data?: unknown,
  ): void {
    const eventItem: RTVIDebugEvent = {
      id: `evt-${Date.now()}-${Math.floor(Math.random() * 10000)}`,
      timestamp: Date.now(),
      direction,
      type,
      label: "rtvi-ai",
      raw,
      data,
    };
    // Keep max 200 events in memory
    const updated = [eventItem, ...this.state.events].slice(0, 200);
    this.updateState({ events: updated });
  }

  public destroy(): void {
    this.listeners.clear();
    this.teardownResources();
  }
}
