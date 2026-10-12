import { useState } from "react";
import { routeSpan, type TurnDTO, type SessionTracesDTO, type TraceSpanDTO, type ToolCallDTO, type TraceEventDTO } from "../../api/client";
import { EmptyState, Status } from "../primitives";

export type TurnTimelineProps = {
  turns: TurnDTO[];
  traces?: SessionTracesDTO | null;
  loading?: boolean;
};

type LatencyData = {
  vad_ms?: number;
  stt_ms?: number;
  llm_ms?: number;
  tts_ms?: number;
  e2e_ms?: number;
  [key: string]: unknown;
};

function parseLatencies(raw: unknown): LatencyData | null {
  if (!raw) return null;
  if (typeof raw === "object") return raw as LatencyData;
  if (typeof raw === "string") {
    try {
      return JSON.parse(raw) as LatencyData;
    } catch {
      return null;
    }
  }
  return null;
}

export function formatDuration(startedAt: string, endedAt?: string | null): string {
  if (!endedAt) return "In progress";
  const ms = Math.max(0, new Date(endedAt).getTime() - new Date(startedAt).getTime());
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

export function TurnTimeline({ turns, traces, loading }: TurnTimelineProps) {
  const [expandedTurns, setExpandedTurns] = useState<Record<string, boolean>>({});

  if (loading) {
    return <p className="loading" role="status">Loading turn timeline…</p>;
  }

  // Sort turns chronologically by ordinal ascending, then started_at
  const sortedTurns = [...turns].sort((a, b) => {
    if (a.ordinal !== b.ordinal) return a.ordinal - b.ordinal;
    return new Date(a.started_at).getTime() - new Date(b.started_at).getTime();
  });

  if (sortedTurns.length === 0) {
    return (
      <EmptyState title="No turns recorded">
        No conversational turns were recorded for this session.
      </EmptyState>
    );
  }

  const toggleTurnTraces = (turnId: string) => {
    setExpandedTurns((prev) => ({ ...prev, [turnId]: !prev[turnId] }));
  };

  return (
    <div className="timeline" aria-label="Turn timeline">
      {sortedTurns.map((turn) => {
        const turnSpans: TraceSpanDTO[] = (traces?.spans ?? []).filter((s) => s.turn_id === turn.id);
        const turnToolCalls: ToolCallDTO[] = (traces?.tool_calls ?? []).filter((t) => t.turn_id === turn.id);
        const turnEvents: TraceEventDTO[] = (traces?.events ?? []).filter((e) => e.turn_id === turn.id);
        const hasTraces = turnSpans.length > 0 || turnToolCalls.length > 0 || turnEvents.length > 0;
        const isExpanded = !!expandedTurns[turn.id];
        const latencies = parseLatencies(turn.latencies);

        return (
          <article
            key={turn.id}
            className="turn-card"
            data-testid={`turn-${turn.ordinal}`}
            aria-labelledby={`turn-heading-${turn.id}`}
          >
            <header className="turn-header">
              <div className="turn-header-left">
                <h3 id={`turn-heading-${turn.id}`} className="turn-number">
                  Turn #{turn.ordinal}
                </h3>
                {turn.interrupted && (
                  <span className="badge badge--bad" role="status">
                    Interrupted
                  </span>
                )}
                <span className="subtext">
                  Duration: {formatDuration(turn.started_at, turn.ended_at)}
                </span>
              </div>
              <time className="subtext" dateTime={turn.started_at}>
                {new Date(turn.started_at).toLocaleTimeString()}
              </time>
            </header>

            <div className="turn-bubbles">
              <div className="turn-bubble turn-bubble--user" data-testid="user-transcript">
                <div className="bubble-speaker">User</div>
                <p className="bubble-text">
                  {turn.transcript && turn.transcript.trim() !== ""
                    ? turn.transcript
                    : "[Metadata only - speech content omitted]"}
                </p>
              </div>

              <div className="turn-bubble turn-bubble--bot" data-testid="bot-response">
                <div className="bubble-speaker">Assistant</div>
                <p className="bubble-text">
                  {turn.assistant_text && turn.assistant_text.trim() !== ""
                    ? turn.assistant_text
                    : "[Metadata only - response content omitted]"}
                </p>
              </div>
            </div>

            {/* Latencies Breakdown */}
            {latencies && (
              <div className="latency-bar" aria-label="Latencies breakdown">
                {(latencies.vad_ms !== undefined || latencies.vad !== undefined) && (
                  <span className="latency-pill">
                    VAD: {latencies.vad_ms ?? (latencies.vad as number)}ms
                  </span>
                )}
                {(latencies.stt_ms !== undefined || latencies.stt !== undefined) && (
                  <span className="latency-pill">
                    STT: {latencies.stt_ms ?? (latencies.stt as number)}ms
                  </span>
                )}
                {(latencies.llm_ms !== undefined || latencies.llm !== undefined) && (
                  <span className="latency-pill">
                    LLM: {latencies.llm_ms ?? (latencies.llm as number)}ms
                  </span>
                )}
                {(latencies.tts_ms !== undefined || latencies.tts !== undefined) && (
                  <span className="latency-pill">
                    TTS: {latencies.tts_ms ?? (latencies.tts as number)}ms
                  </span>
                )}
                {(latencies.e2e_ms !== undefined || latencies.total_ms !== undefined) && (
                  <span className="latency-pill">
                    Total: {latencies.e2e_ms ?? (latencies.total_ms as number)}ms
                  </span>
                )}
              </div>
            )}

            {/* Expandable Trace Tree */}
            {hasTraces && (
              <section className="trace-section" aria-label="Turn trace execution tree">
                <button
                  type="button"
                  className="action-button-small"
                  onClick={() => toggleTurnTraces(turn.id)}
                  aria-expanded={isExpanded}
                >
                  {isExpanded
                    ? "Hide traces"
                    : `Show traces (${turnSpans.length} spans, ${turnToolCalls.length} tool calls, ${turnEvents.length} events)`}
                </button>

                {isExpanded && (
                  <div className="trace-tree" data-testid="trace-tree">
                    {/* Tool Calls */}
                    {turnToolCalls.length > 0 && (
                      <div className="trace-group">
                        <h4 className="trace-heading">
                          Tool Calls ({turnToolCalls.length})
                        </h4>
                        {turnToolCalls.map((tc) => (
                          <div key={tc.id} className="trace-item" data-testid="tool-call-item">
                            <div className="trace-item-header">
                              <span>
                                🔧 <strong>{tc.remote_name}</strong>
                                {tc.mcp_server_id && ` (${tc.mcp_server_id})`}
                              </span>
                              <div className="row">
                                <Status tone={tc.status === "completed" || tc.status === "success" ? "good" : "bad"}>
                                  {tc.status}
                                </Status>
                                <span>{formatDuration(tc.started_at, tc.ended_at)}</span>
                              </div>
                            </div>
                            {tc.arguments_summary && (
                              <div className="subtext">
                                <strong>Args:</strong> <code>{tc.arguments_summary}</code>
                              </div>
                            )}
                            {tc.result_summary && (
                              <div className="subtext">
                                <strong>Result:</strong> <code>{tc.result_summary}</code>
                              </div>
                            )}
                          </div>
                        ))}
                      </div>
                    )}

                    {/* Spans */}
                    {turnSpans.length > 0 && (
                      <div className="trace-group">
                        <h4 className="trace-heading">
                          Trace Spans ({turnSpans.length})
                        </h4>
                        {turnSpans.map((span) => {
                          const r = routeSpan(span);
                          const route = r?.event === "active";
                          return (
                            <div key={span.id} className="trace-item" data-testid="span-item" data-route={route ? "true" : undefined}>
                              <div className="trace-item-header">
                                <span className="row">
                                  {route && <span className="lamp" data-tone="warn" aria-hidden="true" />}
                                  {route ? `Routed to ${r.to}` : span.name}
                                </span>
                                <span>{formatDuration(span.started_at, span.ended_at)}</span>
                              </div>
                              {Boolean(span.attributes) && Object.keys(span.attributes as object).length > 0 && (
                                <pre className="subtext trace-attrs">{JSON.stringify(span.attributes, null, 2)}</pre>
                              )}
                            </div>
                          );
                        })}
                      </div>
                    )}

                    {/* Events */}
                    {turnEvents.length > 0 && (
                      <div className="trace-group">
                        <h4 className="trace-heading">
                          Trace Events ({turnEvents.length})
                        </h4>
                        {turnEvents.map((event) => (
                          <div key={event.id} className="trace-item" data-testid="event-item">
                            <div className="trace-item-header">
                              <span>⏱ {event.name}</span>
                              <time dateTime={event.occurred_at}>
                                {new Date(event.occurred_at).toLocaleTimeString()}
                              </time>
                            </div>
                          </div>
                        ))}
                      </div>
                    )}
                  </div>
                )}
              </section>
            )}
          </article>
        );
      })}
    </div>
  );
}
