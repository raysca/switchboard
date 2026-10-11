import { describe, expect, it } from "vitest";
import type { TraceSpanDTO } from "../../../api/client";
import { deriveView } from "./activity";
import type { GraphEdge } from "./model";

const edges: GraphEdge[] = [
  { id: "e1", from: "front", to: "sales", condition: "buy" },
  { id: "e2", from: "sales", to: "front", condition: "back" },
];
const span = (name: string, from: string, to: string, at: string, id = `${name}-${at}`): TraceSpanDTO => ({
  id,
  name: `handoff.${name}`,
  started_at: at,
  attributes: { from, to },
});
const idleVoice = { userSpeaking: false, botSpeaking: false, thinking: false };

describe("deriveView", () => {
  it("starts on the entry node", () => {
    const v = deriveView("front", [], edges, idleVoice);
    expect(v.nodes.front.state).toBe("active");
    expect(v.nodes.sales.state).toBe("idle");
    expect(v.trail.map((t) => t.node)).toEqual(["front"]);
    expect(v.traversed).toEqual([]);
  });

  it("shows the target preparing, then active with the old node visited", () => {
    const preparing = [span("preparing", "front", "sales", "2026-10-11T10:00:01Z")];
    let v = deriveView("front", preparing, edges, idleVoice);
    expect(v.nodes.front.state).toBe("active");
    expect(v.nodes.sales.state).toBe("preparing");

    const done = [...preparing, span("active", "front", "sales", "2026-10-11T10:00:02Z")];
    v = deriveView("front", done, edges, idleVoice);
    expect(v.nodes.front.state).toBe("visited");
    expect(v.nodes.sales.state).toBe("active");
    expect(v.traversed).toEqual([{ edgeId: "e1", from: "front", to: "sales", at: "2026-10-11T10:00:02Z" }]);
    expect(v.trail.map((t) => t.node)).toEqual(["front", "sales"]);
  });

  it("a failed handoff leaves the old node active", () => {
    const spans = [
      span("preparing", "front", "sales", "2026-10-11T10:00:01Z"),
      span("failed", "front", "sales", "2026-10-11T10:00:02Z"),
    ];
    const v = deriveView("front", spans, edges, idleVoice);
    expect(v.nodes.front.state).toBe("active");
    expect(v.nodes.sales.state).toBe("idle");
    expect(v.traversed).toEqual([]);
  });

  it("orders spans by time, not arrival", () => {
    const spans = [
      span("active", "front", "sales", "2026-10-11T10:00:02Z"),
      span("preparing", "front", "sales", "2026-10-11T10:00:01Z"),
    ];
    expect(deriveView("front", spans, edges, idleVoice).nodes.sales.state).toBe("active");
  });

  it("follows a ping-pong and a revisited node is active again", () => {
    const spans = [
      span("active", "front", "sales", "2026-10-11T10:00:02Z"),
      span("active", "sales", "front", "2026-10-11T10:00:05Z"),
    ];
    const v = deriveView("front", spans, edges, idleVoice);
    expect(v.nodes.front.state).toBe("active");
    expect(v.nodes.sales.state).toBe("visited");
    expect(v.trail.map((t) => t.node)).toEqual(["front", "sales", "front"]);
    expect(v.traversed.map((t) => t.edgeId)).toEqual(["e1", "e2"]);
  });

  it("puts the live voice activity on the active node only", () => {
    const spans = [span("active", "front", "sales", "2026-10-11T10:00:02Z")];
    expect(deriveView("front", spans, edges, { ...idleVoice, botSpeaking: true }).nodes.sales.activity).toBe("speaking");
    expect(deriveView("front", spans, edges, { ...idleVoice, userSpeaking: true }).nodes.sales.activity).toBe("listening");
    expect(deriveView("front", spans, edges, { ...idleVoice, thinking: true }).nodes.sales.activity).toBe("thinking");
    expect(deriveView("front", spans, edges, { ...idleVoice, botSpeaking: true }).nodes.front.activity).toBeNull();
  });

  it("ignores spans it does not understand", () => {
    const spans: TraceSpanDTO[] = [
      { id: "x", name: "turn", started_at: "2026-10-11T10:00:01Z" },
      { id: "y", name: "handoff.active", started_at: "2026-10-11T10:00:02Z", attributes: "nonsense" },
    ];
    expect(deriveView("front", spans, edges, idleVoice).nodes.front.state).toBe("active");
  });
});
