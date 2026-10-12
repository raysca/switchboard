import { Fragment, useState } from "react";
import { Link } from "react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ApiError, type AgentDTO, type ApiClient, type MCPServerDetailDTO } from "../../api/client";
import { ErrorState } from "../../components/primitives";
import { DeleteServerDialog } from "./DeleteServerDialog";
import { DriftPanel, MissingTools } from "./DriftPanel";
import { ToolTestConsole } from "./ToolTestConsole";
import { ago, drifts, flowsUsingServer, flowsUsingTool, formatApiError, serverStatus } from "./mcpStats";

export type ServerDetailProps = {
  client: ApiClient;
  server: MCPServerDetailDTO;
  /** `undefined` when the flow list could not be loaded. */
  flows: AgentDTO[] | undefined;
  /** The flow list reached the API's page size, so some flows were not checked. */
  flowsCapped: boolean;
  now: number;
  onDeleted: () => void;
};

type Open = { tool: string; mode: "schema" | "try" } | null;

/** One server: its facts, the schema-change review, and its tools. */
export function ServerDetail({ client, server, flows, flowsCapped, now, onDeleted }: ServerDetailProps) {
  const queryClient = useQueryClient();
  const [open, setOpen] = useState<Open>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [inUse, setInUse] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["mcp-servers"] });
    void queryClient.invalidateQueries({ queryKey: ["mcp-server", server.id] });
  };
  const action = (mutationFn: () => Promise<unknown>, fallback: string) => ({
    mutationFn,
    onSuccess: () => {
      setActionError(null);
      refresh();
    },
    onError: (err: unknown) => setActionError(formatApiError(err, fallback)),
  });
  const sync = useMutation(action(() => client.discoverMCPServer(server.id), "Could not sync tools"));
  const enable = useMutation(action(() => client.enableMCPServer(server.id), "Could not enable the server"));
  const disable = useMutation(action(() => client.disableMCPServer(server.id), "Could not disable the server"));

  const remove = useMutation({
    mutationFn: (detach: boolean) => (detach ? client.deleteMCPServer(server.id, true) : client.deleteMCPServer(server.id)),
    onSuccess: () => {
      setDeleting(false);
      setInUse(false);
      setDeleteError(null);
      void queryClient.invalidateQueries({ queryKey: ["mcp-servers"] });
      void queryClient.invalidateQueries({ queryKey: ["agents"] });
      onDeleted();
    },
    onError: (err) => {
      if (err instanceof ApiError && (err.status === 409 || err.code === "mcp_server_in_use")) {
        setInUse(true);
        setDeleteError(null);
      } else {
        setDeleteError(formatApiError(err, "Could not delete the server"));
      }
    },
  });

  const st = serverStatus(server);
  const users = flows ? flowsUsingServer(flows, server.id) : undefined;
  const drift = flows ? drifts(flows, server.id) : [];
  const changedTools = new Set(drift.filter((d) => d.kind === "changed").map((d) => d.dep.remote_name));
  const missing = drift.filter((d) => d.kind === "missing");
  const tools = server.discovery?.tools ?? [];
  const synced = ago(server.last_discovered_at, now);

  return (
    <section aria-labelledby="mcp-server-title" className="mcp-detail">
      <div className="mcp-detail__head">
        <div className="mcp-detail__title">
          <h2 id="mcp-server-title">{server.name}</h2>
          <p className="mcp-detail__status">
            <span className="lamp" data-tone={st.tone === "idle" ? undefined : st.tone} aria-hidden="true" />
            {st.label}
            {synced ? ` · synced ${synced}` : ""}
          </p>
          {server.description && <p className="subtext">{server.description}</p>}
          <dl className="mcp-facts">
            <div>
              <dt>Endpoint</dt>
              <dd>
                <code>{server.endpoint}</code>
              </dd>
            </div>
            <div>
              <dt>Auth</dt>
              <dd>
                {server.auth_env ? (
                  <>
                    Bearer from <code>{server.auth_env}</code>, {server.auth_env_set ? "set on the server" : "not set on the server"}
                  </>
                ) : (
                  "None"
                )}
              </dd>
            </div>
            <div>
              <dt>Used by</dt>
              <dd>
                {users === undefined ? (
                  "—"
                ) : users.length === 0 ? (
                  flowsCapped ? "No flow among the first 100" : "No flow yet"
                ) : (
                  users.map((f, i) => (
                    <Fragment key={f.id}>
                      {i > 0 && ", "}
                      <Link to={`/flows/${encodeURIComponent(f.id)}`}>{f.name}</Link>
                    </Fragment>
                  ))
                )}
              </dd>
            </div>
          </dl>
        </div>
        <div className="mcp-detail__actions">
          <button type="button" className="action-button" disabled={sync.isPending} onClick={() => sync.mutate()}>
            {sync.isPending ? "Syncing…" : "Sync tools"}
          </button>
          {server.enabled ? (
            <button type="button" className="action-button-small" disabled={disable.isPending} onClick={() => disable.mutate()}>
              Disable
            </button>
          ) : (
            <button type="button" className="action-button-small" disabled={enable.isPending} onClick={() => enable.mutate()}>
              Enable
            </button>
          )}
          <button
            type="button"
            className="action-button-small action-button-danger"
            onClick={() => {
              setInUse(false);
              setDeleteError(null);
              setDeleting(true);
            }}
          >
            Delete
          </button>
        </div>
      </div>

      {actionError && <ErrorState title="That did not work" error={new ApiError(actionError, "action_failed", 0, "")} onRetry={() => setActionError(null)} />}
      {flows === undefined && <p className="subtext" role="note">The flows could not be loaded, so which flows use this server, and whether any schema changed, is unknown.</p>}

      {[...changedTools].map((name) => (
        <DriftPanel key={name} client={client} tool={name} schema={tools.find((t) => t.remote_name === name)?.input_schema} items={drift.filter((d) => d.kind === "changed" && d.dep.remote_name === name)} />
      ))}
      {missing.length > 0 && <MissingTools items={missing} />}

      {tools.length === 0 ? (
        <section className="message-panel">
          <h2>No tools discovered</h2>
          <p>Sync tools to connect to the server and list what it offers.</p>
        </section>
      ) : (
        <div className="calls-table">
          <table>
            <caption className="visually-hidden">Tools offered by {server.name}</caption>
            <thead>
              <tr>
                <th scope="col">Tool</th>
                <th scope="col">What the model sees</th>
                <th scope="col">Schema</th>
                <th scope="col" className="num">
                  Flows
                </th>
                <th scope="col">
                  <span className="visually-hidden">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {tools.map((t) => {
                const schemaOpen = open?.tool === t.remote_name && open.mode === "schema";
                const tryOpen = open?.tool === t.remote_name && open.mode === "try";
                return (
                  <Fragment key={t.remote_name}>
                    <tr>
                      <td>
                        <code>{t.remote_name}</code>
                      </td>
                      <td className="mcp-tool__description">{t.description}</td>
                      <td>{changedTools.has(t.remote_name) ? <span className="mcp-tool__changed">changed</span> : <code>{t.schema_hash.slice(0, 8)}</code>}</td>
                      <td className="num">{flows ? flowsUsingTool(flows, server.id, t.remote_name).length : "—"}</td>
                      <td className="mcp-tool__actions">
                        <button type="button" className="action-button-small" aria-label={`${schemaOpen ? "Hide" : "View"} schema for ${t.remote_name}`} onClick={() => setOpen(schemaOpen ? null : { tool: t.remote_name, mode: "schema" })}>
                          {schemaOpen ? "Hide schema" : "Schema"}
                        </button>
                        <button type="button" className="action-button-small" aria-label={`Try ${t.remote_name}`} aria-expanded={tryOpen} onClick={() => setOpen(tryOpen ? null : { tool: t.remote_name, mode: "try" })}>
                          Try it
                        </button>
                      </td>
                    </tr>
                    {(schemaOpen || tryOpen) && (
                      <tr>
                        <td colSpan={5}>
                          {schemaOpen ? (
                            <pre className="drift-schema">
                              <code>{JSON.stringify(t.input_schema, null, 2)}</code>
                            </pre>
                          ) : (
                            <ToolTestConsole client={client} serverId={server.id} tool={t.remote_name} />
                          )}
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      <p className="subtext">Tokens are read only from <code>MCP_TOKEN_*</code> variables. Private and loopback addresses are refused.</p>

      {deleting && (
        <DeleteServerDialog
          name={server.name}
          flows={users}
          inUse={inUse}
          pending={remove.isPending}
          error={deleteError}
          onDelete={() => remove.mutate(false)}
          onDetach={() => remove.mutate(true)}
          onCancel={() => setDeleting(false)}
        />
      )}
    </section>
  );
}
