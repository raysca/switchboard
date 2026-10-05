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

  return (
    <section
      aria-label="Pipecat RTVI Debug Panel"
      className="pipecat-debug-panel"
      style={{
        background: "#ffffff",
        border: "1px solid #c9d3d2",
        padding: "20px",
        marginTop: "20px",
      }}
    >
      {/* Top Header & Speaking State Badges */}
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "center",
          flexWrap: "wrap",
          gap: "12px",
          borderBottom: "1px solid #c9d3d2",
          paddingBottom: "16px",
          marginBottom: "16px",
        }}
      >
        <div>
          <h3 style={{ margin: "0 0 4px", fontSize: "1.1rem" }}>
            Pipecat RTVI Debug Panel
          </h3>
          <span style={{ fontSize: "0.82rem", color: "#53666b" }}>
            Wire protocol inspection, speaking telemetry, and audio frame metrics
          </span>
        </div>

        <div style={{ display: "flex", alignItems: "center", gap: "10px", flexWrap: "wrap" }}>
          {/* Connection Status */}
          <Status tone={getStatusTone(connectionStatus)}>
            {connectionStatus.toUpperCase()}
          </Status>

          {/* User Speaking Indicator */}
          <span
            data-testid="indicator-user-speaking"
            style={{
              fontSize: "0.8rem",
              padding: "4px 8px",
              borderRadius: "4px",
              fontWeight: 500,
              background: speakingState.isUserSpeaking ? "#d8efe8" : "#f0f2f2",
              color: speakingState.isUserSpeaking ? "#006356" : "#6f8285",
              border: `1px solid ${speakingState.isUserSpeaking ? "#007e82" : "#c9d3d2"}`,
            }}
          >
            ● User {speakingState.isUserSpeaking ? "Speaking" : "Silent"}
          </span>

          {/* Bot Speaking Indicator */}
          <span
            data-testid="indicator-bot-speaking"
            style={{
              fontSize: "0.8rem",
              padding: "4px 8px",
              borderRadius: "4px",
              fontWeight: 500,
              background: speakingState.isBotSpeaking ? "#d8efe8" : "#f0f2f2",
              color: speakingState.isBotSpeaking ? "#006356" : "#6f8285",
              border: `1px solid ${speakingState.isBotSpeaking ? "#007e82" : "#c9d3d2"}`,
            }}
          >
            ● Bot {speakingState.isBotSpeaking ? "Speaking" : "Silent"}
          </span>

          {/* Barge-in Indicator */}
          <span
            data-testid="indicator-barge-in"
            style={{
              fontSize: "0.8rem",
              padding: "4px 8px",
              borderRadius: "4px",
              fontWeight: 500,
              background: speakingState.isBargeInActive
                ? "#fde8e4"
                : metrics.bargeInCount > 0
                  ? "#fdf2d8"
                  : "#f0f2f2",
              color: speakingState.isBargeInActive
                ? "#a43e30"
                : metrics.bargeInCount > 0
                  ? "#8f5c00"
                  : "#6f8285",
              border: `1px solid ${
                speakingState.isBargeInActive
                  ? "#a43e30"
                  : metrics.bargeInCount > 0
                    ? "#d4a017"
                    : "#c9d3d2"
              }`,
            }}
          >
            ⚡ Barge-ins: {metrics.bargeInCount}
            {speakingState.isBargeInActive && " (ACTIVE)"}
          </span>
        </div>
      </div>

      {/* Telemetry & Metrics Cards */}
      <div
        style={{
          display: "grid",
          gridTemplateColumns: "repeat(auto-fit, minmax(180px, 1fr))",
          gap: "12px",
          marginBottom: "20px",
        }}
      >
        <div
          style={{
            background: "#f7f9f8",
            border: "1px solid #d9e3e1",
            padding: "12px",
          }}
        >
          <div style={{ fontSize: "0.75rem", color: "#53666b", textTransform: "uppercase" }}>
            WS Connect Latency
          </div>
          <div style={{ fontSize: "1.2rem", fontWeight: 600, marginTop: "4px" }}>
            {metrics.connectLatencyMs !== null ? `${metrics.connectLatencyMs} ms` : "—"}
          </div>
        </div>

        <div
          style={{
            background: "#f7f9f8",
            border: "1px solid #d9e3e1",
            padding: "12px",
          }}
        >
          <div style={{ fontSize: "0.75rem", color: "#53666b", textTransform: "uppercase" }}>
            RTVI Ready Latency
          </div>
          <div style={{ fontSize: "1.2rem", fontWeight: 600, marginTop: "4px" }}>
            {metrics.readyLatencyMs !== null ? `${metrics.readyLatencyMs} ms` : "—"}
          </div>
        </div>

        <div
          style={{
            background: "#f7f9f8",
            border: "1px solid #d9e3e1",
            padding: "12px",
          }}
        >
          <div style={{ fontSize: "0.75rem", color: "#53666b", textTransform: "uppercase" }}>
            Audio In (24kHz)
          </div>
          <div style={{ fontSize: "1.2rem", fontWeight: 600, marginTop: "4px" }}>
            {metrics.audioFramesIn} frames
          </div>
          <div style={{ fontSize: "0.75rem", color: "#53666b", marginTop: "2px" }}>
            {(metrics.audioBytesIn / 1024).toFixed(1)} KB ({metrics.currentFrameRate.toFixed(1)} fps)
          </div>
        </div>

        <div
          style={{
            background: "#f7f9f8",
            border: "1px solid #d9e3e1",
            padding: "12px",
          }}
        >
          <div style={{ fontSize: "0.75rem", color: "#53666b", textTransform: "uppercase" }}>
            Audio Out (16kHz)
          </div>
          <div style={{ fontSize: "1.2rem", fontWeight: 600, marginTop: "4px" }}>
            {metrics.audioFramesOut} frames
          </div>
          <div style={{ fontSize: "0.75rem", color: "#53666b", marginTop: "2px" }}>
            {(metrics.audioBytesOut / 1024).toFixed(1)} KB
          </div>
        </div>

        <div
          style={{
            background: "#f7f9f8",
            border: "1px solid #d9e3e1",
            padding: "12px",
          }}
        >
          <div style={{ fontSize: "0.75rem", color: "#53666b", textTransform: "uppercase" }}>
            User Speaking
          </div>
          <div style={{ fontSize: "1.2rem", fontWeight: 600, marginTop: "4px" }}>
            {(metrics.totalUserSpeakingMs / 1000).toFixed(1)}s
          </div>
        </div>

        <div
          style={{
            background: "#f7f9f8",
            border: "1px solid #d9e3e1",
            padding: "12px",
          }}
        >
          <div style={{ fontSize: "0.75rem", color: "#53666b", textTransform: "uppercase" }}>
            Bot Speaking
          </div>
          <div style={{ fontSize: "1.2rem", fontWeight: 600, marginTop: "4px" }}>
            {(metrics.totalBotSpeakingMs / 1000).toFixed(1)}s
          </div>
        </div>
      </div>

      {/* RTVI Events Header & Search */}
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "center",
          marginBottom: "10px",
          gap: "10px",
        }}
      >
        <div style={{ display: "flex", alignItems: "center", gap: "10px" }}>
          <h4 style={{ margin: 0, fontSize: "0.95rem" }}>
            RTVI Events ({filteredEvents.length})
          </h4>
          <input
            type="search"
            placeholder="Filter events…"
            value={filterQuery}
            onChange={(e) => setFilterQuery(e.target.value)}
            style={{
              padding: "4px 8px",
              fontSize: "0.8rem",
              border: "1px solid #c9d3d2",
              width: "160px",
            }}
            aria-label="Filter events"
          />
        </div>

        {onClearEvents && (
          <button
            type="button"
            className="action-button-small"
            onClick={onClearEvents}
            disabled={events.length === 0}
            style={{ fontSize: "0.8rem", padding: "4px 10px" }}
          >
            Clear Log
          </button>
        )}
      </div>

      {/* Events Stream & Message Inspector */}
      <div
        style={{
          display: "grid",
          gridTemplateColumns: selectedEvent ? "1fr 1fr" : "1fr",
          gap: "12px",
          minHeight: "220px",
        }}
      >
        {/* Events List */}
        <div
          style={{
            maxHeight: "320px",
            overflowY: "auto",
            border: "1px solid #c9d3d2",
            background: "#fafbfb",
          }}
          tabIndex={0}
          aria-label="RTVI event log"
        >
          {filteredEvents.length === 0 ? (
            <div style={{ padding: "20px", textAlign: "center", color: "#6f8285", fontSize: "0.85rem" }}>
              {events.length === 0 ? "No RTVI events logged yet." : "No events match the search filter."}
            </div>
          ) : (
            <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
              {filteredEvents.map((evt) => {
                const isSelected = evt.id === selectedEventId;
                const isOut = evt.direction === "out";
                return (
                  <li
                    key={evt.id}
                    onClick={() => setSelectedEventId(isSelected ? null : evt.id)}
                    style={{
                      padding: "8px 12px",
                      borderBottom: "1px solid #e1e7e6",
                      cursor: "pointer",
                      background: isSelected ? "#e7f4f2" : "transparent",
                      display: "flex",
                      justifyContent: "space-between",
                      alignItems: "center",
                      fontSize: "0.83rem",
                    }}
                  >
                    <div style={{ display: "flex", alignItems: "center", gap: "8px" }}>
                      <span
                        style={{
                          fontSize: "0.75rem",
                          fontWeight: 600,
                          padding: "1px 5px",
                          borderRadius: "3px",
                          background: isOut ? "#e0edec" : "#ede7f6",
                          color: isOut ? "#005658" : "#4a148c",
                        }}
                      >
                        {isOut ? "OUT" : "IN"}
                      </span>
                      <code style={{ fontWeight: 600 }}>{evt.type}</code>
                    </div>
                    <span style={{ fontSize: "0.75rem", color: "#6f8285", fontFamily: "monospace" }}>
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

        {/* Selected Event Inspector */}
        {selectedEvent && (
          <div
            style={{
              maxHeight: "320px",
              overflowY: "auto",
              border: "1px solid #c9d3d2",
              background: "#f3f5f4",
              padding: "12px",
            }}
          >
            <div
              style={{
                display: "flex",
                justifyContent: "space-between",
                alignItems: "center",
                marginBottom: "8px",
              }}
            >
              <h5 style={{ margin: 0, fontSize: "0.88rem" }}>
                Message Inspector: <code>{selectedEvent.type}</code>
              </h5>
              <button
                type="button"
                className="action-button-small"
                onClick={() => setSelectedEventId(null)}
                style={{ fontSize: "0.75rem", padding: "2px 6px" }}
              >
                Close
              </button>
            </div>
            <pre
              style={{
                margin: 0,
                fontSize: "0.78rem",
                fontFamily: "IBM Plex Mono, monospace",
                background: "#ffffff",
                padding: "8px",
                border: "1px solid #c9d3d2",
                overflowX: "auto",
                whiteSpace: "pre-wrap",
                wordBreak: "break-all",
              }}
            >
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
