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
import { NO_CALL, type CallInfo } from "./graph/useCallView";
import { fromAgentConfig } from "./graph/model";

export interface AgentVoiceTestProps {
  agent: AgentDTO;
  client: ApiClient;
  keyValue: string | null;
  wsBaseUrl?: string;
  audioController?: BrowserAudioController;
  /** The page shows the call on its own graph (the flow editor): skip the embedded one. */
  hideGraph?: boolean;
  /** Told whenever the call changes; must be stable (a state setter). */
  onCall?: (call: CallInfo) => void;
}

export function AgentVoiceTest({
  agent,
  client,
  keyValue,
  wsBaseUrl,
  audioController,
  hideGraph,
  onCall,
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
  useEffect(() => {
    onCall?.({ sessionId: state.sessionId, userSpeaking: state.isUserSpeaking, botSpeaking: state.isBotSpeaking, live: inCall });
  }, [onCall, state.sessionId, state.isUserSpeaking, state.isBotSpeaking, inCall]);
  useEffect(() => () => onCall?.(NO_CALL), [onCall]);
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

  const statusTone = state.isBargeInActive ? "bad" : state.isUserSpeaking || state.isBotSpeaking ? "ok" : "idle";

  return (
    <section aria-labelledby="voice-test-heading" className="agent-voice-test-panel stage">
      <div className="stage__head">
        <div>
          <h3 id="voice-test-heading">Live call</h3>
          <p className="subtext">Talk to this agent over the same WebSocket path callers use.</p>
        </div>
        <div className="stage__meta">
          <span>
            Agent: <code>{agent.id}</code>
          </span>
          <span>
            Readiness: <code data-testid="agent-readiness">{agent.readiness.state}</code>
          </span>
          <span data-testid="connection-state">
            <Status tone={state.sessionState === "ready" ? "good" : state.sessionState === "error" ? "bad" : "neutral"}>
              {state.sessionState.toUpperCase()}
            </Status>
          </span>
        </div>
      </div>

      {/* Readiness: the server refuses calls to agents that are not callable (409). */}
      {!callable && !isConnectedOrReady && (
        <div role="note" className="note">
          <strong>Notice:</strong> This agent is marked as <code>{agent.readiness.state}</code> (
          {agent.readiness.reasons.join(", ") || "configuration incomplete"}). Fix these before starting a voice test; the server will not accept calls to this agent.
        </div>
      )}

      {state.error && (
        <div role="alert" className="note note--bad">
          <strong>
            {state.error.category === "mic"
              ? "Microphone Access Denied"
              : state.error.category === "token"
                ? "Token Creation Failed"
                : "Voice Session Error"}
          </strong>
          <p>{state.error.message}</p>
          <button type="button" className="action-button-small" onClick={handleRetry} disabled={!keyValue || !callable}>
            Retry Connection
          </button>
        </div>
      )}

      {callIsStale && (
        <p role="status" className="note">
          This call is on v{callVersion?.number}. Disconnect and reconnect to test v{agent.current_version?.number}.
        </p>
      )}

      {graph && !hideGraph && (
        <div className="stage__graph">
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

      {isConnectedOrReady && (
        <div className="stage__state">
          <p className="stage__status">
            <span className="lamp" data-tone={statusTone} aria-hidden="true" />
            {speechStatusDescription}
          </p>
          <p className="stage__flags">
            {state.isMuted && <span className="flag flag--bad">[Mic Muted]</span>}
            {state.metrics.bargeInCount > 0 && <span className="flag">Interrupted: {state.metrics.bargeInCount}x</span>}
          </p>
        </div>
      )}

      <div className="stage__controls">
        <label className="stage__ptt">
          <input type="checkbox" checked={pushToTalk} disabled={isConnectedOrReady || isConnecting} onChange={(e) => setPushToTalk(e.target.checked)} />
          Push to talk
        </label>

        {!isConnectedOrReady && !isConnecting && (
          <ActionButton type="button" onClick={handleConnect} disabled={!keyValue || !callable} data-testid="start-voice-test-btn">
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
            <button type="button" className="action-button-small action-button-danger" onClick={handleDisconnect} data-testid="disconnect-btn">
              Disconnect
            </button>

            <button type="button" className="action-button-small" onClick={handleToggleMute} data-testid="mute-btn" aria-pressed={state.isMuted}>
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
              >
                {state.isTalking ? "Talking… release to send" : "Hold to talk"}
              </button>
            )}
          </>
        )}

        {!keyValue && <span className="flag flag--bad">Enter management key to run voice tests.</span>}
      </div>

      <div className="transcript" tabIndex={0} aria-label="Conversation transcripts">
        {state.transcripts.length === 0 && !state.currentInterimTranscript && !state.currentBotStreamingText ? (
          <p className="transcript__empty">
            {isConnectedOrReady
              ? "Listening… Speak to the agent to start the transcript."
              : "No voice session active. Click \"Start voice test\" to test audio."}
          </p>
        ) : (
          <div className="transcript__lines">
            {state.transcripts.map((item) => (
              <div key={item.id} data-testid={item.speaker === "user" ? "transcript-user" : "transcript-bot"} className={`line line--${item.speaker === "user" ? "caller" : "agent"}`}>
                <div className="line__who">{item.speaker === "user" ? "You (User)" : agent.name}</div>
                <div>{item.text}</div>
              </div>
            ))}

            {state.currentInterimTranscript && (
              <div className="line line--caller line--live" data-testid="interim-transcript">
                <div className="line__who">Listening…</div>
                <div>{state.currentInterimTranscript}</div>
              </div>
            )}

            {state.currentBotStreamingText && (
              <div className="line line--agent line--live" data-testid="bot-streaming-text">
                <div className="line__who">{agent.name} (streaming…)</div>
                <div>{state.currentBotStreamingText}</div>
              </div>
            )}
          </div>
        )}
      </div>

      {/* Typed user turns: sent as RTVI send-text; the server echoes a final user transcription. */}
      <form onSubmit={handleSendText} aria-label="Send text message" className="stage__send">
        <label htmlFor="voice-test-text">Message</label>
        <input
          id="voice-test-text"
          type="text"
          value={textMessage}
          onChange={(e) => setTextMessage(e.target.value)}
          disabled={!isConnectedOrReady}
          placeholder={isConnectedOrReady ? "Type a message to the agent" : "Connect to send text"}
        />
        <button type="submit" className="action-button-small" disabled={!isConnectedOrReady || !textMessage.trim()} data-testid="send-text-btn">
          Send text
        </button>
      </form>

      {state.sessionId && (
        <div className="session-summary-card" data-testid="session-summary">
          <div>
            <div className="subtext">Voice Session ID</div>
            <div className="session-summary-card__id">
              <code>{state.sessionId}</code>
              <span className="subtext">
                ({state.transcripts.length} turns, {state.metrics.bargeInCount} barge-ins)
              </span>
            </div>
          </div>
          <Link to={`/sessions/${state.sessionId}`} className="action-button-small" data-testid="view-trace-link">
            View session trace
          </Link>
        </div>
      )}

      <div className="stage__debug">
        <button type="button" className="action-button-small" onClick={() => setShowDebugPanel(!showDebugPanel)} data-testid="toggle-debug-btn">
          {showDebugPanel ? "Hide Pipecat Debug Panel" : "Show Pipecat Debug Panel"}
        </button>
      </div>

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
