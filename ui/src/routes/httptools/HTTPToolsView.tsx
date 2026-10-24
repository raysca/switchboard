import { useId, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, type AgentDTO, type ApiClient, type HTTPToolDTO, httpToolServerId } from "../../api/client";
import { Modal } from "../../components/Modal";
import { EmptyState, ErrorState, LoadingState, Status } from "../../components/primitives";
import { referencedAgents } from "../mcp/mcpStats";
import { METHODS, emptyForm, fromForm, toForm, type HTTPToolForm } from "./httpToolForm";

export type HTTPToolsViewProps = { client: ApiClient; keyValue: string | null };

/** The text of a refused definition: the server's reason is already a sentence. */
function apiMessage(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    const reason = (err.details as { reason?: unknown } | undefined)?.reason;
    const text = err.code === "invalid_http_tool" && typeof reason === "string" ? reason : err.message;
    return err.requestId ? `${text} (Request ID: ${err.requestId})` : text;
  }
  return err instanceof Error ? err.message : fallback;
}

const host = (url: string) => {
  try {
    return new URL(url.replace(/\{[a-z0-9_]+\}/g, "x")).host;
  } catch {
    return url;
  }
};

/** HTTP tools on the left, the selected one (or a new one) on the right: its definition, a test call, and what it affects. */
export function HTTPToolsView({ client, keyValue }: HTTPToolsViewProps) {
  const { toolId } = useParams();
  const toolsQuery = useQuery({ queryKey: ["http-tools"], queryFn: () => client.getHTTPTools(), enabled: !!keyValue, retry: false });
  const secretsQuery = useQuery({ queryKey: ["secrets"], queryFn: () => client.getSecrets(), enabled: !!keyValue, retry: false });
  const agentsQuery = useQuery({ queryKey: ["agents"], queryFn: () => client.getAgents(), enabled: !!keyValue, retry: false });

  const head = (
    <header className="page-head">
      <h1 tabIndex={-1}>HTTP tools</h1>
      <span className="subtext">Web endpoints an agent can call mid-conversation, without writing an MCP server.</span>
      <span className="grow" />
      {keyValue && (
        <Link to="/http-tools/new" className="action-button">
          New HTTP tool
        </Link>
      )}
    </header>
  );
  const page = (body: ReactNode) => (
    <div className="mcp-page">
      {head}
      {body}
    </div>
  );

  if (!keyValue) return page(<EmptyState title="Connect to see HTTP tools">Enter a management key above to see and edit HTTP tools.</EmptyState>);
  if (toolsQuery.isPending) return page(<LoadingState label="Loading HTTP tools" />);
  if (toolsQuery.isError) return page(<ErrorState title="Could not load HTTP tools" error={toolsQuery.error as ApiError} onRetry={() => void toolsQuery.refetch()} reassure />);

  const tools = toolsQuery.data;
  const creating = toolId === "new";
  const selected = creating ? undefined : tools.find((t) => t.id === toolId) ?? (toolId ? undefined : tools[0]);
  if (tools.length === 0 && !creating) {
    return page(
      <section className="message-panel">
        <h2>No HTTP tools yet.</h2>
        <p>Add a tool to let an agent call a web endpoint: a URL, the arguments the model may send, and an optional secret for authentication.</p>
      </section>,
    );
  }
  return page(
    <div className="mcp-layout">
      <nav aria-label="HTTP tools" className="mcp-side">
        <ul className="mcp-list">
          {tools.map((t) => {
            const noSecret = t.definition.auth && t.auth_secret_set === false;
            const tone = noSecret ? "bad" : t.enabled ? "ok" : undefined;
            return (
              <li key={t.id}>
                <Link to={`/http-tools/${encodeURIComponent(t.id)}`} aria-current={selected?.id === t.id ? "page" : undefined} className="mcp-item">
                  <span className="lamp" data-tone={tone} aria-hidden="true" />
                  <b>{t.name}</b>
                  <span className="mcp-item__count">{t.definition.method}</span>
                  <span className="mcp-item__note" data-tone={tone}>
                    {noSecret ? `Secret ${t.definition.auth?.secret} is not set` : t.enabled ? host(t.definition.url) : "Disabled"}
                  </span>
                </Link>
              </li>
            );
          })}
        </ul>
      </nav>
      <div className="mcp-main">
        {!creating && !selected ? (
          <section className="agent-missing">
            <p role="status">That HTTP tool was not found.</p>
            <Link to="/http-tools" className="action-button-small">
              Back to HTTP tools
            </Link>
          </section>
        ) : (
          <ToolEditor key={selected?.id ?? "new"} client={client} tool={selected} secretNames={secretsQuery.data?.secrets.map((s) => s.name) ?? []} waiting={selected ? waitingAgents(agentsQuery.data, selected.id) : []} />
        )}
      </div>
    </div>,
  );
}

/** Agents that pinned an older definition of the tool and need to choose it again. */
function waitingAgents(agents: AgentDTO[] | undefined, toolId: string): AgentDTO[] {
  const ref = httpToolServerId(toolId);
  return (agents ?? []).filter((a) => !a.archived && (a.readiness.tool_dependencies ?? []).some((d) => d.mcp_server_id === ref && d.state === "tool_edited"));
}

function ToolEditor({ client, tool, secretNames, waiting }: { client: ApiClient; tool?: HTTPToolDTO; secretNames: string[]; waiting: AgentDTO[] }) {
  const uid = useId();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [form, setForm] = useState<HTTPToolForm>(() => (tool ? toForm(tool.definition) : emptyForm()));
  const [problem, setProblem] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const set = (patch: Partial<HTTPToolForm>) => setForm((f) => ({ ...f, ...patch }));
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["http-tools"] });
    void queryClient.invalidateQueries({ queryKey: ["agents"] });
  };

  const save = useMutation({
    mutationFn: async () => {
      const parsed = fromForm(form);
      if ("error" in parsed) throw new Error(parsed.error);
      return tool ? client.updateHTTPTool(tool.id, parsed.definition) : client.createHTTPTool(parsed.definition);
    },
    onSuccess: (saved) => {
      setProblem(null);
      refresh();
      navigate(`/http-tools/${encodeURIComponent(saved.id)}`);
    },
    onError: (err) => setProblem(apiMessage(err, "Could not save the tool")),
  });
  const toggle = useMutation({
    mutationFn: () => client.setHTTPToolEnabled(tool!.id, !tool!.enabled),
    onSuccess: refresh,
    onError: (err) => setProblem(apiMessage(err, "Could not change the tool")),
  });
  const remove = useMutation({
    mutationFn: () => client.deleteHTTPTool(tool!.id),
    onSuccess: () => {
      refresh();
      navigate("/http-tools");
    },
    onError: (err) => {
      const agents = err instanceof ApiError && err.code === "http_tool_in_use" ? referencedAgents(err.details) : [];
      const inUse = err instanceof ApiError && err.code === "http_tool_in_use";
      setDeleteError(agents.length ? `Still used by: ${agents.map((a) => a.name).join(", ")}. Remove it from those agents first.` : inUse ? "Older versions of an agent still use this tool. Disable it instead." : apiMessage(err, "Could not delete the tool"));
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    save.mutate();
  }
  const field = (label: string, key: keyof HTTPToolForm, extra?: { placeholder?: string; hint?: ReactNode; list?: string; disabled?: boolean }) => (
    <div className="form-field">
      <label htmlFor={`${uid}-${key}`}>{label}</label>
      <input id={`${uid}-${key}`} type="text" value={form[key]} placeholder={extra?.placeholder} list={extra?.list} disabled={extra?.disabled} autoComplete="off" onChange={(e) => set({ [key]: e.target.value })} />
      {extra?.hint && <small>{extra.hint}</small>}
    </div>
  );
  const area = (label: string, key: keyof HTTPToolForm, rows: number, hint?: ReactNode, mono = false) => (
    <div className="form-field">
      <label htmlFor={`${uid}-${key}`}>{label}</label>
      <textarea id={`${uid}-${key}`} rows={rows} value={form[key]} spellCheck={false} className={mono ? "mono" : undefined} onChange={(e) => set({ [key]: e.target.value })} />
      {hint && <small>{hint}</small>}
    </div>
  );

  return (
    <section className="http-tool" aria-label={tool ? tool.name : "New HTTP tool"}>
      {waiting.length > 0 && (
        <p role="status" className="note note--bad">
          Edited since these agents chose it, so they need to choose it again: {waiting.map((a, i) => [i > 0 && ", ", <Link key={a.id} to={`/agents/${encodeURIComponent(a.id)}`}>{a.name}</Link>])}.
        </p>
      )}
      <form onSubmit={submit} className="dialog__form" noValidate>
        {problem && (
          <p role="alert" className="tool-console__error">
            {problem}
          </p>
        )}
        {field("Name", "name", { placeholder: "get_order", disabled: !!tool, hint: tool ? "A tool's name cannot be changed: agents refer to it by name." : "What the model calls the tool: lower-case letters, digits and _." })}
        {area("Description", "description", 2, "Tells the model when to use the tool.")}
        <div className="form-field">
          <label htmlFor={`${uid}-method`}>Method</label>
          <select id={`${uid}-method`} value={form.method} onChange={(e) => set({ method: e.target.value })}>
            {METHODS.map((m) => (
              <option key={m}>{m}</option>
            ))}
          </select>
        </div>
        {field("URL", "url", { placeholder: "https://api.example.com/orders/{order_id}", hint: "https only. {name} in the path is filled from the model's arguments. Other arguments become the query string (GET, DELETE) or the JSON body." })}
        {area("Input schema", "schema", 8, "JSON Schema of the arguments the model may send. Every {placeholder} must be a required string or integer property.", true)}
        {area("Static headers", "headers", 2, "One per line, Name: value. Not for credentials; use a secret.", true)}
        <datalist id={`${uid}-secrets`}>
          {secretNames.map((n) => (
            <option key={n} value={n} />
          ))}
        </datalist>
        {field("Secret (optional)", "authSecret", { placeholder: "crm-api", list: `${uid}-secrets`, hint: <>The name of a secret on the <Link to="/secrets">Secrets</Link> page, or an HTTP_TOKEN_ variable. Sent as the Authorization bearer token unless you name another header.</> })}
        {field("Header for the secret (optional)", "authHeader", { placeholder: "Authorization" })}
        {field("Timeout, ms (optional)", "timeoutMs", { placeholder: "10000", hint: "Default 10 s, at most 30 s." })}
        {field("Response limit, bytes (optional)", "maxResponseBytes", { placeholder: "65536", hint: "A larger answer fails instead of being cut. At most 256 KiB." })}
        {area("Keep only these parts (optional)", "pick", 2, "JSON pointers, one per line, such as /data/status. Everything else in a JSON answer is dropped.", true)}
        <div className="dialog__actions">
          {tool && (
            <>
              <button type="button" className="action-button-small" disabled={toggle.isPending} onClick={() => toggle.mutate()}>
                {tool.enabled ? "Disable" : "Enable"}
              </button>
              <button type="button" className="action-button-small" onClick={() => { setDeleteError(null); setDeleting(true); }}>
                Delete
              </button>
            </>
          )}
          <span className="grow" />
          <button type="submit" className="action-button" disabled={save.isPending}>
            {save.isPending ? "Saving…" : tool ? "Save" : "Create tool"}
          </button>
        </div>
      </form>
      {tool && <TestConsole key={tool.hash} client={client} tool={tool} />}
      {deleting && tool && (
        <Modal title={`Delete ${tool.name}?`} onCancel={() => setDeleting(false)}>
          <div className="dialog__form">
            {deleteError && (
              <p role="alert" className="tool-console__error">
                {deleteError}
              </p>
            )}
            <p>A tool that any agent version has used cannot be deleted; disable it instead.</p>
            <div className="dialog__actions">
              <button type="button" className="action-button-small" onClick={() => setDeleting(false)}>
                Cancel
              </button>
              <button type="button" className="action-button" disabled={remove.isPending} onClick={() => remove.mutate()}>
                {remove.isPending ? "Deleting…" : "Delete tool"}
              </button>
            </div>
          </div>
        </Modal>
      )}
    </section>
  );
}

const CATEGORIES: Record<string, string> = {
  invalid_arguments: "The arguments do not match the input schema",
  timeout: "The endpoint did not answer in time",
  unavailable: "The endpoint could not be reached, or its secret is not set",
  result_too_large: "The answer was larger than the response limit",
  tool_error: "The endpoint answered with an error",
};

/** Calls the saved definition once with arguments typed here, the way a model's call would run. Nothing typed is kept. */
function TestConsole({ client, tool }: { client: ApiClient; tool: HTTPToolDTO }) {
  const id = useId();
  const [text, setText] = useState("{}");
  const [error, setError] = useState<string | null>(null);
  const [run, setRun] = useState<Awaited<ReturnType<ApiClient["testHTTPTool"]>> | null>(null);
  const [running, setRunning] = useState(false);

  async function execute() {
    setError(null);
    setRun(null);
    let args: unknown;
    try {
      args = JSON.parse(text || "{}");
    } catch {
      return setError("Arguments must be valid JSON");
    }
    if (typeof args !== "object" || args === null || Array.isArray(args)) return setError("Arguments must be a JSON object");
    setRunning(true);
    try {
      setRun(await client.testHTTPTool(tool.id, args));
    } catch (err) {
      setError(apiMessage(err, "The test could not run"));
    } finally {
      setRunning(false);
    }
  }
  return (
    <section aria-label={`Try ${tool.name}`} className="tool-console">
      <p className="subtext">Calls the saved tool once, outside a conversation, with these arguments. It really sends the request.</p>
      <label htmlFor={id}>JSON arguments</label>
      <textarea id={id} rows={3} value={text} onChange={(e) => setText(e.target.value)} placeholder='{ "order_id": "A-1" }' spellCheck={false} className="mono" />
      <div>
        <button type="button" className="action-button-small" disabled={running} onClick={() => void execute()}>
          {running ? "Running…" : "Run test"}
        </button>
      </div>
      {error && (
        <p role="alert" className="tool-console__error">
          {error}
        </p>
      )}
      {run && (
        <div>
          <p className="tool-console__meta">
            <Status tone={run.status === "completed" ? "good" : "bad"}>{run.status}</Status>
            <span className="subtext">{run.duration_ms} ms</span>
          </p>
          {run.status === "completed" ? (
            <pre className="tool-console__result">
              <code>{JSON.stringify(run.result, null, 2)}</code>
            </pre>
          ) : (
            <p role="status" className="tool-console__error">
              {CATEGORIES[run.error_category ?? ""] ?? "The call failed"}
              {run.message ? `: ${run.message}` : ""}
            </p>
          )}
        </div>
      )}
    </section>
  );
}
