import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { TurnTimeline } from "./TurnTimeline";
import type { TurnDTO, SessionTracesDTO } from "../../api/client";

describe("TurnTimeline", () => {
  it("renders empty state when no turns exist", () => {
    render(<TurnTimeline turns={[]} />);
    expect(screen.getByText("No turns recorded")).toBeVisible();
    expect(
      screen.getByText("No conversational turns were recorded for this session."),
    ).toBeVisible();
  });

  it("renders conversational turns in chronological order regardless of input array order", () => {
    const turns: TurnDTO[] = [
      {
        id: "turn-2",
        session_id: "sess-1",
        ordinal: 2,
        started_at: "2026-10-05T12:00:10Z",
        ended_at: "2026-10-05T12:00:15Z",
        transcript: "What is my order status?",
        assistant_text: "Your order has been shipped.",
        interrupted: false,
      },
      {
        id: "turn-1",
        session_id: "sess-1",
        ordinal: 1,
        started_at: "2026-10-05T12:00:00Z",
        ended_at: "2026-10-05T12:00:05Z",
        transcript: "Hello there",
        assistant_text: "Hello! How can I help you?",
        interrupted: false,
      },
    ];

    render(<TurnTimeline turns={turns} />);

    const turnHeadings = screen.getAllByRole("heading", { name: /Turn #/i });

    expect(turnHeadings).toHaveLength(2);
    expect(turnHeadings[0]).toHaveTextContent("Turn #1");
    expect(turnHeadings[1]).toHaveTextContent("Turn #2");
  });

  it("displays interruption badge and latency metrics", () => {
    const turns: TurnDTO[] = [
      {
        id: "turn-1",
        session_id: "sess-1",
        ordinal: 1,
        started_at: "2026-10-05T12:00:00Z",
        ended_at: "2026-10-05T12:00:03Z",
        transcript: "Stop talking please",
        assistant_text: "I was just about to...",
        interrupted: true,
        latencies: {
          stt_final_ms: 120,
          llm_ttft_ms: 250,
          tts_ttfb_ms: 90,
          voice_to_voice_ms: 495,
        },
      },
    ];

    render(<TurnTimeline turns={turns} />);

    expect(screen.getByText("Interrupted")).toBeVisible();
    expect(screen.getByText("Heard: 120 ms")).toBeVisible();
    expect(screen.getByText("Model: 250 ms")).toBeVisible();
    expect(screen.getByText("Voice: 90 ms")).toBeVisible();
    expect(screen.getByText("Total: 495 ms")).toBeVisible();
  });

  it("shows no latency bar for a turn with no measurements", () => {
    const turn: TurnDTO = { id: "t", session_id: "s", ordinal: 1, started_at: "2026-10-05T12:00:00Z", ended_at: null, transcript: "hi", assistant_text: "yo", interrupted: false, latencies: {} };
    render(<TurnTimeline turns={[turn]} />);
    expect(screen.queryByLabelText("Latencies breakdown")).toBeNull();
  });

  it("maintains privacy mode compliance with no raw audio and metadata-only placeholders", () => {
    const turns: TurnDTO[] = [
      {
        id: "turn-1",
        session_id: "sess-1",
        ordinal: 1,
        started_at: "2026-10-05T12:00:00Z",
        transcript: "", // Redacted or omitted under metadata-only policy
        assistant_text: "",
        interrupted: false,
      },
    ];

    const { container } = render(<TurnTimeline turns={turns} />);

    // Privacy invariant 1: No <audio> tags rendered in DOM
    expect(container.querySelectorAll("audio")).toHaveLength(0);
    expect(container.querySelectorAll("video")).toHaveLength(0);

    // Privacy invariant 2: Metadata-only sentinel shown instead of leaking or blank space
    expect(screen.getByText("[Metadata only - speech content omitted]")).toBeVisible();
    expect(screen.getByText("[Metadata only - response content omitted]")).toBeVisible();
  });

  it("expands trace tree showing correlated spans, events, and tool calls with arguments and results", async () => {
    const user = userEvent.setup();

    const turns: TurnDTO[] = [
      {
        id: "turn-1",
        session_id: "sess-1",
        ordinal: 1,
        started_at: "2026-10-05T12:00:00Z",
        transcript: "Lookup order 42",
        assistant_text: "Found order 42.",
        interrupted: false,
      },
    ];

    const traces: SessionTracesDTO = {
      session_id: "sess-1",
      spans: [
        {
          id: "span-1",
          session_id: "sess-1",
          turn_id: "turn-1",
          name: "llm.generate",
          started_at: "2026-10-05T12:00:01Z",
          ended_at: "2026-10-05T12:00:02Z",
          attributes: { model: "gemini-2.0-flash" },
        },
      ],
      events: [
        {
          id: "event-1",
          session_id: "sess-1",
          turn_id: "turn-1",
          name: "speech_started",
          occurred_at: "2026-10-05T12:00:00.500Z",
        },
      ],
      tool_calls: [
        {
          id: "tc-1",
          session_id: "sess-1",
          turn_id: "turn-1",
          mcp_server_id: "mcp-orders",
          remote_name: "lookup_order",
          schema_hash: "sh123",
          started_at: "2026-10-05T12:00:01.200Z",
          ended_at: "2026-10-05T12:00:01.800Z",
          status: "completed",
        },
      ],
    };

    render(<TurnTimeline turns={turns} traces={traces} />);

    const toggleButton = screen.getByRole("button", { name: /Show traces/i });
    expect(toggleButton).toBeVisible();

    await user.click(toggleButton);

    // Correlated tool call verified
    expect(screen.getByText("lookup_order")).toBeVisible();
    expect(screen.getByText(/mcp-orders/)).toBeVisible();

    // Correlated span verified
    expect(screen.getByText("llm.generate")).toBeVisible();

    // Correlated event verified
    expect(screen.getByText(/speech_started/)).toBeVisible();

    // Collapse again
    await user.click(screen.getByRole("button", { name: /Hide traces/i }));
    expect(screen.queryByTestId("trace-tree")).not.toBeInTheDocument();
  });
  it("marks a route span with a lamp and says which step took the call", async () => {
    const user = userEvent.setup();
    const turns: TurnDTO[] = [
      { id: "t1", session_id: "s", ordinal: 1, started_at: "2026-10-05T12:00:00Z", transcript: "I want to buy a car", assistant_text: "Connecting you.", interrupted: false },
    ];
    const traces: SessionTracesDTO = {
      session_id: "s",
      events: [],
      tool_calls: [],
      spans: [
        { id: "h1", session_id: "s", turn_id: "t1", name: "handoff.active", started_at: "2026-10-05T12:00:01Z", ended_at: "2026-10-05T12:00:02Z", attributes: { from_step: "front", to_step: "sales" } },
        { id: "o1", session_id: "s", turn_id: "t1", name: "llm.generate", started_at: "2026-10-05T12:00:01Z", ended_at: "2026-10-05T12:00:02Z", attributes: {} },
      ],
    };
    render(<TurnTimeline turns={turns} traces={traces} />);
    await user.click(screen.getByRole("button", { name: /Show traces/i }));

    const route = screen.getByText("Routed to sales").closest("[data-testid='span-item']") as HTMLElement;
    expect(route).toHaveAttribute("data-route", "true");
    expect(route.querySelector(".lamp")).not.toBeNull();
    // Other spans stay as they were.
    expect(screen.getByText("llm.generate").closest("[data-testid='span-item']")).not.toHaveAttribute("data-route");
  });

  it("shows each tool call's outcome in words and tone", async () => {
    const user = userEvent.setup();
    const turns: TurnDTO[] = [
      { id: "t1", session_id: "s", ordinal: 1, started_at: "2026-10-05T12:00:00Z", transcript: "Book me in", assistant_text: "Done.", interrupted: false },
    ];
    const call = (id: string, status: string) => ({ id, session_id: "s", turn_id: "t1", remote_name: `tool_${id}`, schema_hash: "h", started_at: "2026-10-05T12:00:01Z", ended_at: "2026-10-05T12:00:01.200Z", status });
    const traces: SessionTracesDTO = { session_id: "s", spans: [], events: [], tool_calls: [call("a", "succeeded"), call("b", "failed"), call("c", "cancelled")] };
    render(<TurnTimeline turns={turns} traces={traces} />);
    await user.click(screen.getByRole("button", { name: /Show traces/i }));
    expect(screen.getByText("succeeded")).toHaveClass("status--good");
    expect(screen.getByText("failed")).toHaveClass("status--bad");
    expect(screen.getByText("cancelled")).toHaveClass("status--neutral");
  });

  it("shows a failed tool call's error category next to its status", async () => {
    const user = userEvent.setup();
    const turns: TurnDTO[] = [{ id: "t1", session_id: "s", ordinal: 1, started_at: "2026-10-05T12:00:00Z", transcript: "x", assistant_text: "y", interrupted: false }];
    const call = { id: "a", session_id: "s", turn_id: "t1", remote_name: "tool_a", schema_hash: "h", started_at: "2026-10-05T12:00:01Z", ended_at: "2026-10-05T12:00:01.200Z", status: "failed", error_category: "timeout" };
    render(<TurnTimeline turns={turns} traces={{ session_id: "s", spans: [], events: [], tool_calls: [call] }} />);
    await user.click(screen.getByRole("button", { name: /Show traces/i }));
    expect(screen.getByText("failed")).toBeVisible();
    expect(screen.getByText("timeout")).toBeVisible();
  });
});
