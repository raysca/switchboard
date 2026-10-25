import { useState } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, asApiError, type ApiClient, type EvalCaseDTO, type EvalRunDetailDTO, type EvalRunDTO, type EvalScorerDTO } from "../../api/client";
import { DataTable, EmptyState, ErrorState, LoadingState } from "../../components/primitives";

type Judge = { provider: string; model?: string } | null;
type Providers = "offline" | "echo" | "keep";

// The server's message already says what to do for these; the others get a console-worded one.
const MESSAGES: Record<string, string> = {
  eval_queue_full: "This agent already has the most eval calls queued. Wait for the current runs to finish, then try again.",
  suite_too_large: "Cases times repeats is over 200, the most one agent can queue. Remove cases or lower Runs in the suite settings.",
  no_cases: "This agent has no test cases yet. Add one first.",
  agent_not_ready: "This agent cannot take calls yet (disabled, archived, or not published). Fix that in the agent editor.",
  duplicate_case_name: "A case with that name already exists for this agent. Rename it.",
  too_many_cases: "An agent keeps at most 200 cases. Delete some first.",
  payload_too_large: "That YAML is over 1 MiB. Split it into smaller batches.",
};
/** Keeps eval_live_required and invalid_case as the server worded them (they name the cause). */
export function evalError(err: unknown): ApiError {
  const e = asApiError(err, "Request failed");
  const m = MESSAGES[e.code];
  return m ? new ApiError(m, e.code, e.status, e.requestId, e.details) : e;
}

const pct = (v: number) => `${Math.round(v * 100)}%`;
const PROVIDER_LABELS: Record<Providers, string> = {
  offline: "Offline model",
  echo: "Echo (plumbing only)",
  keep: "Agent's own models (paid; needs GOVOICE_LIVE=1 on the server)",
};
const active = (status: string) => status === "pending" || status === "running";
const POLL_MS = 2000;

/** Provider select (LLM providers only) plus model; "None" is null. */
function JudgeFields({ id, judge, onChange, providers }: { id: string; judge: Judge; onChange: (j: Judge) => void; providers: string[] }) {
  const options = judge && !providers.includes(judge.provider) ? [judge.provider, ...providers] : providers;
  return (
    <>
      <label className="form-field" htmlFor={`${id}-provider`}>
        <span>Judge provider</span>
        <select id={`${id}-provider`} value={judge?.provider ?? ""} onChange={(e) => onChange(e.target.value ? { provider: e.target.value, model: judge?.model } : null)}>
          <option value="">None</option>
          {options.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
      </label>
      {judge && (
        <label className="form-field" htmlFor={`${id}-model`}>
          <span>Judge model</span>
          <input id={`${id}-model`} value={judge.model ?? ""} onChange={(e) => onChange({ provider: judge.provider, model: e.target.value || undefined })} />
        </label>
      )}
    </>
  );
}

function SuiteSettings({ client, agentId, providers }: { client: ApiClient; agentId: string; providers: string[] }) {
  const qc = useQueryClient();
  const suite = useQuery({ queryKey: ["eval", agentId, "suite"], queryFn: () => client.getEvalSuite(agentId), retry: false });
  const [draft, setDraft] = useState<{ runs: string; minPass: string; threshold: string; judge: Judge } | null>(null);
  const save = useMutation({
    mutationFn: () =>
      client.putEvalSuite(agentId, { runs: Number(form.runs), min_pass: Number(form.minPass), pass_threshold: Number(form.threshold) / 100, judge: form.judge }),
    onSuccess: () => {
      setDraft(null);
      return qc.invalidateQueries({ queryKey: ["eval"] });
    },
  });
  if (!suite.data) {
    return suite.isError ? <ErrorState title="Could not load suite settings" error={suite.error as ApiError} onRetry={() => void suite.refetch()} /> : <LoadingState label="Loading suite settings" />;
  }
  const s = suite.data;
  const form = draft ?? { runs: String(s.runs), minPass: String(s.min_pass), threshold: String(Math.round(s.pass_threshold * 100)), judge: s.judge };
  const set = (patch: Partial<typeof form>) => setDraft({ ...form, ...patch });
  return (
    <form
      aria-label="Suite settings"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <h2>Suite settings</h2>
      <div className="row">
        <label className="form-field" htmlFor="suite-runs">
          <span>Runs per case (1 to 5)</span>
          <input id="suite-runs" type="number" min={1} max={5} value={form.runs} onChange={(e) => set({ runs: e.target.value })} />
        </label>
        <label className="form-field" htmlFor="suite-min-pass">
          <span>Passes needed per case</span>
          <input id="suite-min-pass" type="number" min={1} max={5} value={form.minPass} onChange={(e) => set({ minPass: e.target.value })} />
        </label>
        <label className="form-field" htmlFor="suite-threshold">
          <span>Pass threshold (%)</span>
          <input id="suite-threshold" type="number" min={0} max={100} value={form.threshold} onChange={(e) => set({ threshold: e.target.value })} />
        </label>
        <JudgeFields id="suite-judge" judge={form.judge} onChange={(judge) => set({ judge })} providers={providers} />
      </div>
      <p className="subtext">Without a judge, correctness, answer relevancy and toxicity are recorded as skipped.</p>
      {save.isError && <ErrorState title="Could not save suite settings" error={evalError(save.error)} />}
      <button type="submit" className="action-button" disabled={save.isPending}>
        Save suite settings
      </button>
    </form>
  );
}

function ScorerRow({ client, agentId, scorer, providers }: { client: ApiClient; agentId: string; scorer: EvalScorerDTO; providers: string[] }) {
  const qc = useQueryClient();
  const [threshold, setThreshold] = useState(scorer.threshold === undefined ? "" : String(Math.round(scorer.threshold * 100)));
  const [judge, setJudge] = useState<Judge>(scorer.judge ?? null);
  const save = useMutation({
    mutationFn: () => client.putEvalScorer(agentId, scorer.name, { ...(threshold === "" ? {} : { threshold: Number(threshold) / 100 }), ...(scorer.judged ? { judge } : {}) }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval", agentId, "scorers"] }),
  });
  return (
    <li>
      <form
        aria-label={`Scorer ${scorer.name}`}
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <strong>{scorer.name}</strong> <span className="subtext">{scorer.judged ? "model-graded" : "deterministic"}</span>
        <div className="row">
          <label className="form-field" htmlFor={`scorer-${scorer.name}-threshold`}>
            <span>Threshold (%), default {pct(scorer.default_threshold)}</span>
            <input id={`scorer-${scorer.name}-threshold`} type="number" min={0} max={100} value={threshold} onChange={(e) => setThreshold(e.target.value)} />
          </label>
          {scorer.judged && <JudgeFields id={`scorer-${scorer.name}-judge`} judge={judge} onChange={setJudge} providers={providers} />}
          <button type="submit" className="action-button-small" disabled={save.isPending}>
            Save {scorer.name}
          </button>
        </div>
        {save.isError && <ErrorState title={`Could not save ${scorer.name}`} error={evalError(save.error)} />}
      </form>
    </li>
  );
}

const outcome = (s: { outcome: string; reason: string }) => (s.reason ? `${s.outcome} — ${s.reason}` : s.outcome);

function RunDetail({ detail }: { detail: EvalRunDetailDTO }) {
  return (
    <DataTable
      caption="Case runs"
      columns={["Case", "Repeat", "Result", "Scores", "Call"]}
      rows={detail.case_runs.map((c) => [
        c.case_name,
        String(c.repeat + 1),
        active(c.status) ? c.status : c.status === "dead" ? `Harness error: ${c.error_category ?? "unknown"}` : c.passed ? "Passed" : "Failed",
        c.scores.length === 0 ? (
          "—"
        ) : (
          <ul key="s" className="plain-list">
            {c.scores.map((s) => (
              <li key={s.scorer}>
                {s.scorer}: {outcome(s)}
              </li>
            ))}
          </ul>
        ),
        c.session_id ? <Link key="c" to={`/sessions/${encodeURIComponent(c.session_id)}`}>Open call</Link> : "—",
      ])}
    />
  );
}

export type TestSuiteTabProps = { client: ApiClient; agentId: string; agentName: string; currentVersion: number | null };

export function TestSuiteTab({ client, agentId, agentName, currentVersion }: TestSuiteTabProps) {
  const qc = useQueryClient();
  const refresh = () => qc.invalidateQueries({ queryKey: ["eval"] });
  const [editing, setEditing] = useState<EvalCaseDTO | null>(null);
  const [yaml, setYaml] = useState("");
  const [version, setVersion] = useState(currentVersion);
  const [providers, setProviders] = useState<Providers>("offline");
  const [openRun, setOpenRun] = useState<string | null>(null);

  const cases = useQuery({ queryKey: ["eval", agentId, "cases"], queryFn: () => client.listEvalCases(agentId), retry: false });
  // Polling is bounded: a fixed interval that stops once no run is pending or running, and ends with the component.
  const runs = useQuery({
    queryKey: ["eval", agentId, "runs"],
    queryFn: () => client.listEvalRuns(agentId),
    retry: false,
    refetchInterval: (q) => (q.state.data?.some((r) => active(r.status)) ? POLL_MS : false),
  });
  const llmProviders = useQuery({ queryKey: ["providers"], queryFn: () => client.getProviders(), retry: false });
  const scorers = useQuery({ queryKey: ["eval", agentId, "scorers"], queryFn: () => client.getEvalScorers(agentId), retry: false });

  const runList: EvalRunDTO[] = runs.data ?? [];
  const lastDone = runList.find((r) => r.status === "done");
  const shown = openRun ?? runList[0]?.id ?? null;
  const detail = useQuery({
    queryKey: ["eval", agentId, "run", shown],
    queryFn: () => client.getEvalRun(agentId, shown!),
    enabled: !!shown,
    retry: false,
    refetchInterval: (q) => (!q.state.data || active(q.state.data.run.status) ? POLL_MS : false),
  });
  const lastDoneDetail = useQuery({
    queryKey: ["eval", agentId, "run", lastDone?.id],
    queryFn: () => client.getEvalRun(agentId, lastDone!.id),
    enabled: !!lastDone,
    retry: false,
  });

  const saveCase = useMutation({
    mutationFn: async () => {
      if (editing) await client.updateEvalCase(agentId, editing.id, yaml);
      else await client.createEvalCases(agentId, yaml);
    },
    onSuccess: () => {
      setEditing(null);
      setYaml("");
      return refresh();
    },
  });
  const del = useMutation({ mutationFn: (id: string) => client.deleteEvalCase(agentId, id), onSuccess: refresh });
  const start = useMutation({
    mutationFn: (caseId?: string) =>
      client.startEvalRun(agentId, { ...(version ? { version } : {}), providers, ...(caseId ? { case_id: caseId } : {}) }),
    onSuccess: (run) => {
      setOpenRun(run.id);
      return refresh();
    },
  });
  const exportCases = useMutation({
    mutationFn: async () => {
      const blob = new Blob([await client.exportEvalCases(agentId)], { type: "application/x-yaml" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `${agentName}-cases.yaml`;
      a.click();
      URL.revokeObjectURL(url);
    },
  });

  const lastResult = (caseId: string) => {
    const mine = lastDoneDetail.data?.case_runs.filter((c) => c.case_id === caseId) ?? [];
    return mine.length ? `${mine.filter((c) => c.passed).length} of ${mine.length} runs passed` : "Not run yet";
  };
  const llm = (llmProviders.data ?? []).filter((p) => p.capabilities.includes("llm")).map((p) => p.id);

  return (
    <>
      <p role="note" className="note">
        Eval calls run this agent's real tools. Point suites at agents whose tools reach test systems.
      </p>

      <section aria-label="Cases">
        <h2>Cases</h2>
        {!cases.data ? (
          cases.isError ? <ErrorState title="Could not load cases" error={cases.error as ApiError} onRetry={() => void cases.refetch()} reassure /> : <LoadingState label="Loading cases" skeleton={3} />
        ) : cases.data.length === 0 ? (
          <EmptyState title="No test cases yet">Paste a case as YAML below. A case is a scripted or persona caller and what the agent must (or must not) do.</EmptyState>
        ) : (
          <DataTable
            caption="Test cases"
            columns={["Case", "Caller", "Latest result", "Actions"]}
            rows={cases.data.map((c) => [
              c.name,
              "persona" in c.caller ? "persona" : "script",
              lastResult(c.id),
              <span key="a" className="row">
                <button type="button" className="action-button-small" aria-label={`Run ${c.name}`} disabled={start.isPending} onClick={() => start.mutate(c.id)}>
                  Run
                </button>
                <button
                  type="button"
                  className="action-button-small"
                  aria-label={`Edit ${c.name}`}
                  onClick={() => {
                    setEditing(c);
                    setYaml(c.yaml);
                  }}
                >
                  Edit
                </button>
                <button
                  type="button"
                  className="action-button-small action-button-danger"
                  aria-label={`Delete ${c.name}`}
                  disabled={del.isPending}
                  onClick={() => window.confirm(`Delete the case ${c.name}?`) && del.mutate(c.id)}
                >
                  Delete
                </button>
              </span>,
            ])}
          />
        )}
        {del.isError && <ErrorState title="Could not delete the case" error={evalError(del.error)} />}
        <form
          aria-label="Case editor"
          onSubmit={(e) => {
            e.preventDefault();
            saveCase.mutate();
          }}
        >
          <label className="form-field" htmlFor="case-yaml">
            <span>{editing ? `Edit case ${editing.name}` : "Add cases"}</span>
            <textarea id="case-yaml" className="mono" rows={10} value={yaml} onChange={(e) => setYaml(e.target.value)} placeholder={"name: books-a-visit\ncaller:\n  script: [\"I need a visit\"]\nexpect:\n  say: [\"(?i)visit\"]\n---\nname: another-case\n..."} />
          </label>
          <p className="subtext">YAML in the format the CLI reads. Separate several cases with a line of three dashes.</p>
          {saveCase.isError && <ErrorState title="Could not save the case" error={evalError(saveCase.error)} />}
          <div className="row">
            <button type="submit" className="action-button" disabled={saveCase.isPending || yaml.trim() === ""}>
              {editing ? "Save case" : "Add cases"}
            </button>
            {editing && (
              <button
                type="button"
                className="action-button-small"
                onClick={() => {
                  setEditing(null);
                  setYaml("");
                }}
              >
                Cancel
              </button>
            )}
            <button type="button" className="action-button-small" disabled={exportCases.isPending} onClick={() => exportCases.mutate()}>
              Export YAML
            </button>
          </div>
          {exportCases.isError && <ErrorState title="Could not export" error={evalError(exportCases.error)} />}
        </form>
      </section>

      <SuiteSettings client={client} agentId={agentId} providers={llm} />

      <section aria-label="Scorers">
        <h2>Scorers</h2>
        {!scorers.data ? (
          scorers.isError ? <ErrorState title="Could not load scorers" error={scorers.error as ApiError} onRetry={() => void scorers.refetch()} /> : <LoadingState label="Loading scorers" />
        ) : (
          <ul className="plain-list">
            {scorers.data.map((s) => (
              <ScorerRow key={`${s.name}-${s.threshold}-${s.judge?.provider}-${s.judge?.model}`} client={client} agentId={agentId} scorer={s} providers={llm} />
            ))}
          </ul>
        )}
      </section>

      <section aria-label="Runs">
        <h2>Runs</h2>
        <div className="row">
          <label className="form-field" htmlFor="run-version">
            <span>Version</span>
            <select id="run-version" value={version ?? ""} disabled={currentVersion === null} onChange={(e) => setVersion(Number(e.target.value))}>
              {Array.from({ length: currentVersion ?? 0 }, (_, i) => currentVersion! - i).map((n) => (
                <option key={n} value={n}>
                  v{n}
                </option>
              ))}
            </select>
          </label>
          <label className="form-field" htmlFor="run-providers">
            <span>Models</span>
            <select id="run-providers" value={providers} onChange={(e) => setProviders(e.target.value as Providers)}>
              {(Object.keys(PROVIDER_LABELS) as Providers[]).map((p) => (
                <option key={p} value={p}>
                  {PROVIDER_LABELS[p]}
                </option>
              ))}
            </select>
          </label>
          <button type="button" className="action-button" disabled={start.isPending} onClick={() => start.mutate(undefined)}>
            Run suite
          </button>
        </div>
        {providers === "keep" && (
          <p role="note" className="note note--bad">
            This calls the agent's own models. It may spend money, and the server refuses it unless it was started with GOVOICE_LIVE=1.
          </p>
        )}
        {start.isError && <ErrorState title="Could not start the run" error={evalError(start.error)} />}

        {!runs.data ? (
          runs.isError ? <ErrorState title="Could not load runs" error={runs.error as ApiError} onRetry={() => void runs.refetch()} reassure /> : <LoadingState label="Loading runs" skeleton={2} />
        ) : runList.length === 0 ? (
          <EmptyState title="No runs yet">Run the suite to see results here.</EmptyState>
        ) : (
          <DataTable
            caption="Runs"
            columns={["Started", "Version", "Status", "Passed", "Started by", ""]}
            rows={runList.map((r) => [
              new Date(r.created_at).toLocaleString(),
              r.draft ? "draft" : `v${r.version ?? "?"}`,
              r.status,
              r.status === "done" && r.total_cases !== undefined ? `${r.passed_cases ?? 0} of ${r.total_cases} cases` : "—",
              r.triggered_by,
              <button key="v" type="button" className="action-button-small" aria-label={`View run from ${new Date(r.created_at).toLocaleString()}`} aria-pressed={shown === r.id} onClick={() => setOpenRun(r.id)}>
                View
              </button>,
            ])}
          />
        )}

        {shown &&
          (detail.data ? (
            <section aria-label="Run detail">
              <h3>Run detail: {detail.data.run.status}</h3>
              <RunDetail detail={detail.data} />
            </section>
          ) : detail.isError ? (
            <ErrorState title="Could not load the run" error={detail.error as ApiError} onRetry={() => void detail.refetch()} reassure />
          ) : (
            <LoadingState label="Loading run" />
          ))}
      </section>
    </>
  );
}
