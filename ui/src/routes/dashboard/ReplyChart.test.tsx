import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import type { SessionDTO } from "../../api/client";
import { ReplyChart, replyPoints, replyScale } from "./ReplyChart";

const call = (median_reply_ms: number | null | undefined) => ({ id: String(median_reply_ms), started_at: "2026-10-01T10:00:00Z", median_reply_ms }) as unknown as SessionDTO;

describe("replyPoints", () => {
  it("drops calls without a reply time and puts the oldest first", () => {
    expect(replyPoints([call(900), call(null), call(undefined), call(400)]).map((p) => p.ms)).toEqual([400, 900]);
  });
});

describe("replyScale", () => {
  it("always reaches the slow line and uses at most four round steps", () => {
    expect(replyScale(300)).toEqual({ max: 1000, ticks: [0, 500, 1000] });
    expect(replyScale(1500)).toEqual({ max: 1500, ticks: [0, 500, 1000, 1500] });
    expect(replyScale(7300)).toEqual({ max: 8000, ticks: [0, 2000, 4000, 6000, 8000] });
  });
});

describe("ReplyChart", () => {
  const bars = (c: HTMLElement) => c.querySelectorAll(".reply-chart__bar[data-slow]");

  it("summarises typical and slowest reply and flags slow bars", () => {
    const { container } = render(<ReplyChart sessions={[call(800), call(1500), call(400)]} />);
    expect(screen.getByRole("group", { name: /last 3 calls: typical 800 ms, slowest 1.5 s/ })).toBeVisible();
    expect(bars(container)).toHaveLength(1);
    expect(screen.getByText(/slow: over 1.0 s/)).toBeVisible();
  });

  it("reads a call with the arrow keys and clears on Escape", () => {
    render(<ReplyChart sessions={[call(800), call(1500), call(400)]} />);
    const plot = screen.getByRole("group");
    fireEvent.keyDown(plot, { key: "Home" });
    expect(screen.getByRole("status")).toHaveTextContent("median reply 400 ms");
    fireEvent.keyDown(plot, { key: "ArrowRight" });
    expect(screen.getByRole("status")).toHaveTextContent("median reply 1.5 s, slower than 1.0 s");
    fireEvent.keyDown(plot, { key: "Escape" });
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("switches to a table of the same numbers", () => {
    render(<ReplyChart sessions={[call(800), call(1500)]} />);
    fireEvent.click(screen.getByRole("button", { name: "Show table" }));
    expect(screen.getByRole("table")).toBeVisible();
    expect(screen.getByText("1.5 s")).toBeVisible();
    expect(screen.queryByRole("group")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show chart" }));
    expect(screen.getByRole("group")).toBeVisible();
  });

  it("renders nothing when no call has a reply time", () => {
    const { container } = render(<ReplyChart sessions={[call(null)]} />);
    expect(container).toBeEmptyDOMElement();
  });
});
