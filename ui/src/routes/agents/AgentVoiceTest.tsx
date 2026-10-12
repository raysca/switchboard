import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router";
import type { ApiClient, AgentDTO } from "../../api/client";
import {
  VoiceClient,
  type VoiceClientState,
} from "../../voice/VoiceClient";
import type { BrowserAudioController } from "../../voice/audio";
import { PipecatDebugPanel } from "../../components/debug/PipecatDebugPanel";
import { ActionButton, Status } from "../../components/primitives";
import { LiveGraph } from "./graph/LiveGraph";
import { fromAgentConfig } from "./graph/model";

export interface AgentVoiceTestProps {
  agent: AgentDTO;
  client: ApiClient;
  keyValue: string | null;
  wsBaseUrl?: string;
  audioController?: BrowserAudioController;
}

export function AgentVoiceTest({
  agent,
  client,
  keyValue,
  wsBaseUrl,
  audioController,
}: AgentVoiceTestProps) {
  const [showDebugPanel, setShowDebugPanel] = useState(false);
  const [textMessage, setTextMessage] = useState("");
  const [pushToTalk, setPushToTalk] = useState(false);
  const voiceClientRef = useRef<VoiceClient | null>(null);

  // Initialize or maintain VoiceClient instance
  if (!voiceClientRef.current) {
    voiceClientRef.current = new VoiceClient(client, audioController);
  }

  // Mirrors the server: only these readiness states may take calls.
  const callable = agent.readiness.state === "ready" || agent.readiness.state === "ready_with_tool_warning";
  const voiceClient = voiceClientRef.current;
  // A call keeps the version it started on: remember it, so a publish during the call neither
  // changes the graph being shown nor passes for a test of the new version.
  const [callVersion, setCallVersion] = useState<{ number: number; config: unknown } | null>(null);
  const [state, setState] = useState<VoiceClientState>(() =>
    voiceClient.getState(),
  );

  useEffect(() => {
    const unsubscribe = voiceClient.subscribe((nextState) => {
      setState(nextState);
    });

    return () => {
      unsubscribe();
      void voiceClient.disconnect();
    };
  }, [voiceClient]);

  const isConnectedOrReady =
    state.sessionState === "connected" || state.sessionState === "ready";
  const isConnecting =
    state.sessionState === "requesting_token" ||
    state.sessionState === "connecting";
  const inCall = isConnectedOrReady || isConnecting;
  const shown = inCall && callVersion ? callVersion : { number: agent.current_version?.number, config: agent.current_version?.config };
  const callIsStale = inCall && callVersion !== null && callVersion.number !== agent.current_version?.number;
  // A graph agent shows the call moving between its nodes while you test it.
  const graph = useMemo(() => {
    const g = fromAgentConfig(shown.config);
    return g.nodes.length > 1 ? g : null;
  }, [shown.config]);

  // Alt-tab while holding: pointerup never arrives, so release on window blur.
  useEffect(() => {
    if (!state.isTalking) return;
    const release = () => voiceClient.setTalking(false);
    window.addEventListener("blur", release);
    return () => window.removeEventListener("blur", release);
  }, [state.isTalking, voiceClient]);

  const handleConnect = async () => {
    setCallVersion({ number: agent.current_version?.number ?? 0, config: agent.current_version?.config });
    await voiceClient.connect({
      agentId: agent.id,
      principal: `agent-test-${agent.id}`,
      wsBaseUrl,
      pushToTalk,
    });
  };

  const handleDisconnect = async () => {
    await voiceClient.disconnect();
  };

  const handleSendText = (e: FormEvent) => {
    e.preventDefault();
    if (voiceClient.sendText(textMessage)) setTextMessage("");
  };

  const handleToggleMute = () => {
    voiceClient.toggleMute();
  };

  const handleRetry = async () => {
    await handleConnect();
  };

  // Determine current active speech status description
  let speechStatusDescription = "Idle";
  if (state.isBargeInActive) {
    speechStatusDescription = "Barge-in: user interrupted assistant!";
  } else if (state.isUserSpeaking) {
    speechStatusDescription = "Listening (user speaking)…";
  } else if (state.isBotSpeaking) {
    speechStatusDescription = "Assistant speaking…";
  } else if (state.sessionState === "ready") {
    speechStatusDescription = "Live call active — speak into your microphone";
  }

  return (
    <section
      aria-labelledby="voice-test-heading"
      className="agent-voice-test-panel"
      style={{
        background: "#ffffff",
        border: "1px solid #c9d3d2",
        padding: "24px",
        marginTop: "24px",
      }}
    >
      {/* Voice Test Header */}
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "flex-start",
          borderBottom: "1px solid #c9d3d2",
          paddingBottom: "16px",
          marginBottom: "20px",
        }}
      >
        <div>
          <h3 id="voice-test-heading" style={{ margin: "0 0 4px", fontSize: "1.2rem" }}>
            Live Voice Test (Pipecat RTVI)
          </h3>
          <p style={{ margin: 0, color: "#53666b", fontSize: "0.88rem" }}>
            Test real-time conversation over the production RTVI WebSocket session path.
          </p>
        </div>

        <div style={{ display: "flex", alignItems: "center", gap: "10px" }}>
          <span style={{ fontSize: "0.82rem", color: "#53666b" }}>
            Agent: <code>{agent.id}</code>
          </span>
          <span style={{ fontSize: "0.82rem", color: "#53666b" }}>
            Readiness:{" "}
            <code data-testid="agent-readiness">
              {agent.readiness.state}
            </code>
          </span>
          <span data-testid="connection-state">
          <Status
            tone={
              state.sessionState === "ready"
                ? "good"
                : state.sessionState === "error"
                  ? "bad"
                  : "neutral"
            }
          >
            {state.sessionState.toUpperCase()}
          </Status>
          </span>
        </div>
      </div>

      {/* Readiness: the server refuses calls to agents that are not callable (409). */}
      {!callable && !isConnectedOrReady && (
        <div
          role="note"
          style={{
            background: "#fff8e6",
            border: "1px solid #e6c875",
            padding: "10px 14px",
            marginBottom: "16px",
            fontSize: "0.85rem",
            color: "#6b5100",
          }}
        >
          <strong>Notice:</strong> This agent is marked as <code>{agent.readiness.state}</code> (
          {agent.readiness.reasons.join(", ") || "configuration incomplete"}). Fix these before starting a voice test; the server will not accept calls to this agent.
        </div>
      )}

      {/* Error Banner with Retry */}
      {state.error && (
        <div
          role="alert"
          style={{
            background: "#fde8e4",
            border: "1px solid #e8a89e",
            padding: "14px",
            marginBottom: "20px",
          }}
        >
          <div style={{ fontWeight: 600, color: "#a43e30", marginBottom: "4px" }}>
            {state.error.category === "mic"
              ? "Microphone Access Denied"
              : state.error.category === "token"
                ? "Token Creation Failed"
                : "Voice Session Error"}
          </div>
          <p style={{ margin: "0 0 10px", fontSize: "0.88rem", color: "#681e14" }}>
            {state.error.message}
          </p>
          <button
            type="button"
            className="action-button-small"
            onClick={handleRetry}
            disabled={!keyValue || !callable}
          >
            Retry Connection
          </button>
        </div>
      )}

      {/* Action Controls Row */}
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "center",
          flexWrap: "wrap",
          gap: "12px",
          marginBottom: "20px",
        }}
      >
        <div style={{ display: "flex", gap: "10px", alignItems: "center" }}>
          <label style={{ fontSize: "0.85rem", display: "flex", gap: "4px", alignItems: "center" }}>
            <input
              type="checkbox"
              checked={pushToTalk}
              disabled={isConnectedOrReady || isConnecting}
              onChange={(e) => setPushToTalk(e.target.checked)}
            />
            Push to talk
          </label>

          {!isConnectedOrReady && !isConnecting && (
            <ActionButton
              type="button"
              onClick={handleConnect}
              disabled={!keyValue || !callable}
              data-testid="start-voice-test-btn"
            >
              Start voice test
            </ActionButton>
          )}

          {isConnecting && (
            <ActionButton type="button" disabled>
              Connecting…
            </ActionButton>
          )}

          {isConnectedOrReady && (
            <>
              <button
                type="button"
                className="action-button-small"
                onClick={handleDisconnect}
                data-testid="disconnect-btn"
                style={{
                  background: "#a43e30",
                  color: "#ffffff",
                  borderColor: "#a43e30",
                }}
              >
                Disconnect
              </button>

              <button
                type="button"
                className="action-button-small"
                onClick={handleToggleMute}
                data-testid="mute-btn"
                style={{
                  background: state.isMuted ? "#fde8e4" : "#ffffff",
                  borderColor: state.isMuted ? "#a43e30" : "#c9d3d2",
                }}
              >
                {state.isMuted ? "Unmute mic" : "Mute mic"}
              </button>

              {state.pushToTalk && (
                <button
                  type="button"
                  className="action-button-small"
                  data-testid="ptt-btn"
                  aria-pressed={state.isTalking}
                  onPointerDown={(e) => {
                    // Keep the press while the pointer drifts off the button.
                    e.currentTarget.setPointerCapture?.(e.pointerId);
                    voiceClient.setTalking(true);
                  }}
                  onPointerUp={() => voiceClient.setTalking(false)}
                  onPointerCancel={() => voiceClient.setTalking(false)}
                  onBlur={() => voiceClient.setTalking(false)}
                  onKeyDown={(e) => {
                    if ((e.key === " " || e.key === "Enter") && !e.repeat) {
                      e.preventDefault();
                      voiceClient.setTalking(true);
                    }
                  }}
                  onKeyUp={(e) => {
                    if (e.key === " " || e.key === "Enter") {
                      e.preventDefault();
                      voiceClient.setTalking(false);
                    }
                  }}
                  style={{
                    background: state.isTalking ? "#e4f3ec" : "#ffffff",
                    borderColor: state.isTalking ? "#2f7a55" : "#c9d3d2",
                  }}
                >
                  {state.isTalking ? "Talking… release to send" : "Hold to talk"}
                </button>
              )}
            </>
          )}

          {!keyValue && (
            <span style={{ fontSize: "0.82rem", color: "#a43e30" }}>
              Enter management key to run voice tests.
            </span>
          )}
        </div>

        <div>
          <button
            type="button"
            className="action-button-small"
            onClick={() => setShowDebugPanel(!showDebugPanel)}
            data-testid="toggle-debug-btn"
          >
            {showDebugPanel ? "Hide Pipecat Debug Panel" : "Show Pipecat Debug Panel"}
          </button>
        </div>
      </div>

      {/* Active Call State & Speaking Badges */}
      {isConnectedOrReady && (
        <div
          style={{
            background: "#f7f9f8",
            border: "1px solid #d9e3e1",
            padding: "12px 16px",
            marginBottom: "20px",
            display: "flex",
            justifyContent: "space-between",
            alignItems: "center",
            flexWrap: "wrap",
            gap: "10px",
          }}
        >
          <div style={{ display: "flex", alignItems: "center", gap: "12px" }}>
            <span
              style={{
                display: "inline-block",
                width: "10px",
                height: "10px",
                borderRadius: "50%",
                background: state.isBargeInActive
                  ? "#a43e30"
                  : state.isUserSpeaking || state.isBotSpeaking
                    ? "#007e82"
                    : "#6f8285",
              }}
            />
            <strong style={{ fontSize: "0.92rem" }}>{speechStatusDescription}</strong>
          </div>

          <div style={{ display: "flex", gap: "8px", fontSize: "0.82rem" }}>
            {state.isMuted && (
              <span style={{ color: "#a43e30", fontWeight: 600 }}>[Mic Muted]</span>
            )}
            {state.metrics.bargeInCount > 0 && (
              <span style={{ color: "#8f5c00" }}>
                Interrupted: {state.metrics.bargeInCount}x
              </span>
            )}
          </div>
        </div>
      )}

      {/* Live Transcript & Assistant Stream Feed */}
      <div
        className="transcript-container"
        style={{
          border: "1px solid #c9d3d2",
          background: "#fafbfb",
          minHeight: "180px",
          maxHeight: "360px",
          overflowY: "auto",
          padding: "16px",
          marginBottom: "20px",
        }}
        tabIndex={0}
        aria-label="Conversation transcripts"
      >
        {state.transcripts.length === 0 &&
        !state.currentInterimTranscript &&
        !state.currentBotStreamingText ? (
          <div
            style={{
              padding: "30px 10px",
              textAlign: "center",
              color: "#53666b",
              fontSize: "0.88rem",
            }}
          >
            {isConnectedOrReady
              ? "Listening… Speak to the agent to start the transcript."
              : "No voice session active. Click \"Start voice test\" to test audio."}
          </div>
        ) : (
          <div style={{ display: "flex", flexDirection: "column", gap: "12px" }}>
            {state.transcripts.map((item) => (
              <div
                key={item.id}
                data-testid={item.speaker === "user" ? "transcript-user" : "transcript-bot"}
                style={{
                  alignSelf: item.speaker === "user" ? "flex-end" : "flex-start",
                  maxWidth: "80%",
                  padding: "10px 14px",
                  borderRadius: "6px",
                  background: item.speaker === "user" ? "#d8efe8" : "#ffffff",
                  border: `1px solid ${item.speaker === "user" ? "#a8dcd1" : "#c9d3d2"}`,
                  fontSize: "0.9rem",
                }}
              >
                <div
                  style={{
                    fontSize: "0.72rem",
                    fontWeight: 600,
                    textTransform: "uppercase",
                    color: item.speaker === "user" ? "#006356" : "#53666b",
                    marginBottom: "4px",
                  }}
                >
                  {item.speaker === "user" ? "You (User)" : agent.name}
                </div>
                <div>{item.text}</div>
              </div>
            ))}

            {/* Live User Interim Transcript */}
            {state.currentInterimTranscript && (
              <div
                style={{
                  alignSelf: "flex-end",
                  maxWidth: "80%",
                  padding: "10px 14px",
                  borderRadius: "6px",
                  background: "#edf7f5",
                  border: "1px dashed #007e82",
                  fontSize: "0.9rem",
                  fontStyle: "italic",
                }}
                data-testid="interim-transcript"
              >
                <div
                  style={{
                    fontSize: "0.72rem",
                    fontWeight: 600,
                    color: "#006356",
                    marginBottom: "4px",
                  }}
                >
                  Listening…
                </div>
                <div>{state.currentInterimTranscript}</div>
              </div>
            )}

            {/* Live Assistant Streaming Response */}
            {state.currentBotStreamingText && (
              <div
                style={{
                  alignSelf: "flex-start",
                  maxWidth: "80%",
                  padding: "10px 14px",
                  borderRadius: "6px",
                  background: "#f7f9f8",
                  border: "1px dashed #7ea39e",
                  fontSize: "0.9rem",
                }}
                data-testid="bot-streaming-text"
              >
                <div
                  style={{
                    fontSize: "0.72rem",
                    fontWeight: 600,
                    color: "#53666b",
                    marginBottom: "4px",
                  }}
                >
                  {agent.name} (streaming…)
                </div>
                <div>{state.currentBotStreamingText}</div>
              </div>
            )}
          </div>
        )}
      </div>

      {/* Typed user turns: sent as RTVI send-text; the server echoes a final user transcription. */}
      <form
        onSubmit={handleSendText}
        aria-label="Send text message"
        style={{ display: "flex", gap: "8px", marginBottom: "20px" }}
      >
        <label htmlFor="voice-test-text" style={{ alignSelf: "center", fontSize: "0.85rem" }}>
          Message
        </label>
        <input
          id="voice-test-text"
          type="text"
          value={textMessage}
          onChange={(e) => setTextMessage(e.target.value)}
          disabled={!isConnectedOrReady}
          placeholder={isConnectedOrReady ? "Type a message to the agent" : "Connect to send text"}
          style={{ flex: 1, padding: "6px 8px", border: "1px solid #c9d3d2" }}
        />
        <button
          type="submit"
          className="action-button-small"
          disabled={!isConnectedOrReady || !textMessage.trim()}
          data-testid="send-text-btn"
        >
          Send text
        </button>
      </form>

      {/* Session Summary & Trace Timeline Link */}
      {state.sessionId && (
        <div
          className="session-summary-card"
          data-testid="session-summary"
          style={{
            background: "#ffffff",
            border: "1px solid #c9d3d2",
            padding: "16px",
            marginBottom: "20px",
            display: "flex",
            justifyContent: "space-between",
            alignItems: "center",
            flexWrap: "wrap",
            gap: "12px",
          }}
        >
          <div>
            <div style={{ fontSize: "0.8rem", color: "#53666b", marginBottom: "2px" }}>
              Voice Session ID
            </div>
            <div style={{ display: "flex", alignItems: "center", gap: "8px" }}>
              <code style={{ fontSize: "0.9rem", fontWeight: 600 }}>{state.sessionId}</code>
              <span style={{ fontSize: "0.78rem", color: "#53666b" }}>
                ({state.transcripts.length} turns, {state.metrics.bargeInCount} barge-ins)
              </span>
            </div>
          </div>

          <div>
            <Link
              to={`/sessions/${state.sessionId}`}
              className="action-button-small"
              data-testid="view-trace-link"
              style={{ textDecoration: "none", display: "inline-block" }}
            >
              View session trace
            </Link>
          </div>
        </div>
      )}

      {callIsStale && (
        <p role="status">
          This call is on v{callVersion?.number}. Disconnect and reconnect to test v{agent.current_version?.number}.
        </p>
      )}

      {graph && (
        <div style={{ marginBottom: "20px" }}>
          <LiveGraph
            graph={graph}
            sessionId={state.sessionId}
            client={client}
            userSpeaking={state.isUserSpeaking}
            botSpeaking={state.isBotSpeaking}
            live={isConnectedOrReady || isConnecting}
          />
        </div>
      )}

      {/* Pipecat Debug Panel (Collapsible) */}
      {showDebugPanel && (
        <PipecatDebugPanel
          events={state.events}
          metrics={state.metrics}
          speakingState={{
            isUserSpeaking: state.isUserSpeaking,
            isBotSpeaking: state.isBotSpeaking,
            isBargeInActive: state.isBargeInActive,
          }}
          connectionStatus={state.sessionState}
          onClearEvents={() => voiceClient.clearLogs()}
        />
      )}
    </section>
  );
}
