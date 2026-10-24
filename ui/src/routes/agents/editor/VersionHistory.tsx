import { useId, useState } from "react";
import type { AgentVersionSummary } from "../../../api/client";

export type VersionHistoryProps = {
  load: () => Promise<AgentVersionSummary[]>;
  /** Publishes that version's content as the next one. A rejection's message is shown. */
  restore: (number: number) => Promise<void>;
};

/** The agent's publishes, newest first, each restorable as a new version (history is never rewritten). */
export function VersionHistory({ load, restore }: VersionHistoryProps) {
  const uid = useId();
  const [versions, setVersions] = useState<AgentVersionSummary[] | null>(null);
  const [open, setOpen] = useState(false);
  const [confirming, setConfirming] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reload = () => load().then(setVersions, (e: unknown) => setError(e instanceof Error ? e.message : "The version history could not be loaded."));
  const toggle = () => {
    const next = !open;
    setOpen(next);
    if (next) {
      setError(null);
      void reload();
    }
  };
  const latest = versions?.[0]?.number ?? 0;

  return (
    <div className="version-history">
      <button type="button" className="action-button-small" aria-expanded={open} aria-controls={`${uid}-list`} onClick={toggle}>
        {open ? "Hide version history" : "Version history"}
      </button>
      {open && (
        <div id={`${uid}-list`}>
          {versions === null && !error && <p className="subtext">Loading versions…</p>}
          {versions && (
            <ol className="version-history__list">
              {versions.map((v) => (
                <li key={v.number}>
                  <span>
                    <b>v{v.number}</b>
                    {v.current && <span className="subtext"> · current</span>}
                    <span className="subtext">
                      {" "}
                      · {new Date(v.created_at).toLocaleString()} · {v.tool_count} {v.tool_count === 1 ? "tool" : "tools"}
                    </span>
                  </span>
                  {!v.current &&
                    (confirming === v.number ? (
                      <div role="group" aria-label="Confirm restore" className="restore-confirm">
                        <p>
                          Publish v{v.number}'s settings as v{latest + 1}? Calls already in progress finish on the version they started with. Changes you have not published here are replaced.
                        </p>
                        <div className="row">
                          <button
                            type="button"
                            className="action-button"
                            disabled={busy}
                            onClick={() => {
                              setError(null);
                              setBusy(true);
                              restore(v.number)
                                .catch((e: unknown) => {
                                  setConfirming(null);
                                  setError(e instanceof Error ? e.message : "That version could not be restored.");
                                  void reload(); // someone else may have published meanwhile
                                })
                                .finally(() => setBusy(false));
                            }}
                          >
                            Restore v{v.number}
                          </button>
                          <button type="button" className="action-button-small" onClick={() => setConfirming(null)}>
                            Cancel
                          </button>
                        </div>
                      </div>
                    ) : (
                      <button type="button" className="action-button-small" onClick={() => setConfirming(v.number)}>
                        Restore v{v.number}…
                      </button>
                    ))}
                </li>
              ))}
            </ol>
          )}
          {error && (
            <p role="alert" className="note note--bad">
              {error}
            </p>
          )}
        </div>
      )}
    </div>
  );
}
