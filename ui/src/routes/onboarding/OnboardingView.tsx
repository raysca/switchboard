import { useState, type FormEvent } from "react";
import { Link, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { asApiError, type ApiClient, type ApiError, type FlowDTO } from "../../api/client";
import { EmptyState, ErrorState, LoadingState, Status } from "../../components/primitives";
import { DEFAULT_JOB, flowFor, stacksFrom, type Stack } from "./firstRun";

export type OnboardingViewProps = { client: ApiClient; keyValue: string | null };

const STEPS = [
  { name: "Voice stack", sub: "Providers for each stage" },
  { name: "Flow", sub: "Name, job, voice" },
  { name: "Test call", sub: "Talk to it in the browser" },
  { name: "Connect", sub: "Token and web client" },
];

/** First run: pick a voice stack, make a flow, call it, connect a client. The step lives in the URL, so a reload resumes. */
export function OnboardingView({ client, keyValue }: OnboardingViewProps) {
  const queryClient = useQueryClient();
  const [params, setParams] = useSearchParams();
  const [stackId, setStackId] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [voice, setVoice] = useState("");
  const [job, setJob] = useState(DEFAULT_JOB);
  const [created, setCreated] = useState<FlowDTO | null>(null);
  const [createError, setCreateError] = useState<ApiError | null>(null);

  const providersQuery = useQuery({ queryKey: ["providers"], queryFn: client.getProviders, enabled: !!keyValue, retry: false });
  const flowsQuery = useQuery({ queryKey: ["flows"], queryFn: () => client.getFlows(), enabled: !!keyValue, retry: false });
  const create = useMutation({
    mutationFn: (stack: Stack) => client.createFlow(flowFor(stack, { name, job, voice })),
    onSuccess: (flow) => {
      setCreateError(null);
      setCreated(flow);
      void queryClient.invalidateQueries({ queryKey: ["flows"] });
      setParams({ step: "3", flow: flow.id });
    },
    onError: (err) => setCreateError(asApiError(err, "Failed to create flow")),
  });

  const head = (
    <header className="page-head">
      <h1 tabIndex={-1}>First run</h1>
      <span className="subtext">First call in about three minutes.</span>
    </header>
  );
  if (!keyValue) {
    return (
      <>
        {head}
        <EmptyState title="Connect to start first run">Enter a management key above to begin setup.</EmptyState>
      </>
    );
  }
  if (providersQuery.isPending || flowsQuery.isPending) return <>{head}<LoadingState label="Checking system configuration" skeleton={4} /></>;
  for (const q of [providersQuery, flowsQuery]) {
    if (q.isError) {
      return (
        <>
          {head}
          <ErrorState title="Setup check failed" error={q.error as ApiError} onRetry={() => void q.refetch()} reassure />
        </>
      );
    }
  }

  const providers = providersQuery.data ?? [];
  const detected = providers.filter((p) => p.detected);
  const flows = (flowsQuery.data ?? []).filter((f) => !f.archived);
  const stacks = stacksFrom(providers);
  const ready = stacks.filter((s) => s.ready);
  const stack = stacks.find((s) => s.provider.id === stackId && s.ready) ?? ready[0];
  const flowId = params.get("flow");
  const flow = (created && created.id === flowId ? created : flows.find((f) => f.id === flowId)) ?? null;

  const raw = params.get("step");
  const asked = raw === null ? null : Number(raw);
  let step = asked !== null && Number.isInteger(asked) && asked >= 1 && asked <= 4 ? asked : asked === null ? (flows.length > 0 && detected.length > 0 ? 0 : 1) : 1;
  if (step >= 3 && !flow) step = ready.length > 0 ? 2 : 1;
  if (step === 2 && !stack) step = 1;
  const go = (n: number, keep?: boolean) => setParams(keep && flow ? { step: String(n), flow: flow.id } : { step: String(n) });

  const nav = (
    <nav aria-label="Setup steps" className="setup-steps">
      <ol>
        {STEPS.map((s, i) => {
          const n = i + 1;
          const state = step === 0 ? "done" : n < step ? "done" : n === step ? "current" : "todo";
          return (
            <li key={s.name} aria-current={state === "current" ? "step" : undefined} data-state={state}>
              <span className="setup-steps__mark" aria-hidden="true">
                {state === "done" ? "✓" : n}
              </span>
              <span>
                <b>{s.name}</b>
                <span className="subtext">{s.sub}</span>
              </span>
            </li>
          );
        })}
      </ol>
    </nav>
  );

  if (step === 0) {
    return (
      <>
        {head}
        <div className="first-run">
          {nav}
          <div className="first-run__main">
            <section className="message-panel" role="status">
              <h2>System configured and ready</h2>
              <p>
                {detected.length} detected {detected.length === 1 ? "provider" : "providers"} and {flows.length} {flows.length === 1 ? "flow" : "flows"}.
              </p>
            </section>
            <div className="first-run__summary">
              <section>
                <h3>Providers ({detected.length})</h3>
                <ul>
                  {detected.map((p) => (
                    <li key={p.id}>{p.display_name}</li>
                  ))}
                </ul>
                <Link to="/providers" className="action-button-small">
                  View providers
                </Link>
              </section>
              <section>
                <h3>Flows ({flows.length})</h3>
                <ul>
                  {flows.map((f) => (
                    <li key={f.id}>
                      {f.name} <Status tone={f.readiness.state === "ready" ? "good" : "bad"}>{f.readiness.state === "ready" ? "Ready" : f.readiness.state}</Status>
                    </li>
                  ))}
                </ul>
                <div className="row">
                  <Link to="/flows" className="action-button-small">
                    Manage flows
                  </Link>
                  <Link to={`/onboarding?step=3&flow=${encodeURIComponent(flows[0].id)}`} className="action-button-small">
                    Test a flow
                  </Link>
                  <Link to={`/onboarding?step=4&flow=${encodeURIComponent(flows[0].id)}`} className="action-button-small">
                    Connect a client
                  </Link>
                </div>
              </section>
            </div>
          </div>
        </div>
      </>
    );
  }

  const submitFlow = (e: FormEvent) => {
    e.preventDefault();
    if (!stack || !name.trim() || !job.trim()) return;
    create.mutate(stack);
  };

  return (
    <>
      {head}
      <div className="first-run">
        {nav}
        <div className="first-run__main">
          {step === 1 && (
            <section aria-labelledby="fr-title" className="first-run__step">
              <p className="subtext">Step 1 of 4</p>
              <h2 id="fr-title">Choose how your flow hears, thinks and speaks.</h2>
              {stacks.length === 0 ? (
                <p>
                  No provider on this server covers speech-to-text, a language model and text-to-speech yet. Set one of these variables on the server and restart it:{" "}
                  {[...new Set(providers.flatMap((p) => p.env))].join(", ")}. <Link to="/providers">See providers</Link>
                </p>
              ) : (
                <>
                  <p>Provider keys are read from the server's environment, so there is nothing to paste here. Pick a stack that is ready, or set the variable it names and reload. You can change every part later.</p>
                  <div role="radiogroup" aria-label="Voice stack" className="stack-list">
                    {stacks.map((s) => (
                      <label key={s.provider.id} className="option-card" data-selected={stack?.provider.id === s.provider.id ? "true" : undefined}>
                        <input type="radio" name="voice-stack" checked={stack?.provider.id === s.provider.id} disabled={!s.ready} onChange={() => setStackId(s.provider.id)} />
                        <span>
                          <b>{s.provider.display_name}</b>
                          <span className="subtext stack-list__parts">
                            {s.stt.label} → {s.llm.label} → {s.tts.label}
                          </span>
                        </span>
                        <span className="subtext">{s.ready ? "Ready on this server" : `Set ${s.missing.join(", ")} on the server and restart it`}</span>
                      </label>
                    ))}
                  </div>
                </>
              )}
              <div className="first-run__foot">
                <span className="grow" />
                <span className="subtext">{stack ? "Ready to go" : "Set a provider key on the server to continue"}</span>
                <button type="button" className="action-button" disabled={!stack} onClick={() => go(2)}>
                  Continue
                </button>
              </div>
            </section>
          )}

          {step === 2 && stack && (
            <form onSubmit={submitFlow} className="first-run__step" aria-labelledby="fr-title" noValidate={false}>
              <p className="subtext">Step 2 of 4</p>
              <h2 id="fr-title">Tell it what job it's doing.</h2>
              <p>Plain sentences are enough. This becomes the flow's first step, using {stack.provider.display_name}.</p>
              {createError && <ErrorState title="Failed to create flow" error={createError} onRetry={() => setCreateError(null)} />}
              <div className="first-run__fields">
                <label className="form-field" htmlFor="fr-name">
                  <span>Name</span>
                  <input id="fr-name" type="text" required maxLength={128} value={name} autoComplete="off" onChange={(e) => setName(e.target.value)} />
                </label>
                <label className="form-field" htmlFor="fr-voice">
                  <span>Voice</span>
                  <select id="fr-voice" value={voice || stack.tts.voice} onChange={(e) => setVoice(e.target.value)}>
                    {stack.tts.voices.map((v) => (
                      <option key={v.id} value={v.id}>
                        {v.label}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="form-field first-run__wide" htmlFor="fr-job">
                  <span>What should it do?</span>
                  <textarea id="fr-job" rows={5} required value={job} onChange={(e) => setJob(e.target.value)} />
                </label>
              </div>
              <div className="first-run__foot">
                <button type="button" className="action-button-small" onClick={() => go(1)}>
                  Back
                </button>
                <span className="grow" />
                <span className="subtext">Creates the flow as v1 and opens a test call</span>
                <button type="submit" className="action-button" disabled={create.isPending}>
                  {create.isPending ? "Creating…" : "Create and call it"}
                </button>
              </div>
            </form>
          )}

          {step === 3 && flow && <p>Step 3</p>}
          {step === 4 && flow && <p>Step 4</p>}
        </div>
      </div>
    </>
  );
}
