import { useQueries, useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { httpToolServerId, isHTTPToolSelection, type ApiClient, type ApiError, type HTTPToolDTO, type MCPServerDetailDTO, type ToolSelection } from "../../../api/client";
import { LoadingState } from "../../../components/primitives";
import { SettingHelp } from "./SettingHelp";

const keyOf = (t: ToolSelection) => `${t.mcp_server_id}/${t.remote_name}`;

type Entry = { name: string; description: string; hash: string };

/** Why a server's tools cannot be newly chosen, or null when they can. */
function unusable(d: MCPServerDetailDTO): string | null {
  if (!d.enabled) return "Disabled";
  if (d.validation_state !== "healthy") return "Not reachable";
  if (!d.discovery) return "Not synced yet";
  return null;
}

/**
 * The agent's MCP tools: which servers' tools the agent may call. Shared by every step; each step then
 * chooses its own subset. The lists are read from the servers' latest discoveries only while this is
 * shown (it sits in the step panel's Tools tab), through the same queries the MCP page uses.
 */
export function ToolPicker({
  client,
  selected,
  onChange,
  asyncKeys,
  onAsyncChange,
}: {
  client: ApiClient;
  selected: ToolSelection[];
  onChange: (next: ToolSelection[]) => void;
  /** The chosen tools (`<server>/<tool>`) that answer the model at once and finish in the background. */
  asyncKeys: string[];
  onAsyncChange: (keys: string[]) => void;
}) {
  const serversQuery = useQuery({ queryKey: ["mcp-servers"], queryFn: () => client.getMCPServers(), retry: false });
  const httpToolsQuery = useQuery({ queryKey: ["http-tools"], queryFn: () => client.getHTTPTools(), retry: false });
  const httpTools = httpToolsQuery.data ?? [];
  const servers = serversQuery.data ?? [];
  const details = useQueries({
    queries: servers.map((s) => ({ queryKey: ["mcp-server", s.id], queryFn: () => client.getMCPServer(s.id), retry: false })),
  });

  if (serversQuery.isPending) return <LoadingState label="Loading MCP servers" />;
  if (serversQuery.isError) {
    return (
      <p role="alert" className="note note--bad">
        {(serversQuery.error as ApiError).message}{" "}
        <button type="button" className="action-button-small" onClick={() => void serversQuery.refetch()}>
          Try again
        </button>
      </p>
    );
  }
  if (servers.length === 0 && httpTools.length === 0 && selected.length === 0) {
    return (
      <p className="subtext">
        No tools yet. <Link to="/mcp-servers">Connect an MCP server</Link> or <Link to="/http-tools/new">add an HTTP tool</Link> to give this agent tools.
      </p>
    );
  }

  const chosen = new Map(selected.map((t) => [keyOf(t), t]));
  const serverIds = new Set(servers.map((s) => s.id));
  const removed = selected.filter((t) => !isHTTPToolSelection(t) && !serverIds.has(t.mcp_server_id));
  const httpToolIds = new Set(httpTools.map((t) => httpToolServerId(t.id)));
  const removedHTTP = selected.filter((t) => isHTTPToolSelection(t) && !httpToolIds.has(t.mcp_server_id));
  const toggle = (serverId: string, e: Entry, on: boolean) => {
    const k = `${serverId}/${e.name}`;
    onChange(on ? [...selected, { mcp_server_id: serverId, remote_name: e.name, schema_hash: e.hash, required: false }] : selected.filter((t) => keyOf(t) !== k));
  };
  const setBackground = (t: ToolSelection, on: boolean) => {
    const k = keyOf(t);
    onAsyncChange(selected.map(keyOf).filter((x) => (x === k ? on : asyncKeys.includes(x))));
  };
  const setRequired = (t: ToolSelection, required: boolean) => onChange(selected.map((x) => (keyOf(x) === keyOf(t) ? { ...x, required } : x)));

  const toggleHTTP = (tool: HTTPToolDTO, on: boolean) => {
    const ref = httpToolServerId(tool.id);
    onChange(on ? [...selected, { mcp_server_id: ref, remote_name: tool.name, schema_hash: tool.hash, required: false }] : selected.filter((t) => t.mcp_server_id !== ref));
  };
  // An edited tool keeps the definition the agent chose until the author chooses it again.
  const useLatest = (tool: HTTPToolDTO) => onChange(selected.map((t) => (t.mcp_server_id === httpToolServerId(tool.id) ? { ...t, remote_name: tool.name, schema_hash: tool.hash } : t)));
  const httpRow = (tool: HTTPToolDTO) => {
    const picked = selected.find((t) => t.mcp_server_id === httpToolServerId(tool.id));
    const edited = !!picked && picked.schema_hash !== tool.hash;
    const noSecret = !!tool.definition.auth && tool.auth_secret_set === false;
    const note = !tool.enabled ? "Disabled" : noSecret ? `Secret ${tool.definition.auth?.secret} is not set` : null;
    return (
      <li key={tool.id} className="tool-pick">
        <label className="check-row">
          <input type="checkbox" checked={!!picked} disabled={!picked && !tool.enabled} onChange={(ev) => toggleHTTP(tool, ev.target.checked)} />
          <code>{tool.name}</code>
          <span className="subtext">{tool.definition.description}</span>
          {note && <span className="subtext">{note}</span>}
        </label>
        {picked && (
          <>
            <label className="check-row tool-pick__required">
              <input type="checkbox" aria-label={`Required: ${tool.name}`} checked={picked.required} onChange={(ev) => setRequired(picked, ev.target.checked)} />
              Required
            </label>
            {edited && (
              <span className="tool-pick__required">
                <span className="subtext">Edited since you chose it</span>{" "}
                <button type="button" className="action-button-small" onClick={() => useLatest(tool)}>
                  Use the latest definition
                </button>
              </span>
            )}
          </>
        )}
      </li>
    );
  };

  const row = (serverId: string, name: string, description: string, e: Entry | null, blocked: boolean, label = name, reasonId?: string) => {
    const k = `${serverId}/${name}`;
    const picked = chosen.get(k);
    return (
      <li key={k} className="tool-pick">
        <label className="check-row">
          <input
            type="checkbox"
            checked={!!picked}
            disabled={!picked && (blocked || !e)}
            aria-describedby={reasonId}
            onChange={(ev) => e && toggle(serverId, e, ev.target.checked)}
            // A tool that is no longer offered can only be taken off.
            {...(picked && !e ? { onClick: () => onChange(selected.filter((t) => keyOf(t) !== k)) } : {})}
          />
          <code>{label}</code>
          {description && <span className="subtext">{description}</span>}
          {picked && !e && <span className="subtext">No longer offered</span>}
        </label>
        {picked && (
          <>
            <label className="check-row tool-pick__required">
              <input type="checkbox" aria-label={`Required: ${name}`} checked={picked.required} onChange={(ev) => setRequired(picked, ev.target.checked)} />
              Required
            </label>
            <label className="check-row tool-pick__required">
              <input type="checkbox" aria-label={`Background: ${name}`} checked={asyncKeys.includes(k)} onChange={(ev) => setBackground(picked, ev.target.checked)} />
              Runs in the background
            </label>
          </>
        )}
      </li>
    );
  };

  return (
    <div className="tool-picker">
      {selected.length > 0 && (
        <>
          <p className="subtext">The model gets an immediate acknowledgement and is told the result when it arrives.</p>
          <SettingHelp id="async_tools" />
        </>
      )}
      {servers.map((s, i) => {
        const q = details[i];
        const d = q?.data;
        const reason = d ? unusable(d) : null;
        const offered = new Map<string, Entry>((d?.discovery?.tools ?? []).map((t) => [t.remote_name, { name: t.remote_name, description: t.description ?? "", hash: t.schema_hash }]));
        const mine = selected.filter((t) => t.mcp_server_id === s.id && !offered.has(t.remote_name));
        return (
          <fieldset key={s.id} className="stage-box" aria-label={s.name}>
            <legend>{s.name}</legend>
            {q?.isPending && <LoadingState label={`Loading ${s.name}`} />}
            {q?.isError && (
              <p className="subtext">
                Could not load tools{" "}
                <button type="button" className="action-button-small" onClick={() => void q.refetch()}>
                  Try again
                </button>
              </p>
            )}
            {reason && (
              <p id={`reason-${s.id}`} className="subtext">
                {reason}
              </p>
            )}
            {d && (
              <ul className="plain-list">
                {[...offered.values()].map((e) => row(s.id, e.name, e.description, e, reason !== null, e.name, reason ? `reason-${s.id}` : undefined))}
                {mine.map((t) => row(s.id, t.remote_name, "", null, true))}
              </ul>
            )}
          </fieldset>
        );
      })}
      {(httpTools.length > 0 || removedHTTP.length > 0) && (
        <fieldset className="stage-box" aria-label="HTTP tools">
          <legend>HTTP tools</legend>
          <ul className="plain-list">
            {httpTools.map(httpRow)}
            {removedHTTP.map((t) => (
              <li key={keyOf(t)} className="tool-pick">
                <label className="check-row">
                  <input type="checkbox" checked onChange={() => onChange(selected.filter((x) => keyOf(x) !== keyOf(t)))} />
                  <code>{t.remote_name}</code>
                  <span className="subtext">No longer exists</span>
                </label>
              </li>
            ))}
          </ul>
        </fieldset>
      )}
      {removed.length > 0 && (
        <fieldset className="stage-box" aria-label="Removed servers">
          <legend>Removed servers</legend>
          <ul className="plain-list">{removed.map((t) => row(t.mcp_server_id, t.remote_name, "", null, true, keyOf(t)))}</ul>
        </fieldset>
      )}
    </div>
  );
}
