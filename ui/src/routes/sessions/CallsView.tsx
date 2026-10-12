import { Link, useSearchParams } from "react-router";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import type { AgentDTO, ApiClient, ApiError } from "../../api/client";
import { EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { useActiveSessions } from "../agents/useActiveSessions";
import { clock, outcome, versionLabel } from "./callStats";
import { useNow } from "./useNow";

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
  const pageNumber = Number(params.get("page"));
  // Shown to people as 1, 2, 3…; kept in the URL only from page 2, so Back and links land on the same page.
  const page = Number.isInteger(pageNumber) && pageNumber >= 2 ? pageNumber - 1 : 0;
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
    // Keep the table (and the focused pager button) on screen while the next page loads.
    placeholderData: keepPreviousData,
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
  };
  const goToPage = (index: number) => {
    const p = new URLSearchParams(params);
    if (index > 0) p.set("page", String(index + 1));
    else p.delete("page");
    setParams(p);
  };

  const rows = (calls.data ?? []).slice(0, PAGE);
  const now = useNow(rows.some((s) => outcome(s).live));

  const head = (summary?: string) => (
    <header className="calls-head">
      <h1 tabIndex={-1}>Calls</h1>
      {summary && (
        <span className="subtext" role="status">
          {summary}
        </span>
      )}
    </header>
  );
  if (!keyValue) {
    return (
      <>
        {head()}
        <EmptyState title="Connect to see calls">Enter a management key above to see calls.</EmptyState>
      </>
    );
  }

  const flows = new Map<string, AgentDTO>((flowsQuery.data ?? []).map((f) => [f.id, f]));
  // The live count follows the flow filter, like the table does.
  const liveNow = active.data?.filter((s) => !flowId || s.agent_id === flowId).length;
  // The API returns at most LIVE_CAP live calls: at the cap the count is a floor, with or without a flow filter.
  const liveText = liveNow === undefined ? "" : (active.data?.length ?? 0) >= LIVE_CAP ? `${liveNow}+` : String(liveNow);
  const hasOlder = (calls.data?.length ?? 0) > PAGE;
  const filtered = flowId !== "" || outcomeId !== "";

  return (
    <>
      {head(liveNow === undefined ? "Newest first" : `${liveText} live now`)}
      <div className="calls-toolbar">
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
      ) : rows.length === 0 && page > 0 ? (
        <section className="message-panel">
          <h2>No calls on this page.</h2>
          <p>
            <button type="button" className="action-button-small" onClick={() => goToPage(page - 1)}>
              Newer
            </button>
          </p>
        </section>
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
          <div className="calls-table">
            <table>
              <caption className="visually-hidden">Calls, newest first</caption>
              <thead>
                <tr>
                  <th scope="col">Started</th>
                  <th scope="col">Flow</th>
                  <th scope="col" className="num">
                    Duration
                  </th>
                  <th scope="col" className="num">
                    Turns
                  </th>
                  <th scope="col">Outcome</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((s) => {
                  const o = outcome(s);
                  const f = flows.get(s.agent_id);
                  return (
                    <tr key={s.id}>
                      <td>
                        <Link to={`/sessions/${encodeURIComponent(s.id)}`}>{new Date(s.started_at).toLocaleString()}</Link>
                      </td>
                      <td>
                        {f?.name ?? "Unknown flow"} <span className="chip">{versionLabel(s, f)}</span>
                      </td>
                      <td className="num">{clock(s.started_at, s.ended_at, now)}</td>
                      <td className="num">{s.turn_count}</td>
                      <td>
                        <span className="row">
                          <span className="lamp" data-tone={o.live ? "warn" : undefined} aria-hidden="true" />
                          {o.label}
                        </span>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <nav className="calls-pager" aria-label="Pagination">
            <span>
              Calls {page * PAGE + 1}–{page * PAGE + rows.length}
            </span>
            <span className="grow" />
            <button type="button" className="action-button-small" disabled={page === 0} onClick={() => goToPage(page - 1)}>
              Newer
            </button>
            <button type="button" className="action-button-small" disabled={!hasOlder} onClick={() => goToPage(page + 1)}>
              Older
            </button>
          </nav>
        </>
      )}
    </>
  );
}
