import { useEffect, useId, useRef } from "react";

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
  const id = useId();
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => heading.current?.focus(), []);
  const blocked = problems.length > 0;
  return (
    <div className="dialog-backdrop">
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={id}
        className="dialog"
        onKeyDown={(e) => {
          if (e.key === "Escape") onCancel();
        }}
      >
        <h2 id={id} ref={heading} tabIndex={-1}>
          Publish {flowName.trim() || "this flow"} v{version}?
        </h2>
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
          <section aria-labelledby={`${id}-changes`}>
            <h3 id={`${id}-changes`}>What changes</h3>
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
      </div>
    </div>
  );
}
