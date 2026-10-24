import { Modal } from "../../components/Modal";

export type ArchiveAgentDialogProps = {
  name: string;
  pending: boolean;
  error: string | null;
  /** Calls in progress on this agent right now. */
  liveCalls?: number;
  /** The tools its current version uses (MCP servers' and HTTP tools'). */
  tools?: string[];
  onConfirm: () => void;
  onCancel: () => void;
};

/** Archiving hides an agent and stops it taking calls; its history stays. */
export function ArchiveAgentDialog({ name, pending, error, liveCalls = 0, tools = [], onConfirm, onCancel }: ArchiveAgentDialogProps) {
  return (
    <Modal title={`Archive ${name}?`} onCancel={onCancel}>
      <p>
        It stops taking calls and leaves this list. Its version history and past calls are kept. Calls in progress are not cut off.
      </p>
      {liveCalls > 0 && (
        <p role="note">
          {liveCalls === 1 ? "1 call is" : `${liveCalls} calls are`} in progress. {liveCalls === 1 ? "It finishes" : "They finish"} normally; no new calls start.
        </p>
      )}
      {tools.length > 0 && (
        <p className="subtext">
          It uses {tools.length === 1 ? "this tool" : "these tools"}: {tools.map((t, i) => [i > 0 && ", ", <code key={t}>{t}</code>])}. Archiving does not release {tools.length === 1 ? "it" : "them"}: the agent's current version keeps using {tools.length === 1 ? "it" : "them"}, so deleting {tools.length === 1 ? "its" : "their"} MCP server or HTTP tool is still refused (an HTTP tool any version has used can only be disabled).
        </p>
      )}
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
