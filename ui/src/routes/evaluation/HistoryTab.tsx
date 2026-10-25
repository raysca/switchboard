import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { ApiClient, ApiError, EvalHistoryDTO } from "../../api/client";
import { DataTable, EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { Sparkline } from "./Sparkline";

type Row = EvalHistoryDTO[number];
const pct = (v: number) => `${Math.round(v * 100)}%`;
const label = (r: Row) => `v${r.version}, ${new Date(r.ended_at).toLocaleString()}`;
const signed = (v: number) => `${v >= 0 ? "+" : "−"}${Math.abs(v).toFixed(2)}`;

/** Finished, non-draft suite runs, newest first, with a per-scorer compare of any two. */
export function HistoryTab({ client, agentId }: { client: ApiClient; agentId: string }) {
  const history = useQuery({ queryKey: ["eval", agentId, "history"], queryFn: () => client.getEvalHistory(agentId), retry: false });
  const [pick, setPick] = useState<{ a?: string; b?: string }>({});
  if (!history.data) {
    return history.isError ? (
      <ErrorState title="Could not load history" error={history.error as ApiError} onRetry={() => void history.refetch()} reassure />
    ) : (
      <LoadingState label="Loading history" skeleton={4} />
    );
  }
  const rows = history.data;
  if (rows.length === 0) return <EmptyState title="No finished suite runs yet">Run the suite on the Test suite tab. Each finished run appears here.</EmptyState>;

  const scorers = [...new Set(rows.flatMap((r) => r.scorers.map((s) => s.scorer)))];
  const byId = (id?: string) => rows.find((r) => r.run_id === id);
  const b = byId(pick.b) ?? rows[0]!;
  const a = byId(pick.a) ?? rows[1];
  const stat = (r: Row, name: string) => r.scorers.find((s) => s.scorer === name);
  return (
    <>
      <p className="subtext">
        Trend <Sparkline values={rows.map((r) => r.pass_rate ?? 0).reverse()} />
      </p>
      <DataTable
        caption="Finished suite runs"
        columns={["Version", "Finished", "Pass rate", ...scorers]}
        rows={rows.map((r) => [
          `v${r.version}`,
          new Date(r.ended_at).toLocaleString(),
          `${typeof r.pass_rate === "number" ? pct(r.pass_rate) : "—"} (${r.passed} of ${r.total} cases)`,
          ...scorers.map((n) => {
            const s = stat(r, n);
            return s ? `mean ${s.mean.toFixed(2)}: ${s.pass} pass, ${s.fail} fail, ${s.skipped} skipped, ${s.errors} errors` : "—";
          }),
        ])}
      />
      {a && (
        <section aria-label="Compare runs">
          <h2>Compare</h2>
          <div className="row">
            {(
              [
                ["From", a, "a"],
                ["To", b, "b"],
              ] as const
            ).map(([text, cur, key]) => (
              <label key={key} className="form-field">
                <span>{text}</span>
                <select value={cur.run_id} onChange={(e) => setPick({ a: a.run_id, b: b.run_id, [key]: e.target.value })}>
                  {rows.map((r) => (
                    <option key={r.run_id} value={r.run_id}>
                      {label(r)}
                    </option>
                  ))}
                </select>
              </label>
            ))}
          </div>
          <DataTable
            caption="Change in mean score per scorer"
            columns={["Scorer", `v${a.version}`, `v${b.version}`, "Change"]}
            rows={scorers.map((n) => {
              const x = stat(a, n);
              const y = stat(b, n);
              return [n, x ? x.mean.toFixed(2) : "—", y ? y.mean.toFixed(2) : "—", x && y ? signed(y.mean - x.mean) : "—"];
            })}
          />
        </section>
      )}
    </>
  );
}
