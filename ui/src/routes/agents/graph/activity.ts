import type { TraceSpanDTO } from "../../../api/client";
import type { Activity, NodeVisual } from "./Canvas";
import type { GraphEdge } from "./model";

/** What the voice client says the call is doing right now (it is the call's only live signal). */
export type VoiceActivity = { userSpeaking: boolean; botSpeaking: boolean; thinking: boolean };

export type GraphView = {
  nodes: Record<string, NodeVisual>;
  /** Handoffs the call has taken, oldest first. */
  traversed: { edgeId: string | null; from: string; to: string; at: string }[];
  /** Nodes the call has been in, oldest first. */
  trail: { node: string; since: string }[];
  active: string;
};

const attr = (span: TraceSpanDTO, key: string): string | null => {
  const a = span.attributes;
  if (a && typeof a === "object" && key in a) {
    const v = (a as Record<string, unknown>)[key];
    return typeof v === "string" ? v : null;
  }
  return null;
};

function activityOf(v: VoiceActivity): Activity {
  if (v.botSpeaking) return "speaking";
  if (v.userSpeaking) return "listening";
  if (v.thinking) return "thinking";
  return null;
}

/**
 * Replays the server's handoff trace spans (handoff.preparing, .active,
 * .failed) into which node holds the call. Pure: the same spans always give
 * the same view, so a finished session replays the path it took.
 */
export function deriveView(entry: string, allSpans: TraceSpanDTO[], edges: GraphEdge[], voice: VoiceActivity): GraphView {
  const spans = allSpans
    .filter((s) => s.name.startsWith("handoff."))
    .sort((a, b) => Date.parse(a.started_at) - Date.parse(b.started_at));

  let active = entry;
  let preparing: string | null = null;
  const visited = new Set<string>();
  const traversed: GraphView["traversed"] = [];
  const trail: GraphView["trail"] = [{ node: entry, since: spans[0]?.started_at ?? "" }];

  for (const s of spans) {
    const from = attr(s, "from_node");
    const to = attr(s, "to_node");
    if (!from || !to) continue;
    switch (s.name) {
      case "handoff.preparing":
        preparing = to;
        break;
      case "handoff.active":
        visited.add(from);
        active = to;
        preparing = null;
        traversed.push({ edgeId: edges.find((e) => e.from === from && e.to === to)?.id ?? null, from, to, at: s.started_at });
        trail.push({ node: to, since: s.started_at });
        break;
      case "handoff.failed":
        if (preparing === to) preparing = null;
        break;
    }
  }

  const ids = new Set<string>([entry, ...visited, active, ...edges.flatMap((e) => [e.from, e.to])]);
  const nodes: GraphView["nodes"] = {};
  for (const id of ids) {
    if (id === active) nodes[id] = { state: "active", activity: activityOf(voice) };
    else if (id === preparing) nodes[id] = { state: "preparing", activity: null };
    else if (visited.has(id)) nodes[id] = { state: "visited", activity: null };
    else nodes[id] = { state: "idle", activity: null };
  }
  return { nodes, traversed, trail, active };
}
