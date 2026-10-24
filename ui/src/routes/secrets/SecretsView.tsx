import { useId, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, type ApiClient, type SecretDTO } from "../../api/client";
import { DataTable, EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { Modal } from "../../components/Modal";
import { ago, formatApiError } from "../mcp/mcpStats";

export type SecretsViewProps = { client: ApiClient; keyValue: string | null };

// Mirrors the server: stored names are lower case, so they never overlap the MCP_TOKEN_* environment names.
const NAME_PATTERN = /^[a-z0-9][a-z0-9_-]{0,62}$/;
const SOURCE_LABEL: Record<SecretDTO["source"], string> = { environment: "Environment variable", directory: "Secrets directory", store: "Encrypted store" };

/** The credentials tools and MCP servers can use, by name. A value is typed in once and never shown again. */
export function SecretsView({ client, keyValue }: SecretsViewProps) {
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<{ name: string | null } | null>(null); // name null: a new secret
  const [deleting, setDeleting] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const query = useQuery({ queryKey: ["secrets"], queryFn: () => client.getSecrets(), enabled: !!keyValue, retry: false });
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["secrets"] });

  const save = useMutation({
    mutationFn: ({ name, value }: { name: string; value: string }) => client.putSecret(name, value),
    onSuccess: () => {
      setEditing(null);
      setActionError(null);
      void refresh();
    },
    onError: (err) => setActionError(formatApiError(err, "Could not save the secret")),
  });
  const remove = useMutation({
    mutationFn: (name: string) => client.deleteSecret(name),
    onSuccess: () => {
      setDeleting(null);
      setActionError(null);
      void refresh();
    },
    onError: (err) => setActionError(inUseSentence(err) ?? formatApiError(err, "Could not delete the secret")),
  });

  const storeEnabled = query.data?.store_enabled ?? false;
  const head = (
    <header className="page-head">
      <h1 tabIndex={-1}>Secrets</h1>
      <span className="subtext">Credentials that tools and MCP servers send to other services. Values are never shown.</span>
      <span className="grow" />
      {keyValue && storeEnabled && (
        <button
          type="button"
          className="action-button"
          onClick={() => {
            setActionError(null);
            setEditing({ name: null });
          }}
        >
          Add secret
        </button>
      )}
    </header>
  );

  let body;
  if (!keyValue) {
    body = <EmptyState title="Connect to see secrets">Enter a management key above to see the secrets this server can use.</EmptyState>;
  } else if (query.isPending) {
    body = <LoadingState label="Loading secrets" />;
  } else if (query.isError) {
    body = <ErrorState title="Could not load secrets" error={query.error as ApiError} onRetry={() => void query.refetch()} reassure />;
  } else {
    const secrets = query.data.secrets;
    const now = Date.now();
    body = (
      <>
        {!storeEnabled && (
          <p className="subtext" role="status">
            The encrypted store is off. Set <code>SWITCHBOARD_SECRETS_KEY</code> on the server and restart to add secrets here; secrets from the environment or the secrets directory still work.
          </p>
        )}
        {secrets.length === 0 ? (
          <EmptyState title="No secrets yet">Add a secret, then name it in an MCP server's secret field.</EmptyState>
        ) : (
          <DataTable
            caption="Secrets available to tools and MCP servers"
            columns={["Name", "Where it lives", "Last used", ""]}
            rows={secrets.map((s) => [
              <code key="n">{s.name}</code>,
              SOURCE_LABEL[s.source],
              s.last_used_at ? ago(s.last_used_at, now) : s.writable ? "Never" : "Not tracked",
              s.writable ? (
                <span key="a" className="row-actions">
                  <button type="button" className="action-button-small" onClick={() => { setActionError(null); setEditing({ name: s.name }); }}>
                    Replace
                  </button>
                  <button type="button" className="action-button-small" onClick={() => { setActionError(null); setDeleting(s.name); }}>
                    Delete
                  </button>
                </span>
              ) : (
                ""
              ),
            ])}
          />
        )}
      </>
    );
  }

  return (
    <>
      <div className="mcp-page">
        {head}
        <section className="dash-card">{body}</section>
      </div>
      {editing && <SecretDialog name={editing.name} pending={save.isPending} error={actionError} onSubmit={(name, value) => save.mutate({ name, value })} onCancel={() => setEditing(null)} />}
      {deleting && (
        <Modal title={`Delete ${deleting}?`} onCancel={() => setDeleting(null)}>
          <div className="dialog__form">
            {actionError && (
              <p role="alert" className="tool-console__error">
                {actionError}
              </p>
            )}
            <p>Anything that still names this secret stops authenticating. A secret in use cannot be deleted.</p>
            <div className="dialog__actions">
              <button type="button" className="action-button-small" onClick={() => setDeleting(null)}>
                Cancel
              </button>
              <button type="button" className="action-button" disabled={remove.isPending} onClick={() => remove.mutate(deleting)}>
                {remove.isPending ? "Deleting…" : "Delete secret"}
              </button>
            </div>
          </div>
        </Modal>
      )}
    </>
  );
}

/** The 409 lists what uses the secret: say so by name. */
function inUseSentence(err: unknown): string | null {
  if (!(err instanceof ApiError) || err.code !== "secret_in_use" || !Array.isArray(err.details)) return null;
  const names = (err.details as { name?: string }[]).map((u) => u.name).filter(Boolean);
  return names.length ? `Still used by: ${names.join(", ")}. Change those first.` : "Still in use by tools or servers.";
}

function SecretDialog({ name, pending, error, onSubmit, onCancel }: { name: string | null; pending: boolean; error: string | null; onSubmit: (name: string, value: string) => void; onCancel: () => void }) {
  const uid = useId();
  const [nameValue, setNameValue] = useState(name ?? "");
  const [value, setValue] = useState("");
  const [problem, setProblem] = useState<string | null>(null);

  function submit(e: FormEvent) {
    e.preventDefault();
    if (!NAME_PATTERN.test(nameValue.trim())) return setProblem("Name: lower-case letters, digits, '-' or '_' (up to 63), starting with a letter or digit.");
    if (!value || /\s/.test(value)) return setProblem("Value is required and cannot contain spaces.");
    setProblem(null);
    onSubmit(nameValue.trim(), value);
  }
  const shown = problem ?? error;
  return (
    <Modal title={name ? `Replace ${name}` : "Add secret"} onCancel={onCancel}>
      <form onSubmit={submit} className="dialog__form" noValidate>
        {shown && (
          <p role="alert" className="tool-console__error">
            {shown}
          </p>
        )}
        {!name && (
          <label className="form-field" htmlFor={`${uid}-name`}>
            <span>Name</span>
            <input id={`${uid}-name`} type="text" value={nameValue} placeholder="crm-api" autoComplete="off" onChange={(e) => setNameValue(e.target.value)} />
          </label>
        )}
        <div className="form-field">
          <label htmlFor={`${uid}-value`}>Value</label>
          <input id={`${uid}-value`} type="password" value={value} autoComplete="new-password" onChange={(e) => setValue(e.target.value)} />
          <small>Stored encrypted and never shown again. To change it, replace it.</small>
        </div>
        <div className="dialog__actions">
          <button type="button" className="action-button-small" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="action-button" disabled={pending}>
            {pending ? "Saving…" : "Save"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
