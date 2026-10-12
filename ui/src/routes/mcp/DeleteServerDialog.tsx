import { Link } from "react-router";
import type { AgentDTO } from "../../api/client";
import { Modal } from "../../components/Modal";

export type DeleteServerDialogProps = {
  name: string;
  /** The flows that pin this server's tools; `undefined` when the flow list could not be loaded. */
  flows: AgentDTO[] | undefined;
  /** The server refused a plain delete because flows or calls in progress use it. */
  inUse: boolean;
  pending: boolean;
  error: string | null;
  onDelete: () => void;
  onDetach: () => void;
  onCancel: () => void;
};

/** Delete asks first. When the server says the server is in use, detaching is a second, explicit step that names what it affects. */
export function DeleteServerDialog({ name, flows, inUse, pending, error, onDelete, onDetach, onCancel }: DeleteServerDialogProps) {
  return (
    <Modal title={inUse ? `${name} is still in use` : `Delete ${name}?`} onCancel={onCancel}>
      {inUse ? (
        <>
          <p>
            Flows or calls in progress still use this server's tools. Detaching removes its tools from the flows that use them; those flows need review before they take calls again. Calls in progress finish first.
          </p>
          {flows === undefined ? (
            <p className="subtext">Which flows use it could not be loaded.</p>
          ) : flows.length > 0 ? (
            <ul className="drift-flows">
              {flows.map((f) => (
                <li key={f.id}>
                  <Link to={`/flows/${encodeURIComponent(f.id)}`}>{f.name}</Link>
                </li>
              ))}
            </ul>
          ) : null}
        </>
      ) : (
        <p>This removes the server and its tool list. It does not touch the remote server.</p>
      )}
      {error && (
        <p role="alert" className="tool-console__error">
          {error}
        </p>
      )}
      <div className="dialog__actions">
        <button type="button" className="action-button-small" onClick={onCancel}>
          Keep server
        </button>
        {inUse ? (
          <button type="button" className="action-button-small action-button-danger" disabled={pending} onClick={onDetach}>
            {pending ? "Detaching…" : "Detach and delete"}
          </button>
        ) : (
          <button type="button" className="action-button-small action-button-danger" disabled={pending} onClick={onDelete}>
            {pending ? "Deleting…" : "Delete server"}
          </button>
        )}
      </div>
    </Modal>
  );
}
