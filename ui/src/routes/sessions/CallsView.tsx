import { useState } from "react";
import { Link, useSearchParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import type { AgentDTO, ApiClient, ApiError } from "../../api/client";
import { DataTable, EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { useActiveSessions } from "../agents/useActiveSessions";
import { clock, outcome, versionLabel } from "./callStats";

export type CallsViewProps = { client: ApiClient; keyValue: string | null };

const PAGE = 25;
const LIVE_CAP = 100;
type OutcomeFilter = "" | "live" | "ended";
const OUTCOMES: { id: OutcomeFilter; label: string }[] = [
  { id: "", label: "All" },
  { id: "live", label: "Live" },
  { id: "ended", label: "Ended" },
];

/** Every call, newest first. Filters live in the URL so a link (from the Flows list) can open one. */
export function CallsView({ client, keyValue }: CallsViewProps) {
  const [params, setParams] = useSearchParams();
  const [page, setPage] = useState(0);
  const flowId = params.get("flow") ?? "";
  const raw = params.get("outcome");
  const outcomeId: OutcomeFilter = raw === "live" || raw === "ended" ? raw : "";
  const status = outcomeId === "live" ? "active" : outcomeId === "ended" ? "ended" : undefined;

  // One extra row tells us whether an older page exists; the API returns no total.
  const calls = useQuery({
    queryKey: ["sessions", "list", flowId, outcomeId, page],
    queryFn: () => client.getSessions({ limit: PAGE + 1, offset: page * PAGE, status, agent_id: flowId || undefined }),
    enabled: !!keyValue,
    retry: false,
    refetchInterval: 10_000,
  });
  const flowsQuery = useQuery({ queryKey: ["agents"], queryFn: () => client.getAgents(), enabled: !!keyValue, retry: false });
  const active = useActiveSessions(client, keyValue);

  const setFilter = (next: { flow?: string; outcome?: OutcomeFilter }) => {
    const flow = next.flow ?? flowId;
    const out = next.outcome ?? outcomeId;
    const p = new URLSearchParams();
    if (flow) p.set("flow", flow);
    if (out) p.set("outcome", out);
    setParams(p, { replace: true });
    setPage(0);
  };

  const heading = (
    <header className="section-heading">
      <h1 tabIndex={-1}>Calls</h1>
      <p>Every call, newest first. Open one to read what was said and see where it moved.</p>
    </header>
  );
  if (!keyValue) {
    return (
      <>
        {heading}
        <EmptyState title="Connect to see calls">Enter a management key above to see calls.</EmptyState>
      </>
    );
  }

  const flows = new Map<string, AgentDTO>((flowsQuery.data ?? []).map((f) => [f.id, f]));
  const liveNow = active.data?.length;
  const liveText = liveNow === undefined ? "" : liveNow >= LIVE_CAP ? `${LIVE_CAP}+` : String(liveNow);
  const rows = (calls.data ?? []).slice(0, PAGE);
  const hasOlder = (calls.data?.length ?? 0) > PAGE;
  const filtered = flowId !== "" || outcomeId !== "";
  const now = Date.now();

  return (
    <>
      {heading}
      <div className="calls-toolbar">
        <p className="subtext" role="status">
          {liveNow === undefined ? "Most recent first" : `${liveText} live now`}
        </p>
        <span className="grow" />
        <label className="form-field calls-flow" htmlFor="calls-flow">
          <span>Flow</span>
          <select id="calls-flow" value={flowId} onChange={(e) => setFilter({ flow: e.target.value })}>
            <option value="">All flows</option>
            {flowId !== "" && !flows.has(flowId) && <option value={flowId}>Unknown flow</option>}
            {[...flows.values()].map((f) => (
              <option key={f.id} value={f.id}>
                {f.name}
                {f.archived ? " (archived)" : ""}
              </option>
            ))}
          </select>
        </label>
      </div>
      <div role="group" aria-label="Outcome" className="calls-chips">
        {OUTCOMES.map((o) => (
          <button key={o.id} type="button" className="chip-button" aria-pressed={outcomeId === o.id} onClick={() => setFilter({ outcome: o.id })}>
            {o.id === "live" && liveNow !== undefined ? `Live · ${liveText}` : o.label}
          </button>
        ))}
      </div>

      {calls.isPending ? (
        <LoadingState label="Loading calls" />
      ) : calls.isError ? (
        <ErrorState title="Could not load calls" error={calls.error as ApiError} onRetry={() => void calls.refetch()} />
      ) : rows.length === 0 ? (
        filtered ? (
          <section className="message-panel">
            <h2>No calls match these filters.</h2>
            <p>
              <button type="button" className="action-button-small" onClick={() => setFilter({ flow: "", outcome: "" })}>
                Clear filters
              </button>
            </p>
          </section>
        ) : (
          <section className="message-panel">
            <h2>No calls yet.</h2>
            <p>Calls appear here as soon as someone talks to a flow. Test one from the editor to see how this looks.</p>
            <p>
              <Link to="/flows" className="action-button-small">
                Go to Flows
              </Link>
            </p>
          </section>
        )
      ) : (
        <>
          <DataTable
            caption="Calls, newest first"
            columns={["Started", "Flow", "Duration", "Turns", "Outcome"]}
            rows={rows.map((s) => {
              const o = outcome(s);
              const f = flows.get(s.agent_id);
              return [
                <Link key="started" to={`/sessions/${encodeURIComponent(s.id)}`}>
                  {new Date(s.started_at).toLocaleString()}
                </Link>,
                <span key="flow">
                  {f?.name ?? "Unknown flow"} <span className="chip">{versionLabel(s, f)}</span>
                </span>,
                clock(s.started_at, s.ended_at, now),
                s.turn_count,
                <span key="outcome" className="row">
                  <span className="lamp" data-tone={o.live ? "warn" : undefined} aria-hidden="true" />
                  {o.label}
                </span>,
              ];
            })}
          />
          <nav className="calls-pager" aria-label="Pagination">
            <span>
              Calls {page * PAGE + 1}–{page * PAGE + rows.length}
            </span>
            <span className="grow" />
            <button type="button" className="action-button-small" disabled={page === 0} onClick={() => setPage(page - 1)}>
              Newer
            </button>
            <button type="button" className="action-button-small" disabled={!hasOlder} onClick={() => setPage(page + 1)}>
              Older
            </button>
          </nav>
        </>
      )}
    </>
  );
}
