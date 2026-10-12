import type { CallLine } from "./callStats";

/** What was said, each reply labelled by the step that said it; routes are marked where they happened. */
export function CallTranscript({ lines }: { lines: CallLine[] }) {
  const spoken = lines.some((l) => l.kind === "say");
  return (
    <>
      {!spoken && <p className="subtext">No words were recorded for this call.</p>}
      {lines.length > 0 && (
        <ol className="call-transcript" aria-label="Transcript">
          {lines.map((l, i) =>
            l.kind === "route" ? (
              <li key={i} className="call-transcript__route">
                <span aria-hidden="true">⇢</span> Routed {l.from} → {l.to}
              </li>
            ) : (
              <li key={i}>
                <span className={l.who === "caller" ? "call-transcript__who call-transcript__who--caller" : "call-transcript__who"}>
                  {l.label} · {l.at}
                </span>
                <p>
                  {l.text}
                  {l.interrupted && <span className="subtext"> [interrupted]</span>}
                </p>
              </li>
            ),
          )}
        </ol>
      )}
    </>
  );
}
