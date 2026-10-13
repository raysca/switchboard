import { useState } from "react";
import type {
  ButtonHTMLAttributes,
  InputHTMLAttributes,
  ReactNode,
  Ref,
} from "react";
import type { ApiError } from "../api/client";

export function Status({
  tone = "neutral",
  children,
}: {
  tone?: "neutral" | "good" | "bad";
  children: ReactNode;
}) {
  return (
    <span className={`status status--${tone}`} role="status">
      {children}
    </span>
  );
}
export function EmptyState({
  title,
  children,
}: {
  title: string;
  children: ReactNode;
}) {
  return (
    <section className="message-panel">
      <h2>{title}</h2>
      <p>{children}</p>
    </section>
  );
}
export function LoadingState({ label = "Loading", skeleton }: { label?: string; skeleton?: number }) {
  if (!skeleton) {
    return (
      <p className="loading" role="status">
        {label}…
      </p>
    );
  }
  return (
    <div className="skeleton" aria-busy="true">
      <p className="visually-hidden" role="status">
        {label}…
      </p>
      {Array.from({ length: skeleton }, (_, i) => (
        <div key={i} className="skeleton__row" aria-hidden="true">
          <span style={{ width: `${55 + ((i * 13) % 35)}%` }} />
          <span style={{ width: `${45 + ((i * 17) % 40)}%` }} />
          <span style={{ width: `${60 + ((i * 7) % 30)}%` }} />
        </div>
      ))}
    </div>
  );
}
export function ErrorState({
  error,
  onRetry,
  title = "Request failed",
  reassure = false,
}: {
  error: ApiError;
  onRetry?: () => void;
  title?: string;
  /** A read failure of the management API: say that live calls are not affected. */
  reassure?: boolean;
}) {
  const [copied, setCopied] = useState<"idle" | "yes" | "no">("idle");
  const hasDetails = error.status > 0 || !!error.code || !!error.requestId;
  const copy = async () => {
    const text = `status ${error.status} ${error.code}\nrequest ${error.requestId}\n${error.message}`;
    try {
      await navigator.clipboard.writeText(text);
      setCopied("yes");
    } catch {
      setCopied("no");
    }
  };
  return (
    <section className="message-panel message-panel--error" role="alert">
      <h2>{title}</h2>
      <p>{error.message}</p>
      {reassure && <p>Live calls are not affected. This only stops the console from reading.</p>}
      {hasDetails && (
        <dl className="error-details">
          {error.status > 0 && (
            <>
              <dt>status</dt>
              <dd>
                {error.status} {error.code}
              </dd>
            </>
          )}
          {error.requestId && (
            <>
              <dt>request</dt>
              <dd>{error.requestId}</dd>
            </>
          )}
        </dl>
      )}
      <div className="row">
        {onRetry && <button onClick={onRetry}>Try again</button>}
        {hasDetails && (
          <button type="button" className="action-button-small" onClick={() => void copy()}>
            Copy details
          </button>
        )}
        {copied === "yes" && <span className="subtext">Copied</span>}
        {copied === "no" && <span className="subtext">Could not copy. Select the details and copy them yourself.</span>}
      </div>
    </section>
  );
}
export function FormField({
  label,
  id,
  inputRef,
  ...props
}: {
  label: string;
  id: string;
  inputRef?: Ref<HTMLInputElement>;
} & InputHTMLAttributes<HTMLInputElement>) {
  return (
    <label className="form-field" htmlFor={id}>
      <span>{label}</span>
      <input id={id} ref={inputRef} {...props} />
    </label>
  );
}
export function ActionButton({
  children,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button className="action-button" {...props}>
      {children}
    </button>
  );
}
export function DataTable({
  caption,
  columns,
  rows,
}: {
  caption: string;
  columns: string[];
  rows: ReactNode[][];
}) {
  return (
    <div className="table-scroll">
      <table>
        <caption>{caption}</caption>
        <thead>
          <tr>
            {columns.map((column) => (
              <th scope="col" key={column}>
                {column}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, index) => (
            <tr key={index}>
              {row.map((cell, cellIndex) => (
                <td key={cellIndex}>{cell}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
