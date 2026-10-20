import { useId, useRef, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { ApiError, type ApiClient } from "../../api/client";
import { ActionButton } from "../../components/primitives";

const MAX_NOTE_BYTES = 4096; // the server counts bytes (server/live_calls.go), not UTF-16 units
const bytes = (s: string) => new TextEncoder().encode(s).length;

/** The error text for a failed request. Never includes the note; only the server's own message for a 400. */
function errorCopy(err: unknown): string {
  if (!(err instanceof ApiError)) return "Could not reach the server. Try again.";
  switch (err.status) {
    case 400: return err.message;
    case 404: return "The call has ended, or it runs on another server instance.";
    case 409: return "The call is still starting. Try again in a moment.";
    case 429: return "Too many requests for this call. Wait a second and try again.";
    default: return err.message;
  }
}

/** Guides a running call: an operator note the agent sees once the call is quiet, or a request to reply now. */
export function LiveCallControls({ client, sessionId, onEnded }: { client: ApiClient; sessionId: string; onEnded: () => void }) {
  const id = useId();
  const box = useRef<HTMLTextAreaElement>(null);
  // Held in component state and closed over: no mutation variables, so the MutationCache never holds the note.
  const [note, setNote] = useState("");
  const [reply, setReply] = useState(false);
  const [outcome, setOutcome] = useState("");
  const [error, setError] = useState("");
  const size = bytes(note.trim());
  const tooLong = size > MAX_NOTE_BYTES;

  const fail = (err: unknown) => {
    setOutcome("");
    setError(errorCopy(err));
    if (err instanceof ApiError && err.status === 404) onEnded();
  };
  const send = useMutation({
    mutationFn: () => client.addSessionNote(sessionId, note.trim(), reply),
    onMutate: () => { setOutcome(""); setError(""); },
    onSuccess: () => { setNote(""); setReply(false); setOutcome("Note sent."); box.current?.focus(); },
    onError: fail,
  });
  const ask = useMutation({
    mutationFn: () => client.runSession(sessionId),
    onMutate: () => { setOutcome(""); setError(""); },
    onSuccess: () => setOutcome("Reply requested."),
    onError: fail,
  });

  return (
    <section aria-labelledby={`${id}-title`} className="call-section call-control">
      <h2 id={`${id}-title`}>Guide this call</h2>
      <p className="subtext">A note reaches the agent as an operator note, never as the caller's words, once the call is quiet. The note itself is not saved; the agent's reply is.</p>
      <form onSubmit={(e) => { e.preventDefault(); if (note.trim() && !tooLong) send.mutate(); }}>
        <label htmlFor={`${id}-note`}>Note for the agent</label>
        <textarea id={`${id}-note`} ref={box} rows={3} aria-describedby={tooLong ? `${id}-size` : undefined} value={note} onChange={(e) => setNote(e.target.value)} />
        {tooLong && <p id={`${id}-size`} role="alert" className="call-control__error">This note is {size} bytes; the limit is {MAX_NOTE_BYTES}. Shorten it (characters outside plain English take several bytes each).</p>}
        <label className="check-row">
          <input type="checkbox" checked={reply} onChange={(e) => setReply(e.target.checked)} /> Reply now
        </label>
        <ActionButton type="submit" disabled={send.isPending || !note.trim() || tooLong}>Send note</ActionButton>{" "}
        <button type="button" className="action-button-small" disabled={ask.isPending} onClick={() => ask.mutate()}>Ask for a reply</button>
      </form>
      <p role="status" aria-live="polite">{outcome}</p>
      {error && <p role="alert" className="call-control__error">{error}</p>}
    </section>
  );
}
