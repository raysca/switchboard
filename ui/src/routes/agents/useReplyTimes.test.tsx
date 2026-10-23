import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import type { TurnDTO } from "../../api/client";
import { ReplyStrip, replyByLine, useReplyTimes } from "./useReplyTimes";

const turn = (n: number, reply: string, latencies: unknown): TurnDTO => ({
  id: `t${n}`,
  session_id: "s1",
  ordinal: n,
  started_at: "2026-10-12T14:00:00Z",
  ended_at: "2026-10-12T14:00:01Z",
  transcript: "hi",
  assistant_text: reply,
  interrupted: false,
  latencies,
});

function Probe({ client, id, live }: { client: { getSessionTurns: (id: string) => Promise<TurnDTO[]> }; id: string | null; live: boolean }) {
  const turns = useReplyTimes(client, id, live);
  return <p data-testid="n">{turns.length}</p>;
}

describe("useReplyTimes", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("asks for nothing without a session", async () => {
    const getSessionTurns = vi.fn().mockResolvedValue([]);
    render(<Probe client={{ getSessionTurns }} id={null} live />);
    await act(() => vi.advanceTimersByTimeAsync(6000));
    expect(getSessionTurns).not.toHaveBeenCalled();
  });

  it("asks every 2 seconds while the call is live, once more when it ends, and then stops", async () => {
    const getSessionTurns = vi.fn().mockResolvedValue([turn(1, "Hello", { voice_to_voice_ms: 900 })]);
    const { rerender } = render(<Probe client={{ getSessionTurns }} id="s1" live />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(getSessionTurns).toHaveBeenCalledTimes(1);
    expect(screen.getByTestId("n")).toHaveTextContent("1");
    await act(() => vi.advanceTimersByTimeAsync(4100));
    expect(getSessionTurns).toHaveBeenCalledTimes(3);
    rerender(<Probe client={{ getSessionTurns }} id="s1" live={false} />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(getSessionTurns).toHaveBeenCalledTimes(4);
    await act(() => vi.advanceTimersByTimeAsync(10000));
    expect(getSessionTurns).toHaveBeenCalledTimes(4);
  });

  it("never overlaps requests and survives a failing one", async () => {
    let release!: (v: TurnDTO[]) => void;
    const getSessionTurns = vi.fn().mockImplementationOnce(() => new Promise<TurnDTO[]>((r) => (release = r))).mockRejectedValue(new Error("boom"));
    render(<Probe client={{ getSessionTurns }} id="s1" live />);
    await act(() => vi.advanceTimersByTimeAsync(6000));
    expect(getSessionTurns).toHaveBeenCalledTimes(1); // the first is still in flight
    await act(async () => release([]));
    await act(() => vi.advanceTimersByTimeAsync(2100));
    expect(getSessionTurns).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId("n")).toHaveTextContent("0");
  });

  it("stops asking when the panel goes away", async () => {
    const getSessionTurns = vi.fn().mockResolvedValue([]);
    const { unmount } = render(<Probe client={{ getSessionTurns }} id="s1" live />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    unmount();
    await act(() => vi.advanceTimersByTimeAsync(10000));
    expect(getSessionTurns).toHaveBeenCalledTimes(1);
  });
});

describe("ReplyStrip", () => {
  it("shows nothing before a reply has been measured", () => {
    const { container } = render(<ReplyStrip turns={[]} />);
    expect(container).toBeEmptyDOMElement();
    const none = render(<ReplyStrip turns={[turn(1, "x", {})]} />);
    expect(none.container).toBeEmptyDOMElement();
  });

  it("shows the last and the median reply from the server's figures", () => {
    render(<ReplyStrip turns={[turn(1, "a", { voice_to_voice_ms: 600 }), turn(2, "b", { voice_to_voice_ms: 1500 }), turn(3, "c", { llm_ttft_ms: 300, tts_ttfb_ms: 200 })]} />);
    expect(screen.getByText("Last reply")).toBeVisible();
    expect(screen.getByText("500 ms")).toBeVisible();
    expect(screen.getByText("Median reply")).toBeVisible();
    expect(screen.getByText("600 ms")).toBeVisible();
  });
});

describe("replyByLine", () => {
  it("matches each bot line to the turn that said it, in order, and leaves unmatched lines alone", () => {
    const turns = [turn(1, "Hello there.", { voice_to_voice_ms: 700 }), turn(2, "Hello there.", { voice_to_voice_ms: 900 }), turn(3, "Different", {})];
    const m = replyByLine(turns, [
      { id: "a", text: "Hello there." },
      { id: "b", text: "Hello there." },
      { id: "c", text: "Not recorded yet" },
      { id: "d", text: "Different" },
    ]);
    expect(m.get("a")).toBe(700);
    expect(m.get("b")).toBe(900);
    expect(m.has("c")).toBe(false);
    expect(m.has("d")).toBe(false); // matched but not measured: no number
  });
});
