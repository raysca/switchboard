import { useEffect, useRef, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router";
import type { ApiClient, ApiError } from "../../api/client";
import { EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { TurnTimeline } from "../../components/timeline/TurnTimeline";
import { CallTranscript } from "./CallTranscript";
import { LiveCallControls } from "./LiveCallControls";
import { ReplyWaterfall } from "./ReplyWaterfall";
import { callerLabel, clock, endReasonText, endedByAgent, failedHandoffs, formatMs, interruptedCount, medianReplyMs, outcome, path, stepsFor, transcript, turnRows, versionLabel } from "./callStats";
import { useNow } from "./useNow";

export type CallViewProps = { client: ApiClient; keyValue: string | null };

const LIVE_POLL_MS = 5_000;

/** One call: the figures, the path through the steps, the transcript, and the raw trace one click away. */
export function CallView({ client, keyValue }: CallViewProps) {
  const { sessionId = "" } = useParams();
  const on = !!keyValue && !!sessionId;
  const sessionQuery = useQuery({
    queryKey: ["session", sessionId],
    queryFn: () => client.getSession(sessionId),
    enabled: on,
    retry: false,
    refetchInterval: (q) => (q.state.data?.status === "active" ? LIVE_POLL_MS : false),
  });
  const running = sessionQuery.data?.status === "active";
  const turnsQuery = useQuery({
    queryKey: ["session-turns", sessionId],
    queryFn: () => client.getSessionTurns(sessionId),
    enabled: on,
    retry: false,
    refetchInterval: running ? LIVE_POLL_MS : false,
  });
  const tracesQuery = useQuery({
    queryKey: ["session-traces", sessionId],
    queryFn: () => client.getSessionTraces(sessionId),
    enabled: on,
    retry: false,
    refetchInterval: running ? LIVE_POLL_MS : false,
  });
  const agentsQuery = useQuery({ queryKey: ["agents"], queryFn: () => client.getAgents(), enabled: on, retry: false });

  // Polling stops with the status, and its last fetch predates the final replies: fetch once more.
  const status = sessionQuery.data?.status;
  const was = useRef(status);
  const { refetch: refetchTurns } = turnsQuery;
  const { refetch: refetchTraces } = tracesQuery;
  useEffect(() => {
    if (was.current === "active" && status !== "active") {
      void refetchTurns();
      void refetchTraces();
    }
    was.current = status;
  }, [status, refetchTurns, refetchTraces]);

  const now = useNow(running);
  // The heading and the way back are always there, so focus and the page's outline never depend on loading.
  const top = (state?: ReactNode, status?: ReactNode) => (
    <div className="call-page">
      <nav aria-label="Breadcrumb" className="agent-page__crumb">
        <Link to="/sessions">Calls</Link>
      </nav>
      <header className="call-bar">
        <h1 tabIndex={-1} className="call-id">call_{sessionId.slice(0, 8)}</h1>
        {status}
      </header>
      {state}
    </div>
  );

  if (!keyValue) return top(<EmptyState title="Connect to see this call">Enter a management key above to see this call.</EmptyState>);
  if (sessionQuery.isPending) return top(<LoadingState label="Loading call" />);
  if (sessionQuery.isError) {
    const err = sessionQuery.error as ApiError;
    if (err.status === 404) {
      return top(
        <section className="agent-missing">
          <p role="status">That call was not found.</p>
          <Link to="/sessions" className="action-button-small">
            Back to calls
          </Link>
        </section>,
      );
    }
    return top(<ErrorState title="Could not load the call" error={err} onRetry={() => void sessionQuery.refetch()} reassure />);
  }

  const s = sessionQuery.data;
  const agent = (agentsQuery.data ?? []).find((f) => f.id === s.agent_id);
  const fallback = agent?.name ?? "Assistant";
  const st = stepsFor(s, agent);
  const turns = turnsQuery.data ?? [];
  const median = medianReplyMs(turns);
  const traces = tracesQuery.data ?? null;
  const o = outcome(s);
  const label = (id: string) => st.names.get(id) ?? id;
  const ended = endedByAgent(traces);
  const failed = failedHandoffs(traces);

  const stateBar = (
    <span className="call-state">
      <span className="lamp" data-tone={o.live ? "warn" : undefined} aria-hidden="true" />
      {o.label} · {clock(s.started_at, s.ended_at, now)}
    </span>
  );
  const started = <span className="subtext">Started {new Date(s.started_at).toLocaleString()}</span>;

  return top(
    <>
      <dl className="call-figures">
        <div>
          <dt>Agent</dt>
          <dd>
            {agent && !agent.archived ? <Link to={`/agents/${encodeURIComponent(agent.id)}`}>{agent.name}</Link> : <span>{agent ? `${agent.name} (archived)` : "Unknown agent"}</span>}{" "}
            <span className="chip">{versionLabel(s, agent)}</span>
            {s.draft && <> <span className="chip">Draft</span></>}
          </dd>
        </div>
        <div>
          <dt>Path</dt>
          <dd>{path(traces, st, fallback).join(" → ")}</dd>
        </div>
        <div>
          <dt>Duration</dt>
          <dd>{clock(s.started_at, s.ended_at, now)}</dd>
        </div>
        <div>
          <dt>Caller</dt>
          <dd>{callerLabel(s.caller)}</dd>
        </div>
        {s.status === "ended" && (s.end_reason || ended) && (
          <div>
            <dt>Ended</dt>
            <dd>{endReasonText(s.end_reason || "ended_by_agent", ended ? label(ended.step) : undefined)}</dd>
          </div>
        )}
        <div>
          <dt>Turns</dt>
          <dd>{s.turn_count}</dd>
        </div>
        <div>
          <dt>Interrupted replies</dt>
          <dd>{interruptedCount(turns)}</dd>
        </div>
        {median !== null && (
          <div>
            <dt>Median reply</dt>
            <dd>{formatMs(median)}</dd>
          </div>
        )}
      </dl>

      {failed.map((f, i) => (
        <p key={i} role="note" className="call-note">
          A handoff to {label(f.to)} failed. The caller stayed with {label(f.from)}.
        </p>
      ))}
      {tracesQuery.isError && <p role="note" className="call-note">Steps and handoffs could not be loaded, so the transcript is not labelled by step.</p>}

      {s.status === "active" && <LiveCallControls key={sessionId} client={client} sessionId={sessionId} onEnded={() => void sessionQuery.refetch()} />}

      <div className="call-columns">
        <section aria-labelledby="call-turns-title" className="call-section call-columns__main">
          <h2 id="call-turns-title">Turns</h2>
          <p className="subtext">What each turn did, and how long the reply took. In a bar chart, empty space is time spent waiting, for the caller to finish or for tools.</p>
          {turnsQuery.isPending ? (
            <LoadingState label="Loading turns" />
          ) : (
            <ol className="call-turns">
              {turnRows(s, turns).map((r) => (
                <li key={r.id}>
                  <span className="call-turns__at">{r.at}</span>
                  <span className="call-turns__body">
                    <span className="call-turns__text">{r.text}</span>
                    {r.notes.map((n, i) => (
                      <span key={i} className="call-turns__note">
                        {n}
                      </span>
                    ))}
                    <ReplyWaterfall latencies={r.latencies} />
                  </span>
                </li>
              ))}
            </ol>
          )}
        </section>

        <aside aria-labelledby="call-transcript-title" className="call-section call-columns__side">
          <h2 id="call-transcript-title">Transcript</h2>
          {turnsQuery.isError ? (
            <ErrorState title="Could not load the transcript" error={turnsQuery.error as ApiError} onRetry={() => void turnsQuery.refetch()} reassure />
          ) : turnsQuery.isPending ? (
            <LoadingState label="Loading transcript" />
          ) : (
            <CallTranscript lines={transcript(s, turns, traces, st, fallback)} />
          )}
        </aside>
      </div>

      <details className="call-technical">
        <summary>Technical trace</summary>
        <dl className="call-figures">
          <div>
            <dt>Call id</dt>
            <dd>
              <code>{s.id}</code>
            </dd>
          </div>
          <div>
            <dt>Config hash</dt>
            <dd>
              <code>{s.config_hash || "none"}</code>
            </dd>
          </div>
          {s.safe_summary && (
            <div>
              <dt>Summary</dt>
              <dd>{s.safe_summary}</dd>
            </div>
          )}
        </dl>
        {s.runtime_tools && s.runtime_tools.length > 0 && (
          <section aria-label="Tools captured for this call">
            <h3>Tools captured for this call ({s.runtime_tools.length})</h3>
            <ul className="call-tools">
              {s.runtime_tools.map((t) => (
                <li key={`${t.mcp_server_id}/${t.remote_name}`}>
                  <strong>{t.remote_name}</strong> <span className="subtext">on {t.mcp_server_id.startsWith("http:") ? "an HTTP tool" : t.mcp_server_id}</span> <code>{t.schema_hash.slice(0, 8)}</code> {t.required ? "Required" : "Optional"}
                </li>
              ))}
            </ul>
          </section>
        )}
        <TurnTimeline turns={turns} traces={traces} loading={turnsQuery.isPending} />
      </details>
    </>,
    <>{stateBar}{started}</>,
  );
}
