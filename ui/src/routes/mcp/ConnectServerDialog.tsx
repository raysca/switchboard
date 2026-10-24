import { useId, useState, type FormEvent } from "react";
import type { CreateMCPServerInput, MCPServerDTO, UpdateMCPServerInput } from "../../api/client";
import { Modal } from "../../components/Modal";

// Mirrors the server: a secret is an MCP_TOKEN_* or HTTP_TOKEN_* variable, or a lower-case stored or directory name,
// so a management key cannot point a server at an unrelated variable such as a provider key.
const SECRET_NAME_PATTERN = /^((MCP|HTTP)_TOKEN_[A-Z0-9_]{1,48}|[a-z0-9][a-z0-9_-]{0,62})$/;

type Common = { pending: boolean; error: string | null; onCancel: () => void; /** Names the server can already resolve, offered as suggestions. */ secrets?: string[] };
/** Without `server` the dialog connects a new one; with it, it edits that server. */
export type ConnectServerDialogProps = Common &
  ({ server?: undefined; onSubmit: (input: CreateMCPServerInput) => void } | { server: MCPServerDTO; onSubmit: (patch: UpdateMCPServerInput) => void });

/** Connect (or edit) a remote MCP server by endpoint. Only the name of a secret is entered, never its value. */
export function ConnectServerDialog(props: ConnectServerDialogProps) {
  const { pending, error, onCancel, server, secrets = [] } = props;
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
    if (authEnv.trim() && !SECRET_NAME_PATTERN.test(authEnv.trim())) {
      return setProblem("Secret must be an MCP_TOKEN_ variable name or a lower-case name such as crm-api.");
    }
    setProblem(null);
    if (props.server) {
      // The server compares the endpoint and token variable with the stored ones and re-syncs only on a change.
      return props.onSubmit({ name: name.trim(), description: description.trim(), endpoint: endpoint.trim(), auth_env: authEnv.trim() });
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
          <label htmlFor={`${uid}-auth`}>Secret (optional)</label>
          <input id={`${uid}-auth`} type="text" list={`${uid}-secrets`} value={authEnv} placeholder="crm-api" autoComplete="off" onChange={(e) => setAuthEnv(e.target.value)} />
          <datalist id={`${uid}-secrets`}>
            {secrets.map((s) => (
              <option key={s} value={s} />
            ))}
          </datalist>
          <small>
            The name of a secret on the <a href="/secrets">Secrets</a> page, or an <code>MCP_TOKEN_</code> environment variable. Its value is sent as the bearer token to this server only. Public HTTPS hosts only (development servers also reach local test servers).
          </small>
        </div>
        {!server && (
          <label className="check-row" htmlFor={`${uid}-enabled`}>
            <input id={`${uid}-enabled`} type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
            Enabled for agents
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
