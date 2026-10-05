import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PipecatDebugPanel } from "./PipecatDebugPanel";
import type { RTVIDebugEvent, VoiceMetrics } from "../../voice/VoiceClient";

const mockMetrics: VoiceMetrics = {
  connectLatencyMs: 42,
  readyLatencyMs: 85,
  userSpeakingDurationMs: 1200,
  botSpeakingDurationMs: 2500,
  totalUserSpeakingMs: 3400,
  totalBotSpeakingMs: 5100,
  audioFramesIn: 120,
  audioFramesOut: 95,
  audioBytesIn: 115200,
  audioBytesOut: 60800,
  messageFramesIn: 8,
  messageFramesOut: 4,
  bargeInCount: 2,
  currentFrameRate: 50.0,
};

const mockEvents: RTVIDebugEvent[] = [
  {
    id: "evt-1",
    timestamp: Date.now() - 2000,
    direction: "out",
    type: "client-ready",
    label: "rtvi-ai",
    raw: JSON.stringify({ label: "rtvi-ai", type: "client-ready", data: { version: "2.1.0" } }),
    data: { version: "2.1.0" },
  },
  {
    id: "evt-2",
    timestamp: Date.now() - 1900,
    direction: "in",
    type: "bot-ready",
    label: "rtvi-ai",
    raw: JSON.stringify({ label: "rtvi-ai", type: "bot-ready", data: { version: "2.1.0" } }),
    data: { version: "2.1.0" },
  },
  {
    id: "evt-3",
    timestamp: Date.now() - 1000,
    direction: "in",
    type: "user-transcription",
    label: "rtvi-ai",
    raw: JSON.stringify({ label: "rtvi-ai", type: "user-transcription", data: { text: "Hello", final: true } }),
    data: { text: "Hello", final: true },
  },
];

describe("PipecatDebugPanel", () => {
  it("renders connection status, speaking states, and latency metrics", () => {
    render(
      <PipecatDebugPanel
        events={mockEvents}
        metrics={mockMetrics}
        speakingState={{
          isUserSpeaking: true,
          isBotSpeaking: false,
          isBargeInActive: false,
        }}
        connectionStatus="ready"
      />,
    );

    // Connection status
    expect(screen.getByText("READY")).toBeVisible();

    // Speaking indicators
    const userIndicator = screen.getByTestId("indicator-user-speaking");
    expect(userIndicator).toHaveTextContent(/User Speaking/i);

    const botIndicator = screen.getByTestId("indicator-bot-speaking");
    expect(botIndicator).toHaveTextContent(/Bot Silent/i);

    const bargeIndicator = screen.getByTestId("indicator-barge-in");
    expect(bargeIndicator).toHaveTextContent(/Barge-ins: 2/i);

    // Latency metrics
    expect(screen.getByText("42 ms")).toBeVisible();
    expect(screen.getByText("85 ms")).toBeVisible();

    // Audio metrics
    expect(screen.getByText("120 frames")).toBeVisible();
    expect(screen.getByText("95 frames")).toBeVisible();
  });

  it("filters events using search input", async () => {
    const user = userEvent.setup();
    render(
      <PipecatDebugPanel
        events={mockEvents}
        metrics={mockMetrics}
        speakingState={{
          isUserSpeaking: false,
          isBotSpeaking: false,
          isBargeInActive: false,
        }}
        connectionStatus="ready"
      />,
    );

    expect(screen.getByText("client-ready")).toBeVisible();
    expect(screen.getByText("bot-ready")).toBeVisible();
    expect(screen.getByText("user-transcription")).toBeVisible();

    const searchInput = screen.getByLabelText(/filter events/i);
    await user.type(searchInput, "transcription");

    expect(screen.queryByText("client-ready")).toBeNull();
    expect(screen.getByText("user-transcription")).toBeVisible();
  });

  it("inspects event payload when an event is clicked", async () => {
    const user = userEvent.setup();
    render(
      <PipecatDebugPanel
        events={mockEvents}
        metrics={mockMetrics}
        speakingState={{
          isUserSpeaking: false,
          isBotSpeaking: false,
          isBargeInActive: false,
        }}
        connectionStatus="ready"
      />,
    );

    await user.click(screen.getByText("user-transcription"));

    expect(screen.getByText(/Message Inspector/i)).toBeVisible();
    expect(screen.getByText(/Hello/i)).toBeVisible();

    // Close button dismisses inspector
    await user.click(screen.getByRole("button", { name: /close/i }));
    expect(screen.queryByText(/Message Inspector/i)).toBeNull();
  });

  it("calls onClearEvents when clear log button is clicked", async () => {
    const onClear = vi.fn();
    const user = userEvent.setup();
    render(
      <PipecatDebugPanel
        events={mockEvents}
        metrics={mockMetrics}
        speakingState={{
          isUserSpeaking: false,
          isBotSpeaking: false,
          isBargeInActive: false,
        }}
        connectionStatus="ready"
        onClearEvents={onClear}
      />,
    );

    const clearBtn = screen.getByRole("button", { name: /clear log/i });
    await user.click(clearBtn);
    expect(onClear).toHaveBeenCalled();
  });

  it("displays active barge-in indicator when isBargeInActive is true", () => {
    render(
      <PipecatDebugPanel
        events={mockEvents}
        metrics={{ ...mockMetrics, bargeInCount: 3 }}
        speakingState={{
          isUserSpeaking: true,
          isBotSpeaking: false,
          isBargeInActive: true,
        }}
        connectionStatus="ready"
      />,
    );

    const bargeIndicator = screen.getByTestId("indicator-barge-in");
    expect(bargeIndicator).toHaveTextContent(/Barge-ins: 3 \(ACTIVE\)/i);
  });
});
