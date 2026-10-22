import { useId, useState, type FormEvent } from "react";
import type { CreateMCPServerInput, MCPServerDTO, UpdateMCPServerInput } from "../../api/client";
import { Modal } from "../../components/Modal";

// Mirrors the server: only MCP_TOKEN_* variables can be named, so a management key cannot point a server at an unrelated secret.
const AUTH_ENV_PREFIX = "MCP_TOKEN_";
const AUTH_ENV_PATTERN = /^MCP_TOKEN_[A-Z0-9_]{1,48}$/;

type Common = { pending: boolean; error: string | null; onCancel: () => void };
/** Without `server` the dialog connects a new one; with it, it edits that server and sends only what changed. */
export type ConnectServerDialogProps = Common &
  ({ server?: undefined; onSubmit: (input: CreateMCPServerInput) => void } | { server: MCPServerDTO; onSubmit: (patch: UpdateMCPServerInput) => void });

/** Connect (or edit) a remote MCP server by endpoint. Only the name of the token variable is entered, never a token. */
export function ConnectServerDialog(props: ConnectServerDialogProps) {
  const { pending, error, onCancel, server } = props;
  const uid = useId();
  const [name, setName] = useState(server?.name ?? "");
  const [endpoint, setEndpoint] = useState(server?.endpoint ?? "");
  const [description, setDescription] = useState(server?.description ?? "");
  const [authEnv, setAuthEnv] = useState(server?.auth_env ?? "");
  const [enabled, setEnabled] = useState(true);
  const [problem, setProblem] = useState<string | null>(null);

  function submit(e: FormEvent) {
    e.preventDefault();
    if (!name.trim()) return setProblem("Server name is required");
    if (!endpoint.trim()) return setProblem("Endpoint URL is required");
    if (authEnv.trim() && !AUTH_ENV_PATTERN.test(authEnv.trim())) {
      return setProblem(`Token variable must match ${AUTH_ENV_PREFIX} followed by A-Z, 0-9 or _ (max 48).`);
    }
    setProblem(null);
    if (props.server) {
      const patch: UpdateMCPServerInput = {};
      if (name.trim() !== props.server.name) patch.name = name.trim();
      if (endpoint.trim() !== props.server.endpoint) patch.endpoint = endpoint.trim();
      if (description.trim() !== props.server.description) patch.description = description.trim();
      if (authEnv.trim() !== props.server.auth_env) patch.auth_env = authEnv.trim();
      if (Object.keys(patch).length === 0) return onCancel();
      return props.onSubmit(patch);
    }
    props.onSubmit({
      name: name.trim(),
      description: description.trim() || undefined,
      endpoint: endpoint.trim(),
      transport: "streamable-http",
      auth_env: authEnv.trim() || undefined,
      enabled,
    });
  }

  const shown = problem ?? error;
  return (
    <Modal title={server ? "Edit server" : "Connect server"} onCancel={onCancel}>
      <form onSubmit={submit} className="dialog__form" noValidate>
        {shown && (
          <p role="alert" className="tool-console__error">
            {shown}
          </p>
        )}
        <label className="form-field" htmlFor={`${uid}-name`}>
          <span>Name</span>
          <input id={`${uid}-name`} type="text" value={name} autoComplete="off" onChange={(e) => setName(e.target.value)} />
        </label>
        <label className="form-field" htmlFor={`${uid}-endpoint`}>
          <span>Endpoint URL</span>
          <input id={`${uid}-endpoint`} type="url" value={endpoint} placeholder="https://mcp.example.com/rpc" autoComplete="off" onChange={(e) => setEndpoint(e.target.value)} />
        </label>
        <label className="form-field" htmlFor={`${uid}-desc`}>
          <span>Description (optional)</span>
          <input id={`${uid}-desc`} type="text" value={description} autoComplete="off" onChange={(e) => setDescription(e.target.value)} />
        </label>
        <div className="form-field">
          <label htmlFor={`${uid}-auth`}>Token variable (optional)</label>
          <input id={`${uid}-auth`} type="text" value={authEnv} placeholder={`${AUTH_ENV_PREFIX}CRM`} autoComplete="off" onChange={(e) => setAuthEnv(e.target.value)} />
          <small>
            The name of a server environment variable starting with <code>{AUTH_ENV_PREFIX}</code>. Its value is sent as the bearer token to this server only. Public HTTPS hosts only (development servers also reach local test servers).
          </small>
        </div>
        {!server && (
          <label className="check-row" htmlFor={`${uid}-enabled`}>
            <input id={`${uid}-enabled`} type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
            Enabled for flows
          </label>
        )}
        <div className="dialog__actions">
          <button type="button" className="action-button-small" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="action-button" disabled={pending}>
            {server ? (pending ? "Saving…" : "Save") : pending ? "Connecting…" : "Connect"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
