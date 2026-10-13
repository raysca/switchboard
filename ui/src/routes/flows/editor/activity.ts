import { routeSpan, type TraceSpanDTO } from "../../../api/client";
import type { Activity, StepVisual } from "./Canvas";
import type { DraftRoute } from "./model";

/** What the voice client says the call is doing right now (it is the call's only live signal). */
export type VoiceActivity = { userSpeaking: boolean; botSpeaking: boolean; thinking: boolean };

export type GraphView = {
  steps: Record<string, StepVisual>;
  /** Routes the call has taken, oldest first. */
  traversed: { routeId: string | null; from: string; to: string; at: string }[];
  /** Steps the call has been in, oldest first. */
  trail: { step: string; since: string }[];
  active: string;
};

function activityOf(v: VoiceActivity): Activity {
  if (v.botSpeaking) return "speaking";
  if (v.userSpeaking) return "listening";
  if (v.thinking) return "thinking";
  return null;
}

/**
 * Replays the server's route trace spans (route.preparing, .active, .failed)
 * into which step holds the call. Pure: the same spans always give the same
 * view, so a finished session replays the path it took.
 */
export function deriveView(start: string, allSpans: TraceSpanDTO[], routes: DraftRoute[], voice: VoiceActivity): GraphView {
  const spans = allSpans
    .filter((s) => routeSpan(s) !== null)
    .sort((a, b) => Date.parse(a.started_at) - Date.parse(b.started_at));

  let active = start;
  let preparing: string | null = null;
  const visited = new Set<string>();
  const traversed: GraphView["traversed"] = [];
  const trail: GraphView["trail"] = [{ step: start, since: spans[0]?.started_at ?? "" }];

  for (const s of spans) {
    const r = routeSpan(s);
    if (!r) continue;
    const { from, to } = r;
    switch (r.event) {
      case "preparing":
        preparing = to;
        break;
      case "active":
        visited.add(from);
        active = to;
        preparing = null;
        traversed.push({ routeId: routes.find((e) => e.from === from && e.to === to)?.id ?? null, from, to, at: s.started_at });
        trail.push({ step: to, since: s.started_at });
        break;
      case "failed":
        if (preparing === to) preparing = null;
        break;
    }
  }

  const ids = new Set<string>([start, ...visited, active, ...routes.flatMap((e) => [e.from, e.to])]);
  const steps: GraphView["steps"] = {};
  for (const id of ids) {
    if (id === active) steps[id] = { state: "active", activity: activityOf(voice) };
    else if (id === preparing) steps[id] = { state: "preparing", activity: null };
    else if (visited.has(id)) steps[id] = { state: "visited", activity: null };
    else steps[id] = { state: "idle", activity: null };
  }
  return { steps, traversed, trail, active };
}
