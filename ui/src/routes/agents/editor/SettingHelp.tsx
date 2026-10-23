import { useId, useRef, useState } from "react";
import { SETTING_HELP, type SettingHelpEntry, type SettingId } from "./settingCopy";

const PARTS: [keyof Omit<SettingHelpEntry, "label">, string][] = [
  ["what", "What it does"],
  ["when", "When to change it"],
  ["defaults", "Default and range"],
  ["careful", "Careful"],
];

/**
 * An "Explain" button that opens a short panel under the field. A button, not a hover tip, so it works
 * from a keyboard, a screen reader and touch. Place it beside the field's label, not inside it: the
 * label's accessible name stays the field's.
 */
export function SettingHelp({ id }: { id: SettingId }) {
  const entry: SettingHelpEntry = SETTING_HELP[id];
  const [open, setOpen] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  const uid = useId();
  const name = `Explain ${entry.label}`;
  return (
    <div className="setting-help">
      <button
        ref={button}
        id={`${uid}-button`}
        type="button"
        className="action-button-small"
        aria-label={name}
        aria-expanded={open}
        aria-controls={`${uid}-panel`}
        onClick={() => setOpen(!open)}
        onKeyDown={(e) => {
          if (e.key !== "Escape" || !open) return;
          e.stopPropagation();
          setOpen(false);
        }}
      >
        <svg className="setting-help__icon" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <circle cx="12" cy="12" r="9" />
          <path d="M12 11v5M12 8h.01" />
        </svg>
        Explain
      </button>
      {open && (
        <section
          id={`${uid}-panel`}
          role="region"
          aria-labelledby={`${uid}-button`}
          className="setting-help__panel"
        >
          <dl>
            {PARTS.map(([key, title]) =>
              entry[key] ? (
                <div key={key}>
                  <dt>{title}</dt>
                  <dd>{entry[key]}</dd>
                </div>
              ) : null,
            )}
          </dl>
        </section>
      )}
    </div>
  );
}
