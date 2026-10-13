import { describe, expect, it } from "vitest";
import type { TraceSpanDTO } from "../../../api/client";
import { deriveView } from "./activity";
import type { DraftRoute } from "./model";

const routes: DraftRoute[] = [
  { id: "e1", from: "front", to: "sales", condition: "buy" },
  { id: "e2", from: "sales", to: "front", condition: "back" },
];
const span = (name: string, from: string, to: string, at: string, id = `${name}-${at}`): TraceSpanDTO => ({
  id,
  name: `route.${name}`,
  started_at: at,
  attributes: { from_step: from, to_step: to },
});
const idleVoice = { userSpeaking: false, botSpeaking: false, thinking: false };

describe("deriveView", () => {
  it("starts on the start step", () => {
    const v = deriveView("front", [], routes, idleVoice);
    expect(v.steps.front.state).toBe("active");
    expect(v.steps.sales.state).toBe("idle");
    expect(v.trail.map((t) => t.step)).toEqual(["front"]);
    expect(v.traversed).toEqual([]);
  });

  it("shows the target preparing, then active with the old step visited", () => {
    const preparing = [span("preparing", "front", "sales", "2026-10-11T10:00:01Z")];
    let v = deriveView("front", preparing, routes, idleVoice);
    expect(v.steps.front.state).toBe("active");
    expect(v.steps.sales.state).toBe("preparing");

    const done = [...preparing, span("active", "front", "sales", "2026-10-11T10:00:02Z")];
    v = deriveView("front", done, routes, idleVoice);
    expect(v.steps.front.state).toBe("visited");
    expect(v.steps.sales.state).toBe("active");
    expect(v.traversed).toEqual([{ routeId: "e1", from: "front", to: "sales", at: "2026-10-11T10:00:02Z" }]);
    expect(v.trail.map((t) => t.step)).toEqual(["front", "sales"]);
  });

  it("a failed route leaves the old step active", () => {
    const spans = [
      span("preparing", "front", "sales", "2026-10-11T10:00:01Z"),
      span("failed", "front", "sales", "2026-10-11T10:00:02Z"),
    ];
    const v = deriveView("front", spans, routes, idleVoice);
    expect(v.steps.front.state).toBe("active");
    expect(v.steps.sales.state).toBe("idle");
    expect(v.traversed).toEqual([]);
  });

  it("orders spans by time, not arrival", () => {
    const spans = [
      span("active", "front", "sales", "2026-10-11T10:00:02Z"),
      span("preparing", "front", "sales", "2026-10-11T10:00:01Z"),
    ];
    expect(deriveView("front", spans, routes, idleVoice).steps.sales.state).toBe("active");
  });

  it("follows a ping-pong and a revisited step is active again", () => {
    const spans = [
      span("active", "front", "sales", "2026-10-11T10:00:02Z"),
      span("active", "sales", "front", "2026-10-11T10:00:05Z"),
    ];
    const v = deriveView("front", spans, routes, idleVoice);
    expect(v.steps.front.state).toBe("active");
    expect(v.steps.sales.state).toBe("visited");
    expect(v.trail.map((t) => t.step)).toEqual(["front", "sales", "front"]);
    expect(v.traversed.map((t) => t.routeId)).toEqual(["e1", "e2"]);
  });

  it("puts the live voice activity on the active step only", () => {
    const spans = [span("active", "front", "sales", "2026-10-11T10:00:02Z")];
    expect(deriveView("front", spans, routes, { ...idleVoice, botSpeaking: true }).steps.sales.activity).toBe("speaking");
    expect(deriveView("front", spans, routes, { ...idleVoice, userSpeaking: true }).steps.sales.activity).toBe("listening");
    expect(deriveView("front", spans, routes, { ...idleVoice, thinking: true }).steps.sales.activity).toBe("thinking");
    expect(deriveView("front", spans, routes, { ...idleVoice, botSpeaking: true }).steps.front.activity).toBeNull();
  });

  it("ignores spans it does not understand", () => {
    const spans: TraceSpanDTO[] = [
      { id: "x", name: "turn", started_at: "2026-10-11T10:00:01Z" },
      { id: "y", name: "route.active", started_at: "2026-10-11T10:00:02Z", attributes: "nonsense" },
    ];
    expect(deriveView("front", spans, routes, idleVoice).steps.front.state).toBe("active");
  });
});
