import { useId, useState } from "react";
import type { ApiClient } from "../../api/client";
import { Status } from "../../components/primitives";
import { formatApiError } from "./mcpStats";

const MAX_ARGUMENT_BYTES = 65_536;

type Run = { executionId: string; status: string; durationMs: number; result: unknown };

/** Runs one tool once, outside a conversation, with arguments typed here. Nothing typed is kept. */
export function ToolTestConsole({ client, serverId, tool }: { client: ApiClient; serverId: string; tool: string }) {
  const id = useId();
  const [text, setText] = useState("{}");
  const [run, setRun] = useState<Run | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [running, setRunning] = useState(false);

  async function execute() {
    setError(null);
    setRun(null);
    let parsed: unknown;
    try {
      parsed = JSON.parse(text || "{}");
    } catch {
      setError("Arguments must be valid JSON");
      return;
    }
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      setError("Arguments must be a valid JSON object");
      return;
    }
    if (new TextEncoder().encode(text).length > MAX_ARGUMENT_BYTES) {
      setError("Arguments exceed the 64 KiB execution limit");
      return;
    }
    setRunning(true);
    try {
      const res = await client.testMCPTool(serverId, tool, parsed as Record<string, unknown>);
      setRun({ executionId: res.execution_id, status: res.status, durationMs: res.duration_ms, result: res.result });
    } catch (err) {
      setError(formatApiError(err, "Tool execution failed"));
    } finally {
      setRunning(false);
    }
  }

  return (
    <section aria-label={`Try ${tool}`} className="tool-console">
      <p className="subtext">Runs this tool once, outside a conversation, with the arguments below.</p>
      <label htmlFor={id}>JSON arguments (max 64 KiB)</label>
      <textarea id={id} rows={4} value={text} onChange={(e) => setText(e.target.value)} placeholder='{ "param": "value" }' />
      <div>
        <button type="button" className="action-button-small" disabled={running} onClick={() => void execute()}>
          {running ? "Running…" : "Run tool"}
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
            <Status tone={run.status === "completed" || run.status === "success" ? "good" : "bad"}>{run.status}</Status>
            <span className="subtext">{run.durationMs} ms</span>
            <span className="subtext">
              Execution <code>{run.executionId.slice(0, 8)}…</code>
            </span>
          </p>
          <pre className="tool-console__result">
            <code>{JSON.stringify(run.result, null, 2)}</code>
          </pre>
        </div>
      )}
    </section>
  );
}
