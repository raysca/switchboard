import type { FlowDTO, SessionDTO } from "../../api/client";

export type Tone = "ok" | "bad" | "idle";

export function stepCount(a: FlowDTO): number {
  const steps = a.current_version?.config?.steps;
  return Array.isArray(steps) ? steps.length : 1;
}

export function routeCount(a: FlowDTO): number {
  const routes = a.current_version?.config?.routes;
  return Array.isArray(routes) ? routes.length : 0;
}

export function toolCount(a: FlowDTO): number {
  return a.current_version?.mcp_tools?.length ?? 0;
}

/** The status lamp and its words. A disabled flow reads as Disabled whatever the server says. */
export function readiness(a: FlowDTO): { tone: Tone; label: string } {
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
export function canTest(a: FlowDTO): boolean {
  return a.enabled && (a.readiness.state === "ready" || a.readiness.state === "ready_with_tool_warning");
}

export type LiveCalls = { live: number; onOlderVersion: number };

/** A flow's calls in progress, and how many started on a version that is no longer current. */
export function liveCalls(sessions: SessionDTO[], a: FlowDTO): LiveCalls {
  const mine = sessions.filter((s) => s.status === "active" && s.flow_id === a.id);
  const current = a.current_version?.id;
  return { live: mine.length, onOlderVersion: mine.filter((s) => s.flow_version_id !== current).length };
}
