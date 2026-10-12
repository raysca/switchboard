import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { ApiClient } from "../../../api/client";
import { deriveView } from "./activity";
import type { FlowDraft } from "./model";

/** What a voice test tells the page about its call. */
export type CallInfo = { sessionId: string | null; userSpeaking: boolean; botSpeaking: boolean; live: boolean };
export const NO_CALL: CallInfo = { sessionId: null, userSpeaking: false, botSpeaking: false, live: false };

const ROUTE_MS = 450;

/**
 * Which step holds the call, which it is preparing, and the routes taken,
 * from the server's route trace spans plus what the voice client hears.
 */
export function useCallView(graph: FlowDraft, call: CallInfo, client: ApiClient, pollMs = 500) {
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
    () => deriveView(graph.start, traces.data?.spans ?? [], graph.routes, { userSpeaking, botSpeaking, thinking }),
    [graph.start, graph.routes, traces.data, userSpeaking, botSpeaking, thinking],
  );
  const traversedRoutes = useMemo(() => view.traversed.flatMap((t) => (t.routeId ? [t.routeId] : [])), [view]);

  // The route just taken is marked for a beat so the canvas can draw the cable.
  const [justHanded, setJustHanded] = useState<string | undefined>();
  const seen = useRef<number | null>(null);
  useEffect(() => {
    const count = traversedRoutes.length;
    const grew = seen.current !== null && count > seen.current;
    seen.current = count;
    if (!live || !grew) return;
    setJustHanded(traversedRoutes[count - 1]);
    const t = setTimeout(() => setJustHanded(undefined), ROUTE_MS);
    return () => clearTimeout(t);
    // Only a new route restarts the beat: the array is rebuilt whenever the speaker changes.
  }, [traversedRoutes.length, live]);

  return { view, traversedRoutes, justHanded };
}
