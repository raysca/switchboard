import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import type { ApiClient, RetentionStatus } from "../../api/client";
import { DataTable } from "../../components/primitives";
import { ago } from "../mcp/mcpStats";

const count = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;
const days = (n: number) => (n > 0 ? `${n} ${n === 1 ? "day" : "days"}` : "Until deleted by hand");

/** The retention policy in force, the last run, and a preview of what the next run would remove. Read-only. */
export function RetentionCard({ client }: { client: ApiClient }) {
  const query = useQuery({ queryKey: ["retention"], queryFn: () => client.getRetention(), retry: false, refetchInterval: 60_000 });
  const [preview, setPreview] = useState<{ counts: NonNullable<RetentionStatus["dry_run"]>; at: Date } | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const dryRun = useMutation({
    mutationFn: () => client.getRetention(true),
    onSuccess: (r) => {
      setPreviewError(null);
      setPreview(r.dry_run ? { counts: r.dry_run, at: new Date() } : null);
    },
    onError: (e) => setPreviewError(e instanceof Error ? e.message : "The preview could not be counted."),
  });

  if (query.isPending) return null;
  if (query.isError) {
    return (
      <section className="server-card" aria-label="Data retention">
        <p className="subtext" role="note">
          Data retention status is unavailable.
        </p>
      </section>
    );
  }
  const r = query.data;
  const last = r.last_run;
  const lastText = !r.enabled
    ? "Off"
    : !last
      ? "No run since the server started"
      : last.error
        ? `Failed ${ago(last.started_at, Date.now())}: ${last.error}`
        : `${ago(last.started_at, Date.now())}: ${last.turns_purged} ${last.turns_purged === 1 ? "turn" : "turns"} blanked, ${last.sessions_deleted} ${last.sessions_deleted === 1 ? "call" : "calls"} deleted`;
  return (
    <section className="server-card" aria-label="Data retention">
      <DataTable
        caption="Data retention"
        columns={["Setting", "Value"]}
        rows={[
          ["Automatic purging", <span key="on" className={`badge badge--${r.enabled ? "good" : "warning"}`}>{r.enabled ? "On" : "Off"}</span>],
          ["Transcripts kept", days(r.enabled ? r.content_retention_days : 0)],
          ["Call records kept", days(r.enabled ? r.metadata_retention_days : 0)],
          ["Recordings kept", r.recording_retention_days > 0 ? days(r.recording_retention_days) : "Not recorded"],
          ["Last run", lastText],
        ]}
      />
      {!r.enabled && <p className="subtext">With automatic purging off, transcripts and call records stay until someone deletes them. Turn it on with <code>retention.enabled</code>.</p>}
      {r.enabled && (
        <div className="retention-preview">
          <button type="button" className="action-button-small" disabled={dryRun.isPending} onClick={() => dryRun.mutate()}>
            {dryRun.isPending ? "Counting…" : "Preview the next run"}
          </button>
          {previewError && (
            <p role="alert" className="tool-console__error">
              {previewError}
            </p>
          )}
          {preview && (
            <p role="status">
              At {preview.at.toLocaleTimeString()}, the next run would blank {count(preview.counts.content_turns, "turn", "turns")} and delete {count(preview.counts.metadata_sessions, "call", "calls")} ({count(preview.counts.metadata_turns, "turn", "turns")}, {count(preview.counts.metadata_tool_calls, "tool call", "tool calls")}, {count(preview.counts.metadata_spans, "span", "spans")}, {count(preview.counts.metadata_events, "event", "events")}). Nothing was changed.
            </p>
          )}
        </div>
      )}
    </section>
  );
}
