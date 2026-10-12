import { useEffect, useId, useRef, type ReactNode } from "react";

export type SidePanelProps = {
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
  onToggleWide?: () => void;
  /** Where focus goes back to when the panel closes: the step or button that opened it. */
  returnFocusTo?: HTMLElement | null;
};

/** Slides over the right of its positioned parent; what is behind it stays visible and usable. */
export function SidePanel({ title, onClose, children, footer, wide, onToggleWide, returnFocusTo }: SidePanelProps) {
  const id = useId();
  const panel = useRef<HTMLElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  const back = useRef(returnFocusTo);
  useEffect(() => {
    // An autoFocus field inside (a route just drawn) keeps focus; otherwise start at the title.
    if (!panel.current?.contains(document.activeElement)) heading.current?.focus();
    const target = back.current;
    return () => target?.focus();
  }, []);
  return (
    <aside
      ref={panel}
      aria-labelledby={id}
      className={`side-panel${wide ? " side-panel--wide" : ""}`}
      onKeyDown={(e) => {
        if (e.key === "Escape") {
          e.stopPropagation();
          onClose();
        }
      }}
    >
      <div className="side-panel__head">
        <h2 id={id} ref={heading} tabIndex={-1}>
          {title}
        </h2>
        {onToggleWide && (
          <button type="button" className="action-button-small" aria-pressed={!!wide} onClick={onToggleWide}>
            {wide ? "Narrow" : "Widen"}
          </button>
        )}
        <button type="button" className="icon-button" aria-label={`Close ${title}`} onClick={onClose}>
          ×
        </button>
      </div>
      <div className="side-panel__body">{children}</div>
      {footer && <div className="side-panel__foot">{footer}</div>}
    </aside>
  );
}
