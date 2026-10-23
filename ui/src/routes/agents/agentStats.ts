import type { AgentDTO, SessionDTO } from "../../api/client";

type Tone = "ok" | "bad" | "idle";

export function stepCount(a: AgentDTO): number {
  const steps = a.current_version?.config?.steps;
  return Array.isArray(steps) ? steps.length : 1;
}

export function handoffCount(a: AgentDTO): number {
  const handoffs = a.current_version?.config?.handoffs;
  return Array.isArray(handoffs) ? handoffs.length : 0;
}

export function toolCount(a: AgentDTO): number {
  return a.current_version?.mcp_tools?.length ?? 0;
}

/** The status lamp and its words. A disabled flow reads as Disabled whatever the server says. */
export function readiness(a: AgentDTO): { tone: Tone; label: string } {
  if (!a.enabled) return { tone: "idle", label: "Disabled" };
  switch (a.readiness.state) {
    case "ready":
      return { tone: "ok", label: "Ready" };
    case "ready_with_tool_warning":
      return { tone: "ok", label: "Ready (tool warning)" };
    case "needs_tool_review":
      return { tone: "bad", label: "Needs tool review" };
    case "tool_dependency_unavailable":
      return { tone: "bad", label: "Tool unavailable" };
    default:
      return { tone: "bad", label: "Not ready" };
  }
}

/** Mirrors the server: only these readiness states may take calls. */
export function canTest(a: AgentDTO): boolean {
  return a.enabled && (a.readiness.state === "ready" || a.readiness.state === "ready_with_tool_warning");
}

type LiveCalls = { live: number; onOlderVersion: number };

/** A flow's calls in progress, and how many started on a version that is no longer current. */
export function liveCalls(sessions: SessionDTO[], a: AgentDTO): LiveCalls {
  const mine = sessions.filter((s) => s.status === "active" && s.agent_id === a.id);
  const current = a.current_version?.id;
  return { live: mine.length, onOlderVersion: mine.filter((s) => s.agent_version_id !== current).length };
}

const REASON_TEXT: Record<string, string> = {
  smart_turn_unavailable: "Smart end-of-turn needs turn.analyzer.for_agents on this server",
  idle_timeout_too_short: "The silence wait is shorter than the DTMF window",
};

/** A readiness reason in words; a code this screen does not know stays as the server sent it. */
export const reasonText = (code: string): string => REASON_TEXT[code] ?? code;
