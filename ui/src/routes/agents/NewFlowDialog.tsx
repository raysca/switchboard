import { useId, useState, type FormEvent } from "react";
import { Modal } from "../../components/Modal";

export type FlowChoice = { id: string; name: string };

export type NewFlowDialogProps = {
  flows: FlowChoice[];
  pending: boolean;
  error: string | null;
  onBlank: (name: string) => void;
  onCopy: (sourceId: string, name: string) => void;
  onCancel: () => void;
};

/** Name a flow, then start from nothing or from a copy of an existing one. */
export function NewFlowDialog({ flows, pending, error, onBlank, onCopy, onCancel }: NewFlowDialogProps) {
  const uid = useId();
  const [name, setName] = useState("");
  const [mode, setMode] = useState<"blank" | "copy">("blank");
  const [source, setSource] = useState(flows[0]?.id ?? "");
  const copying = mode === "copy" && !!source;
  const ready = name.trim() !== "" && !pending;

  function submit(e: FormEvent) {
    e.preventDefault();
    if (!ready) return;
    if (copying) onCopy(source, name.trim());
    else onBlank(name.trim());
  }

  return (
    <Modal title="New flow" onCancel={onCancel}>
      <form onSubmit={submit} className="dialog__form" noValidate>
        <label className="form-field" htmlFor={`${uid}-name`}>
          <span>Name</span>
          <input id={`${uid}-name`} type="text" value={name} maxLength={128} autoComplete="off" onChange={(e) => setName(e.target.value)} />
        </label>
        <fieldset className="stage-box">
          <legend>Start from</legend>
          <label className="check-row" htmlFor={`${uid}-blank`}>
            <input id={`${uid}-blank`} type="radio" name={`${uid}-mode`} checked={mode === "blank"} onChange={() => setMode("blank")} />
            <span>
              <b>Blank</b> An empty canvas: add the first step yourself.
            </span>
          </label>
          <label className="check-row" htmlFor={`${uid}-copy`}>
            <input id={`${uid}-copy`} type="radio" name={`${uid}-mode`} checked={mode === "copy"} disabled={flows.length === 0} onChange={() => setMode("copy")} />
            <span>
              <b>Copy an existing flow</b> Its steps, routes and settings. Call history is not copied.
            </span>
          </label>
          {mode === "copy" && flows.length > 0 && (
            <label className="form-field" htmlFor={`${uid}-source`}>
              <span>Flow to copy</span>
              <select id={`${uid}-source`} value={source} onChange={(e) => setSource(e.target.value)}>
                {flows.map((f) => (
                  <option key={f.id} value={f.id}>
                    {f.name}
                  </option>
                ))}
              </select>
            </label>
          )}
        </fieldset>
        {error && (
          <p role="alert" className="note note--bad">
            {error}
          </p>
        )}
        <p className="subtext">A new flow is a draft in this editor until you publish it. Nothing takes calls before that.</p>
        <div className="row row--end">
          <button type="button" className="action-button-small" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="action-button" disabled={!ready}>
            {pending ? "Working…" : copying ? "Copy and open editor" : "Create and open editor"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
