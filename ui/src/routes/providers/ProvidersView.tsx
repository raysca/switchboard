import type { ReactNode } from "react";
import { Link, useSearchParams } from "react-router";
import { useMutation, useQuery } from "@tanstack/react-query";
import type { FlowDTO, ApiClient, ApiError, ProviderStatus, ProviderTest } from "../../api/client";
import { EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import {
  SERVICES,
  SERVICE_NAMES,
  catalogLines,
  credential,
  flowsUsing,
  hasService,
  missingEnv,
  serviceList,
  serviceState,
  type Service,
} from "./providerStats";

export type ProvidersViewProps = { client: ApiClient; keyValue: string | null };

// GET /flows returns at most this many flows; a count that reaches it is a floor.
const FLOW_CAP = 100;

const CIRCUIT_TEXT: Partial<Record<ProviderTest["results"][number]["circuit"], string>> = { open: "Circuit open", half_open: "Circuit half open" };

function resultText(r: ProviderTest["results"][number]): string {
  const base =
    r.status === "ok" ? (r.latency_ms !== undefined ? `OK, ${r.latency_ms} ms` : "OK") : r.status === "skipped" ? `Skipped: ${r.detail ?? ""}` : `Error: ${r.detail ?? ""}`;
  const circuit = CIRCUIT_TEXT[r.circuit];
  return `${SERVICE_NAMES[r.stage]} — ${base}${circuit ? ` · ${circuit}` : ""}`;
}

/** One click sends one tiny real request per service; nothing is asked of the provider until it is pressed. */
function ConnectionTest({ p, client }: { p: ProviderStatus; client: ApiClient }) {
  const test = useMutation({ mutationFn: () => client.testProvider(p.id) });
  const cred = credential(p);
  return (
    <section aria-labelledby="provider-test">
      <h3 id="provider-test">Connection</h3>
      <button type="button" className="action-button-small" disabled={!cred.ok || test.isPending} onClick={() => test.mutate()}>
        {test.isPending ? "Testing…" : "Test connection"}
      </button>
      {!cred.ok && <p className="subtext">No credential. Set the variable first.</p>}
      {cred.ok && !test.data && !test.isError && <p className="subtext">Sends one tiny request per service, using the key in the server's environment.</p>}
      {test.isError && (
        <p role="alert" className="note note--bad">
          {(test.error as ApiError).message}
        </p>
      )}
      {test.data && (
        <>
          <ul className="provider-flows" aria-label="Connection test results" aria-live="polite">
            {test.data.results.map((r) => (
              <li key={r.stage}>{resultText(r)}</li>
            ))}
          </ul>
          <p className="subtext">Checked {new Date(test.data.checked_at).toLocaleTimeString()}</p>
        </>
      )}
    </section>
  );
}

function Panel({ p, flows, flowsCapped, client }: { p: ProviderStatus; flows: FlowDTO[] | undefined; flowsCapped: boolean; client: ApiClient }) {
  const cred = credential(p);
  const missing = new Set(missingEnv(p));
  const using = flows ? flowsUsing(flows, p.id) : undefined;
  return (
    <aside aria-labelledby="provider-title" className="provider-panel">
      <div>
        <p className="subtext">Provider</p>
        <h2 id="provider-title">{p.display_name}</h2>
        <p className="provider-panel__health">
          <span className="lamp" data-tone={cred.ok ? "ok" : undefined} aria-hidden="true" />
          {cred.ok ? "Credential found in the server environment." : "No credential. Flows cannot use it yet."}
        </p>
      </div>

      <section aria-labelledby="provider-credential">
        <h3 id="provider-credential">Credential</h3>
        <ul className="provider-env">
          {p.env.map((name) => (
            <li key={name}>
              <code>{name}</code>
              {/* The API names the variables still missing; it cannot say that another one is set. */}
              {missing.has(name) && <span className="subtext">Not set</span>}
            </li>
          ))}
        </ul>
        <p className="subtext">Set it in the server's environment and restart the server. Keys are never stored or shown here.</p>
      </section>

      <section aria-labelledby="provider-catalog">
        <h3 id="provider-catalog">Catalog</h3>
        <dl className="provider-catalog">
          {catalogLines(p).map((c) => {
            const state = serviceState(p, c.service);
            return (
              <div key={c.service}>
                <dt>{SERVICE_NAMES[c.service]}</dt>
                <dd>
                  {c.text}
                  {state && (
                    <span className="pill" data-state={state.state}>
                      {state.text}
                    </span>
                  )}
                </dd>
              </div>
            );
          })}
        </dl>
      </section>

      <ConnectionTest p={p} client={client} />

      <section aria-labelledby="provider-used-by">
        <h3 id="provider-used-by">Used by</h3>
        {using === undefined ? (
          <p className="subtext">The flows could not be loaded, so which flows use this provider is unknown.</p>
        ) : using.length === 0 ? (
          <p className="subtext">{flowsCapped ? `No flow among the first ${FLOW_CAP} uses this provider; later flows were not checked.` : "No flow uses this provider yet."}</p>
        ) : (
          <>
            <ul className="provider-flows">
              {using.map((f) => (
                <li key={f.id}>
                  <Link to={`/flows/${encodeURIComponent(f.id)}`}>{f.name}</Link>
                </li>
              ))}
            </ul>
            {flowsCapped && <p className="subtext">Only the first {FLOW_CAP} flows were checked.</p>}
          </>
        )}
      </section>
    </aside>
  );
}

/** Catalog providers, and what each one needs to be usable. Read-only: the server reads keys from its environment. */
export function ProvidersView({ client, keyValue }: ProvidersViewProps) {
  const [params, setParams] = useSearchParams();
  const providersQuery = useQuery({ queryKey: ["providers"], queryFn: client.getProviders, enabled: !!keyValue, retry: false });
  const flowsQuery = useQuery({ queryKey: ["flows"], queryFn: () => client.getFlows(), enabled: !!keyValue, retry: false });

  const rawService = params.get("service");
  const service: Service | "" = SERVICES.find((s) => s === rawService) ?? "";
  const setParam = (next: { provider?: string; service?: Service | "" }) => {
    const p = new URLSearchParams(params);
    if (next.provider !== undefined) p.set("provider", next.provider);
    if (next.service !== undefined) {
      if (next.service) p.set("service", next.service);
      else p.delete("service");
    }
    setParams(p, { replace: true });
  };

  const head = (summary?: string) => (
    <header className="page-head">
      <h1 tabIndex={-1}>Providers</h1>
      {summary && <span className="subtext">{summary}</span>}
    </header>
  );

  // The page uses the full width of the workspace, like the canvas: the list on the left, the panel as a column on the right.
  const page = (body: ReactNode) => <div className="providers-page">{body}</div>;
  if (!keyValue) {
    return page(
      <>
        {head()}
        <div className="providers-note">
          <EmptyState title="Connect to view providers">Enter a management key above to see which providers the server detected.</EmptyState>
        </div>
      </>,
    );
  }
  if (providersQuery.isPending) {
    return page(
      <>
        {head()}
        <div className="providers-note">
          <LoadingState label="Loading providers" skeleton={6} />
        </div>
      </>,
    );
  }
  if (providersQuery.isError) {
    return page(
      <>
        {head()}
        <div className="providers-note">
          <ErrorState title="Failed to load providers" error={providersQuery.error as ApiError} onRetry={() => void providersQuery.refetch()} reassure />
        </div>
      </>,
    );
  }

  const providers = providersQuery.data;
  const shown = service ? providers.filter((p) => hasService(p, service)) : providers;
  const selected = providers.find((p) => p.id === params.get("provider")) ?? providers.find((p) => p.detected && p.id !== "echo") ?? providers.find((p) => p.detected) ?? providers[0];
  const flows = flowsQuery.data;
  const capped = (flows?.length ?? 0) >= FLOW_CAP;
  const detected = providers.filter((p) => p.detected).length;
  const countFor = (p: ProviderStatus) => {
    if (!flows) return "—";
    const n = flowsUsing(flows, p.id).length;
    return capped ? `${n}+` : String(n);
  };

  return page(
    <>
      <div className="providers-layout">
        <section aria-label="Providers" className="providers-main">
        {head(`${detected} of ${providers.length} providers detected. Keys come from the server's environment and are never stored or shown here.`)}
        {detected === 0 && (
          <div className="providers-note">
            <EmptyState title="No providers detected">
              Set at least one of these environment variables on the server and restart it: {[...new Set(providers.flatMap((p) => p.env))].join(", ")}.
            </EmptyState>
          </div>
        )}
          <div role="group" aria-label="Filter by service" className="chips">
            <button type="button" className="chip-button" aria-pressed={service === ""} onClick={() => setParam({ service: "" })}>
              All
            </button>
            {SERVICES.map((s) => (
              <button key={s} type="button" className="chip-button" aria-pressed={service === s} onClick={() => setParam({ service: s })}>
                {SERVICE_NAMES[s]}
              </button>
            ))}
          </div>
          <div className="calls-table">
            <table>
              <caption className="visually-hidden">Providers and the services they unlock</caption>
              <thead>
                <tr>
                  <th scope="col">Provider</th>
                  <th scope="col">Services</th>
                  <th scope="col">Credential</th>
                  <th scope="col" className="num">
                    Flows
                  </th>
                </tr>
              </thead>
              <tbody>
                {shown.map((p) => {
                  const on = selected?.id === p.id;
                  const cred = credential(p);
                  return (
                    <tr key={p.id} data-selected={on || undefined}>
                      <td>
                        <button type="button" className="provider-pick" aria-pressed={on} onClick={() => setParam({ provider: p.id })}>
                          <span className="lamp" data-tone={cred.ok ? "ok" : undefined} aria-hidden="true" />
                          {p.display_name}
                        </button>
                      </td>
                      <td>{serviceList(p)}</td>
                      <td>{cred.text}</td>
                      <td className="num">{countFor(p)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <p className="subtext">
            Catalog providers are defined in YAML and can be overlaid per deployment. An OpenAI-compatible provider with OPENAI_BASE_URL set covers Groq, Together and local servers.
          </p>
        </section>
        {selected && <Panel key={selected.id} p={selected} flows={flows} flowsCapped={capped} client={client} />}
      </div>
    </>,
  );
}
