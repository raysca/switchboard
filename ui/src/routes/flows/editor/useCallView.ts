import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { ApiClient } from "../../../api/client";
import { deriveView } from "./activity";
import type { GraphDraft } from "./model";

/** What a voice test tells the page about its call. */
export type CallInfo = { sessionId: string | null; userSpeaking: boolean; botSpeaking: boolean; live: boolean };
export const NO_CALL: CallInfo = { sessionId: null, userSpeaking: false, botSpeaking: false, live: false };

const HANDOFF_MS = 450;

/**
 * Which step holds the call, which it is preparing, and the routes taken,
 * from the server's handoff trace spans plus what the voice client hears.
 */
export function useCallView(graph: GraphDraft, call: CallInfo, client: ApiClient, pollMs = 500) {
  const { sessionId, userSpeaking, botSpeaking, live } = call;
  const traces = useQuery({
    queryKey: ["session-traces", sessionId],
    queryFn: () => client.getSessionTraces(sessionId as string),
    enabled: !!sessionId,
    refetchInterval: live ? pollMs : false,
    retry: false,
  });

  // "Thinking" = the caller finished speaking and the step has not started yet.
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
  const traversedEdges = useMemo(() => view.traversed.flatMap((t) => (t.edgeId ? [t.edgeId] : [])), [view]);

  // The route just taken is marked for a beat so the canvas can draw the cable.
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
    // Only a new route restarts the beat: the array is rebuilt whenever the speaker changes.
  }, [traversedEdges.length, live]);

  return { view, traversedEdges, justHanded };
}
