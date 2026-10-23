import { Modal } from "../../components/Modal";

export type ArchiveAgentDialogProps = {
  name: string;
  pending: boolean;
  error: string | null;
  onConfirm: () => void;
  onCancel: () => void;
};

/** Archiving hides an agent and stops it taking calls; its history stays. */
export function ArchiveAgentDialog({ name, pending, error, onConfirm, onCancel }: ArchiveAgentDialogProps) {
  return (
    <Modal title={`Archive ${name}?`} onCancel={onCancel}>
      <p>
        It stops taking calls and leaves this list. Its version history and past calls are kept. Calls in progress are not cut off.
      </p>
      {error && (
        <p role="alert" className="note note--bad">
          {error}
        </p>
      )}
      <div className="row row--end">
        <button type="button" className="action-button-small" onClick={onCancel}>
          Keep agent
        </button>
        <button type="button" className="action-button action-button-danger" disabled={pending} onClick={onConfirm}>
          {pending ? "Archiving…" : "Archive agent"}
        </button>
      </div>
    </Modal>
  );
}
