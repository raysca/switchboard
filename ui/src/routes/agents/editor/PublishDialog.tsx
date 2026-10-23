import { useId } from "react";
import { Modal } from "../../../components/Modal";

export type PublishDialogProps = {
  agentName: string;
  version: number;
  /** The version in use now, and how many calls are running on it. */
  currentVersion?: number;
  callsInProgress?: number;
  /** Steps no path from the start step reaches; they are named instead of ticking the reachability check. */
  unreachable?: string[];
  /** Whether a test call has run on exactly this draft. Absent when the flow cannot be tested yet (never published). */
  tested?: boolean;
  firstVersion: boolean;
  changes: string[];
  problems: string[];
  pending: boolean;
  onConfirm: () => void;
  onCancel: () => void;
};

/** Publishing is the one irreversible step: say what changes and what happens to live calls. */
export function PublishDialog({ agentName, version, currentVersion, callsInProgress = 0, unreachable = [], tested, firstVersion, changes, problems, pending, onConfirm, onCancel }: PublishDialogProps) {
  const changesId = useId();
  const checksId = useId();
  const blocked = problems.length > 0;
  return (
    <Modal title={`Publish ${agentName.trim() || "this flow"} v${version}?`} onCancel={onCancel}>
      <p>
        New calls get v{version} straight away.{" "}
        {callsInProgress > 0 && currentVersion !== undefined ? (
          <>
            <b>
              The {callsInProgress} {callsInProgress === 1 ? "call" : "calls"} on v{currentVersion} right now
            </b>{" "}
            finish on v{currentVersion}: publishing never changes a call in progress.
          </>
        ) : (
          "Calls already in progress finish on the version they started with."
        )}
      </p>
      {blocked ? (
        <div role="alert" className="note note--bad">
          <p>Fix these before publishing:</p>
          <ul className="plain-list">
            {problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </div>
      ) : (
        <>
          {firstVersion ? (
            <p className="subtext">This is the first version of this flow.</p>
          ) : changes.length ? (
            <section aria-labelledby={changesId}>
              <h3 id={changesId}>What changes · {changes.length}</h3>
              <ul className="change-rows">
                {changes.map((c, i) => (
                  <li key={i}>{c}</li>
                ))}
              </ul>
            </section>
          ) : (
            <p className="subtext">No changes since v{version - 1}.</p>
          )}
          <section aria-labelledby={checksId}>
            <h3 id={checksId}>Checks</h3>
            <ul className="change-rows">
              <li>
                <span aria-hidden="true" className="check-mark">
                  ✓
                </span>
                Every step is set up
              </li>
              {unreachable.length === 0 ? (
                <li>
                  <span aria-hidden="true" className="check-mark">
                    ✓
                  </span>
                  Every step is reachable from the start step
                </li>
              ) : (
                <li>
                  <span aria-hidden="true" className="check-mark check-mark--warn">
                    !
                  </span>
                  {unreachable.join(", ")} cannot be reached from the start step.
                </li>
              )}
              {tested !== undefined &&
                (tested ? (
                  <li>
                    <span aria-hidden="true" className="check-mark">
                      ✓
                    </span>
                    Tested with a call on this draft
                  </li>
                ) : (
                  <li>
                    <span aria-hidden="true" className="check-mark check-mark--warn">
                      !
                    </span>
                    Not tested with a call since your last change. You can still publish.
                  </li>
                ))}
            </ul>
          </section>
        </>
      )}
      <div className="row row--end">
        <button type="button" className="action-button-small" onClick={onCancel}>
          Keep editing
        </button>
        <button type="button" className="action-button" disabled={blocked || pending} onClick={onConfirm}>
          {pending ? "Publishing…" : `Publish v${version}`}
        </button>
      </div>
    </Modal>
  );
}
