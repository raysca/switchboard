import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { ApiClient } from "../../../api/client";
import { deriveView } from "./activity";
import { Canvas } from "./Canvas";
import type { GraphDraft } from "./model";

type LiveGraphProps = {
  graph: GraphDraft;
  sessionId: string | null;
  client: ApiClient;
  /** What the voice client hears right now (a finished session passes false for both). */
  userSpeaking: boolean;
  botSpeaking: boolean;
  /** true while the call runs (polls the trace spans); false replays a finished session once. */
  live: boolean;
  pollMs?: number;
};

const HANDOFF_MS = 450;
const ACTIVITY_TEXT = { listening: "listening", thinking: "thinking", speaking: "speaking" } as const;

/**
 * The graph with the call moving through it: the agent holding the call is
 * highlighted, the agent being prepared pulses, left agents dim, and the
 * handoffs taken are drawn. State comes from the server's handoff trace spans;
 * what the active agent is doing comes from the voice client.
 */
export function LiveGraph({ graph, sessionId, client, userSpeaking, botSpeaking, live, pollMs = 500 }: LiveGraphProps) {
  const traces = useQuery({
    queryKey: ["session-traces", sessionId],
    queryFn: () => client.getSessionTraces(sessionId as string),
    enabled: !!sessionId,
    refetchInterval: live ? pollMs : false,
    retry: false,
  });

  // "Thinking" = the caller finished speaking and the agent has not started yet.
  const [thinking, setThinking] = useState(false);
  const wasSpeaking = useRef(false);
  useEffect(() => {
    if (botSpeaking || userSpeaking) setThinking(false);
    else if (wasSpeaking.current) setThinking(true);
    wasSpeaking.current = userSpeaking;
  }, [userSpeaking, botSpeaking]);

  const view = useMemo(
    () => deriveView(graph.entry, traces.data?.spans ?? [], graph.edges, { userSpeaking, botSpeaking, thinking }),
    [graph.entry, graph.edges, traces.data, userSpeaking, botSpeaking, thinking],
  );
  const nameOf = (id: string) => graph.nodes.find((n) => n.id === id)?.name ?? id;
  const preparing = Object.entries(view.nodes).find(([, v]) => v.state === "preparing")?.[0];
  const activity = view.nodes[view.active]?.activity;
  const traversedEdges = view.traversed.flatMap((t) => (t.edgeId ? [t.edgeId] : []));

  // The handoff moment: the edge just taken is marked for a beat so the canvas can draw the cable.
  const [justHanded, setJustHanded] = useState<string | undefined>();
  const seen = useRef<number | null>(null);
  useEffect(() => {
    const count = traversedEdges.length;
    const grew = seen.current !== null && count > seen.current;
    seen.current = count;
    if (!live || !grew) return;
    setJustHanded(traversedEdges[count - 1]);
    const t = setTimeout(() => setJustHanded(undefined), HANDOFF_MS);
    return () => clearTimeout(t);
    // traversedEdges is rebuilt every render; its length is what matters.
  }, [traversedEdges.length, live]);

  return (
    <div className="live-graph">
      <Canvas graph={graph} visual={view.nodes} traversedEdges={traversedEdges} justHandedEdge={justHanded} readOnly height={360} />
      <p role="status" aria-live="polite" className="live-graph__status">
        {nameOf(view.active)} {view.trail.length > 1 ? "is answering" : "is answering the call"}
        {activity ? ` (${ACTIVITY_TEXT[activity as keyof typeof ACTIVITY_TEXT] ?? activity})` : ""}
        {preparing ? ` — preparing ${nameOf(preparing)}` : ""}
      </p>
      <ol aria-label="Path taken" className="live-graph__trail">
        {view.trail.map((t, i) => (
          <li key={`${t.node}-${i}`}>
            {i > 0 && "→ "}
            {nameOf(t.node)}
          </li>
        ))}
      </ol>
    </div>
  );
}
