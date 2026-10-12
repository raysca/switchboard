import { useId, useState } from "react";
import { Link } from "react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { ApiClient } from "../../api/client";
import { Modal } from "../../components/Modal";
import { acceptedSelection, formatApiError, type Drift } from "./mcpStats";

const short = (hash: string | undefined) => (hash ?? "").slice(0, 8);

/** A tool whose input schema changed: what it means for each flow that pinned the old one, and a way to accept the new one. */
export function DriftPanel({ client, tool, schema, items }: { client: ApiClient; tool: string; schema: unknown; items: Drift[] }) {
  const titleId = useId();
  const queryClient = useQueryClient();
  const [accepting, setAccepting] = useState<Drift | null>(null);
  const [error, setError] = useState<string | null>(null);

  const accept = useMutation({
    mutationFn: (d: Drift) => {
      const selection = acceptedSelection(d.flow, d.dep);
      if (!selection) throw new Error("The server's current schema is unknown, so there is nothing to accept.");
      return client.setAgentMCPTools(d.flow.id, selection);
    },
    onSuccess: () => {
      setAccepting(null);
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["agents"] });
    },
    onError: (err) => setError(formatApiError(err, "Could not publish the new version")),
  });

  return (
    <section aria-labelledby={titleId} className="drift-panel">
      <h3 id={titleId}>{tool} changed its input schema</h3>
      <p>Flows that require it need your review before they take calls again. Flows that use it optionally keep working without it.</p>
      <ul className="drift-flows">
        {items.map((d) => (
          <li key={d.flow.id}>
            <Link to={`/flows/${encodeURIComponent(d.flow.id)}`}>{d.flow.name}</Link>
            <span className="subtext">{d.dep.required ? "requires it" : "uses it optionally"}</span>
            <code>
              {short(d.dep.selected_schema_hash)} → {short(d.dep.current_schema_hash)}
            </code>
            {d.dep.current_schema_hash && (
              <button
                type="button"
                className="action-button-small"
                aria-label={`Accept new schema for ${d.flow.name}`}
                onClick={() => {
                  setError(null);
                  setAccepting(d);
                }}
              >
                Accept new schema
              </button>
            )}
          </li>
        ))}
      </ul>
      <p className="subtext">The server now offers this input schema:</p>
      <pre className="drift-schema">
        <code>{JSON.stringify(schema, null, 2)}</code>
      </pre>

      {accepting && (
        <Modal title={`Accept the new schema for ${accepting.flow.name}?`} onCancel={() => setAccepting(null)}>
          <p>
            This publishes a new version of {accepting.flow.name} that pins {tool} to the schema the server offers now. Calls in progress finish on the version they started on.
          </p>
          {error && (
            <p role="alert" className="tool-console__error">
              {error}
            </p>
          )}
          <div className="dialog__actions">
            <button type="button" className="action-button-small" onClick={() => setAccepting(null)}>
              Cancel
            </button>
            <button type="button" className="action-button" disabled={accept.isPending} onClick={() => accept.mutate(accepting)}>
              {accept.isPending ? "Publishing…" : "Publish new version"}
            </button>
          </div>
        </Modal>
      )}
    </section>
  );
}

/** Pinned tools the server no longer offers. Nothing to accept: the flows need a different tool. */
export function MissingTools({ items }: { items: Drift[] }) {
  const names = [...new Set(items.map((d) => d.dep.remote_name))];
  return (
    <>
      {names.map((name) => (
        <section key={name} className="drift-panel" aria-label={`${name} is no longer offered`}>
          <h3>{name} is no longer offered by this server</h3>
          <p>Flows that require it need your review before they take calls again. Change the step that uses it, then publish.</p>
          <ul className="drift-flows">
            {items
              .filter((d) => d.dep.remote_name === name)
              .map((d) => (
                <li key={d.flow.id}>
                  <Link to={`/flows/${encodeURIComponent(d.flow.id)}`}>{d.flow.name}</Link>
                  <span className="subtext">{d.dep.required ? "requires it" : "uses it optionally"}</span>
                </li>
              ))}
          </ul>
        </section>
      ))}
    </>
  );
}
