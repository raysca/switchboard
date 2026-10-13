import { useEffect, useId, useRef, useState, type ReactNode } from "react";

export type ModalProps = { title: string; onCancel: () => void; children: ReactNode };

/** A native modal dialog: the page behind is inert, Tab stays inside, Escape cancels. */
export function Modal({ title, onCancel, children }: ModalProps) {
  const id = useId();
  const dialog = useRef<HTMLDialogElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  // Read during the first render, before any effect: under StrictMode the effect runs twice, and by
  // the second run focus is already inside the dialog.
  const [opener] = useState(() => document.activeElement as HTMLElement | null);
  useEffect(() => {
    dialog.current?.showModal();
    // A dialog with a field puts the cursor in it; one with only buttons starts on its title.
    const field = dialog.current?.querySelector<HTMLElement>("input:not([disabled]), select, textarea");
    (field ?? heading.current)?.focus();
    return () => {
      if (opener?.isConnected) opener.focus();
    };
  }, [opener]);
  return (
    <dialog
      ref={dialog}
      aria-labelledby={id}
      className="dialog"
      onCancel={(e) => {
        e.preventDefault();
        onCancel();
      }}
    >
      <div className="dialog__head">
        <h2 id={id} ref={heading} tabIndex={-1}>
          {title}
        </h2>
        <button type="button" className="icon-button" aria-label="Close dialog" onClick={onCancel}>
          ×
        </button>
      </div>
      {children}
    </dialog>
  );
}
