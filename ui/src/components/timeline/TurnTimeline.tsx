import { useState } from "react";
import { handoffSpan, type TurnDTO, type SessionTracesDTO, type TraceSpanDTO, type ToolCallDTO, type TraceEventDTO } from "../../api/client";
import { EmptyState, Status } from "../primitives";

export type TurnTimelineProps = {
  turns: TurnDTO[];
  traces?: SessionTracesDTO | null;
  loading?: boolean;
};

type LatencyData = {
  stt_final_ms?: number;
  llm_ttft_ms?: number;
  tts_ttfb_ms?: number;
  voice_to_voice_ms?: number;
};

const PILLS: [keyof LatencyData, string][] = [
  ["stt_final_ms", "Heard"],
  ["llm_ttft_ms", "Model"],
  ["tts_ttfb_ms", "Voice"],
  ["voice_to_voice_ms", "Total"],
];

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

function formatDuration(startedAt: string, endedAt?: string | null): string {
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

            {/* Latencies Breakdown: only the stages measured for this turn */}
            {latencies && PILLS.some(([key]) => typeof latencies[key] === "number") && (
              <div className="latency-bar" aria-label="Latencies breakdown">
                {PILLS.map(([key, label]) =>
                  typeof latencies[key] === "number" ? (
                    <span key={key} className="latency-pill">
                      {label}: {latencies[key]} ms
                    </span>
                  ) : null,
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
                                <Status tone={tc.status === "succeeded" ? "good" : tc.status === "cancelled" ? "neutral" : "bad"}>
                                  {tc.status}
                                </Status>
                                {tc.error_category && <span className="subtext">{tc.error_category}</span>}
                                <span>{formatDuration(tc.started_at, tc.ended_at)}</span>
                              </div>
                            </div>
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
                          const r = handoffSpan(span);
                          const handoff = r?.event === "active";
                          return (
                            <div key={span.id} className="trace-item" data-testid="span-item" data-route={handoff ? "true" : undefined}>
                              <div className="trace-item-header">
                                <span className="row">
                                  {handoff && <span className="lamp" data-tone="warn" aria-hidden="true" />}
                                  {handoff ? `Routed to ${r.to}` : span.name}
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
