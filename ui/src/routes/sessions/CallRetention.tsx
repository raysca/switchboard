import { useId, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router";
import { ApiError, type ApiClient, type SessionDTO } from "../../api/client";
import { Modal } from "../../components/Modal";

const MAX_REASON_BYTES = 200; // the server counts bytes (storage.MaxHoldReason)
const bytes = (s: string) => new TextEncoder().encode(s).length;

const errorText = (err: unknown) => (err instanceof ApiError ? err.message : "Could not reach the server. Try again.");

/** Keeps a call out of the retention job's reach (a hold), or erases it for good. */
export function CallRetention({ client, session }: { client: ApiClient; session: SessionDTO }) {
  const id = useId();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [dialog, setDialog] = useState<"hold" | "erase" | null>(null);
  const [error, setError] = useState("");
  const live = session.status !== "ended";
  const short = `call_${session.id.slice(0, 8)}`;

  const changed = (next: SessionDTO) => {
    queryClient.setQueryData(["session", session.id], next);
    void queryClient.invalidateQueries({ queryKey: ["retention"] });
    void queryClient.invalidateQueries({ queryKey: ["sessions"] });
    setDialog(null);
    setError("");
  };
  const hold = useMutation({ mutationFn: (reason: string) => client.holdSession(session.id, reason), onSuccess: changed, onError: (e) => setError(errorText(e)) });
  const release = useMutation({ mutationFn: () => client.releaseSession(session.id), onSuccess: changed, onError: (e) => setError(errorText(e)) });
  const gone = () => {
    queryClient.removeQueries({ queryKey: ["session", session.id] });
    queryClient.removeQueries({ queryKey: ["session-turns", session.id] });
    queryClient.removeQueries({ queryKey: ["session-traces", session.id] });
    void queryClient.invalidateQueries({ queryKey: ["sessions"] });
    void queryClient.invalidateQueries({ queryKey: ["retention"] });
    navigate("/sessions");
  };
  const erase = useMutation({
    mutationFn: () => client.eraseSession(session.id),
    onSuccess: gone,
    // Nothing left to erase (a retry after a recording failed): the job is done.
    onError: (e) => (e instanceof ApiError && e.status === 404 ? gone() : setError(errorText(e))),
  });

  return (
    <section aria-labelledby={`${id}-title`} className="call-section">
      <h2 id={`${id}-title`}>Retention</h2>
      {session.held ? (
        <p>
          <span className="chip">On hold</span> {session.hold_reason || "No reason given."}{" "}
          <span className="subtext">The retention job skips this call, and it cannot be erased until the hold is released.</span>
        </p>
      ) : (
        <p className="subtext">This call follows the retention policy. Hold it to keep it, or erase it now.</p>
      )}
      {error && !dialog && (
        <p role="alert" className="call-control__error">
          {error}
        </p>
      )}
      <div className="row">
        {session.held ? (
          <button type="button" className="action-button-small" disabled={release.isPending} onClick={() => { setError(""); release.mutate(); }}>
            {release.isPending ? "Releasing…" : "Release hold"}
          </button>
        ) : (
          <button type="button" className="action-button-small" onClick={() => { setError(""); setDialog("hold"); }}>
            Hold this call…
          </button>
        )}
        <button type="button" className="action-button-small" disabled={session.held || live} onClick={() => { setError(""); setDialog("erase"); }}>
          Erase this call…
        </button>
      </div>
      {(session.held || live) && <p className="subtext">{live ? "A call can be erased once it has ended." : "Release the hold to erase this call."}</p>}

      {dialog === "hold" && <HoldDialog pending={hold.isPending} error={error} onSubmit={(reason) => hold.mutate(reason)} onCancel={() => { setDialog(null); setError(""); }} />}
      {dialog === "erase" && (
        <EraseDialog short={short} pending={erase.isPending} error={error} onConfirm={() => erase.mutate()} onCancel={() => { setDialog(null); setError(""); }} />
      )}
    </section>
  );
}

function HoldDialog({ pending, error, onSubmit, onCancel }: { pending: boolean; error: string; onSubmit: (reason: string) => void; onCancel: () => void }) {
  const id = useId();
  const [reason, setReason] = useState("");
  const size = bytes(reason.trim());
  const tooLong = size > MAX_REASON_BYTES;
  return (
    <Modal title="Hold this call?" onCancel={onCancel}>
      <form
        className="dialog__form"
        onSubmit={(e) => {
          e.preventDefault();
          if (!tooLong) onSubmit(reason.trim());
        }}
      >
        <p>The retention job will not blank or delete this call while it is on hold, however old it gets. Its recording is kept too, if it has one.</p>
        <label htmlFor={`${id}-reason`}>Reason (optional)</label>
        <textarea id={`${id}-reason`} rows={2} value={reason} onChange={(e) => setReason(e.target.value)} aria-describedby={tooLong ? `${id}-size` : undefined} />
        {tooLong && (
          <p id={`${id}-size`} role="alert" className="call-control__error">
            This reason is {size} bytes; the limit is {MAX_REASON_BYTES}.
          </p>
        )}
        {error && (
          <p role="alert" className="note note--bad">
            {error}
          </p>
        )}
        <div className="row row--end">
          <button type="button" className="action-button-small" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="action-button" disabled={pending || tooLong}>
            {pending ? "Holding…" : "Hold call"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function EraseDialog({ short, pending, error, onConfirm, onCancel }: { short: string; pending: boolean; error: string; onConfirm: () => void; onCancel: () => void }) {
  const id = useId();
  const [typed, setTyped] = useState("");
  return (
    <Modal title={`Erase ${short}?`} onCancel={onCancel}>
      <form
        className="dialog__form"
        onSubmit={(e) => {
          e.preventDefault();
          if (typed === short) onConfirm();
        }}
      >
        <p>This cannot be undone. It removes the call's transcript, turns, tool calls and trace, and its recording on this server. Copies in backups, logs and other systems stay where they are.</p>
        <p className="subtext">A line saying the call was erased is kept in the retention history, without any of its content.</p>
        <label htmlFor={`${id}-confirm`}>
          Type <code>{short}</code> to confirm
        </label>
        <input id={`${id}-confirm`} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
        {error && (
          <p role="alert" className="note note--bad">
            {error}
          </p>
        )}
        <div className="row row--end">
          <button type="button" className="action-button-small" onClick={onCancel}>
            Keep call
          </button>
          <button type="submit" className="action-button action-button-danger" disabled={pending || typed !== short}>
            {pending ? "Erasing…" : "Erase call"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
