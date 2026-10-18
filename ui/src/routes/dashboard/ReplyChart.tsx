import type { SessionDTO } from "../../api/client";
import { formatMs } from "../sessions/callStats";

/** A reply over this is drawn in amber. */
const SLOW_REPLY_MS = 1000;

/** Median reply of the calls that have one, oldest first (the API lists newest first). */
export function replyTimes(sessions: SessionDTO[]): number[] {
  return sessions
    .filter((s) => typeof s.median_reply_ms === "number")
    .map((s) => s.median_reply_ms as number)
    .reverse();
}

function median(values: number[]): number {
  const sorted = [...values].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
}

/** One bar per recent call: how long the agent took to answer. Built from the calls the dashboard already loads. */
export function ReplyChart({ sessions }: { sessions: SessionDTO[] }) {
  const times = replyTimes(sessions);
  if (times.length === 0) return null;
  const top = Math.max(...times, SLOW_REPLY_MS);
  const typical = formatMs(median(times));
  const slowest = formatMs(Math.max(...times));
  return (
    <section className="dash-card reply-chart" aria-label="Reply time">
      <div className="section-row">
        <h2>Reply time</h2>
        <span className="subtext">
          Last {times.length} calls · typical {typical} · slowest {slowest}
        </span>
      </div>
      <div className="reply-chart__plot" role="img" aria-label={`Median reply time over the last ${times.length} calls: typical ${typical}, slowest ${slowest}.`}>
        {times.map((ms, i) => (
          <span key={i} className="reply-chart__bar" data-slow={ms > SLOW_REPLY_MS || undefined} style={{ height: `${Math.max(4, (ms / top) * 100)}%` }} title={formatMs(ms)} />
        ))}
      </div>
      <p className="reply-chart__legend subtext">Each bar is one call, oldest on the left. Amber is slower than {formatMs(SLOW_REPLY_MS)}.</p>
    </section>
  );
}
