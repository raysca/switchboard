import { useState, type KeyboardEvent } from "react";
import type { SessionDTO } from "../../api/client";
import { DataTable } from "../../components/primitives";
import { formatMs } from "../sessions/callStats";

/** A reply over this is drawn striped amber, and its tooltip says so (colour is never the only cue). */
const SLOW_REPLY_MS = 1000;

export type ReplyPoint = { id: string; at: string; ms: number };

/** The calls that have a median reply, oldest first (the API lists newest first). */
export function replyPoints(sessions: SessionDTO[]): ReplyPoint[] {
  return sessions
    .filter((s) => typeof s.median_reply_ms === "number")
    .map((s) => ({ id: s.id, at: s.started_at, ms: s.median_reply_ms as number }))
    .reverse();
}

function median(values: number[]): number {
  const sorted = [...values].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
}

/** A scale from 0 up to a round maximum with at most four steps, always passing the slow line. */
export function replyScale(maxMs: number): { max: number; ticks: number[] } {
  const top = Math.max(maxMs, SLOW_REPLY_MS);
  const step = [500, 1000, 2000, 5000, 10000, 20000].find((s) => Math.ceil(top / s) <= 4) ?? 50000;
  const max = Math.ceil(top / step) * step;
  return { max, ticks: Array.from({ length: max / step + 1 }, (_, i) => i * step) };
}

const when = (iso: string) => new Date(iso).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
const read = (p: ReplyPoint) => `${when(p.at)}: median reply ${formatMs(p.ms)}${p.ms > SLOW_REPLY_MS ? ", slower than " + formatMs(SLOW_REPLY_MS) : ""}`;

/**
 * One bar per recent call: how long the agent took to answer (the call's median reply). Built from the calls the
 * dashboard already loads. Hover a bar, or focus the chart and use the arrow keys, to read a call; the table view
 * lists the same numbers.
 */
export function ReplyChart({ sessions }: { sessions: SessionDTO[] }) {
  const [active, setActive] = useState<number | null>(null);
  const [asTable, setAsTable] = useState(false);
  const points = replyPoints(sessions);
  if (points.length === 0) return null;

  const typical = median(points.map((p) => p.ms));
  const slowest = Math.max(...points.map((p) => p.ms));
  const { max, ticks } = replyScale(slowest);
  const pct = (ms: number) => (ms / max) * 100;
  const summary = `Median reply time over the last ${points.length} calls: typical ${formatMs(typical)}, slowest ${formatMs(slowest)}.`;

  const onKey = (e: KeyboardEvent) => {
    const last = points.length - 1;
    const next: Record<string, number> = { ArrowRight: Math.min(last, (active ?? -1) + 1), ArrowLeft: Math.max(0, (active ?? points.length) - 1), Home: 0, End: last };
    if (e.key === "Escape") setActive(null);
    else if (e.key in next) {
      e.preventDefault();
      setActive(next[e.key]);
    }
  };

  return (
    <section className="dash-card reply-chart" aria-label="Reply time">
      <div className="section-row">
        <h2>Reply time</h2>
        <span className="subtext">
          Last {points.length} calls · typical {formatMs(typical)} · slowest {formatMs(slowest)}
        </span>
        <button type="button" className="action-button-small" aria-pressed={asTable} onClick={() => setAsTable(!asTable)}>
          {asTable ? "Show chart" : "Show table"}
        </button>
      </div>

      {asTable ? (
        <DataTable caption="Median reply time by call, newest first" columns={["Started", "Median reply"]} rows={[...points].reverse().map((p) => [when(p.at), formatMs(p.ms)])} />
      ) : (
        <>
          <div className="reply-chart__frame">
            <div className="reply-chart__axis" aria-hidden="true">
              {ticks.map((t) => (
                <span key={t} className="reply-chart__tick" style={{ bottom: `${pct(t)}%` }}>
                  {t === 0 ? "0" : formatMs(t)}
                </span>
              ))}
            </div>
            <div className="reply-chart__plot" role="group" aria-label={`${summary} Use the arrow keys to read each call.`} tabIndex={0} onKeyDown={onKey} onBlur={() => setActive(null)} onMouseLeave={() => setActive(null)}>
              {ticks.slice(1).filter((t) => t !== SLOW_REPLY_MS).map((t) => (
                <span key={t} className="reply-chart__line" style={{ bottom: `${pct(t)}%` }} aria-hidden="true" />
              ))}
              <span className="reply-chart__line" data-slow style={{ bottom: `${pct(SLOW_REPLY_MS)}%` }} aria-hidden="true">
                <span className="reply-chart__slowlabel">slow: over {formatMs(SLOW_REPLY_MS)}</span>
              </span>
              <span className="reply-chart__line reply-chart__line--typical" style={{ bottom: `${pct(typical)}%` }} aria-hidden="true">
                <span className="reply-chart__typical">typical {formatMs(typical)}</span>
              </span>
              <div className="reply-chart__bars">
                {points.map((p, i) => (
                  <div key={p.id} className="reply-chart__col" data-active={i === active || undefined} onMouseEnter={() => setActive(i)}>
                    <span className="reply-chart__bar" data-slow={p.ms > SLOW_REPLY_MS || undefined} style={{ height: `${Math.max(2, pct(p.ms))}%` }} />
                  </div>
                ))}
              </div>
              {active !== null && (
                <div className="reply-chart__tip" style={{ [active >= points.length / 2 ? "right" : "left"]: `${active >= points.length / 2 ? 100 - ((active + 1) / points.length) * 100 : (active / points.length) * 100}%` }} role="status">
                  {read(points[active])}
                </div>
              )}
            </div>
          </div>
          <p className="reply-chart__legend subtext">
            Each bar is one call, oldest on the left. Striped amber is slower than {formatMs(SLOW_REPLY_MS)} (the dashed amber line).
          </p>
        </>
      )}
    </section>
  );
}
