import type { ApiClient } from "../../../api/client";
import { Canvas } from "./Canvas";
import type { GraphDraft } from "./model";
import { useCallView } from "./useCallView";

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

const ACTIVITY_TEXT = { listening: "listening", thinking: "thinking", speaking: "speaking" } as const;

/**
 * The graph with the call moving through it: the agent holding the call is
 * highlighted, the agent being prepared pulses, left agents dim, and the
 * handoffs taken are drawn. State comes from the server's handoff trace spans;
 * what the active agent is doing comes from the voice client.
 */
export function LiveGraph({ graph, sessionId, client, userSpeaking, botSpeaking, live, pollMs = 500 }: LiveGraphProps) {
  const { view, traversedEdges, justHanded } = useCallView(graph, { sessionId, userSpeaking, botSpeaking, live }, client, pollMs);
  const nameOf = (id: string) => graph.nodes.find((n) => n.id === id)?.name ?? id;
  const preparing = Object.entries(view.nodes).find(([, v]) => v.state === "preparing")?.[0];
  const activity = view.nodes[view.active]?.activity;

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
