import { useEffect, useState } from "react";
import type { ApiClient, TurnDTO } from "../../api/client";
import { formatMs, medianReplyMs, replyMs } from "../sessions/callStats";

/**
 * The turns of a test call as the server recorded them, asked for every 2 seconds while the call is live
 * and once more when it ends. The reply times in them are the server's own measurement (the same figures as
 * Call detail), so they leave out the network. A failed request is skipped; polls never overlap (when the call ends, the one in flight is dropped and a last one is made).
 */
export function useReplyTimes(client: Pick<ApiClient, "getSessionTurns">, sessionId: string | null, live: boolean, pollMs = 2000): TurnDTO[] {
  const [got, setGot] = useState<{ id: string; turns: TurnDTO[] } | null>(null);
  useEffect(() => {
    if (!sessionId) return;
    let stopped = false;
    let inFlight = false;
    const load = async () => {
      if (inFlight || stopped) return;
      inFlight = true;
      try {
        const turns = await client.getSessionTurns?.(sessionId);
        if (!stopped && turns) setGot({ id: sessionId, turns });
      } catch {
        // The next poll tries again; a missing figure is not an error on the call.
      } finally {
        inFlight = false;
      }
    };
    void load();
    if (!live) return () => void (stopped = true);
    const timer = setInterval(() => void load(), pollMs);
    return () => {
      stopped = true;
      clearInterval(timer);
    };
  }, [client, sessionId, live, pollMs]);
  return got && got.id === sessionId ? got.turns : [];
}

/** The last and the median reply of the call so far; nothing until a reply has been measured. */
export function ReplyStrip({ turns }: { turns: TurnDTO[] }) {
  const ordered = [...turns].sort((a, b) => a.ordinal - b.ordinal);
  const last = [...ordered].reverse().map(replyMs).find((v): v is number => v !== null);
  const median = medianReplyMs(turns);
  if (last === undefined || median === null) return null;
  return (
    <dl className="reply-strip" aria-label="Reply times">
      <div>
        <dt>Last reply</dt>
        <dd>{formatMs(last)}</dd>
      </div>
      <div>
        <dt>Median reply</dt>
        <dd>{formatMs(median)}</dd>
      </div>
    </dl>
  );
}

/**
 * Pairs each bot line of the transcript with the reply time of the turn that said it (same words, in order),
 * so a note can sit under it. A line with no matching measured turn has no entry: nothing is guessed. (A stored
 * reply is redacted and the line is the raw streamed text, so a reply with masked content gets no note.)
 */
export function replyByLine(turns: TurnDTO[], lines: { id: string; text: string }[]): Map<string, number> {
  const pool = [...turns].sort((a, b) => a.ordinal - b.ordinal).map((t) => ({ text: t.assistant_text.trim(), ms: replyMs(t), used: false }));
  const out = new Map<string, number>();
  for (const line of lines) {
    const hit = pool.find((p) => !p.used && p.text !== "" && p.text === line.text.trim());
    if (!hit) continue;
    hit.used = true;
    if (hit.ms !== null) out.set(line.id, hit.ms);
  }
  return out;
}
