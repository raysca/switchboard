import { useRef } from "react";
import { Link, useParams, useSearchParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import type { ApiClient, ApiError } from "../../api/client";
import { EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { HistoryTab } from "./HistoryTab";
import { TestSuiteTab } from "./TestSuiteTab";

const TABS = [
  { id: "suite", label: "Test suite" },
  { id: "history", label: "History" },
] as const;

/** /evaluation/:agentId: one agent's test suite and its run history. */
export function AgentEvaluation({ client, keyValue }: { client: ApiClient; keyValue: string | null }) {
  const { agentId = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") === "history" ? "history" : "suite";
  const tabRefs = useRef<Record<string, HTMLButtonElement | null>>({});
  const agent = useQuery({ queryKey: ["agents", agentId], queryFn: () => client.getAgent(agentId), enabled: !!keyValue, retry: false });

  const select = (id: string, focus = false) => {
    setParams({ tab: id }, { replace: true });
    if (focus) tabRefs.current[id]?.focus();
  };
  const onKey = (e: React.KeyboardEvent, index: number) => {
    const next = e.key === "ArrowRight" ? index + 1 : e.key === "ArrowLeft" ? index - 1 : e.key === "Home" ? 0 : e.key === "End" ? TABS.length - 1 : null;
    if (next === null) return;
    e.preventDefault();
    select(TABS[(next + TABS.length) % TABS.length]!.id, true);
  };

  const name = agent.data?.name ?? "Agent";
  const head = (
    <header className="page-head">
      <nav aria-label="Breadcrumb" className="eval-breadcrumb subtext">
        <Link to="/evaluation">Evaluation</Link> / {agent.data?.name ?? "…"}
      </nav>
      <h1 tabIndex={-1}>{name}</h1>
    </header>
  );
  if (!keyValue) {
    return (
      <>
        {head}
        <EmptyState title="Connect to see evaluation results">Enter a management key above to see this agent's test suite.</EmptyState>
      </>
    );
  }
  if (!agent.data) {
    return (
      <>
        {head}
        {agent.isError ? (
          <ErrorState title="Could not load the agent" error={agent.error as ApiError} onRetry={() => void agent.refetch()} reassure />
        ) : (
          <LoadingState label="Loading agent" />
        )}
      </>
    );
  }
  return (
    <>
      {head}
      <div role="tablist" aria-label="Evaluation" className="tabs page-tabs">
        {TABS.map((t, i) => (
          <button
            key={t.id}
            ref={(el) => {
              tabRefs.current[t.id] = el;
            }}
            type="button"
            role="tab"
            id={`eval-tab-${t.id}`}
            aria-selected={tab === t.id}
            aria-controls="eval-tabpanel"
            tabIndex={tab === t.id ? 0 : -1}
            className={tab === t.id ? "tab tab--current" : "tab"}
            onClick={() => select(t.id)}
            onKeyDown={(e) => onKey(e, i)}
          >
            {t.label}
          </button>
        ))}
      </div>
      <div role="tabpanel" id="eval-tabpanel" aria-labelledby={`eval-tab-${tab}`}>
        {tab === "suite" ? (
          <TestSuiteTab client={client} agentId={agentId} agentName={agent.data.name} currentVersion={agent.data.current_version?.number ?? null} />
        ) : (
          <HistoryTab client={client} agentId={agentId} />
        )}
      </div>
    </>
  );
}
