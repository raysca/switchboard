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
    heading.current?.focus();
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
      <h2 id={id} ref={heading} tabIndex={-1}>
        {title}
      </h2>
      {children}
    </dialog>
  );
}
