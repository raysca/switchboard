import { useState } from "react";
import { Link } from "react-router";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import type { ApiClient, ApiError } from "../../api/client";
import { DataTable, EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { Sparkline } from "./Sparkline";

export type EvaluationViewProps = { client: ApiClient; keyValue: string | null };

const PAGE = 25;
const pct = (v: number) => `${Math.round(v * 100)}%`;

/** The fleet dashboard: every agent's latest whole-suite result against its threshold. */
export function EvaluationView({ client, keyValue }: EvaluationViewProps) {
  const [page, setPage] = useState(0);
  const fleet = useQuery({
    queryKey: ["eval", "fleet", page],
    queryFn: () => client.getEvalFleet(PAGE, page * PAGE),
    enabled: !!keyValue,
    retry: false,
    placeholderData: keepPreviousData,
  });
  const data = fleet.data;
  const below = data?.below_threshold ?? 0;

  const head = (
    <header className="page-head">
      <h1 tabIndex={-1}>Evaluation</h1>
      {below > 0 && (
        <span className="subtext" role="status">
          {below} {below === 1 ? "agent" : "agents"} below {below === 1 ? "its" : "their"} suite threshold
        </span>
      )}
    </header>
  );
  if (!keyValue) {
    return (
      <>
        {head}
        <EmptyState title="Connect to see evaluation results">Enter a management key above to see how each agent's test suite is doing.</EmptyState>
      </>
    );
  }
  if (!data) {
    return (
      <>
        {head}
        {fleet.isError ? (
          <ErrorState title="Could not load evaluation results" error={fleet.error as ApiError} onRetry={() => void fleet.refetch()} reassure />
        ) : (
          <LoadingState label="Loading evaluation results" skeleton={6} />
        )}
      </>
    );
  }
  if (data.agents.every((a) => !a.latest) && data.total <= PAGE) {
    return (
      <>
        {head}
        <EmptyState title="No suite has run yet">Add test cases to an agent and run its suite. The result appears here.</EmptyState>
      </>
    );
  }
  const first = page * PAGE + 1;
  const last = page * PAGE + data.agents.length;
  return (
    <>
      {head}
      <DataTable
        caption="Latest suite result per agent"
        columns={["Agent", "Latest suite", "Pass rate", "Trend", "Threshold", "Status"]}
        rows={data.agents.map((a) => [
          <Link key="n" to={`/evaluation/${encodeURIComponent(a.agent_id)}`}>
            {a.name}
          </Link>,
          a.latest ? `${a.latest.passed} of ${a.latest.total} cases, v${a.latest.version}` : "No suite run yet",
          typeof a.latest?.pass_rate === "number" ? pct(a.latest.pass_rate) : "—",
          <Sparkline key="t" values={a.trend} />,
          pct(a.threshold),
          a.latest ? (
            <span key="s" className="row">
              <span className="lamp" data-tone={a.below_threshold ? "bad" : "ok"} aria-hidden="true" />
              {a.below_threshold ? "Below threshold" : "OK"}
            </span>
          ) : (
            "—"
          ),
        ])}
      />
      <nav className="calls-pager" aria-label="Pagination">
        <span>
          Agents {first}–{last} of {data.total}
        </span>
        <span className="grow" />
        <button type="button" className="pager-button" disabled={page === 0} onClick={() => setPage(page - 1)}>
          Previous
        </button>
        <button type="button" className="pager-button" disabled={last >= data.total} onClick={() => setPage(page + 1)}>
          Next
        </button>
      </nav>
    </>
  );
}
