import { Link } from "react-router";
import { Modal } from "../../components/Modal";

export type DeleteServerDialogProps = {
  name: string;
  /** The agents that pin this server's tools; `undefined` when the agent list could not be loaded. */
  agents: { id: string; name: string }[] | undefined;
  /** The server refused a plain delete because agents or calls in progress use it. */
  inUse: boolean;
  pending: boolean;
  error: string | null;
  onDelete: () => void;
  onDetach: () => void;
  onCancel: () => void;
};

/** Delete asks first. When the server says the server is in use, detaching is a second, explicit step that names what it affects. */
export function DeleteServerDialog({ name, agents, inUse, pending, error, onDelete, onDetach, onCancel }: DeleteServerDialogProps) {
  return (
    <Modal title={inUse ? `${name} is still in use` : `Delete ${name}?`} onCancel={onCancel}>
      {inUse ? (
        <>
          <p>
            Agents or calls in progress still use this server's tools. Detaching removes its tools from the agents that use them; those agents need review before they take calls again. Calls in progress finish first.
          </p>
          {agents === undefined ? (
            <p className="subtext">Which agents use it could not be loaded.</p>
          ) : agents.length > 0 ? (
            <ul className="drift-agents">
              {agents.map((f) => (
                <li key={f.id}>
                  <Link to={`/agents/${encodeURIComponent(f.id)}`}>{f.name}</Link>
                </li>
              ))}
            </ul>
          ) : null}
        </>
      ) : (
        <>
          <p>This removes the server and its tool list. It does not touch the remote server.</p>
          {agents !== undefined && agents.length > 0 && (
            <>
              <p>
                {agents.length === 1 ? "This agent uses" : "These agents use"} its tools, so deleting is refused until you detach it, which takes the tools off {agents.length === 1 ? "it" : "them"}:
              </p>
              <ul className="drift-agents">
                {agents.map((f) => (
                  <li key={f.id}>
                    <Link to={`/agents/${encodeURIComponent(f.id)}`}>{f.name}</Link>
                  </li>
                ))}
              </ul>
            </>
          )}
        </>
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
