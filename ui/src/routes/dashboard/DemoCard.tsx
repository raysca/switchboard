import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import type { ApiClient } from "../../api/client";

type DemoEntry = { available: boolean; installed: boolean; agent_id?: string };

type DemoPanelProps = {
  kind: "dental" | "atlas";
  /** Also the panel's accessible region name, so a browser test can scope its controls. */
  title: string;
  description: string;
  agentName: string;
  /** Distinct per panel: an unscoped "Load demo" query must match only the dental button. */
  buttonLabel: string;
  /** Shown when the agent exists: re-running the installer repairs a stale pin or a drifted tool. */
  reloadLabel: string;
  demo: DemoEntry;
  install: () => Promise<unknown>;
};

/** One installable demo: its own install mutation, pending state and failure alert. */
function DemoPanel({ kind, title, description, agentName, buttonLabel, reloadLabel, demo, install }: DemoPanelProps) {
  const qc = useQueryClient();
  const mutation = useMutation({
    mutationFn: install,
    onSuccess: () => Promise.all([qc.invalidateQueries({ queryKey: ["demo"] }), qc.invalidateQueries({ queryKey: ["agents"] })]),
  });
  return (
    <section className="message-panel" role="region" aria-label={title} data-demo={kind}>
      <h2>{title}</h2>
      <p>{description}</p>
      {demo.installed && demo.agent_id ? (
        <p>
          Installed: <Link to={`/agents/${encodeURIComponent(demo.agent_id)}`}>{agentName}</Link>{" "}
          <button type="button" className="action-button-small" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
            {mutation.isPending ? "Loading…" : reloadLabel}
          </button>
        </p>
      ) : (
        <button type="button" className="action-button-small" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
          {mutation.isPending ? "Loading…" : buttonLabel}
        </button>
      )}
      {mutation.isError && <p role="alert">The demo could not be installed. Check the server log and try again.</p>}
    </section>
  );
}

/** Shown on the Dashboard only when this server has a dental or Atlas demo endpoint. */
export function DemoCard({ client }: { client: ApiClient }) {
  const status = useQuery({ queryKey: ["demo"], queryFn: () => client.getDemo(), retry: false });
  const dental = status.data?.dental;
  const atlas = status.data?.atlas;
  if (!dental?.available && !atlas?.available) return null;
  return (
    <>
      {dental?.available && (
        <DemoPanel
          kind="dental"
          title="Dentistry demo"
          description="A front desk that hands new and returning patients to different steps, with a scheduling tool server."
          agentName="Smile Dental front desk"
          buttonLabel="Load demo"
          reloadLabel="Reload demo"
          demo={dental}
          install={() => client.installDentalDemo()}
        />
      )}
      {atlas?.available && (
        <DemoPanel
          kind="atlas"
          title="Atlas Travel demo"
          description="A travel concierge built from 8 steps and 32 handoffs, with a travel tool server and two HTTP tools."
          agentName="Atlas Travel Concierge"
          buttonLabel="Load Atlas demo"
          reloadLabel="Reload Atlas demo"
          demo={atlas}
          install={() => client.installAtlasDemo()}
        />
      )}
    </>
  );
}
