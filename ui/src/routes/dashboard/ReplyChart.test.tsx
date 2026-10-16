import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import type { SessionDTO } from "../../api/client";
import { ReplyChart, replyTimes } from "./ReplyChart";

const call = (median_reply_ms: number | null | undefined) => ({ id: String(median_reply_ms), median_reply_ms }) as unknown as SessionDTO;

describe("replyTimes", () => {
  it("drops calls without a reply time and puts the oldest first", () => {
    expect(replyTimes([call(900), call(null), call(undefined), call(400)])).toEqual([400, 900]);
  });
});

describe("ReplyChart", () => {
  it("summarises typical and slowest reply and flags slow bars", () => {
    const { container } = render(<ReplyChart sessions={[call(800), call(1500), call(400)]} />);
    expect(screen.getByRole("img", { name: /last 3 calls: typical 800 ms, slowest 1.5 s/ })).toBeVisible();
    expect(container.querySelectorAll("[data-slow]")).toHaveLength(1);
  });

  it("renders nothing when no call has a reply time", () => {
    const { container } = render(<ReplyChart sessions={[call(null)]} />);
    expect(container).toBeEmptyDOMElement();
  });
});
