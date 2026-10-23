import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import type { ApiClient } from "../../api/client";

/** Shown on the Dashboard only when this server has a dental MCP endpoint (DENTAL_MCP_URL). */
export function DemoCard({ client }: { client: ApiClient }) {
  const qc = useQueryClient();
  const status = useQuery({ queryKey: ["demo"], queryFn: () => client.getDemo(), retry: false });
  const install = useMutation({
    mutationFn: () => client.installDentalDemo(),
    onSuccess: () => Promise.all([qc.invalidateQueries({ queryKey: ["demo"] }), qc.invalidateQueries({ queryKey: ["agents"] })]),
  });
  const d = status.data?.dental;
  if (!d?.available) return null;
  return (
    <div className="message-panel">
      <h2>Dentistry demo</h2>
      <p>A front desk that hands new and returning patients to different steps, with a scheduling tool server.</p>
      {d.installed && d.agent_id ? (
        <p>
          Installed: <Link to={`/agents/${encodeURIComponent(d.agent_id)}`}>Smile Dental front desk</Link>
        </p>
      ) : (
        <button type="button" className="action-button-small" disabled={install.isPending} onClick={() => install.mutate()}>
          {install.isPending ? "Loading…" : "Load demo"}
        </button>
      )}
      {install.isError && <p role="alert">The demo could not be installed. Check the server log and try again.</p>}
    </div>
  );
}
