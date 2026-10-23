import { useEffect, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router";
import type { ApiClient, AgentDTO, ToolSelection } from "../../api/client";
import {
  VoiceClient,
  type VoiceClientState,
} from "../../voice/VoiceClient";
import type { BrowserAudioController } from "../../voice/audio";
import { isTextOnly } from "./textOnly";
import { PipecatDebugPanel } from "../../components/debug/PipecatDebugPanel";
import { ActionButton, Status } from "../../components/primitives";
import { NO_CALL, type CallInfo } from "./editor/useCallView";
import { formatMs, mmss } from "../sessions/callStats";
import { useNow } from "../sessions/useNow";
import { ReplyStrip, replyByLine, useReplyTimes } from "./useReplyTimes";

export interface AgentVoiceTestProps {
  agent: AgentDTO;
  client: ApiClient;
  keyValue: string | null;
  wsBaseUrl?: string;
  audioController?: BrowserAudioController;
  /** Told whenever the call changes; must be stable (a state setter). */
  onCall?: (call: CallInfo) => void;
  /** The name of the step that holds the call now; each reply is labelled with the step it came from. */
  activeStep?: string;
  /** An unpublished config to run instead of the published version: the call is a draft test. */
  draft?: { config: unknown; mcp_tools?: ToolSelection[] };
  /** Why a call cannot start right now (the draft is not valid); shown as a note. */
  blocked?: string;
}

export function AgentVoiceTest({
  agent,
  client,
  keyValue,
  wsBaseUrl,
  audioController,
  onCall,
  activeStep,
  draft,
  blocked,
}: AgentVoiceTestProps) {
  const [showDebugPanel, setShowDebugPanel] = useState(false);
  const [textMessage, setTextMessage] = useState("");
  const [pushToTalk, setPushToTalk] = useState(false);
  const voiceClientRef = useRef<VoiceClient | null>(null);
  // The clock counts from the moment the call connects.
  const startedAt = useRef<number | null>(null);
  // Which step held the call when each reply first appeared. Written once per line, so earlier lines never change.
  const stepOf = useRef(new Map<string, string>());
  const lastActive = useRef<string | undefined>(undefined);

  // Initialize or maintain VoiceClient instance
  if (!voiceClientRef.current) {
    voiceClientRef.current = new VoiceClient(client, audioController);
  }

  // Mirrors the server: only these readiness states may take calls.
  // A flow whose speech-to-text is `silence` takes typed turns only: no microphone, no push to talk, no mute.
  const textOnly = isTextOnly(draft?.config ?? agent.current_version?.config);
  const callable = agent.readiness.state === "ready" || agent.readiness.state === "ready_with_tool_warning";
  const voiceClient = voiceClientRef.current;
  // A call keeps the version it started on: remember it, so a publish during the call neither
  // changes the flow being shown nor passes for a test of the new version.
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
    state.sessionState === "requesting_microphone" ||
    state.sessionState === "requesting_token" ||
    state.sessionState === "connecting";
  const inCall = isConnectedOrReady || isConnecting;
  const now = useNow(inCall);
  const turns = useReplyTimes(client, state.sessionId, inCall);
  const botLines = state.transcripts.filter((t) => t.speaker !== "user").map((t) => ({ id: t.id, text: t.text }));
  const replyOf = replyByLine(turns, botLines);
  if (inCall && startedAt.current === null) startedAt.current = Date.now();
  if (!inCall) startedAt.current = null;
  const clock = mmss(inCall && startedAt.current !== null ? (now - startedAt.current) / 1000 : 0);
  for (const item of state.transcripts) if (item.speaker !== "user" && !stepOf.current.has(item.id)) stepOf.current.set(item.id, activeStep || agent.name);
  // The step a route leads to speaks its greeting before the server reports it active, so that greeting was first
  // labelled with the old step. Once the step changes, the lines after the old step's one goodbye (the first bot line
  // after the caller's last message) are the new step's. Without a caller turn there is nothing to tell them apart.
  if (activeStep && lastActive.current && activeStep !== lastActive.current) {
    const lastUser = state.transcripts.reduce((at, t, i) => (t.speaker === "user" ? i : at), -1);
    if (lastUser >= 0) {
      for (const item of state.transcripts.slice(lastUser + 1).filter((t) => t.speaker !== "user").slice(1)) {
        if (stepOf.current.get(item.id) === lastActive.current) stepOf.current.set(item.id, activeStep);
      }
    }
  }
  if (activeStep) lastActive.current = activeStep;
  useEffect(() => {
    onCall?.({ sessionId: state.sessionId, userSpeaking: state.isUserSpeaking, botSpeaking: state.isBotSpeaking, live: inCall });
  }, [onCall, state.sessionId, state.isUserSpeaking, state.isBotSpeaking, inCall]);
  useEffect(() => () => onCall?.(NO_CALL), [onCall]);
  const callIsStale = !draft && inCall && callVersion !== null && callVersion.number !== agent.current_version?.number;

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
      pushToTalk: pushToTalk && !textOnly,
      draft,
      textOnly,
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
    speechStatusDescription = textOnly ? "Text-only call: type a message below" : "Live call active — speak into your microphone";
  }


  return (
    <section aria-label="Voice test" className="call-panel">
      <div className="call-panel__top">
        <div className="call-panel__title">
          <span id="voice-test-heading">
            {draft ? "Testing draft (based on " : "Testing "}v{(inCall && callVersion ? callVersion.number : agent.current_version?.number) ?? 0}{draft ? ")" : ""}
          </span>
          <code data-testid="flow-readiness" className="call-panel__ready">
            {agent.readiness.state}
          </code>
          <span className="grow" />
          <span className="call-panel__clock" data-testid="call-clock">
            {clock}
          </span>
        </div>
        <p className="call-panel__state">
          <span data-testid="connection-state">
            <Status tone={state.sessionState === "ready" ? "good" : state.sessionState === "error" ? "bad" : "neutral"}>{state.sessionState.toUpperCase()}</Status>
          </span>
          {isConnectedOrReady && <span aria-live="polite">{speechStatusDescription}</span>}
          {state.isMuted && <span className="flag flag--bad">[Mic Muted]</span>}
          {state.metrics.bargeInCount > 0 && <span className="flag">Interrupted: {state.metrics.bargeInCount}x</span>}
        </p>
        <div className="call-panel__controls">
          {!textOnly && (
            <label className="stage__ptt">
              <input type="checkbox" checked={pushToTalk} disabled={isConnectedOrReady || isConnecting} onChange={(e) => setPushToTalk(e.target.checked)} />
              Push to talk
            </label>
          )}
          {!isConnectedOrReady && !isConnecting && (
            <ActionButton type="button" onClick={handleConnect} disabled={!keyValue || !callable || !!blocked} data-testid="start-voice-test-btn">
              {textOnly ? "Start text test" : "Start voice test"}
            </ActionButton>
          )}
          {isConnecting && (
            <ActionButton type="button" disabled>
              Connecting…
            </ActionButton>
          )}
          {isConnectedOrReady && (
            <>
              {!textOnly && (
                <button type="button" className="action-button-small" onClick={handleToggleMute} data-testid="mute-btn" aria-pressed={state.isMuted}>
                  {state.isMuted ? "Unmute" : "Mute"}
                </button>
              )}
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
              <span className="grow" />
              <button type="button" className="action-button-small action-button-danger" onClick={handleDisconnect} data-testid="disconnect-btn">
                End call
              </button>
            </>
          )}
          {!keyValue && <span className="flag flag--bad">Enter management key to run voice tests.</span>}
        </div>
      </div>

      {blocked && (
        <p role="note" className="note">
          {blocked}
        </p>
      )}

      {!callable && !isConnectedOrReady && (
        <div role="note" className="note">
          <strong>Notice:</strong> This flow is marked as <code>{agent.readiness.state}</code> ({agent.readiness.reasons.join(", ") || "configuration incomplete"}). Fix these before starting a voice test; the server will not accept calls to this flow.
        </div>
      )}

      {state.error && (
        <div role="alert" className="note note--bad">
          <strong>
            {state.error.category === "mic" ? "Microphone Access Denied" : state.error.category === "token" ? "Token Creation Failed" : "Voice Session Error"}
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

      <ReplyStrip turns={turns} />

      <div className="transcript" tabIndex={0} aria-label="Conversation transcripts">
        {state.transcripts.length === 0 && !state.currentInterimTranscript && !state.currentBotStreamingText ? (
          <p className="transcript__empty">
            {textOnly
              ? isConnectedOrReady
                ? "Type a message below to start the conversation."
                : "No text session active. Click \"Start text test\" to talk to this flow."
              : isConnectedOrReady
                ? "Listening… Speak to start the transcript."
                : "No voice session active. Click \"Start voice test\" to test audio."}
          </p>
        ) : (
          <div className="transcript__lines">
            {state.transcripts.map((item) => (
              <div key={item.id} data-testid={item.speaker === "user" ? "transcript-user" : "transcript-bot"} className={`line line--${item.speaker === "user" ? "caller" : "bot"}`}>
                <div className="line__who">{item.speaker === "user" ? "You" : (stepOf.current.get(item.id) ?? agent.name)}</div>
                <div>{item.text}</div>
                {replyOf.has(item.id) && <div className="line__note">Replied in {formatMs(replyOf.get(item.id)!)}</div>}
              </div>
            ))}
            {state.currentInterimTranscript && (
              <div className="line line--caller line--live" data-testid="interim-transcript">
                <div className="line__who">Listening…</div>
                <div>{state.currentInterimTranscript}</div>
              </div>
            )}
            {state.currentBotStreamingText && (
              <div className="line line--bot line--live" data-testid="bot-streaming-text">
                <div className="line__who">{activeStep || agent.name}</div>
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
          placeholder={isConnectedOrReady ? "Type a message" : "Connect to send text"}
        />
        <button type="submit" className="action-button-small" disabled={!isConnectedOrReady || !textMessage.trim()} data-testid="send-text-btn">
          Send text
        </button>
      </form>

      {state.sessionId && (
        <div className="session-summary-card" data-testid="session-summary">
          <div>
            <div className="subtext">{inCall ? "Voice session" : "Call ended"}</div>
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
          speakingState={{ isUserSpeaking: state.isUserSpeaking, isBotSpeaking: state.isBotSpeaking, isBargeInActive: state.isBargeInActive }}
          connectionStatus={state.sessionState}
          onClearEvents={() => voiceClient.clearLogs()}
        />
      )}
    </section>
  );
}
