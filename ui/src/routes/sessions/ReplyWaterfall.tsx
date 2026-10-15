import { formatMs, parseStages } from "./callStats";

/**
 * One reply as a waterfall: a bar per stage on a shared time axis, so the wait before the model started and
 * between the stages is visible as empty space. The same numbers are the chart's text alternative.
 */
export function ReplyWaterfall({ latencies }: { latencies: unknown }) {
  const w = parseStages(latencies);
  if (!w) return null;
  const pct = (v: number) => `${(v / w.total) * 100}%`;
  const words = w.stages.map((s) => `${s.label} ${Math.round(s.start)} to ${Math.round(s.end)} ms`).join(", ");
  return (
    <span className="waterfall" role="img" aria-label={words}>
      {w.stages.map((s) => (
        <span key={s.label} className="waterfall__row">
          <span className="waterfall__label">
            {s.label} {formatMs(s.end - s.start)}
          </span>
          <span className="waterfall__track">
            <span className="waterfall__bar" data-stage={s.label} style={{ marginLeft: pct(s.start), width: pct(s.end - s.start) }} />
          </span>
        </span>
      ))}
    </span>
  );
}
