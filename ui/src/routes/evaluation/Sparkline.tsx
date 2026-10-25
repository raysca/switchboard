const W = 80;
const H = 20;

const pct = (v: number) => `${Math.round(v * 100)}%`;

/** A pass-rate trend, oldest first, values 0 to 1. The text is for screen readers; the drawing is not. */
export function Sparkline({ values }: { values: number[] }) {
  if (values.length === 0) return <span>—</span>;
  const step = values.length > 1 ? W / (values.length - 1) : 0;
  const points = (values.length > 1 ? values : [values[0]!, values[0]!])
    .map((v, i) => `${(i * (values.length > 1 ? step : W)).toFixed(1)},${(H - 2 - v * (H - 4)).toFixed(1)}`)
    .join(" ");
  return (
    <>
      <svg className="sparkline" width={W} height={H} viewBox={`0 0 ${W} ${H}`} aria-hidden="true">
        <polyline points={points} fill="none" strokeWidth="1.5" strokeLinejoin="round" strokeLinecap="round" />
      </svg>
      <span className="visually-hidden">trend: {values.map(pct).join(", ")}</span>
    </>
  );
}
