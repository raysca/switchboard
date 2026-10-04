import type {
  ButtonHTMLAttributes,
  InputHTMLAttributes,
  ReactNode,
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
export function LoadingState({ label = "Loading" }: { label?: string }) {
  return (
    <p className="loading" role="status">
      {label}…
    </p>
  );
}
export function ErrorState({
  error,
  onRetry,
}: {
  error: ApiError;
  onRetry?: () => void;
}) {
  return (
    <section className="message-panel message-panel--error" role="alert">
      <h2>Request failed</h2>
      <p>{error.message}</p>
      {error.requestId && (
        <p className="request-id">Request ID: {error.requestId}</p>
      )}
      {onRetry && <button onClick={onRetry}>Try again</button>}
    </section>
  );
}
export function FormField({
  label,
  id,
  ...props
}: { label: string; id: string } & InputHTMLAttributes<HTMLInputElement>) {
  return (
    <label className="form-field" htmlFor={id}>
      <span>{label}</span>
      <input id={id} {...props} />
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
