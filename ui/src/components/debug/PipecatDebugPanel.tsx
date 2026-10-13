import { useState, useMemo } from "react";
import type { RTVIDebugEvent, VoiceMetrics } from "../../voice/VoiceClient";
import { Status } from "../primitives";

export interface PipecatDebugPanelProps {
  events: RTVIDebugEvent[];
  metrics: VoiceMetrics;
  speakingState: {
    isUserSpeaking: boolean;
    isBotSpeaking: boolean;
    isBargeInActive: boolean;
  };
  connectionStatus: string;
  onClearEvents?: () => void;
}

export function PipecatDebugPanel({
  events,
  metrics,
  speakingState,
  connectionStatus,
  onClearEvents,
}: PipecatDebugPanelProps) {
  const [selectedEventId, setSelectedEventId] = useState<string | null>(null);
  const [filterQuery, setFilterQuery] = useState("");

  const filteredEvents = useMemo(() => {
    if (!filterQuery.trim()) return events;
    const q = filterQuery.toLowerCase();
    return events.filter(
      (e) =>
        e.type.toLowerCase().includes(q) ||
        e.direction.toLowerCase().includes(q) ||
        e.raw.toLowerCase().includes(q),
    );
  }, [events, filterQuery]);

  const selectedEvent = useMemo(
    () => events.find((e) => e.id === selectedEventId) ?? null,
    [events, selectedEventId],
  );

  const getStatusTone = (status: string): "good" | "bad" | "neutral" => {
    switch (status) {
      case "ready":
        return "good";
      case "connected":
      case "connecting":
      case "requesting_token":
        return "neutral";
      case "error":
        return "bad";
      default:
        return "neutral";
    }
  };

  const bargeTone = speakingState.isBargeInActive ? "bad" : metrics.bargeInCount > 0 ? "warn" : "idle";
  const cards: { label: string; value: string; sub?: string }[] = [
    { label: "WS Connect Latency", value: metrics.connectLatencyMs !== null ? `${metrics.connectLatencyMs} ms` : "—" },
    { label: "RTVI Ready Latency", value: metrics.readyLatencyMs !== null ? `${metrics.readyLatencyMs} ms` : "—" },
    {
      label: "Audio In (24kHz)",
      value: `${metrics.audioFramesIn} frames`,
      sub: `${(metrics.audioBytesIn / 1024).toFixed(1)} KB (${metrics.currentFrameRate.toFixed(1)} fps)`,
    },
    {
      label: "Audio Out (16kHz)",
      value: `${metrics.audioFramesOut} frames`,
      sub: `${(metrics.audioBytesOut / 1024).toFixed(1)} KB`,
    },
    { label: "User Speaking", value: `${(metrics.totalUserSpeakingMs / 1000).toFixed(1)}s` },
    { label: "Bot Speaking", value: `${(metrics.totalBotSpeakingMs / 1000).toFixed(1)}s` },
  ];

  return (
    <section aria-label="Pipecat RTVI Debug Panel" className="pipecat-debug-panel dbg">
      <div className="dbg__head">
        <div>
          <h3>Pipecat RTVI Debug Panel</h3>
          <span className="subtext">Wire protocol inspection, speaking telemetry, and audio frame metrics</span>
        </div>

        <div className="dbg__indicators">
          <Status tone={getStatusTone(connectionStatus)}>{connectionStatus.toUpperCase()}</Status>

          <span data-testid="indicator-user-speaking" className="dbg__chip" data-tone={speakingState.isUserSpeaking ? "ok" : "idle"}>
            ● User {speakingState.isUserSpeaking ? "Speaking" : "Silent"}
          </span>

          <span data-testid="indicator-bot-speaking" className="dbg__chip" data-tone={speakingState.isBotSpeaking ? "ok" : "idle"}>
            ● Bot {speakingState.isBotSpeaking ? "Speaking" : "Silent"}
          </span>

          <span data-testid="indicator-barge-in" className="dbg__chip" data-tone={bargeTone}>
            ⚡ Barge-ins: {metrics.bargeInCount}
            {speakingState.isBargeInActive && " (ACTIVE)"}
          </span>
        </div>
      </div>

      <dl className="dbg__cards">
        {cards.map((c) => (
          <div key={c.label}>
            <dt>{c.label}</dt>
            <dd>{c.value}</dd>
            {c.sub && <dd className="dbg__sub">{c.sub}</dd>}
          </div>
        ))}
      </dl>

      <div className="dbg__events-head">
        <div className="row">
          <h4>RTVI Events ({filteredEvents.length})</h4>
          <input type="search" placeholder="Filter events…" value={filterQuery} onChange={(e) => setFilterQuery(e.target.value)} aria-label="Filter events" />
        </div>

        {onClearEvents && (
          <button type="button" className="action-button-small" onClick={onClearEvents} disabled={events.length === 0}>
            Clear Log
          </button>
        )}
      </div>

      <div className={`dbg__stream${selectedEvent ? " dbg__stream--split" : ""}`}>
        <div className="dbg__log" tabIndex={0} aria-label="RTVI event log">
          {filteredEvents.length === 0 ? (
            <p className="dbg__empty">{events.length === 0 ? "No RTVI events logged yet." : "No events match the search filter."}</p>
          ) : (
            <ul>
              {filteredEvents.map((evt) => {
                const isSelected = evt.id === selectedEventId;
                const isOut = evt.direction === "out";
                return (
                  <li key={evt.id} onClick={() => setSelectedEventId(isSelected ? null : evt.id)} className={isSelected ? "is-selected" : undefined}>
                    <div className="row">
                      <span className="dbg__dir" data-dir={isOut ? "out" : "in"}>
                        {isOut ? "OUT" : "IN"}
                      </span>
                      <code>{evt.type}</code>
                    </div>
                    <span className="dbg__time">
                      {new Date(evt.timestamp).toLocaleTimeString([], {
                        hour12: false,
                        hour: "2-digit",
                        minute: "2-digit",
                        second: "2-digit",
                        fractionalSecondDigits: 3,
                      })}
                    </span>
                  </li>
                );
              })}
            </ul>
          )}
        </div>

        {selectedEvent && (
          <div className="dbg__inspector">
            <div className="dbg__inspector-head">
              <h5>
                Message Inspector: <code>{selectedEvent.type}</code>
              </h5>
              <button type="button" className="action-button-small" onClick={() => setSelectedEventId(null)}>
                Close
              </button>
            </div>
            <pre className="code-block">
              {JSON.stringify(
                {
                  id: selectedEvent.id,
                  direction: selectedEvent.direction,
                  type: selectedEvent.type,
                  timestamp: new Date(selectedEvent.timestamp).toISOString(),
                  data: selectedEvent.data,
                  raw: selectedEvent.raw,
                },
                null,
                2,
              )}
            </pre>
          </div>
        )}
      </div>
    </section>
  );
}
