import { useId } from "react";
import { Modal } from "../../../components/Modal";

export type PublishDialogProps = {
  flowName: string;
  version: number;
  firstVersion: boolean;
  changes: string[];
  problems: string[];
  pending: boolean;
  onConfirm: () => void;
  onCancel: () => void;
};

/** Publishing is the one irreversible step: say what changes and what happens to live calls. */
export function PublishDialog({ flowName, version, firstVersion, changes, problems, pending, onConfirm, onCancel }: PublishDialogProps) {
  const changesId = useId();
  const blocked = problems.length > 0;
  return (
    <Modal title={`Publish ${flowName.trim() || "this flow"} v${version}?`} onCancel={onCancel}>
      <p>New calls get v{version} straight away. Calls already in progress finish on the version they started with.</p>
      {blocked ? (
        <div role="alert" className="note note--bad">
          <p>Fix these before publishing:</p>
          <ul className="plain-list">
            {problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </div>
      ) : firstVersion ? (
        <p className="subtext">This is the first version of this flow.</p>
      ) : changes.length ? (
        <section aria-labelledby={changesId}>
          <h3 id={changesId}>What changes</h3>
          <ul className="plain-list">
            {changes.map((c, i) => (
              <li key={i}>{c}</li>
            ))}
          </ul>
        </section>
      ) : (
        <p className="subtext">No changes since v{version - 1}.</p>
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
